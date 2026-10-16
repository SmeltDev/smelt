package cmd

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"time"

	"github.com/spf13/cobra"

	"github.com/smeltdev/smelt/internal/brand"
	"github.com/smeltdev/smelt/internal/config"
	"github.com/smeltdev/smelt/internal/factory"
	"github.com/smeltdev/smelt/internal/iostream"
	"github.com/smeltdev/smelt/internal/quality"
	"github.com/smeltdev/smelt/internal/review"
	"github.com/smeltdev/smelt/internal/sidecar"
	"github.com/smeltdev/smelt/internal/smeltd"
	"github.com/smeltdev/smelt/internal/ui"
)

// qualityWorstShown is how many of the most complex functions are listed.
const qualityWorstShown = 5

func newQualityCmd() *cobra.Command {
	var (
		projectDir string
		base       string
		jsonOut    bool
	)
	cmd := &cobra.Command{
		Use:   "quality",
		Short: "Measure code quality: erosion, complex functions and duplicated code",
		Long: `Measure the code in the working tree, in any language: erosion (the share of
the code's complexity held by functions above complexity 10), the functions
over the complexity limit, and the lines of duplicated blocks. It is the
measurement factory runs make before and after their change, with the limits
from factory.quality in the project config.

The measuring is done by a provider in a throwaway container from the
project's image, so the numbers match a run's: lizard by default, or the
command in factory.quality.command, which prints the metrics contract as JSON.
An image built before agent kit v4 has no lizard; smelt factory image
--update-kit adds it. Docker is needed, as for factory runs.

With --base, the tree is also compared with a revision, and the findings a
factory run would send back to the implementer are listed. The command then
exits non-zero if any of them is medium, so CI can use it. Without --base it
only reports.

Tests, testdata, vendor and generated files are left out, and so is what git
ignores; factory.quality.exclude adds globs. Code in a language the provider
does not know is reported as not measured.`,
		SilenceUsage: true,
		Args:         cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runQuality(cmd, projectDir, base, jsonOut)
		},
	}
	cmd.Flags().StringVar(&projectDir, "project", "", "Override project directory")
	cmd.Flags().StringVar(&base, "base", "", "Compare with this revision and list the findings")
	cmd.Flags().BoolVar(&jsonOut, "json", false, "Print the measurement as JSON")
	return cmd
}

// qualityMeasurerOpener readies a measurer for the project at root and returns
// it with the function that tears it down. It is a seam: production measures in
// a container, tests in a local directory.
type qualityMeasurerOpener func(ctx context.Context, streams iostream.Streams, root string, cfg *config.ProjectConfig) (quality.Measurer, func(), error)

// openQualityMeasurer is the seam runQuality opens its measurer through; only
// tests replace it.
var openQualityMeasurer qualityMeasurerOpener = openContainerMeasurer

func runQuality(cmd *cobra.Command, projectDir, base string, jsonOut bool) error {
	root, err := resolveHookRoot(cmd.Context(), projectDir)
	if err != nil {
		return newUserError("Could not find the project.").wrap(err)
	}
	cfg, err := qualityProjectConfig(root)
	if err != nil {
		return err
	}
	streams := iostream.FromCmd(cmd)
	measurer, closeMeasurer, err := openQualityMeasurer(cmd.Context(), streams, root, cfg)
	if err != nil {
		return err
	}
	defer closeMeasurer()
	a, err := quality.Assess(cmd.Context(), measurer, base, factory.QualityLimits(cfg.Factory))
	if err != nil {
		return newUserError("Could not measure the code.").wrap(err).
			withSuggestion("Check that --base names a commit, and that the image has lizard (smelt factory image --update-kit) or factory.quality.command prints the metrics contract.")
	}
	if jsonOut {
		enc := json.NewEncoder(streams.Out)
		enc.SetIndent("", "  ")
		if err := enc.Encode(wireAssessment(a, base)); err != nil {
			return fmt.Errorf("write the measurement: %w", err)
		}
	} else {
		printAssessment(streams, a)
	}
	if medium := len(quality.Summarize(orEmpty(a.Before), a.After, a.Findings).Failing()); medium > 0 {
		return newUserError(fmt.Sprintf("%d finding(s) to fix.", medium)).withCode("quality.findings").
			withExitCode(ExitGeneral).withoutDetail()
	}
	return nil
}

// containerMeasurerName names the throwaway container.
const containerMeasurerName = brand.Command + "-quality"

// openContainerMeasurer starts a container from the project's image, with the
// project synced into it, and measures there. Like smelt factory image it uses
// Docker whatever backend the project's factory runs use.
func openContainerMeasurer(ctx context.Context, streams iostream.Streams, root string, cfg *config.ProjectConfig) (quality.Measurer, func(), error) {
	status := func(_ iostream.Level, msg string) { streams.ErrPrintln(ui.Dim("  ▸ " + msg)) }
	settings, err := resolveBuildSettings(ctx, streams, root, factorySettings{Backend: config.BackendDocker}, factoryExplicit{Backend: true}, cfg)
	if err != nil {
		return nil, nil, err
	}
	backend, err := factory.DockerBackend(root, cfg)
	if err != nil {
		return nil, nil, dockerUnreachable(err)
	}
	if err := factory.PrepareBackend(ctx, backend, settings.Image, status); err != nil {
		return nil, nil, dockerUnreachable(err)
	}
	repoPath, err := sidecar.RepoWorkspace(ctx, root)
	if err != nil {
		return nil, nil, newUserError("Could not work out where the project goes in the container.").wrap(err)
	}
	inst, err := backend.Create(ctx, containerMeasurerName, settings.Image, repoPath)
	if err != nil {
		return nil, nil, &userError{msg: "Could not start the container to measure in.", err: err}
	}
	closeFn := func() {
		cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), containerCleanupTimeout)
		defer cancel()
		_ = backend.Delete(cleanup, inst.ID)
	}
	if err := backend.Sync(ctx, inst.ID, repoPath, root, false, func(iostream.Level, string) {}); err != nil {
		closeFn()
		return nil, nil, &userError{msg: "Could not copy the project into the container.", err: err}
	}
	qcfg := factory.QualityLimits(cfg.Factory)
	return factory.SandboxMeasurer{
		Exec:     review.ClientExec,
		Entry:    &sidecar.PoolEntry{ID: inst.ID, RepoPath: repoPath, Backend: backend},
		Provider: factory.ProviderFor(qcfg),
		Config:   qcfg,
	}, closeFn, nil
}

