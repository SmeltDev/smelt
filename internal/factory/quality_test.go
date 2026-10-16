package factory

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"gotest.tools/v3/assert"

	"github.com/smeltdev/smelt/internal/config"
	"github.com/smeltdev/smelt/internal/iostream"
	"github.com/smeltdev/smelt/internal/quality"
)

// goFunc is a Go file with one function of n ifs, so its complexity is n+1.
func goFunc(name string, n int) quality.File {
	var b strings.Builder
	fmt.Fprintf(&b, "package p\n\nfunc %s(x int) {\n", name)
	for range n {
		b.WriteString("\tif x > 0 {\n\t}\n")
	}
	b.WriteString("}\n")
	return quality.File{Path: name + ".go", Content: []byte(b.String())}
}

// fakeTree is a quality check reading its files from memory. revReads counts
// how often the baseline was read.
type fakeTree struct {
	base, tree []quality.File
	treeErr    error
	revErr     error
	revReads   int
}

func (f *fakeTree) quality() *Quality {
	return &Quality{
		Dir: "wt", Baseline: "base",
		readRev: func(context.Context, string, string) ([]quality.File, error) {
			f.revReads++
			return f.base, f.revErr
		},
		readTree: func(context.Context, string) ([]quality.File, error) { return f.tree, f.treeErr },
	}
}

func TestQualityFailsOnAComplexFunctionTheChangeAdded(t *testing.T) {
	f := &fakeTree{base: []quality.File{goFunc("old", 2)}, tree: []quality.File{goFunc("old", 2), goFunc("busy", 15)}}
	c := f.quality().Check(context.Background())
	assert.Equal(t, c.Kind, KindQuality)
	assert.Equal(t, c.Status, StatusFailed)
	assert.Assert(t, contains(c.Feedback, "busy.go:3"), c.Feedback)
	assert.Assert(t, contains(c.Feedback, "complexity 16"), c.Feedback)
	assert.Assert(t, contains(Feedback([]Check{c}), "quality: quality"))
	assert.Assert(t, c.Quality != nil && c.Quality.OverThresholdBefore == 0 && c.Quality.OverThresholdAfter == 1)
}

func TestQualityPassesAChangeThatIsNotWorse(t *testing.T) {
	f := &fakeTree{base: []quality.File{goFunc("old", 2)}, tree: []quality.File{goFunc("old", 3)}}
	c := f.quality().Check(context.Background())
	assert.Equal(t, c.Status, StatusPassed)
	assert.Equal(t, c.Feedback, "")
	assert.Assert(t, c.Quality != nil)
}

func TestQualityMeasuresTheBaselineOnce(t *testing.T) {
	f := &fakeTree{base: []quality.File{goFunc("old", 2)}, tree: []quality.File{goFunc("old", 2)}}
	q := f.quality()
	for range 3 {
		assert.Equal(t, q.Check(context.Background()).Status, StatusPassed)
	}
	assert.Equal(t, f.revReads, 1)
}

func TestQualityMeasurementFailureIsErroredAndWarned(t *testing.T) {
	tests := []struct {
		name string
		f    *fakeTree
	}{
		{"tree", &fakeTree{treeErr: errors.New("git is gone")}},
		{"baseline", &fakeTree{revErr: errors.New("bad rev")}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var warned []string
			q := tt.f.quality()
			q.Warn = func(msg string) { warned = append(warned, msg) }
			c := q.Check(context.Background())
			assert.Equal(t, c.Status, StatusErrored)
			assert.Assert(t, c.Error != "")
			assert.Equal(t, Feedback([]Check{c}), "", "an errored check is not fed back")
			assert.Equal(t, len(warned), 1)
		})
	}
}

func TestQualityRetriesABaselineThatFailed(t *testing.T) {
	f := &fakeTree{revErr: errors.New("busy")}
	q := f.quality()
	assert.Equal(t, q.Check(context.Background()).Status, StatusErrored)
	f.revErr = nil
	assert.Equal(t, q.Check(context.Background()).Status, StatusPassed)
	assert.Equal(t, f.revReads, 2)
}

func TestQualityConfigFollowsTheProjectConfig(t *testing.T) {
	off, on := false, true
	assert.Assert(t, QualityConfig(nil) != nil, "on by default")
	assert.Assert(t, QualityConfig(&config.FactoryConfig{Quality: &config.QualityConfig{Enabled: &off}}) == nil)
	cfg := QualityConfig(&config.FactoryConfig{Quality: &config.QualityConfig{Enabled: &on, MaxFunctionComplexity: 9, MaxErosionRise: 2, MinCloneLines: 20}})
	assert.DeepEqual(t, *cfg, quality.Config{MaxFunctionComplexity: 9, MaxErosionRise: 2, MinCloneLines: 20})
	// The limits apply to `smelt quality` even when runs do not check.
	assert.Equal(t, QualityLimits(&config.FactoryConfig{Quality: &config.QualityConfig{Enabled: &off, MinCloneLines: 7}}).MinCloneLines, 7)
}

