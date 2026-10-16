package cmd

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"

	"github.com/spf13/cobra"

	"github.com/smeltdev/smelt/internal/config"
	"github.com/smeltdev/smelt/internal/factory"
	"github.com/smeltdev/smelt/internal/iostream"
	"github.com/smeltdev/smelt/internal/quality"
	"github.com/smeltdev/smelt/internal/smeltd"
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
		Long: `Measure the Go code in the working tree: erosion (the share of the code's
complexity held by functions above complexity 10), the functions over the
complexity limit, and the lines of duplicated blocks. It is the measurement
factory runs make before and after their change, with the limits from
factory.quality in the project config.

With --base, the tree is also compared with a revision, and the findings a
factory run would send back to the implementer are listed. The command then
exits non-zero if any of them is medium, so CI can use it. Without --base it
only reports.

Tests, testdata, vendor and generated files are left out. Projects without Go
are reported as not measured.`,
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

func runQuality(cmd *cobra.Command, projectDir, base string, jsonOut bool) error {
	root, err := resolveHookRoot(cmd.Context(), projectDir)
	if err != nil {
		return newUserError("Could not find the project.").wrap(err)
	}
	cfg, err := qualityConfigOf(root)
	if err != nil {
		return err
	}
	a, err := quality.Assess(cmd.Context(), root, base, cfg)
	if err != nil {
		return newUserError("Could not measure the code.").wrap(err).
			withSuggestion("Run it inside a git repository, and check that --base names a commit.")
	}
	streams := iostream.FromCmd(cmd)
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

// orEmpty is r, or an empty report when there is none.
func orEmpty(r *quality.Report) quality.Report {
	if r == nil {
		return quality.Report{}
	}
	return *r
}

// qualityConfigOf is the project's quality limits. A project without a config
// gets the defaults.
func qualityConfigOf(root string) (quality.Config, error) {
	cfg, err := config.LoadProjectConfig(root)
	if errors.Is(err, fs.ErrNotExist) {
		return quality.Config{}, nil
	}
	if err != nil {
		return quality.Config{}, newUserError("Could not read the project config.").wrap(err)
	}
	return factory.QualityLimits(cfg.Factory), nil
}

func printAssessment(streams iostream.Streams, a quality.Assessment) {
	if !a.After.Measured {
		streams.Println("quality: not measured (no Go code)")
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
			streams.Printf("  %3d  %s  %s:%d (%d lines)\n", f.Complexity, f.Qualified(), f.File, f.Line, f.Lines)
		}
	}
	for _, f := range a.Findings {
		streams.Printf("[%s] %s %s\n", f.Severity, f.Location(), f.Message)
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
	Base          *qualityBaseJSON         `json:"base,omitempty"`
	Findings      []qualityFindingJSON     `json:"findings,omitempty"`
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
		Worst: []smeltd.QualityFunction{},
	}
	for _, f := range a.After.Worst(qualityWorstShown) {
		out.Worst = append(out.Worst, smeltd.QualityFunction{Func: f.Qualified(), File: f.File, CC: f.Complexity, Lines: f.Lines})
	}
	if b := a.Before; b != nil {
		out.Base = &qualityBaseJSON{Rev: rev, Erosion: b.Erosion, OverThreshold: b.OverThreshold, CloneLines: b.CloneLines}
	}
	for _, f := range a.Findings {
		out.Findings = append(out.Findings, qualityFindingJSON{Severity: f.Severity, File: f.File, Line: f.Line, Func: f.Func, Message: f.Message})
	}
	return out
}
