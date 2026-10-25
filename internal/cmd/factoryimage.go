package cmd

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"github.com/spf13/cobra"

	"github.com/smeltdev/smelt/internal/brand"
	"github.com/smeltdev/smelt/internal/codexauth"
	"github.com/smeltdev/smelt/internal/config"
	"github.com/smeltdev/smelt/internal/harness"
	"github.com/smeltdev/smelt/internal/harness/all"
	"github.com/smeltdev/smelt/internal/iostream"
	"github.com/smeltdev/smelt/internal/sandboximage"
	"github.com/smeltdev/smelt/internal/sidecar/docker"
	"github.com/smeltdev/smelt/internal/smeltd"
	"github.com/smeltdev/smelt/internal/ui"
)

// suggestionFactoryImage is what to run after a failed or stale image build.
const suggestionFactoryImage = "Fix Dockerfile.smelt if needed, then run: smelt factory image"

// imageBuildLog is where the full log of a sandbox image build is kept.
func imageBuildLog(root string) string {
	dir, err := config.ProjectDataDir(root)
	if err != nil {
		return ""
	}
	return filepath.Join(dir, "image-build.log")
}

// imageBuild says what buildSandboxImage builds.
type imageBuild struct {
	// Dockerfile is absolute for Kind ToolchainCaller, repo-relative otherwise.
	Dockerfile string
	Kind       sandboximage.ToolchainKind
	// Reason is shown when it builds.
	Reason  string
	Rebuild bool
	// ReadyTimeout bounds the wait for Docker to start; zero is the default.
	ReadyTimeout time.Duration
	// KitFromSource builds the agent kit image from the repo's kit/ instead of
	// pulling the published one.
	KitFromSource bool
}

// builtImage is a sandbox image that is on the Docker host.
type builtImage struct {
	Tag string
	// Sandbox says where its toolchain came from, for the run to record.
	Sandbox *smeltd.FactorySandbox
}

// toolchainPlan plans the toolchain image of b: from the caller's file with the
// repo as the build context, or from a Dockerfile in the repo.
func (b imageBuild) toolchainPlan(root string) (sandboximage.Plan, error) {
	if b.Kind == sandboximage.ToolchainCaller {
		return sandboximage.NewPlanFromFile(root, b.Dockerfile)
	}
	return sandboximage.NewPlan(root, b.Dockerfile)
}

// buildSandboxImage builds the image for b.Dockerfile unless its tag is already
// on the Docker host.
func buildSandboxImage(ctx context.Context, streams iostream.Streams, root string, b imageBuild) (builtImage, error) {
	plan, err := b.toolchainPlan(root)
	if err != nil {
		return builtImage{}, newUserError(fmt.Sprintf("Could not read %s.", b.Dockerfile)).
			withCode("factory.image_dockerfile").
			withSuggestion("Run smelt factory image to write it, or smelt init.").
			withExitCode(ExitBadArgs).
			wrap(err)
	}
	// Wait out a starting engine first: it would answer the build's image
	// lookup with "no such image" and trigger a needless rebuild.
	provider, err := docker.New()
	if err != nil {
		return builtImage{}, dockerUnreachable(err)
	}
	defer func() { _ = provider.Close() }()
	provider.ReadyTimeout = b.ReadyTimeout
	if err := provider.WaitEngine(ctx, func(_ iostream.Level, msg string) { streams.ErrPrintln(ui.Dim("  ▸ " + msg)) }); err != nil {
		return builtImage{}, dockerUnreachable(err)
	}
	cfg, err := config.LoadProjectConfig(root)
	if err != nil {
		return builtImage{}, err
	}
	provider.CacheKey = config.ProjectHash(root)
	provider.ProjectPath = root
	provider.MinFreeBytes = cfg.Factory.MinFreeBytes()
	provider.CacheMaxBytes = cfg.Factory.CacheMaxBytes()
	if err := provider.CheckStorage(ctx); err != nil {
		return builtImage{}, &userError{msg: err.Error(), err: err}
	}
	report, reportErr := provider.ReportStorage(ctx)
	printStorageReport(streams.Out, report, reportErr)
	if reportErr != nil {
		return builtImage{}, &userError{msg: reportErr.Error(), err: reportErr}
	}
	builder, err := sandboximage.Connect()
	if err != nil {
		return builtImage{}, dockerUnreachable(err)
	}
	defer func() { _ = builder.Close() }()
	return ensureSandboxImage(ctx, streams, root, b, plan, builder)
}

