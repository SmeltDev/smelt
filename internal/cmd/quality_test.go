package cmd

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"gotest.tools/v3/assert"
	"gotest.tools/v3/golden"

	"github.com/smeltdev/smelt/internal/config"
	"github.com/smeltdev/smelt/internal/factory"
	"github.com/smeltdev/smelt/internal/iostream"
	"github.com/smeltdev/smelt/internal/quality"
	"github.com/smeltdev/smelt/internal/sidecar"
	"github.com/smeltdev/smelt/internal/smeltd"
)

// qualityFixture is a git repository holding testdata/quality, with a config
// that lowers the complexity limit to 10 and has a project's own provider
// print contract.json, which stands in for lizard: the tests need neither a
// container nor lizard.
func qualityFixture(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	for _, name := range []string{"shape.go", "contract.json"} {
		src, err := os.ReadFile(filepath.Join("testdata", "quality", name))
		assert.NilError(t, err)
		assert.NilError(t, os.WriteFile(filepath.Join(dir, name), src, 0o644))
	}
	assert.NilError(t, os.MkdirAll(filepath.Join(dir, ".smelt"), 0o755))
	assert.NilError(t, os.WriteFile(filepath.Join(dir, ".smelt", "config.json"),
		[]byte(`{"factory":{"quality":{"maxFunctionComplexity":10,"command":"cat contract.json"}}}`), 0o644))
	for _, args := range [][]string{{"init", "-q"}, {"add", "-A"}, {"-c", "user.name=t", "-c", "user.email=t@example.com", "-c", "commit.gpgsign=false", "commit", "-q", "-m", "base"}} {
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		out, err := cmd.CombinedOutput()
		assert.NilError(t, err, string(out))
	}
	return dir
}

// localMeasurer measures in the project directory itself, with sh, as a
// container would in its copy.
func localMeasurer(t *testing.T) qualityMeasurerOpener {
	t.Helper()
	run := func(ctx context.Context, _ *sidecar.PoolEntry, script string, _ map[string]string, out iostream.OutputFn, _ func(string)) (int, error) {
		cmd := exec.CommandContext(ctx, "sh", "-c", script)
		cmd.Stdout = outWriter{out, iostream.StreamStdout}
		cmd.Stderr = outWriter{out, iostream.StreamStderr}
		var exitErr *exec.ExitError
		if err := cmd.Run(); errors.As(err, &exitErr) {
			return exitErr.ExitCode(), nil
		} else if err != nil {
			return 0, err
		}
		return 0, nil
	}
	return func(_ context.Context, _ iostream.Streams, root string, cfg *config.ProjectConfig) (quality.Measurer, func(), error) {
		qcfg := factory.QualityLimits(cfg.Factory)
		return factory.SandboxMeasurer{
			Exec: run, Entry: &sidecar.PoolEntry{ID: "local", RepoPath: root},
			Provider: factory.ProviderFor(qcfg), Config: qcfg, Worktree: filepath.Join(t.TempDir(), "rev"),
		}, func() {}, nil
	}
}

type outWriter struct {
	fn     iostream.OutputFn
	stream string
}

func (w outWriter) Write(b []byte) (int, error) {
	w.fn(w.stream, append([]byte(nil), b...))
	return len(b), nil
}

func useLocalMeasurer(t *testing.T) {
	t.Helper()
	for _, tool := range []string{"git", "sh", "tar", "base64"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skipf("%s is not installed", tool)
		}
	}
	if err := exec.Command("base64", "-w0", os.DevNull).Run(); err != nil {
		t.Skip("this base64 has no -w0")
	}
	old := openQualityMeasurer
	openQualityMeasurer = localMeasurer(t)
	t.Cleanup(func() { openQualityMeasurer = old })
}

func runQualityCmd(t *testing.T, args ...string) (string, error) {
	t.Helper()
	cmd := newQualityCmd()
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	cmd.SetArgs(args)
	cmd.SetContext(context.Background())
	err := cmd.Execute()
	return out.String(), err
}

func TestQualityJSONGolden(t *testing.T) {
	useLocalMeasurer(t)
	dir := qualityFixture(t)
	out, err := runQualityCmd(t, "--json", "--project", dir)
	assert.NilError(t, err)
	golden.Assert(t, out, "quality.golden.json")
}

func TestQualityHumanOutput(t *testing.T) {
	useLocalMeasurer(t)
	dir := qualityFixture(t)
	out, err := runQualityCmd(t, "--project", dir)
	assert.NilError(t, err)
	assert.Assert(t, strings.Contains(out, "quality: erosion 97.7% · functions over 10: 1 · clone lines 0"), out)
	assert.Assert(t, strings.Contains(out, "Classify  shape.go:4"), out)
}

