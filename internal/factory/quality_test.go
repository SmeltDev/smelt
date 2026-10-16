package factory

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"gotest.tools/v3/assert"

	"github.com/smeltdev/smelt/internal/config"
	"github.com/smeltdev/smelt/internal/iostream"
	"github.com/smeltdev/smelt/internal/quality"
	"github.com/smeltdev/smelt/internal/review"
	"github.com/smeltdev/smelt/internal/sidecar"
)

// fnSpec is one function of a fake sandbox's tree: cc is its complexity and
// salt is what its source says, so a different salt is an edited body.
type fnSpec struct {
	file, name string
	cc         int
	start, end int
	salt       string
}

// sandboxTree is what the fake sandbox answers for one tree: what the provider
// prints, and the files the sources are read back from.
type sandboxTree struct {
	// lizard is in lizard's documented layout, written here by hand; the real
	// captures are parsed in internal/quality/lizard.
	lizard   string
	contract quality.Contract
	sources  map[string]string
}

func lizardTree(fns ...fnSpec) sandboxTree {
	var csv strings.Builder
	t := sandboxTree{sources: map[string]string{}}
	langs := map[string]bool{}
	for _, f := range fns {
		n := f.end - f.start + 1
		fmt.Fprintf(&csv, "%d,%d,10,1,%d,\"%s@%d-%d@%s\",\"%s\",\"%s\",\"%s\",%d,%d\n",
			n, f.cc, n, f.name, f.start, f.end, f.file, f.file, f.name, f.name, f.start, f.end)
		t.contract.Functions = append(t.contract.Functions, quality.ContractFunction{
			File: f.file, Name: f.name, Language: "go", Start: f.start, End: f.end, Complexity: f.cc, Lines: n,
		})
		langs[f.file] = true
		lines := strings.Split(t.sources[f.file], "\n")
		for len(lines) <= f.end {
			lines = append(lines, "")
		}
		for i := f.start; i <= f.end; i++ {
			lines[i-1] = fmt.Sprintf("%s %s %d", f.name, f.salt, i)
		}
		t.sources[f.file] = strings.Join(lines, "\n")
	}
	t.contract.Languages = []string{"go"}
	t.lizard = "@@smelt:version@@\n1.24.1\n@@smelt:csv@@\n" + csv.String() + "@@smelt:duplicates@@\nTotal duplicate rate: 0.00%\n"
	return t
}

// fakeSandbox is the implementer's sidecar for a quality check: it records the
// scripts it is asked to run and answers the provider, the checkout of a
// revision and the reading back of sources from the trees it holds.
type fakeSandbox struct {
	mu        sync.Mutex
	now, base sandboxTree
	// custom makes the provider answer with the contract as JSON, as a project's
	// own command does, instead of lizard's output.
	custom bool
	// rawProvider, when set, is what a provider prints instead.
	rawProvider string
	// failProvider is how many of the first provider runs exit 1.
	failProvider int
	addFails     bool
	// edited is set once the implementer has worked: before it the working tree
	// is the baseline.
	edited       bool
	log          []string
	providerRuns int
	revRuns      int
	cleanupCtx   []error
}

func (f *fakeSandbox) quality() *Quality {
	return &Quality{Exec: f.exec, Entry: &sidecar.PoolEntry{ID: "impl", RepoPath: "/work/repo"}}
}