// imageEnsurer is the part of a sandboximage.Builder that buildSandboxImage
// needs once Docker is reachable.
type imageEnsurer interface {
	kitProvider
	Ensure(ctx context.Context, p sandboximage.Plan, opts sandboximage.EnsureOptions) (bool, error)
}

// ensureSandboxImage puts the kit on top of the toolchain plan, builds what is
// missing and reports where the toolchain came from.
func ensureSandboxImage(ctx context.Context, streams iostream.Streams, root string, b imageBuild, toolchain sandboximage.Plan, builder imageEnsurer) (builtImage, error) {
	dockerfile, reason, rebuild := b.Dockerfile, b.Reason, b.Rebuild
	plan, kitRef, err := withAgentKit(ctx, streams, builder, toolchain, kitSource{Root: root, FromSource: b.KitFromSource})
	if err != nil {
		return builtImage{}, err
	}

	_, err = builder.Ensure(ctx, plan, sandboximage.EnsureOptions{
		Rebuild: rebuild,
		LogPath: imageBuildLog(root),
		Building: func(tag string) {
			streams.ErrPrintf("Building factory image %s (%s)\n", tag, reason)
		},
		Progress: func(_ iostream.Level, msg string) { streams.ErrPrintln(ui.Dim("  ▸ " + msg)) },
	})
	var buildErr *sandboximage.BuildError
	switch {
	case errors.As(err, &buildErr):
		return builtImage{}, newUserError(fmt.Sprintf("Building %s failed.", buildErr.Tag)).
			withCode("factory.image_build_failed").
			withDetail(buildErr.LogTail).
			withSuggestion(suggestionFactoryImage + logPathHint(buildErr.LogPath)).
			wrap(err)
	case err != nil:
		return builtImage{}, &userError{msg: "Could not build the factory image.", suggestion: suggestionFactoryImage, err: err}
	}
	return builtImage{Tag: plan.Tag, Sandbox: &smeltd.FactorySandbox{
		Dockerfile:   dockerfile,
		Source:       string(b.Kind),
		ToolchainTag: plan.Toolchain.Tag,
		Kit:          sandboximage.KitLabel(strconv.Itoa(plan.Kit), kitRef),
	}}, nil
}

func logPathHint(path string) string {
	if path == "" {
		return ""
	}
	return " (full log: " + path + ")"
}

// resolveBuildSettings is resolveFactorySettings plus, for a toolchain Dockerfile
// image, the build of that image when it is missing. The daemon only ever sees
// the resulting tag.
func resolveBuildSettings(ctx context.Context, streams iostream.Streams, root string, flags factorySettings, set factoryExplicit, cfg *config.ProjectConfig) (factorySettings, error) {
	if _, statErr := os.Stat(filepath.Join(root, sandboximage.DefaultDockerfile)); statErr == nil {
		flags.RepoDockerfile = true
	}
	var err error
	if flags.SandboxDockerfile, err = absSandboxDockerfile(flags.SandboxDockerfile); err != nil {
		return factorySettings{}, err
	}
	s, err := resolveFactorySettings(flags, set, cfg)
	if err == nil && s.ImageSource == imageFromConfig && s.Backend == config.BackendDocker {
		s.KitWarning = noteStaleImage(ctx, streams, s.Image, cfg)
	}
	if err != nil || s.ImageSource != imageFromDockerfile {
		return s, err
	}
	s.KitWarning = noteKitOutdated(streams, kitNotice{Root: root, Dockerfile: s.Dockerfile})
	built, err := buildImage(ctx, streams, root, imageBuild{Dockerfile: s.Dockerfile, Kind: s.Toolchain, Reason: s.Dockerfile + " changed", ReadyTimeout: flags.ReadyTimeout})
	s.Image, s.Sandbox = built.Tag, built.Sandbox
	return s, err
}

