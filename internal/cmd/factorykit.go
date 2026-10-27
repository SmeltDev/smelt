package cmd

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/smeltdev/smelt/internal/config"
	"github.com/smeltdev/smelt/internal/iostream"
	"github.com/smeltdev/smelt/internal/sandboximage"
	"github.com/smeltdev/smelt/internal/ui"
)

// kitWarnedFile is the stamp, in the project's data dir, that the unused-kit
// note was printed for this project.
const kitWarnedFile = "kit-warned"

// kitNotice says when and how a leftover kit section is reported.
type kitNotice struct {
	Root, Dockerfile string
}

// kitWarning is the text to warn with when the committed dockerfile still has
// an agent kit section, which smelt ignores, "" when it has none or is
// unreadable. It is pure but for reading the file.
func kitWarning(root, dockerfile string) string {
	if !filepath.IsAbs(dockerfile) {
		dockerfile = filepath.Join(root, dockerfile)
	}
	raw, err := os.ReadFile(dockerfile)
	if err != nil {
		return ""
	}
	v, found := sandboximage.KitVersionOf(string(raw))
	if !found {
		return ""
	}
	return sandboximage.KitOutdatedWarning(dockerfile, v)
}

// noteKitOutdated prints the unused-kit note when it is due and returns its
// text, whether or not it was printed, so the run can record it. It never
// fails a run.
func noteKitOutdated(streams iostream.Streams, n kitNotice) string {
	warning := kitWarning(n.Root, n.Dockerfile)
	if warning == "" {
		return ""
	}
	stamp := kitStampPath(n.Root)
	if stamp != "" {
		if _, err := os.Stat(stamp); err == nil {
			return warning
		}
	}
	streams.ErrPrintln(ui.ErrWarning(warning))
	if stamp != "" && os.MkdirAll(filepath.Dir(stamp), 0o700) == nil {
		_ = os.WriteFile(stamp, []byte("1\n"), 0o644)
	}
	return warning
}

func kitStampPath(root string) string {
	dir, err := config.ProjectDataDir(root)
	if err != nil {
		return ""
	}
	return filepath.Join(dir, kitWarnedFile)
}

// noteUpdateKitRetired is all --update-kit does now: smelt puts its agent kit
// on every sandbox image when it builds one, so there is no section of
// Dockerfile.smelt left to update.
func noteUpdateKitRetired(streams iostream.Streams) {
	streams.ErrPrintln(ui.Warning("--update-kit does nothing now and will be removed: smelt adds its agent kit itself when it builds the image, and the kit section of Dockerfile.smelt is no longer used (you can delete it). Run smelt factory image to build with the current kit."))
}

// kitSource says where the agent kit image comes from.
type kitSource struct {
	Root       string
	ShowNotice bool
	// FromSource rebuilds the embedded kit instead of resolving an existing image.
	FromSource bool
}

// kitProvider is the part of a Builder that gets the kit image.
type kitProvider interface {
	PullKit(ctx context.Context, tagRef string) (string, error)
	Exists(ctx context.Context, tag string) (bool, error)
	BuildKit(ctx context.Context, kitDir string, opts sandboximage.BuildOptions) (string, error)
}

// withAgentKit is the sandbox plan on top of the toolchain plan: a layer that
// copies the kit image in, whatever the Dockerfile says about a kit. It also
// returns the kit's reference, for the run to record.
func withAgentKit(ctx context.Context, streams iostream.Streams, kits kitProvider, toolchain sandboximage.Plan, src kitSource) (plan sandboximage.Plan, kitRef string, err error) {
	choice, err := kitImageRef(ctx, streams, kits, src)
	if err != nil {
		return toolchain, "", err
	}
	return toolchain.SandboxPlanWithKit(choice), choice.Ref, nil
}

// kitImageRef resolves the kit image to copy from, telling the user when it is
// the one pulled last because the registry could not be reached.
func kitImageRef(ctx context.Context, streams iostream.Streams, kits kitProvider, src kitSource) (sandboximage.KitChoice, error) {
	dir, err := config.AppData()
	if err != nil {
		return sandboximage.KitChoice{}, &userError{msg: "Could not find smelt's data directory.", err: err}
	}
	state := sandboximage.KitStatePath(dir)
	var choice sandboximage.KitChoice
	if src.FromSource {
		choice, err = sandboximage.BuildEmbeddedKit(ctx, kits, state, sandboximage.BuildOptions{
			LogPath:  kitBuildLog(src.Root),
			Progress: func(_ iostream.Level, msg string) { streams.ErrPrintln(ui.Dim("  ▸ " + msg)) },
		}, nil)
	} else {
		choice, err = sandboximage.ResolveKit(ctx, kits, state)
	}
	if choice.Notice != "" && (sandboximage.ClaimKitNotice(state, choice.Ref) || src.ShowNotice) {
		streams.ErrPrintln(ui.ErrWarning(choice.Notice))
	}
	if errors.Is(err, sandboximage.ErrKitUnavailable) {
		return sandboximage.KitChoice{}, newUserError("Could not get the selected agent kit image.").
			withCode("factory.kit_unavailable").
			withSuggestion("Check Docker and the build log, or retry with: smelt factory image --kit-from-source").
			wrap(err)
	}
	if err != nil {
		return sandboximage.KitChoice{}, &userError{msg: "Could not get the agent kit image.", suggestion: suggestionFactoryImage, err: err}
	}
	if choice.FoundOnDisk {
		streams.ErrPrintln(ui.ErrWarning(fmt.Sprintf("Could not pull the agent kit (%v); using the kit found on disk: %s (kit v%d)", choice.PullErr, choice.Ref, choice.Kit)))
	} else if choice.Offline {
		streams.ErrPrintln(ui.ErrWarning(fmt.Sprintf("Could not pull the agent kit (%v); using the kit pulled last: %s (kit v%d)", choice.PullErr, choice.Ref, choice.Kit)))
	}
	return choice, nil
}

// kitBuildLog is where the full log of a kit build is kept.
func kitBuildLog(root string) string {
	dir, err := config.ProjectDataDir(root)
	if err != nil {
		return ""
	}
	return filepath.Join(dir, "kit-build.log")
}