// TestCheckRunsQualityAfterTheGates runs a real round of checks: the quality
// check comes last, is reported as it finishes, and a round whose gates failed
// still has it.
func TestCheckRunsQualityAfterTheGates(t *testing.T) {
	tests := []struct {
		name string
		gate string
	}{
		{"gates pass", "true"},
		{"gates fail", "exit 1"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx := context.Background()
			s, impl, _, _ := newSidecarsFixture(t, nil)
			s.GatesFirst = true
			s.Commands = []config.Command{{Name: "lint", Run: tt.gate}}
			f := &fakeTree{base: []quality.File{goFunc("old", 2)}, tree: []quality.File{goFunc("busy", 15)}}
			s.Quality = f.quality()
			var reported []string
			s.OnCheck = func(c Check) { reported = append(reported, c.Name+":"+string(c.Status)) }
			writeFile(t, impl.RepoPath, "main.go", "package main\n\n// v1\n")
			_, err := s.Collect(ctx)
			assert.NilError(t, err)

			checks, err := s.Check(ctx, 1)
			assert.NilError(t, err)
			last := checks[len(checks)-1]
			assert.Equal(t, last.Kind, KindQuality)
			assert.Equal(t, last.Status, StatusFailed)
			assert.Equal(t, reported[len(reported)-1], "quality:failed")
			assert.Equal(t, len(reported), 2, "%v", reported)
			assert.Equal(t, reported[0], "lint:"+map[string]string{"true": "passed", "exit 1": "failed"}[tt.gate])
			assert.Assert(t, contains(Feedback(checks), "busy.go"))
		})
	}
}

func TestCheckWithoutQualityHasNoQualityCheck(t *testing.T) {
	ctx := context.Background()
	s, impl, _, _ := newSidecarsFixture(t, nil)
	s.Commands = []config.Command{{Name: "lint", Run: "true"}}
	writeFile(t, impl.RepoPath, "main.go", "package main\n\n// v1\n")
	_, err := s.Collect(ctx)
	assert.NilError(t, err)
	checks, err := s.Check(ctx, 1)
	assert.NilError(t, err)
	for _, c := range checks {
		assert.Assert(t, c.Kind != KindQuality)
	}
	assert.Assert(t, (Outcome{Checks: checks}).Quality() == nil)
}

func TestOutcomeQualityIsTheLastRoundsSummary(t *testing.T) {
	sum := quality.Summary{Measured: true, ErosionBefore: 10, ErosionAfter: 9}
	o := Outcome{Checks: []Check{pass, FromQuality(sum, time.Second, nil)}}
	assert.Equal(t, o.Quality().ErosionAfter, 9.0)
	assert.Assert(t, (Outcome{Checks: []Check{FromQuality(quality.Summary{}, 0, errors.New("x"))}}).Quality() == nil)
}

func TestFromQualityFailsOnlyOnMediumFindings(t *testing.T) {
	low := quality.Finding{Severity: quality.SeverityLow, File: "a.go", Line: 4, Message: "repeats"}
	medium := quality.Finding{Severity: quality.SeverityMedium, File: "b.go", Line: 9, Func: "f", Message: "f has complexity 16: split it"}
	whole := quality.Finding{Severity: quality.SeverityMedium, Message: "erosion rose"}

	c := FromQuality(quality.Summary{Measured: true, Findings: []quality.Finding{low}}, 0, nil)
	assert.Equal(t, c.Status, StatusPassed)
	assert.Equal(t, len(c.Quality.Findings), 1, "low findings stay for display")

	c = FromQuality(quality.Summary{Measured: true, Findings: []quality.Finding{low, medium, whole}}, 0, nil)
	assert.Equal(t, c.Status, StatusFailed)
	assert.Equal(t, c.Feedback, "- b.go:9: f has complexity 16: split it\n- erosion rose")

	c = FromQuality(quality.Summary{}, 0, nil)
	assert.Equal(t, c.Status, StatusPassed, "a project without Go passes")
}

func TestRunOptionsQualityCheckMeasuresTheWorktreeAgainstItsBaseline(t *testing.T) {
	wt := Worktree{Path: "/wt", Baseline: "abc123"}
	status := func(iostream.Level, string) {}
	assert.Assert(t, (&RunOptions{}).qualityCheck(wt, status) == nil, "no config means no check")
	q := (&RunOptions{Quality: &quality.Config{MinCloneLines: 9}}).qualityCheck(wt, status)
	assert.Equal(t, q.Dir, "/wt")
	assert.Equal(t, q.Baseline, "abc123")
	assert.Equal(t, q.Config.MinCloneLines, 9)
}