func TestQualityBaseListsFindingsAndFailsOnMedium(t *testing.T) {
	useLocalMeasurer(t)
	dir := qualityFixture(t)
	// Nothing changed since HEAD.
	out, err := runQualityCmd(t, "--project", dir, "--base", "HEAD")
	assert.NilError(t, err)
	assert.Assert(t, strings.Contains(out, "erosion 97.7% → 97.7%"), out)

	src, err := os.ReadFile(filepath.Join(dir, "shape.go"))
	assert.NilError(t, err)
	grown := bytes.Replace(src, []byte("return \"large\""), []byte("if n == 7 {\n\t\treturn \"seven\"\n\t}\n\treturn \"large\""), 1)
	assert.NilError(t, os.WriteFile(filepath.Join(dir, "shape.go"), grown, 0o644))
	// The provider reports the edit: three lines more and one branch more.
	contract, err := os.ReadFile(filepath.Join(dir, "contract.json"))
	assert.NilError(t, err)
	contract = []byte(strings.NewReplacer(`"end": 31, "complexity": 14, "lines": 28`, `"end": 34, "complexity": 15, "lines": 31`,
		`"start": 34, "end": 36`, `"start": 37, "end": 39`).Replace(string(contract)))
	assert.NilError(t, os.WriteFile(filepath.Join(dir, "contract.json"), contract, 0o644))
	out, err = runQualityCmd(t, "--project", dir, "--base", "HEAD")
	assert.ErrorContains(t, err, "1 finding(s) to fix")
	assert.Assert(t, strings.Contains(out, "[medium] shape.go:4 Classify has cyclomatic complexity 15 (grew from 14"), out)

	// Without --base it only reports.
	_, err = runQualityCmd(t, "--project", dir)
	assert.NilError(t, err)
}

func TestQualityWithNothingMeasuredSaysSo(t *testing.T) {
	useLocalMeasurer(t)
	dir := qualityFixture(t)
	assert.NilError(t, os.WriteFile(filepath.Join(dir, "contract.json"), []byte(`{"functions": [], "not_measured": ["lib/thing.ex"]}`), 0o644))
	out, err := runQualityCmd(t, "--project", dir)
	assert.NilError(t, err)
	assert.Assert(t, strings.Contains(out, "quality: not measured"), out)
	assert.Assert(t, strings.Contains(out, "not measured: lib/thing.ex"), out)
}

func TestQualityFailsOnOutputThatIsNotTheContract(t *testing.T) {
	useLocalMeasurer(t)
	dir := qualityFixture(t)
	assert.NilError(t, os.WriteFile(filepath.Join(dir, "contract.json"), []byte("12 functions"), 0o644))
	_, err := runQualityCmd(t, "--project", dir)
	var ue *userError
	assert.Assert(t, errors.As(err, &ue))
	assert.Equal(t, ue.msg, "Could not measure the code.")
	assert.Assert(t, strings.Contains(ue.err.Error(), "parse the metrics contract"), ue.err.Error())
}

// The --json shape: every function carries its language, and the top level an
// erosion value.
func TestQualityJSONShape(t *testing.T) {
	a := quality.Assessment{After: quality.Measure(
		quality.Contract{Languages: []string{"go", "python"}, Functions: []quality.ContractFunction{
			{File: "a.go", Name: "F", Language: "go", Start: 1, End: 3, Complexity: 12, Lines: 3},
			{File: "b.py", Name: "g", Language: "python", Start: 1, End: 3, Complexity: 2, Lines: 3},
		}, NotMeasured: []string{"c.ex"}},
		[]quality.File{{Path: "a.go", Content: []byte("x\ny\nz\n")}, {Path: "b.py", Content: []byte("x\ny\nz\n")}}, quality.Config{})}
	data, err := json.Marshal(wireAssessment(a, ""))
	assert.NilError(t, err)
	var got struct {
		Measured    bool     `json:"measured"`
		Erosion     float64  `json:"erosion"`
		NotMeasured []string `json:"not_measured"`
		Worst       []struct {
			Func     string `json:"func"`
			Language string `json:"language"`
		} `json:"worst"`
	}
	assert.NilError(t, json.Unmarshal(data, &got))
	assert.Assert(t, got.Measured && got.Erosion > 0)
	assert.DeepEqual(t, got.NotMeasured, []string{"c.ex"})
	assert.Equal(t, got.Worst[0].Func+"/"+got.Worst[0].Language, "F/go")
	assert.Equal(t, got.Worst[1].Func+"/"+got.Worst[1].Language, "g/python")
}

func TestReportCheckPrintsTheQualityLine(t *testing.T) {
	var lines []string
	r := newSessionReporter(func(_ iostream.Level, msg string) { lines = append(lines, msg) })
	const line = "erosion 27.3% → 27.1% · functions over 15: 14 → 14 · clone lines 120 → 120"
	r.reportCheck(smeltd.RoundCheck{Name: "quality", Status: smeltd.CheckPassed, Summary: line})
	r.reportCheck(smeltd.RoundCheck{Name: "quality", Status: smeltd.CheckFailed, Summary: line, Output: "- a.go:3: f has complexity 16"})
	assert.DeepEqual(t, lines, []string{"  quality  " + line, "  quality  " + line, "    - a.go:3: f has complexity 16"})
}
