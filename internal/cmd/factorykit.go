package cmd

import (
	"context"
	"errors"
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
	// EveryTime prints the note on every call; otherwise once per project, so
	// a user who keeps the section is not nagged.
	EveryTime bool
}

// kitWarning is the text to warn with when the committed dockerfile still has
// an agent kit section, which smelt ignores, "" when it has none or is
// unreadable. It is pure but for reading the file.
func kitWarning(root, dockerfile string) string {
	raw, err := os.ReadFile(filepath.Join(root, dockerfile))
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
	if !n.EveryTime && stamp != "" {
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
	Root string
	// FromSource builds it from Root/kit instead of pulling the published one.
	FromSource bool
}

// kitProvider is the part of a Builder that gets the kit image.
type kitProvider interface {
	PullKit(ctx context.Context, tagRef string) (string, error)
	Exists(ctx context.Context, tag string) (bool, error)
	BuildKit(ctx context.Context, kitDir string, opts sandboximage.BuildOptions) (string, error)
}

// withAgentKit is the sandbox plan on top of the toolchain plan: a layer that
// copies the kit image in, whatever the Dockerfile says about a kit.
func withAgentKit(ctx context.Context, streams iostream.Streams, kits kitProvider, toolchain sandboximage.Plan, src kitSource) (sandboximage.Plan, error) {
	ref, err := kitImageRef(ctx, streams, kits, src)
	if err != nil {
		return toolchain, err
	}
	return toolchain.SandboxPlan(ref), nil
}

// kitImageRef resolves the kit image to copy from, telling the user when it is
// the one pulled last because the registry could not be reached.
func kitImageRef(ctx context.Context, streams iostream.Streams, kits kitProvider, src kitSource) (string, error) {
	if src.FromSource {
		return buildKitFromSource(ctx, streams, kits, src.Root)
	}
	dir, err := config.AppData()
	if err != nil {
		return "", &userError{msg: "Could not find smelt's data directory.", err: err}
	}
	choice, err := sandboximage.ResolveKit(ctx, kits, sandboximage.KitStatePath(dir))
	if errors.Is(err, sandboximage.ErrKitUnavailable) {
		return "", newUserError("Could not get the agent kit image " + sandboximage.KitTagRef() + ".").
			withCode("factory.kit_unavailable").
			withSuggestion("Check the network, or build the kit from smelt's source tree with: smelt factory image --kit-from-source").
			wrap(err)
	}
	if err != nil {
		return "", &userError{msg: "Could not get the agent kit image.", suggestion: suggestionFactoryImage, err: err}
	}
	if choice.Offline {
		streams.ErrPrintln(ui.ErrWarning("Could not pull " + sandboximage.KitTagRef() + " (" + choice.PullErr.Error() + "); using the kit pulled last: " + choice.Ref))
	}
	return choice.Ref, nil
}

// buildKitFromSource builds the kit image from root/kit and returns its ID.
func buildKitFromSource(ctx context.Context, streams iostream.Streams, kits kitProvider, root string) (string, error) {
	kitDir := filepath.Join(root, "kit")
	if _, err := os.Stat(filepath.Join(kitDir, "Dockerfile")); err != nil {
		return "", newUserError("--kit-from-source needs kit/Dockerfile in the project, which smelt's own source tree has.").
			withCode("command.invalid_args").
			withSuggestion("Run it from a checkout of smelt, or leave the flag out to pull the published kit.").
			withExitCode(ExitBadArgs).
			wrap(err)
	}
	streams.ErrPrintf("Building the agent kit image %s from %s\n", sandboximage.KitLocalRef(), kitDir)
	id, err := kits.BuildKit(ctx, kitDir, sandboximage.BuildOptions{
		LogPath:  kitBuildLog(root),
		Progress: func(_ iostream.Level, msg string) { streams.ErrPrintln(ui.Dim("  ▸ " + msg)) },
	})
	var buildErr *sandboximage.BuildError
	if errors.As(err, &buildErr) {
		return "", newUserError("Building the agent kit image failed.").
			withCode("factory.kit_build_failed").
			withDetail(buildErr.LogTail).
			wrap(err)
	}
	if err != nil {
		return "", &userError{msg: "Could not build the agent kit image.", err: err}
	}
	return id, nil
}

// kitBuildLog is where the full log of a kit build is kept.
func kitBuildLog(root string) string {
	dir, err := config.ProjectDataDir(root)
	if err != nil {
		return ""
	}
	return filepath.Join(dir, "kit-build.log")
}