func (f *fakeSandbox) exec(ctx context.Context, _ *sidecar.PoolEntry, script string, _ map[string]string, out iostream.OutputFn, _ func(string)) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.log = append(f.log, script)
	if err := ctx.Err(); err != nil {
		return -1, err
	}
	tree := f.now
	if strings.Contains(script, revWorktree) || !f.edited {
		tree = f.base
	}
	switch {
	case strings.Contains(script, "worktree add"):
		if f.addFails {
			out(iostream.StreamStderr, []byte("fatal: invalid reference"))
			return 128, nil
		}
		return 0, nil
	case strings.Contains(script, "worktree remove"):
		f.cleanupCtx = append(f.cleanupCtx, ctx.Err())
		return 0, nil
	case strings.Contains(script, "SMELT_FILES"):
		out(iostream.StreamStdout, []byte(tarOf(tree.sources)))
		return 0, nil
	}
	f.providerRuns++
	if strings.Contains(script, revWorktree) {
		f.revRuns++
	}
	if f.providerRuns <= f.failProvider {
		out(iostream.StreamStderr, []byte("provider crashed"))
		return 1, nil
	}
	switch {
	case f.rawProvider != "":
		out(iostream.StreamStdout, []byte(f.rawProvider))
	case f.custom:
		data, _ := json.Marshal(tree.contract)
		out(iostream.StreamStdout, data)
	default:
		out(iostream.StreamStdout, []byte(tree.lizard))
	}
	return 0, nil
}

// tarOf is the base64 tar the sources script prints.
func tarOf(files map[string]string) string {
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	for name, content := range files {
		_ = tw.WriteHeader(&tar.Header{Name: name, Typeflag: tar.TypeReg, Size: int64(len(content)), Mode: 0o644})
		_, _ = tw.Write([]byte(content))
	}
	_ = tw.Close()
	return base64.StdEncoding.EncodeToString(buf.Bytes())
}

// edit has the implementer do its work: the working tree is now `now`.
func (f *fakeSandbox) edit() { f.mu.Lock(); f.edited = true; f.mu.Unlock() }

func (f *fakeSandbox) scripts(contains string) []string {
	var out []string
	for _, s := range f.log {
		if strings.Contains(s, contains) {
			out = append(out, s)
		}
	}
	return out
}

func TestQualityFailsOnAComplexFunctionTheChangeAdded(t *testing.T) {
	f := &fakeSandbox{
		base: lizardTree(fnSpec{"a.go", "old", 2, 1, 4, "x"}),
		now:  lizardTree(fnSpec{"a.go", "old", 2, 1, 4, "x"}, fnSpec{"a.go", "busy", 16, 5, 30, "y"}),
	}
	q := f.quality()
	q.MeasureBaseline(context.Background(), "base")
	f.edit()
	c := q.Check(context.Background())
	assert.Equal(t, c.Kind, KindQuality)
	assert.Equal(t, c.Status, StatusFailed)
	assert.Assert(t, contains(c.Feedback, "a.go:5"), c.Feedback)
	assert.Assert(t, contains(c.Feedback, "complexity 16"), c.Feedback)
	assert.Assert(t, contains(Feedback([]Check{c}), "quality: quality"))
	assert.Assert(t, c.Quality != nil && c.Quality.OverThresholdBefore == 0 && c.Quality.OverThresholdAfter == 1)
}

func TestQualityPassesAChangeThatIsNotWorse(t *testing.T) {
	f := &fakeSandbox{
		base: lizardTree(fnSpec{"a.go", "old", 2, 1, 4, "x"}),
		now:  lizardTree(fnSpec{"a.go", "old", 3, 1, 4, "y"}),
	}
	q := f.quality()
	q.MeasureBaseline(context.Background(), "base")
	f.edit()
	c := q.Check(context.Background())
	assert.Equal(t, c.Status, StatusPassed)
	assert.Equal(t, c.Feedback, "")
	assert.Assert(t, c.Quality != nil)
}

// (a) The measurement goes through the sandbox's exec: lizard is run there,
// from the repo, and the sources are read back from there too.
func TestQualityRunsLizardOnTheSidecar(t *testing.T) {
	f := &fakeSandbox{
		base: lizardTree(fnSpec{"a.go", "old", 2, 1, 4, "x"}),
		now:  lizardTree(fnSpec{"a.go", "old", 2, 1, 4, "x"}),
	}
	q := f.quality()
	q.MeasureBaseline(context.Background(), "base")
	f.edit()
	assert.Equal(t, q.Check(context.Background()).Status, StatusPassed)

	runs := f.scripts("lizard --csv")
	assert.Equal(t, len(runs), 2, "once for the baseline, once for the round")
	for _, s := range runs {
		assert.Assert(t, strings.HasPrefix(s, "cd '/work/repo' || exit 1\n"), s)
		assert.Assert(t, contains(s, "lizard -Eduplicate"), s)
	}
	reads := f.scripts("SMELT_FILES")
	assert.Equal(t, len(reads), 2)
	assert.Assert(t, contains(reads[0], "\na.go\n"), reads[0])
}

