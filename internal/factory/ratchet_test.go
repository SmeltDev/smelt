package factory

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"gotest.tools/v3/assert"

	"github.com/smeltdev/smelt/internal/config"
	"github.com/smeltdev/smelt/internal/iostream"
	"github.com/smeltdev/smelt/internal/review"
	"github.com/smeltdev/smelt/internal/sidecar"
)

// ratchetRepo is a directory with a source file of 60 numbered lines and a
// tool whose "findings" are the lines of findings.sarif.
type ratchetRepo struct {
	t   *testing.T
	dir string
}

func newRatchetRepo(t *testing.T) ratchetRepo {
	dir := t.TempDir()
	var src strings.Builder
	for i := 1; i <= 60; i++ {
		fmt.Fprintf(&src, "line %d\n", i)
	}
	assert.NilError(t, os.WriteFile(filepath.Join(dir, "app.py"), []byte(src.String()), 0o644))
	return ratchetRepo{t, dir}
}

// sarifFor writes findings.sarif with one result per flagged line of app.py.
func (r ratchetRepo) sarifFor(lines ...int) {
	var results []string
	for _, l := range lines {
		results = append(results, fmt.Sprintf(`{"ruleId":"E%d","message":{"text":"problem %d"},"locations":[{"physicalLocation":{"artifactLocation":{"uri":"app.py"},"region":{"startLine":%d}}}]}`, l, l, l))
	}
	r.write("findings.sarif", `{"version":"2.1.0","runs":[{"results":[`+strings.Join(results, ",")+`]}]}`)
}

func (r ratchetRepo) write(name, content string) {
	assert.NilError(r.t, os.WriteFile(filepath.Join(r.dir, name), []byte(content), 0o644))
}

func (r ratchetRepo) ratchet(execer review.Execer) *Ratchet {
	return &Ratchet{Exec: execer, Entry: &sidecar.PoolEntry{ID: "impl", RepoPath: r.dir}}
}

func rng(from, to int) []int {
	var out []int
	for i := from; i <= to; i++ {
		out = append(out, i)
	}
	return out
}

func lint(run string) config.Command {
	return config.Command{Name: "lint", Run: run, Role: config.RoleGate, Format: config.FormatSARIF}
}

func TestRatchetNewFindingFailsListingOnlyIt(t *testing.T) {
	r := newRatchetRepo(t)
	rt := r.ratchet(localExec(t.TempDir()))
	c := lint("cat findings.sarif; exit 1")

	r.sarifFor(rng(1, 50)...)
	rt.MeasureBaseline(context.Background(), "base", []config.Command{c})

	r.sarifFor(rng(1, 51)...)
	got := rt.Check(context.Background(), c)
	assert.Equal(t, got.Status, StatusFailed, got.Feedback)
	assert.Equal(t, got.Output, "app.py:51 E51 problem 51")
	assert.Assert(t, strings.Contains(got.Feedback, "lint: 1 new, 0 fixed, 50 existing"), got.Feedback)
	assert.Assert(t, !strings.Contains(got.Feedback, "problem 50"), got.Feedback)
	assert.Equal(t, got.Ratchet.New, 1)
	assert.Equal(t, got.Ratchet.Existing, 50)
}

func TestRatchetRemovalsPassWithCounts(t *testing.T) {
	r := newRatchetRepo(t)
	rt := r.ratchet(localExec(t.TempDir()))
	c := lint("cat findings.sarif; exit 1")

	r.sarifFor(rng(1, 50)...)
	rt.MeasureBaseline(context.Background(), "base", []config.Command{c})

	r.sarifFor(rng(2, 50)...)
	got := rt.Check(context.Background(), c)
	assert.Equal(t, got.Status, StatusPassed, got.Feedback)
	assert.Equal(t, got.Output, "lint: 0 new, 1 fixed, 49 existing")
	assert.Equal(t, got.Ratchet.Fixed, 1)
}

func TestRatchetFindingThatOnlyMovedIsExisting(t *testing.T) {
	r := newRatchetRepo(t)
	rt := r.ratchet(localExec(t.TempDir()))
	c := lint("cat findings.sarif")
	r.sarifFor(10)
	rt.MeasureBaseline(context.Background(), "base", []config.Command{c})

	// Same line text, three lines down: app.py has distinct lines, so make
	// line 13 read like line 10 and let the tool report it there.
	r.write("app.py", strings.Replace(mustRead(t, filepath.Join(r.dir, "app.py")), "line 13\n", "line 10\n", 1))
	r.write("findings.sarif", strings.ReplaceAll(mustRead(t, filepath.Join(r.dir, "findings.sarif")), `"startLine":10`, `"startLine":13`))
	got := rt.Check(context.Background(), c)
	assert.Equal(t, got.Status, StatusPassed, got.Feedback)
	assert.Equal(t, got.Ratchet.Existing, 1)
}