// buildImage builds a sandbox image. It is a variable so tests can fake Docker.
var buildImage = buildSandboxImage

// inspectUserImage reads a user-managed image from the local Docker daemon. It
// is a variable so tests can fake the daemon.
var inspectUserImage = func(ctx context.Context, tag string) (sandboximage.ImageInfo, error) {
	return dockerImages{}.InspectImage(ctx, tag)
}

// noteStaleImage warns once when the user-managed image carries an older agent
// kit than this smelt expects, and returns the warning for the run to record.
// It never fails a run: an image that cannot be inspected is left alone.
func noteStaleImage(ctx context.Context, streams iostream.Streams, tag string, cfg *config.ProjectConfig) string {
	info, err := inspectUserImage(ctx, tag)
	dockerfile := ""
	if cfg != nil && cfg.Factory != nil {
		dockerfile = cfg.Factory.Dockerfile
	}
	warning := sandboximage.StaleImageWarning(tag, info, err, dockerfile)
	if warning != "" {
		streams.ErrPrintln(ui.ErrWarning(warning))
	}
	return warning
}

func newFactoryImageCmd() *cobra.Command {
	var rebuild, regenerate, updateKitFlag, kitFromSource, yes, keepRepair bool
	var sandboxDockerfile string
	cmd := &cobra.Command{
		Use:   "image",
		Short: "Build the Docker image factory runs execute in",
		Long: `Build the image described by factory.dockerfile (Dockerfile.smelt, written by
smelt init) if it is not on this Docker host yet. smelt factory build does the
same before a run; use this command after editing Dockerfile.smelt by hand or
when the build failed.

The build has two stages. The toolchain image is built from the Dockerfile (and
only the part above an old "# --- smelt agent kit" section, which is no longer
used); its tag is derived from the Dockerfile and the dependency files it COPYs,
so changing either makes the next build a new image. The sandbox image is built
on top of it from a Dockerfile smelt generates, which copies in the agent kit;
its tag is derived from the toolchain tag and the kit, so a new kit rebuilds
only that layer. The Docker layer cache keeps unchanged layers cheap.

Dockerfile.smelt is never overwritten without asking. If it is missing, it is
generated from detection and the CI config.

--sandbox-dockerfile PATH builds the toolchain from a Dockerfile anywhere on this
machine instead of Dockerfile.smelt or factory.dockerfile. The project stays
the build context, so COPY go.mod go.sum finds the project's files. smelt never
writes the file into the project and never repairs or regenerates it.

--rebuild builds even when the tag exists, without the layer cache.
--regenerate renders Dockerfile.smelt again, shows the diff and asks before
writing it. It re-runs detection, so hand edits are lost.

The agent CLIs (Claude, Codex, Grok) and lizard come from the agent kit image,
ghcr.io/smeltdev/smelt-kit, which smelt pulls by tag, resolves to a digest and
copies into every sandbox image, whatever Dockerfile.smelt says. When the pull
fails, smelt says so and uses the kit it pulled last; with none, it fails and
names --kit-from-source. Dockerfile.smelt needs no update when the kit changes.
The kit also bundles git, bash, pkill and sha256sum; it needs no package installs.

--kit-from-source builds the kit image from the kit/ directory of the project
(smelt's own source tree) and uses it instead of pulling, for development and
air-gapped machines.
--update-kit does nothing now and is kept for one release: the kit is no longer
a section to update. It exits 0 and says why.
--regenerate and --update-kit are mutually exclusive.
The image is verified in a container over a copy of the repo that has its git
history and remote refs, like a factory run. A gate that fails for a reason
other than the image (git refs, the network, a flaky test) is reported as a
warning and the command still succeeds. When the image itself fails, Claude may
be asked to repair Dockerfile.smelt. Each attempt's diff is shown; after a
failed repair Dockerfile.smelt is restored to what it was and the last attempt
is saved as Dockerfile.smelt.repair. --keep-repair leaves the last attempt in
Dockerfile.smelt instead.`,
		Args:         cobra.NoArgs,
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if regenerate && updateKitFlag {
				return newUserError("--regenerate and --update-kit cannot be used together.").
					withCode("command.invalid_args").
					withSuggestion("Drop --update-kit: it does nothing now. --regenerate renders the whole Dockerfile again.").
					withExitCode(ExitBadArgs).
					withoutDetail()
			}
			ctx := cmd.Context()
			streams := iostream.FromCmd(cmd)
			root, err := sessionProjectRoot(ctx)
			if err != nil {
				return err
			}
			cfg, err := config.LoadProjectConfig(root)
			if err != nil {
				return &userError{msg: msgValidateNotConfigured, suggestion: suggestionRunInit, err: err, blocked: true}
			}
			caller, err := absSandboxDockerfile(sandboxDockerfile)
			if err != nil {
				return err
			}
			if caller != "" && regenerate {
				return newUserError("--sandbox-dockerfile and --regenerate cannot be used together.").
					withCode("command.invalid_args").
					withSuggestion("--regenerate rewrites Dockerfile.smelt in the project; smelt never rewrites a file given with --sandbox-dockerfile.").
					withExitCode(ExitBadArgs).
					withoutDetail()
			}
			configured := ""
			if cfg.Factory != nil {
				configured = cfg.Factory.Dockerfile
			}
			src, _ := sandboximage.ResolveToolchain(sandboximage.ToolchainInputs{Caller: caller, Config: configured, RepoHasDefault: true})
			if updateKitFlag {
				noteUpdateKitRetired(streams)
				return nil
			}
			if src.Kind != sandboximage.ToolchainCaller {
				if err := prepareDockerfile(ctx, streams, root, src.Path, cfg, regenerate); err != nil {
					return err
				}
			}
			noteKitOutdated(streams, kitNotice{Root: root, Dockerfile: src.Path})
			tag, err := readyImage(ctx, streams, root, src, cfg, imageOptions{Rebuild: rebuild, KitFromSource: kitFromSource, KeepRepair: keepRepair})
			if err != nil {
				return err
			}
			ready := "Factory image ready: " + tag
			if src.Kind == sandboximage.ToolchainCaller {
				ready += " (toolchain from " + src.Path + ")"
			}
			streams.ErrPrintln(ui.Success(ready))
			return nil
		},
	}
	cmd.Flags().BoolVar(&rebuild, "rebuild", false, "build even when the image exists, without the layer cache")
	cmd.Flags().BoolVar(&regenerate, "regenerate", false, "render the Dockerfile again from detection, show the diff and confirm before writing")
	cmd.Flags().BoolVar(&updateKitFlag, "update-kit", false, "deprecated: does nothing, the agent kit is supplied as an image at build time")
	cmd.Flags().BoolVar(&kitFromSource, "kit-from-source", false, "build the agent kit image from kit/ in the project instead of pulling it")
	cmd.Flags().StringVar(&sandboxDockerfile, "sandbox-dockerfile", "", "build the toolchain from this Dockerfile, anywhere on this machine; the project stays the build context")
	cmd.Flags().BoolVar(&yes, "yes", false, "deprecated: does nothing")
	cmd.Flags().BoolVar(&keepRepair, "keep-repair", false, "leave the last repair attempt in the Dockerfile when the repair fails, instead of restoring it")
	return cmd
}