// (b) factory.quality.command replaces lizard, and what it prints must be the
// contract.
func TestQualityUsesTheConfiguredCommandInsteadOfLizard(t *testing.T) {
	f := &fakeSandbox{
		custom: true,
		base:   lizardTree(fnSpec{"a.go", "old", 2, 1, 4, "x"}),
		now:    lizardTree(fnSpec{"a.go", "old", 2, 1, 4, "x"}, fnSpec{"a.go", "busy", 20, 5, 30, "y"}),
	}
	q := f.quality()
	q.Config = quality.Config{Command: "./tools/metrics.sh --json"}
	q.MeasureBaseline(context.Background(), "base")
	f.edit()
	c := q.Check(context.Background())
	assert.Equal(t, c.Status, StatusFailed, "the command's functions are used: %s", c.Error)
	assert.Assert(t, contains(c.Feedback, "busy has cyclomatic complexity 20"), c.Feedback)

	assert.Equal(t, len(f.scripts("lizard")), 0, "lizard is not run")
	cmds := f.scripts("./tools/metrics.sh --json")
	assert.Equal(t, len(cmds), 2)
	assert.Assert(t, strings.HasPrefix(cmds[0], "cd '/work/repo' || exit 1\n./tools/metrics.sh --json"), cmds[0])
}

func TestQualityFailsOnOutputThatIsNotTheContract(t *testing.T) {
	tests := []struct{ name, out, want string }{
		{"not json", "metrics: 12 functions", "parse the metrics contract"},
		{"nothing", "  ", "printed nothing"},
		{"a bad function", `{"functions":[{"file":"a.go","name":"f","start":0,"end":0}]}`, "lines 0-0"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := &fakeSandbox{rawProvider: tt.out}
			q := f.quality()
			q.Config = quality.Config{Command: "metrics"}
			var warned []string
			q.Warn = func(msg string) { warned = append(warned, msg) }
			q.MeasureBaseline(context.Background(), "base")
			f.edit()
			c := q.Check(context.Background())
			assert.Equal(t, c.Status, StatusErrored)
			assert.Assert(t, contains(c.Error, tt.want), c.Error)
			assert.Assert(t, c.Quality == nil, "no summary, never a silent zero")
			assert.Equal(t, Feedback([]Check{c}), "", "an errored check is not fed back")
			assert.Assert(t, len(warned) >= 2, "%v", warned)
			assert.Assert(t, contains(warned[len(warned)-1], tt.want), warned)
		})
	}
}

func TestQualityTreatsAnEmptyContractAsNotMeasured(t *testing.T) {
	f := &fakeSandbox{rawProvider: `{"functions": []}`}
	q := f.quality()
	q.Config = quality.Config{Command: "metrics"}
	q.MeasureBaseline(context.Background(), "base")
	f.edit()
	c := q.Check(context.Background())
	assert.Equal(t, c.Status, StatusPassed)
	assert.Assert(t, c.Quality != nil && !c.Quality.Measured)
	assert.Assert(t, contains(c.Quality.Line(), "not measured"))
}

