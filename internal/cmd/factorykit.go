package cmd

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/smeltdev/smelt/internal/config"
	"github.com/smeltdev/smelt/internal/iostream"
	"github.com/smeltdev/smelt/internal/sandboximage"
	"github.com/smeltdev/smelt/internal/ui"
)

// kitWarnedFile is the stamp, in the project's data dir, of the day the
// outdated-kit warning was last printed.
const kitWarnedFile = "kit-warned"

// kitNotice says when and how an outdated kit is reported.
type kitNotice struct {
	Root, Dockerfile string
	Now              time.Time
	// EveryTime prints the warning on every call; otherwise at most once a
	// day, so a user who keeps an old kit on purpose is not nagged.
	EveryTime bool
}

// kitWarning is the text to warn with when the committed dockerfile carries an
// older agent kit, "" when it is current, newer or unreadable. It is pure but
// for reading the file.
func kitWarning(root, dockerfile string) string {
	raw, err := os.ReadFile(filepath.Join(root, dockerfile))
	if err != nil {
		return ""
	}
	v, _ := sandboximage.KitVersionOf(string(raw))
	if v >= sandboximage.KitVersion {
		return ""
	}
	return sandboximage.KitOutdatedWarning(dockerfile, v)
}

// noteKitOutdated prints the outdated-kit warning when it is due and returns
// its text, whether or not it was printed, so the run can record it. It never
// fails a run.
func noteKitOutdated(streams iostream.Streams, n kitNotice) string {
	warning := kitWarning(n.Root, n.Dockerfile)
	if warning == "" {
		return ""
	}
	today := n.Now.Format(time.DateOnly)
	stamp := kitStampPath(n.Root)
	if !n.EveryTime && stamp != "" {
		if raw, err := os.ReadFile(stamp); err == nil && strings.TrimSpace(string(raw)) == today {
			return warning
		}
	}
	streams.ErrPrintln(ui.ErrWarning(warning))
	if stamp != "" && os.MkdirAll(filepath.Dir(stamp), 0o700) == nil {
		_ = os.WriteFile(stamp, []byte(today+"\n"), 0o644)
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

// noteUpdateKitRetired is all --update-kit does now: the agent kit is a
// published image that smelt copies in at build time, so there is no section of
// Dockerfile.smelt left to update.
func noteUpdateKitRetired(streams iostream.Streams) {
	streams.ErrPrintln(ui.Warning("--update-kit does nothing now and will be removed: the agent kit is an image smelt copies in when it builds, in place of the kit section of Dockerfile.smelt. Run smelt factory image to build with the current kit."))
}

// kitSource says where the agent kit image comes from.
type kitSource struct {
	Root string
	// FromSource builds it from Root/kit instead of pulling the published one.
	FromSource bool
}

// withAgentKit swaps the plan's Dockerfile kit section for a copy from the kit
// image. A Dockerfile with no kit section is left alone, and then needs no kit
// image, so Docker is not asked for one.
func withAgentKit(ctx context.Context, streams iostream.Streams, builder *sandboximage.Builder, plan sandboximage.Plan, src kitSource) (sandboximage.Plan, error) {
	if _, found := sandboximage.KitVersionOf(plan.Context.Content); !found {
		return plan, nil
	}
	ref, err := kitImageRef(ctx, streams, builder, src)
	if err != nil {
		return plan, err
	}
	composed, err := plan.WithKit(ref)
	if err != nil {
		return plan, &userError{msg: "Could not add the agent kit to the image.", suggestion: suggestionFactoryImage, err: err}
	}
	return composed, nil
}

// kitImageRef resolves the kit image to copy from, telling the user when it is
// the one pulled last because the registry could not be reached.
func kitImageRef(ctx context.Context, streams iostream.Streams, builder *sandboximage.Builder, src kitSource) (string, error) {
	if src.FromSource {
		return buildKitFromSource(ctx, streams, builder, src.Root)
	}
	dir, err := config.AppData()
	if err != nil {
		return "", &userError{msg: "Could not find smelt's data directory.", err: err}
	}
	choice, err := sandboximage.ResolveKit(ctx, builder, sandboximage.KitStatePath(dir))
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
func buildKitFromSource(ctx context.Context, streams iostream.Streams, builder *sandboximage.Builder, root string) (string, error) {
	kitDir := filepath.Join(root, "kit")
	if _, err := os.Stat(filepath.Join(kitDir, "Dockerfile")); err != nil {
		return "", newUserError("--kit-from-source needs kit/Dockerfile in the project, which smelt's own source tree has.").
			withCode("command.invalid_args").
			withSuggestion("Run it from a checkout of smelt, or leave the flag out to pull the published kit.").
			withExitCode(ExitBadArgs).
			wrap(err)
	}
	streams.ErrPrintf("Building the agent kit image %s from %s\n", sandboximage.KitLocalRef(), kitDir)
	id, err := builder.BuildKit(ctx, kitDir, sandboximage.BuildOptions{
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