// absSandboxDockerfile makes the --sandbox-dockerfile value absolute against the
// working directory and checks that it is a file; "" stays "".
func absSandboxDockerfile(path string) (string, error) {
	if path == "" {
		return "", nil
	}
	abs, err := filepath.Abs(path)
	if err == nil {
		var info os.FileInfo
		if info, err = os.Stat(abs); err == nil && info.IsDir() {
			err = errors.New("is a directory")
		}
	}
	if err != nil {
		return "", newUserError(fmt.Sprintf("Could not read %s.", path)).
			withCode("command.invalid_args").
			withSuggestion("--sandbox-dockerfile takes the path of a Dockerfile.").
			withExitCode(ExitBadArgs).
			wrap(err)
	}
	return abs, nil
}

// prepareDockerfile makes sure dockerfile exists: it writes a generated one if
// missing, and with regenerate offers to replace the existing one.
func prepareDockerfile(ctx context.Context, streams iostream.Streams, root, dockerfile string, cfg *config.ProjectConfig, regenerate bool) error {
	_, statErr := os.Stat(filepath.Join(root, dockerfile))
	exists := statErr == nil
	if exists && !regenerate {
		return nil
	}
	gen, err := generateDockerfile(ctx, root, cfg)
	if err != nil {
		return err
	}
	content := gen.Dockerfile
	if !exists {
		if err := sandboximage.WriteDockerfile(root, dockerfile, content); err != nil {
			return &userError{msg: "Could not write " + dockerfile + ".", err: err}
		}
		streams.ErrPrintln(ui.Success("Wrote " + dockerfile))
		return nil
	}
	diff, err := sandboximage.Diff(ctx, root, dockerfile, content)
	if err != nil {
		return &userError{msg: "Could not compare " + dockerfile + ".", err: err}
	}
	if diff == "" {
		streams.ErrPrintln(dockerfile + " already matches what smelt would generate.")
		return nil
	}
	streams.ErrPrintln(diff)
	if !isInteractive() {
		return newUserError(dockerfile + " differs from the regenerated one.").
			withCode("command.invalid_args").
			withSuggestion("Run smelt factory image --regenerate in a terminal to confirm the overwrite.").
			withExitCode(ExitBadArgs).
			withoutDetail()
	}
	ok, err := ui.Confirm("Overwrite "+dockerfile+"?", false)
	if err != nil || !ok {
		return newUserError(dockerfile + " left as it was.").withCode("command.cancelled").withExitCode(ExitGeneral).withoutDetail()
	}
	if err := os.WriteFile(filepath.Join(root, dockerfile), []byte(content), 0o644); err != nil {
		return &userError{msg: "Could not write " + dockerfile + ".", err: err}
	}
	streams.ErrPrintln(ui.Success("Wrote " + dockerfile))
	return nil
}