// containerCleanupTimeout bounds removing the container after the command.
const containerCleanupTimeout = 30 * time.Second

// orEmpty is r, or an empty report when there is none.
func orEmpty(r *quality.Report) quality.Report {
	if r == nil {
		return quality.Report{}
	}
	return *r
}

// qualityProjectConfig is the project's config. A project without one gets the
// defaults.
func qualityProjectConfig(root string) (*config.ProjectConfig, error) {
	cfg, err := config.LoadProjectConfig(root)
	if errors.Is(err, fs.ErrNotExist) {
		return &config.ProjectConfig{}, nil
	}
	if err != nil {
		return nil, newUserError("Could not read the project config.").wrap(err)
	}
	return cfg, nil
}

func printAssessment(streams iostream.Streams, a quality.Assessment) {
	if !a.After.Measured {
		streams.Println("quality: not measured (no supported code found)")
		printNotMeasured(streams, a.After.NotMeasured)
		return
	}
	if a.Before != nil {
		streams.Println("quality: " + quality.Summarize(*a.Before, a.After, a.Findings).Line())
	} else {
		streams.Printf("quality: erosion %.1f%% · functions over %d: %d · clone lines %d\n",
			a.After.Erosion, a.After.Limit, a.After.OverThreshold, a.After.CloneLines)
	}
	if worst := a.After.Worst(qualityWorstShown); len(worst) > 0 {
		streams.Println("most complex:")
		for _, f := range worst {
			streams.Printf("  %3d  %s  %s:%d (%d lines)\n", f.Complexity, f.Name, f.File, f.Line, f.Lines)
		}
	}
	printNotMeasured(streams, a.After.NotMeasured)
	for _, f := range a.Findings {
		streams.Printf("[%s] %s %s\n", f.Severity, f.Location(), f.Message)
	}
}

// notMeasuredShown is how many unmeasured files are listed.
const notMeasuredShown = 10

func printNotMeasured(streams iostream.Streams, entries []string) {
	for i, e := range entries {
		if i == notMeasuredShown {
			streams.Printf("not measured: and %d more\n", len(entries)-notMeasuredShown)
			return
		}
		streams.Printf("not measured: %s\n", e)
	}
}

// qualityJSON is `smelt quality --json`.
type qualityJSON struct {
	Measured      bool                     `json:"measured"`
	Erosion       float64                  `json:"erosion"`
	Limit         int                      `json:"limit"`
	OverThreshold int                      `json:"over_threshold"`
	CloneLines    int                      `json:"clone_lines"`
	Worst         []smeltd.QualityFunction `json:"worst"`
	// NotMeasured are the files, or parts, that could not be measured.
	NotMeasured []string             `json:"not_measured,omitempty"`
	Base        *qualityBaseJSON     `json:"base,omitempty"`
	Findings    []qualityFindingJSON `json:"findings,omitempty"`
}

type qualityBaseJSON struct {
	Rev           string  `json:"rev"`
	Erosion       float64 `json:"erosion"`
	OverThreshold int     `json:"over_threshold"`
	CloneLines    int     `json:"clone_lines"`
}

type qualityFindingJSON struct {
	Severity quality.Severity `json:"severity"`
	File     string           `json:"file,omitempty"`
	Line     int              `json:"line,omitempty"`
	Func     string           `json:"func,omitempty"`
	Message  string           `json:"message"`
}

func wireAssessment(a quality.Assessment, rev string) qualityJSON {
	out := qualityJSON{
		Measured: a.After.Measured, Erosion: a.After.Erosion, Limit: a.After.Limit,
		OverThreshold: a.After.OverThreshold, CloneLines: a.After.CloneLines,
		Worst: []smeltd.QualityFunction{}, NotMeasured: a.After.NotMeasured,
	}
	for _, f := range a.After.Worst(qualityWorstShown) {
		out.Worst = append(out.Worst, smeltd.QualityFunction{Func: f.Name, File: f.File, Language: f.Language, CC: f.Complexity, Lines: f.Lines})
	}
	if b := a.Before; b != nil {
		out.Base = &qualityBaseJSON{Rev: rev, Erosion: b.Erosion, OverThreshold: b.OverThreshold, CloneLines: b.CloneLines}
	}
	for _, f := range a.Findings {
		out.Findings = append(out.Findings, qualityFindingJSON{Severity: f.Severity, File: f.File, Line: f.Line, Func: f.Func, Message: f.Message})
	}
	return out
}