// (c) The baseline is measured once, however many rounds follow.
func TestQualityMeasuresTheBaselineOnce(t *testing.T) {
	f := &fakeSandbox{
		base: lizardTree(fnSpec{"a.go", "old", 2, 1, 4, "x"}),
		now:  lizardTree(fnSpec{"a.go", "old", 2, 1, 4, "x"}),
	}
	q := f.quality()
	q.MeasureBaseline(context.Background(), "base")
	f.edit()
	for range 3 {
		assert.Equal(t, q.Check(context.Background()).Status, StatusPassed)
	}
	assert.Equal(t, f.providerRuns, 4, "one baseline run and one per round")
	assert.Equal(t, f.revRuns, 0)
	assert.Equal(t, len(f.scripts("worktree")), 0)
}

// (d) A baseline that failed is not kept: the next round measures the
// baseline commit in a temporary worktree, and removes it.
func TestQualityRetriesAFailedBaselineInAWorktree(t *testing.T) {
	f := &fakeSandbox{
		failProvider: 1,
		base:         lizardTree(fnSpec{"a.go", "old", 2, 1, 4, "x"}),
		now:          lizardTree(fnSpec{"a.go", "old", 2, 1, 4, "x"}, fnSpec{"a.go", "busy", 16, 5, 30, "y"}),
	}
	q := f.quality()
	var warned []string
	q.Warn = func(msg string) { warned = append(warned, msg) }
	q.MeasureBaseline(context.Background(), "abc123")
	f.edit()
	assert.Equal(t, len(warned), 1)
	assert.Assert(t, contains(warned[0], "baseline quality was not measured"), warned[0])
	assert.Assert(t, contains(warned[0], "provider crashed"), warned[0])

	c := q.Check(context.Background())
	assert.Equal(t, c.Status, StatusFailed, "%s", c.Error)
	assert.Assert(t, c.Quality.OverThresholdBefore == 0 && c.Quality.OverThresholdAfter == 1)
	assert.Equal(t, f.revRuns, 1)
	adds := f.scripts("worktree add")
	assert.Equal(t, len(adds), 1)
	assert.Assert(t, contains(adds[0], "git worktree add --detach -q '"+revWorktree+"' 'abc123'"), adds[0])
	assert.Equal(t, len(f.cleanupCtx), 1, "removed after the measurement")
	cleanup := f.scripts("worktree remove")
	assert.Equal(t, len(cleanup), 2, "the checkout script clears a stale worktree too, and the cleanup comes after the measurement")
	lastRemove, lastRun := -1, -1
	for i, s := range f.log {
		if strings.Contains(s, "worktree remove") {
			lastRemove = i
		}
		if strings.Contains(s, "lizard --csv") {
			lastRun = i
		}
	}
	assert.Assert(t, lastRemove < lastRun, "the worktree is gone before the round's own measurement")

	// The baseline is now kept.
	assert.Equal(t, q.Check(context.Background()).Status, StatusFailed)
	assert.Equal(t, len(f.scripts("worktree add")), 1)
}

func TestQualityRemovesTheWorktreeWhenTheRetryFails(t *testing.T) {
	for name, tt := range map[string]struct {
		addFails     bool
		failProvider int
		cancelled    bool
	}{
		"checkout fails":        {addFails: true, failProvider: 1},
		"provider fails there":  {failProvider: 2},
		"cancelled mid-measure": {failProvider: 1, cancelled: true},
	} {
		t.Run(name, func(t *testing.T) {
			f := &fakeSandbox{addFails: tt.addFails, failProvider: tt.failProvider, base: lizardTree(fnSpec{"a.go", "old", 2, 1, 4, "x"})}
			q := f.quality()
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			q.MeasureBaseline(ctx, "abc123")
			f.edit()
			if tt.cancelled {
				cancel()
			}
			c := q.Check(ctx)
			assert.Equal(t, c.Status, StatusErrored)
			assert.Assert(t, strings.Contains(f.log[len(f.log)-1], "worktree remove"), "the worktree is removed on this path: %v", f.log)
			assert.Equal(t, f.cleanupCtx[len(f.cleanupCtx)-1], nil, "the cleanup runs although the run was cancelled")
		})
	}
}