// generateDockerfile renders a Dockerfile for root from detection and the CI
// config, preferring the environment already saved in cfg.
func generateDockerfile(ctx context.Context, root string, cfg *config.ProjectConfig) (sandboximage.Generated, error) {
	builder, err := sandboximage.Connect()
	if err != nil {
		return sandboximage.Generated{}, dockerUnreachable(err)
	}
	defer func() { _ = builder.Close() }()
	gen, err := builder.Generate(ctx, root, cfg.Environment)
	if err != nil {
		return gen, &userError{msg: "Could not generate a Dockerfile for this repo.", err: err}
	}
	return gen, nil
}

// imageAttempt is one build-and-verify of the image: Failure is empty when it
// is ready, and otherwise says what went wrong in terms Claude can act on.
type imageAttempt struct {
	Tag     string
	Failure string
}

// imageOptions steer readyImage and attemptImage.
type imageOptions struct {
	// Rebuild builds even when the tag exists, without the layer cache.
	Rebuild bool
	// KitFromSource builds the agent kit image from kit/ instead of pulling it.
	KitFromSource bool
	// KeepRepair leaves the last failed repair in the Dockerfile.
	KeepRepair bool
}

// attemptImage builds the image and verifies it. A failed build or verification
// is reported in the result; only a problem with Docker itself is an error.
func attemptImage(ctx context.Context, streams iostream.Streams, root string, src sandboximage.ToolchainSource, cfg *config.ProjectConfig, opts imageOptions) (imageAttempt, error) {
	dockerfile := src.Path
	built, err := buildSandboxImage(ctx, streams, root, imageBuild{Dockerfile: dockerfile, Kind: src.Kind, Reason: dockerfile + " changed", Rebuild: opts.Rebuild, KitFromSource: opts.KitFromSource})
	tag := built.Tag
	var buildErr *sandboximage.BuildError
	if errors.As(err, &buildErr) {
		return imageAttempt{Failure: "docker build failed:\n" + buildErr.LogTail}, nil
	}
	if err != nil {
		return imageAttempt{}, err
	}
	provider, err := docker.New()
	if err != nil {
		return imageAttempt{}, dockerUnreachable(err)
	}
	defer func() { _ = provider.Close() }()
	provider.ProjectPath = root
	provider.MinFreeBytes = cfg.Factory.MinFreeBytes()
	provider.CacheMaxBytes = cfg.Factory.CacheMaxBytes()
	provider.ManagedDockerfile = dockerfile
	status := func(_ iostream.Level, msg string) { streams.ErrPrintln(ui.Dim("  ▸ " + msg)) }
	if err := provider.Ready(ctx, tag, status); err != nil {
		return imageAttempt{}, dockerUnreachable(err)
	}
	res, err := sandboximage.Verify(ctx, provider, sandboximage.VerifyOptions{
		Dir: root, Image: tag, Install: cfg.InstallCommand(), Gate: sandboximage.PickGate(cfg.Commands),
		Progress: status, Codex: codexSmoke(),
	})
	if err != nil {
		return imageAttempt{}, &userError{msg: "Could not verify the factory image.", suggestion: suggestionFactoryImage, err: err}
	}
	return judgeVerification(streams, tag, res), nil
}

