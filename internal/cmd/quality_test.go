package cmd

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"gotest.tools/v3/assert"
	"gotest.tools/v3/golden"

	"github.com/smeltdev/smelt/internal/iostream"
	"github.com/smeltdev/smelt/internal/smeltd"
)

// qualityFixture is a git repository holding testdata/quality, with the config
// that lowers the complexity limit to 10.
func qualityFixture(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	src, err := os.ReadFile("testdata/quality/shape.go")
	assert.NilError(t, err)
	assert.NilError(t, os.WriteFile(filepath.Join(dir, "shape.go"), src, 0o644))
	assert.NilError(t, os.MkdirAll(filepath.Join(dir, ".smelt"), 0o755))
	assert.NilError(t, os.WriteFile(filepath.Join(dir, ".smelt", "config.json"), []byte(`{"factory":{"quality":{"maxFunctionComplexity":10}}}`), 0o644))
	for _, args := range [][]string{{"init", "-q"}, {"add", "-A"}, {"-c", "user.name=t", "-c", "user.email=t@example.com", "-c", "commit.gpgsign=false", "commit", "-q", "-m", "base"}} {
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		out, err := cmd.CombinedOutput()
		assert.NilError(t, err, string(out))
	}
	return dir
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
	dir := qualityFixture(t)
	out, err := runQualityCmd(t, "--json", "--project", dir)
	assert.NilError(t, err)
	golden.Assert(t, out, "quality.golden.json")
}

func TestQualityHumanOutput(t *testing.T) {
	dir := qualityFixture(t)
	out, err := runQualityCmd(t, "--project", dir)
	assert.NilError(t, err)
	assert.Assert(t, strings.Contains(out, "quality: erosion 97.7% · functions over 10: 1 · clone lines 0"), out)
	assert.Assert(t, strings.Contains(out, "Classify  shape.go:4"), out)
}

func TestQualityBaseListsFindingsAndFailsOnMedium(t *testing.T) {
	dir := qualityFixture(t)
	// Nothing changed since HEAD.
	out, err := runQualityCmd(t, "--project", dir, "--base", "HEAD")
	assert.NilError(t, err)
	assert.Assert(t, strings.Contains(out, "erosion 97.7% → 97.7%"), out)

	src, err := os.ReadFile(filepath.Join(dir, "shape.go"))
	assert.NilError(t, err)
	grown := bytes.Replace(src, []byte("return \"large\""), []byte("if n == 7 {\n\t\treturn \"seven\"\n\t}\n\treturn \"large\""), 1)
	assert.NilError(t, os.WriteFile(filepath.Join(dir, "shape.go"), grown, 0o644))
	out, err = runQualityCmd(t, "--project", dir, "--base", "HEAD")
	assert.ErrorContains(t, err, "1 finding(s) to fix")
	assert.Assert(t, strings.Contains(out, "[medium] shape.go:4 Classify has cyclomatic complexity"), out)

	// Without --base it only reports.
	_, err = runQualityCmd(t, "--project", dir)
	assert.NilError(t, err)
}

func TestQualityWithoutGoIsNotMeasured(t *testing.T) {
	dir := t.TempDir()
	assert.NilError(t, os.WriteFile(filepath.Join(dir, "main.py"), []byte("print(1)\n"), 0o644))
	cmd := exec.Command("git", "init", "-q")
	cmd.Dir = dir
	assert.NilError(t, cmd.Run())
	out, err := runQualityCmd(t, "--project", dir)
	assert.NilError(t, err)
	assert.Assert(t, strings.Contains(out, "not measured"), out)
}

func TestReportCheckPrintsTheQualityLine(t *testing.T) {
	var lines []string
	r := newSessionReporter(func(_ iostream.Level, msg string) { lines = append(lines, msg) })
	const line = "erosion 27.3% → 27.1% · functions over 15: 14 → 14 · clone lines 120 → 120"
	r.reportCheck(smeltd.RoundCheck{Name: "quality", Status: smeltd.CheckPassed, Summary: line})
	r.reportCheck(smeltd.RoundCheck{Name: "quality", Status: smeltd.CheckFailed, Summary: line, Output: "- a.go:3: f has complexity 16"})
	assert.DeepEqual(t, lines, []string{"  quality  " + line, "  quality  " + line, "    - a.go:3: f has complexity 16"})
}