func mustRead(t *testing.T, p string) string {
	b, err := os.ReadFile(p)
	assert.NilError(t, err)
	return string(b)
}

func TestRatchetInvalidSarifFailsWithTheParseError(t *testing.T) {
	r := newRatchetRepo(t)
	rt := r.ratchet(localExec(t.TempDir()))
	r.sarifFor(1)
	rt.MeasureBaseline(context.Background(), "base", []config.Command{lint("cat findings.sarif")})

	got := rt.Check(context.Background(), lint("echo 'config error: bad rule' >&2; echo oops"))
	assert.Equal(t, got.Status, StatusFailed)
	assert.Assert(t, strings.Contains(got.Feedback, "not valid JSON"), got.Feedback)
	assert.Assert(t, strings.Contains(got.Feedback, "config error: bad rule"), got.Feedback)
}

func TestRatchetStderrDoesNotCorruptStdout(t *testing.T) {
	r := newRatchetRepo(t)
	rt := r.ratchet(localExec(t.TempDir()))
	c := lint("echo 'warning: deprecated' >&2; cat findings.sarif")
	r.sarifFor(1)
	rt.MeasureBaseline(context.Background(), "base", []config.Command{c})
	assert.Equal(t, rt.Check(context.Background(), c).Status, StatusPassed)
}

func TestRatchetSarifPathIsReadAndRemoved(t *testing.T) {
	r := newRatchetRepo(t)
	rt := r.ratchet(localExec(t.TempDir()))
	c := lint("cp findings.sarif out/r.sarif")
	c.SarifPath = "out/r.sarif"
	assert.NilError(t, os.Mkdir(filepath.Join(r.dir, "out"), 0o755))
	r.sarifFor(1, 2)
	rt.MeasureBaseline(context.Background(), "base", []config.Command{c})
	_, err := os.Stat(filepath.Join(r.dir, "out/r.sarif"))
	assert.Assert(t, os.IsNotExist(err), "removed after the baseline run")

	r.sarifFor(1, 2, 3)
	got := rt.Check(context.Background(), c)
	assert.Equal(t, got.Status, StatusFailed, got.Feedback)
	assert.Equal(t, got.Ratchet.New, 1)
	_, err = os.Stat(filepath.Join(r.dir, "out/r.sarif"))
	assert.Assert(t, os.IsNotExist(err), "removed after the round")

	// A tool that writes nothing does not get a stale file's findings.
	c.Run = "true"
	got = rt.Check(context.Background(), c)
	assert.Equal(t, got.Status, StatusFailed, "the file is removed before the run")
}

func TestRatchetSarifPathMissingFile(t *testing.T) {
	r := newRatchetRepo(t)
	rt := r.ratchet(localExec(t.TempDir()))
	r.sarifFor(1)
	c := lint("true")
	c.SarifPath = "none.sarif"
	got := rt.Check(context.Background(), c)
	assert.Equal(t, got.Status, StatusFailed)
	assert.Assert(t, strings.Contains(got.Feedback, "none.sarif"), got.Feedback)
}

// countingExec counts the runs of scripts that contain marker.
func countingExec(inner review.Execer, marker string, n *atomic.Int32) review.Execer {
	return func(ctx context.Context, e *sidecar.PoolEntry, script string, env map[string]string, on iostream.OutputFn, sub func(string)) (int, error) {
		if strings.Contains(script, marker) {
			n.Add(1)
		}
		return inner(ctx, e, script, env, on, sub)
	}
}

func TestRatchetBaselineIsMadeOnce(t *testing.T) {
	r := newRatchetRepo(t)
	var runs atomic.Int32
	rt := r.ratchet(countingExec(localExec(t.TempDir()), "TOOLRUN", &runs))
	c := lint("cat findings.sarif # TOOLRUN")
	r.sarifFor(1)
	rt.MeasureBaseline(context.Background(), "base", []config.Command{c})
	rt.Check(context.Background(), c)
	rt.Check(context.Background(), c)
	assert.Equal(t, runs.Load(), int32(3), "one baseline run, then one per round")
}