// judgeVerification turns a verification result into an attempt. A gate that
// failed for a reason that is not the image's is a warning, not a failure: the
// image is ready, and repairing the Dockerfile would not help.
func judgeVerification(streams iostream.Streams, tag string, res sandboximage.VerifyResult) imageAttempt {
	switch {
	case res.OK():
		return imageAttempt{Tag: tag}
	case res.ImageFault():
		return imageAttempt{Tag: tag, Failure: res.Describe()}
	}
	streams.ErrPrintln(ui.Warning(fmt.Sprintf("the image is ready; the gate %s failed in verification: %s", res.Gate, res.FirstErrorLine())))
	return imageAttempt{Tag: tag}
}

// codexSmoke is the Codex turn the kit check runs when this machine already has
// a Codex login, and nil when it has none. It never prompts or logs in: an
// expired login only makes the turn unconfirmed.
func codexSmoke() *sandboximage.CodexSmoke {
	login, err := codexauth.Find()
	if err != nil {
		return nil
	}
	cred, err := login.Credential()
	if err != nil {
		return nil
	}
	h, err := all.ByName(harness.Codex)
	if err != nil {
		return nil
	}
	return &sandboximage.CodexSmoke{Harness: h, Credential: cred}
}

// readyImage builds and verifies the image. When that fails in a terminal with
// claude installed, it offers to let Claude repair Dockerfile.smelt, up to
// sandboximage.MaxRepairAttempts times. A Dockerfile the caller named is never
// repaired: it is not the project's to edit.
func readyImage(ctx context.Context, streams iostream.Streams, root string, src sandboximage.ToolchainSource, cfg *config.ProjectConfig, opts imageOptions) (string, error) {
	dockerfile := src.Path
	first, err := attemptImage(ctx, streams, root, src, cfg, opts)
	if err != nil || first.Failure == "" {
		return first.Tag, err
	}
	failed := newUserError("The factory image failed its checks.").
		withCode("factory.image_verify_failed").
		withDetail(first.Failure).
		withSuggestion(suggestionFactoryImage + logPathHint(imageBuildLog(root)))
	if src.Kind == sandboximage.ToolchainCaller {
		return "", failed
	}
	state := sandboximage.RepairState{Interactive: isInteractive(), HaveClaude: sandboximage.HaveClaude()}
	if sandboximage.DecideRepair(state) == sandboximage.RepairAsk {
		ok, err := ui.Confirm(fmt.Sprintf("Let Claude try to fix %s? (up to %d attempts)", dockerfile, sandboximage.MaxRepairAttempts), false)
		warnPromptError(streams, err)
		state.Consent = &ok
	}
	if sandboximage.DecideRepair(state) != sandboximage.RepairAttempt {
		return "", failed
	}
	var token string
	if rc, err := config.Resolve("", "", false); err == nil {
		token = rc.ClaudeOAuthToken
	}
	var final imageAttempt
	return repairImage(ctx, streams, imageRepair{
		Root: root, Dockerfile: dockerfile, Failure: first.Failure, KeepRepair: opts.KeepRepair,
		Fixer: sandboximage.ClaudeFixer{Token: token},
		Retry: func(ctx context.Context) (string, error) {
			var err error
			final, err = attemptImage(ctx, streams, root, src, cfg, imageOptions{KitFromSource: opts.KitFromSource})
			return final.Failure, err
		},
		Tag: func() string { return final.Tag },
	}, failed)
}