// (e) A medium finding makes the round fail with feedback for the implementer.
func TestQualityMediumFindingGoesBackAsFeedback(t *testing.T) {
	f := &fakeSandbox{
		base: lizardTree(fnSpec{"a.go", "old", 2, 1, 4, "x"}),
		now:  lizardTree(fnSpec{"a.go", "old", 2, 1, 4, "x"}, fnSpec{"a.go", "busy", 21, 5, 30, "y"}),
	}
	q := f.quality()
	q.MeasureBaseline(context.Background(), "base")
	f.edit()
	c := q.Check(context.Background())
	assert.Equal(t, c.Status, StatusFailed)
	fb := Feedback([]Check{c})
	assert.Assert(t, contains(fb, "busy has cyclomatic complexity 21"), fb)
	assert.Assert(t, contains(fb, "split it"), fb)
}

func TestQualityMeasurementFailureIsErroredAndWarned(t *testing.T) {
	f := &fakeSandbox{failProvider: 100}
	q := f.quality()
	var warned []string
	q.Warn = func(msg string) { warned = append(warned, msg) }
	c := q.Check(context.Background())
	assert.Equal(t, c.Status, StatusErrored)
	assert.Assert(t, c.Error != "")
	assert.Equal(t, Feedback([]Check{c}), "", "an errored check is not fed back")
	assert.Assert(t, len(warned) >= 1)

	q = &Quality{Exec: f.exec, Entry: &sidecar.PoolEntry{RepoPath: "/r"}}
	assert.Equal(t, q.Check(context.Background()).Status, StatusErrored, "no baseline commit")
}