func TestRatchetBaselineRetriedFromAWorktree(t *testing.T) {
	r := newRatchetRepo(t)
	r.sarifFor(1, 2)
	for _, args := range [][]string{{"init", "-q"}, {"add", "-A"}, {"-c", "user.name=t", "-c", "user.email=t@t", "commit", "-qm", "base"}} {
		cmd := exec.Command("git", args...)
		cmd.Dir = r.dir
		out, err := cmd.CombinedOutput()
		assert.NilError(t, err, string(out))
	}
	rev := strings.TrimSpace(gitOut(t, r.dir, "rev-parse", "HEAD"))

	inner := localExec(t.TempDir())
	var runs atomic.Int32
	flaky := func(ctx context.Context, e *sidecar.PoolEntry, script string, env map[string]string, on iostream.OutputFn, sub func(string)) (int, error) {
		if strings.Contains(script, "TOOLRUN") && runs.Add(1) == 1 {
			return 0, fmt.Errorf("sidecar went away")
		}
		return inner(ctx, e, script, env, on, sub)
	}
	rt := r.ratchet(flaky)
	var warned []string
	rt.Warn = func(_ iostream.Level, msg string) { warned = append(warned, msg) }
	c := lint("cat findings.sarif # TOOLRUN")
	rt.MeasureBaseline(context.Background(), rev, []config.Command{c})
	assert.Equal(t, len(warned), 1)

	r.sarifFor(1, 2, 3)
	got := rt.Check(context.Background(), c)
	assert.Equal(t, got.Status, StatusFailed, got.Feedback)
	assert.Equal(t, got.Ratchet.New, 1)
	assert.Equal(t, got.Ratchet.Existing, 2)
}

func gitOut(t *testing.T, dir string, args ...string) string {
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.Output()
	assert.NilError(t, err)
	return string(out)
}

func TestRatchetNoBaselineErrorsTheCheck(t *testing.T) {
	r := newRatchetRepo(t)
	r.sarifFor(1)
	got := r.ratchet(localExec(t.TempDir())).Check(context.Background(), lint("cat findings.sarif"))
	assert.Equal(t, got.Status, StatusErrored)
	assert.Assert(t, strings.Contains(got.Error, "no baseline"), got.Error)
}

func TestRatchetTimeoutFailsTheGate(t *testing.T) {
	r := newRatchetRepo(t)
	c := lint("sleep 10")
	c.Timeout = 1
	got := r.ratchet(localExec(t.TempDir())).Check(context.Background(), c)
	assert.Equal(t, got.Status, StatusFailed)
	assert.Assert(t, strings.Contains(got.Feedback, "timed out after 1s"), got.Feedback)
}

// A command without a format keeps the exit-code behaviour and reports no
// ratchet counts.
func TestValidateWithoutFormatUsesTheExitCode(t *testing.T) {
	dir := t.TempDir()
	s := &Sidecars{
		Exec:        localExec(t.TempDir()),
		Implementer: &Implementer{Entry: &sidecar.PoolEntry{ID: "impl", RepoPath: dir}},
		Commands:    []config.Command{{Name: "plain", Run: "echo problem; exit 3"}},
	}
	checks := s.validate(context.Background())
	assert.Equal(t, checks[0].Status, StatusFailed)
	assert.Equal(t, checks[0].ExitCode, 3)
	assert.Assert(t, checks[0].Ratchet == nil)
}

// A SARIF gate in a factory run is a ratchet: Prepare takes the baseline, and
// rounds are judged by the findings the change added, whatever the exit code.
func TestSidecarsRunSarifGateAsARatchet(t *testing.T) {
	ctx := context.Background()
	sarifJSON := func(lines ...int) string {
		var rs []string
		for _, l := range lines {
			rs = append(rs, fmt.Sprintf(`{"ruleId":"R","message":{"text":"bad line %d"},"locations":[{"physicalLocation":{"artifactLocation":{"uri":"main.go"},"region":{"startLine":%d}}}]}`, l, l))
		}
		return `{"version":"2.1.0","runs":[{"results":[` + strings.Join(rs, ",") + `]}]}`
	}
	s, impl, _, _ := newSidecarsFixtureWith(t, nil, func(s *Sidecars) {
		writeFile(t, s.Implementer.Entry.RepoPath, "lint.sarif", sarifJSON(1))
		s.Prompts = nil
		s.Commands = []config.Command{lint("cat lint.sarif; exit 1")}
	})

	// Only the existing finding: the gate passes though the tool exits 1.
	writeFile(t, impl.RepoPath, "main.go", "package main\n\n// v1\n")
	_, err := s.Collect(ctx)
	assert.NilError(t, err)
	checks, err := s.Check(ctx, 1)
	assert.NilError(t, err)
	assert.Equal(t, checks[0].Status, StatusPassed, checks[0].Feedback)
	assert.Equal(t, checks[0].Ratchet.Existing, 1)

	// A finding on the new line fails the gate, naming only it.
	writeFile(t, impl.RepoPath, "lint.sarif", sarifJSON(1, 3))
	checks, err = s.Check(ctx, 2)
	assert.NilError(t, err)
	assert.Equal(t, checks[0].Status, StatusFailed)
	assert.Equal(t, checks[0].Output, "main.go:3 R bad line 3")
}