// imageRepair is one repair of the Dockerfile.
type imageRepair struct {
	Root, Dockerfile string
	Failure          string
	Fixer            sandboximage.Fixer
	// Retry rebuilds and verifies, returning the new failure or "".
	Retry func(ctx context.Context) (string, error)
	// Tag is the image tag of the last Retry.
	Tag func() string
	// KeepRepair leaves the last attempt in the Dockerfile when the repair fails.
	KeepRepair bool
}

// repairImage runs the repair loop and reports the outcome. failed is the
// error to return when the repair does not work. A repair that does not work
// never leaves the Dockerfile edited, unless asked to: the original is put
// back and the last attempt is saved next to it.
func repairImage(ctx context.Context, streams iostream.Streams, r imageRepair, failed error) (string, error) {
	path := filepath.Join(r.Root, r.Dockerfile)
	original, err := os.ReadFile(path)
	if err != nil {
		return "", &userError{msg: "Could not read " + r.Dockerfile + ".", err: err}
	}
	res, err := sandboximage.RunRepair(ctx, sandboximage.RepairOptions{
		Dir: r.Root, Dockerfile: r.Dockerfile, Failure: r.Failure, Fixer: r.Fixer, Retry: r.Retry,
		OnAttempt: func(n int) { streams.ErrPrintf("Repair attempt %d of %d...\n", n, sandboximage.MaxRepairAttempts) },
		OnDiff:    func(_ int, diff string) { streams.ErrPrintln(diff) },
	})
	if err == nil && res.Fixed {
		streams.ErrPrintln(ui.Success("Fixed in " + fmt.Sprint(res.Attempts) + " attempt(s). Commit " + r.Dockerfile + " so the team shares it."))
		return r.Tag(), nil
	}
	if note := settleFailedRepair(streams, r, path, original, res.Last); note != nil {
		return "", note
	}
	switch {
	case errors.Is(err, sandboximage.ErrOtherFilesChanged):
		return "", newUserError("Claude changed files other than " + r.Dockerfile + ".").
			withCode("factory.image_repair_scope").
			withSuggestion("Review the changes with git status and revert what you don't want.").
			withoutDetail()
	case err != nil:
		return "", &userError{msg: "The repair could not finish.", suggestion: suggestionFactoryImage, err: err}
	}
	return "", failed
}

// settleFailedRepair saves the last attempt as <dockerfile>.repair and puts
// the original Dockerfile back unless the repair is to be kept. It returns an
// error only when the Dockerfile could not be restored, which must not pass
// silently.
func settleFailedRepair(streams iostream.Streams, r imageRepair, path string, original []byte, last string) error {
	if last != "" {
		repairPath := path + brand.RepairSuffix
		if err := os.WriteFile(repairPath, []byte(last), 0o644); err != nil {
			streams.ErrPrintln(ui.Warning(fmt.Sprintf("Could not save the last attempt as %s: %v", filepath.Base(repairPath), err)))
		}
	}
	if r.KeepRepair {
		streams.ErrPrintln(ui.Warning(fmt.Sprintf("%s is left as the last attempt edited it (--keep-repair).", r.Dockerfile)))
		return nil
	}
	if err := os.WriteFile(path, original, 0o644); err != nil {
		return &userError{msg: "Could not restore " + r.Dockerfile + " after the failed repair.", suggestion: "Restore it with git checkout " + r.Dockerfile + ".", err: err}
	}
	msg := r.Dockerfile + " is restored to what it was."
	if last != "" {
		msg += " The last attempt is saved as " + r.Dockerfile + brand.RepairSuffix + "."
	}
	streams.ErrPrintln(ui.Warning(msg))
	return nil
}

// dockerUnreachable is the error for a Docker daemon smelt could not connect to.
func dockerUnreachable(err error) error {
	return &userError{msg: "Could not reach Docker.", suggestion: "Start Docker, then try again.", err: err}
}