func TestQualityLeavesOutFilesWithoutASource(t *testing.T) {
	f := &fakeSandbox{now: lizardTree(fnSpec{"a.go", "f", 30, 1, 4, "x"}, fnSpec{"b.go", "g", 30, 1, 4, "x"})}
	delete(f.now.sources, "b.go")
	q := f.quality()
	q.base = &quality.Report{Measured: true, Limit: 15}
	f.edit()
	c := q.Check(context.Background())
	assert.Equal(t, c.Quality.OverThresholdAfter, 1, "g cannot be hashed, so it is not measured")
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
			f := &fakeSandbox{base: lizardTree(fnSpec{"a.go", "old", 2, 1, 4, "x"}), now: lizardTree(fnSpec{"a.go", "busy", 16, 1, 30, "y"})}
			s, impl, _, _ := newSidecarsFixtureWith(t, nil, func(s *Sidecars) { s.Quality = f.quality() })
			f.edit()
			s.GatesFirst = true
			s.Commands = []config.Command{{Name: "lint", Run: tt.gate}}
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
			assert.Assert(t, contains(Feedback(checks), "a.go:1"))
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

func TestRunOptionsQualityCheckMeasuresOnTheImplementersSidecar(t *testing.T) {
	status := func(iostream.Level, string) {}
	impl := &sidecar.PoolEntry{ID: "impl", RepoPath: "/work/repo"}
	var exec review.Execer = func(context.Context, *sidecar.PoolEntry, string, map[string]string, iostream.OutputFn, func(string)) (int, error) {
		return 0, nil
	}
	assert.Assert(t, (&RunOptions{}).qualityCheck(exec, impl, status) == nil, "no config means no check")
	q := (&RunOptions{Quality: &quality.Config{MinCloneLines: 9, Command: "m"}}).qualityCheck(exec, impl, status)
	assert.Equal(t, q.Entry, impl)
	assert.Equal(t, q.Config.MinCloneLines, 9)
	assert.Equal(t, q.Config.Command, "m")
}

func TestQualityConfigCarriesTheProviderAndExclusions(t *testing.T) {
	off := false
	cfg := QualityLimits(&config.FactoryConfig{Quality: &config.QualityConfig{
		Command: " ./m.sh ", Exclude: []string{"gen/**"}, Include: []string{"vendor/mine/**"}, DefaultExcludes: &off,
	}})
	assert.Equal(t, cfg.Command, "./m.sh")
	assert.DeepEqual(t, cfg.Exclude, quality.ExcludeConfig{Exclude: []string{"gen/**"}, Include: []string{"vendor/mine/**"}, NoDefaults: true})
	assert.Equal(t, QualityLimits(&config.FactoryConfig{Quality: &config.QualityConfig{}}).Exclude.NoDefaults, false)
}

// (f) With the check turned off nothing is run on the sidecar for it.
func TestQualityOffMakesNoExecCalls(t *testing.T) {
	off := false
	cfg := QualityConfig(&config.FactoryConfig{Quality: &config.QualityConfig{Enabled: &off}})
	assert.Assert(t, cfg == nil)
	var mu sync.Mutex
	var log []string
	spy := func(run review.Execer) review.Execer {
		return func(ctx context.Context, e *sidecar.PoolEntry, script string, env map[string]string, out iostream.OutputFn, sub func(string)) (int, error) {
			mu.Lock()
			log = append(log, script)
			mu.Unlock()
			return run(ctx, e, script, env, out, sub)
		}
	}
	s, _, _, _ := newSidecarsFixtureWith(t, spy, func(s *Sidecars) {
		s.Quality = (&RunOptions{Quality: cfg}).qualityCheck(s.Exec, s.Implementer.Entry, func(iostream.Level, string) {})
	})
	assert.Assert(t, s.Quality == nil)
	assert.Assert(t, len(s.qualityChecks(context.Background())) == 0)
	for _, script := range log {
		assert.Assert(t, !contains(script, "lizard") && !contains(script, "SMELT_FILES"), script)
	}
}

// (g) Prepare measures the baseline right after the baseline commit, before
// anything else of the run, and a failure only warns.
func TestPrepareMeasuresTheBaselineAfterTheBaselineCommit(t *testing.T) {
	for name, fail := range map[string]int{"measured": 0, "fails": 100} {
		t.Run(name, func(t *testing.T) {
			f := &fakeSandbox{failProvider: fail, base: lizardTree(fnSpec{"a.go", "old", 2, 1, 4, "x"}), now: lizardTree(fnSpec{"a.go", "old", 2, 1, 4, "x"})}
			var mu sync.Mutex
			var order []string
			note := func(s string) { mu.Lock(); order = append(order, s); mu.Unlock() }
			var warned []string
			var q *Quality
			spy := func(run review.Execer) review.Execer {
				return func(ctx context.Context, e *sidecar.PoolEntry, script string, env map[string]string, out iostream.OutputFn, sub func(string)) (int, error) {
					if contains(script, "smelt factory baseline") {
						note("commit")
					}
					return run(ctx, e, script, env, out, sub)
				}
			}
			fake := f.exec
			s, _, _, _ := newSidecarsFixtureWith(t, spy, func(s *Sidecars) {
				q = &Quality{Entry: s.Implementer.Entry, Warn: func(msg string) { warned = append(warned, msg) },
					Exec: func(ctx context.Context, e *sidecar.PoolEntry, script string, env map[string]string, out iostream.OutputFn, sub func(string)) (int, error) {
						note("quality")
						return fake(ctx, e, script, env, out, sub)
					}}
				s.Quality = q
			})
			// The commit comes first and the quality run is after it; the
			// reviewers' own baseline commits come later still.
			assert.Equal(t, order[0], "commit")
			assert.Equal(t, order[1], "quality")
			assert.Equal(t, q.Baseline, s.ws.baseline, "the retry measures the sidecar's own baseline commit")
			assert.Assert(t, s.ws.baseline != "")
			if fail > 0 {
				assert.Equal(t, len(warned), 1, "a failed baseline only warns")
			} else {
				assert.Equal(t, len(warned), 0)
				assert.Assert(t, q.base != nil)
			}
		})
	}
}

// The checkout of a revision, the provider and the reading back of the sources
// run for real, with sh and git, on a repository: the tree at the revision is
// what is measured, and the temporary worktree is gone afterwards.
func TestSandboxMeasurerMeasuresARevisionInAWorktree(t *testing.T) {
	for _, tool := range []string{"git", "sh", "tar", "base64"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skipf("%s is not installed", tool)
		}
	}
	if err := exec.Command("base64", "-w0", os.DevNull).Run(); err != nil {
		t.Skip("this base64 has no -w0")
	}
	repo := localRepo(t)
	writeFile(t, repo, "main.go", "func old() {\n\treturn 1\n}\n")
	gitOutput(t, repo, "add", ".")
	gitOutput(t, repo, "-c", "user.name=t", "-c", "user.email=t@t", "commit", "-q", "-m", "base")
	rev := gitOutput(t, repo, "rev-parse", "HEAD")
	writeFile(t, repo, "main.go", "func old() {\n\treturn 2\n}\n")
	writeFile(t, repo, "new.go", "func fresh() {}\n")

	contract := `{"languages":["go"],"functions":[{"file":"main.go","name":"old","start":1,"end":3,"complexity":3,"lines":3},{"file":"new.go","name":"fresh","start":1,"end":1,"complexity":1,"lines":1}]}`
	wt := filepath.Join(t.TempDir(), "rev-wt")
	m := SandboxMeasurer{
		Exec:     localExec(t.TempDir()),
		Entry:    &sidecar.PoolEntry{ID: "impl", RepoPath: repo},
		Provider: quality.CommandProvider("printf '%s' '" + contract + "'"),
		Worktree: wt,
	}
	ctx := context.Background()

	now, err := m.Measure(ctx, "")
	assert.NilError(t, err)
	byPath := map[string]string{}
	for _, f := range now.Sources {
		byPath[f.Path] = string(f.Content)
	}
	assert.Equal(t, byPath["main.go"], "func old() {\n\treturn 2\n}\n", "the working tree, edits included")
	assert.Equal(t, byPath["new.go"], "func fresh() {}\n")

	then, err := m.Measure(ctx, rev)
	assert.NilError(t, err)
	assert.Equal(t, len(then.Sources), 1, "new.go did not exist at the revision")
	assert.Equal(t, string(then.Sources[0].Content), "func old() {\n\treturn 1\n}\n")
	_, statErr := os.Stat(wt)
	assert.Assert(t, os.IsNotExist(statErr), "the worktree is removed")
	assert.Equal(t, strings.Count(gitOutput(t, repo, "worktree", "list"), "\n"), 0, "no worktree is registered")

	_, err = m.Measure(ctx, "no-such-rev")
	assert.ErrorContains(t, err, "check out no-such-rev")
	_, statErr = os.Stat(wt)
	assert.Assert(t, os.IsNotExist(statErr), "and also when the checkout failed")
}

func TestSourcesScriptSkipsWhatPassesTheSizeCap(t *testing.T) {
	for _, tool := range []string{"sh", "tar", "base64"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skipf("%s is not installed", tool)
		}
	}
	dir := t.TempDir()
	writeFile(t, dir, "a.go", "small")
	writeFile(t, dir, "b.go", strings.Repeat("x", quality.MaxSourceBytes+1))
	writeFile(t, dir, "c.go", "also small")
	out, err := exec.Command("sh", "-c", sourcesScript(dir, []string{"a.go", "gone.go", "b.go", "c.go"})).Output()
	if err != nil && strings.Contains(err.Error(), "invalid option") {
		t.Skip("this base64 has no -w0")
	}
	assert.NilError(t, err)
	data, err := base64.StdEncoding.DecodeString(strings.TrimSpace(string(out)))
	assert.NilError(t, err)
	files, _, err := quality.ReadTar(data, quality.MaxSourceBytes)
	assert.NilError(t, err)
	var names []string
	for _, f := range files {
		names = append(names, f.Path)
	}
	assert.DeepEqual(t, names, []string{"a.go", "c.go"})
}
