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

	"github.com/smeltdev/smelt/internal/iostream"
	"github.com/smeltdev/smelt/internal/sandboximage"
)

type editFixer struct{ content string }

func (f editFixer) Fix(_ context.Context, dir, _ string) error {
	return os.WriteFile(filepath.Join(dir, "Dockerfile.smelt"), []byte(f.content), 0o644)
}

func repairRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	assert.NilError(t, os.WriteFile(filepath.Join(dir, "Dockerfile.smelt"), []byte("FROM x\n"), 0o644))
	for _, args := range [][]string{{"init", "-q"}, {"add", "-A"}, {"-c", "user.name=t", "-c", "user.email=t@t", "commit", "-qm", "init"}} {
		c := exec.Command("git", args...)
		c.Dir = dir
		out, err := c.CombinedOutput()
		assert.NilError(t, err, string(out))
	}
	return dir
}

func runRepair(t *testing.T, keep bool, retryFailure string) (dir string, errOut string, err error) {
	t.Helper()
	dir = repairRepo(t)
	var out bytes.Buffer
	streams := iostream.Streams{Out: &out, Err: &out}
	_, err = repairImage(t.Context(), streams, imageRepair{
		Root: dir, Dockerfile: "Dockerfile.smelt", Failure: "bad", KeepRepair: keep,
		Fixer: editFixer{content: "FROM y\n"},
		Retry: func(context.Context) (string, error) { return retryFailure, nil },
		Tag:   func() string { return "tag" },
	}, newUserError("failed"))
	return dir, out.String(), err
}

func TestRepairImageRestoresDockerfileOnFailure(t *testing.T) {
	dir, out, err := runRepair(t, false, "still broken")
	assert.Assert(t, err != nil)
	got, _ := os.ReadFile(filepath.Join(dir, "Dockerfile.smelt"))
	assert.Equal(t, string(got), "FROM x\n")
	saved, rerr := os.ReadFile(filepath.Join(dir, "Dockerfile.smelt.repair"))
	assert.NilError(t, rerr)
	assert.Equal(t, string(saved), "FROM y\n")
	assert.Assert(t, strings.Contains(out, "+FROM y"), out)
}

func TestRepairImageKeepRepairKeepsEdit(t *testing.T) {
	dir, _, err := runRepair(t, true, "still broken")
	assert.Assert(t, err != nil)
	got, _ := os.ReadFile(filepath.Join(dir, "Dockerfile.smelt"))
	assert.Equal(t, string(got), "FROM y\n")
	_, rerr := os.Stat(filepath.Join(dir, "Dockerfile.smelt.repair"))
	assert.NilError(t, rerr)
}

func TestRepairImageSuccessKeepsEdit(t *testing.T) {
	dir, _, err := runRepair(t, false, "")
	assert.NilError(t, err)
	got, _ := os.ReadFile(filepath.Join(dir, "Dockerfile.smelt"))
	assert.Equal(t, string(got), "FROM y\n")
	_, rerr := os.Stat(filepath.Join(dir, "Dockerfile.smelt.repair"))
	assert.Assert(t, os.IsNotExist(rerr))
}

func TestJudgeVerification(t *testing.T) {
	tests := []struct {
		name        string
		res         sandboximage.VerifyResult
		wantFailure bool
		wantWarning string
	}{
		{"passed", sandboximage.VerifyResult{}, false, ""},
		{"git refs are not the image", sandboximage.VerifyResult{Step: sandboximage.StepGate, Gate: "lint", ExitCode: 1, OutputTail: "\ntask: Command failed\nfatal: bad revision 'origin/main'"}, false,
			"the image is ready; the gate lint failed in verification: fatal: bad revision 'origin/main'"},
		{"missing command is the image", sandboximage.VerifyResult{Step: sandboximage.StepGate, Gate: "lint", ExitCode: 127, OutputTail: "golangci-lint: command not found"}, true, ""},
		{"install is the image", sandboximage.VerifyResult{Step: sandboximage.StepInstall, ExitCode: 1, OutputTail: "boom"}, true, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var out bytes.Buffer
			got := judgeVerification(iostream.Streams{Out: &out, Err: &out}, "tag", tt.res)
			assert.Equal(t, got.Tag, "tag")
			assert.Equal(t, got.Failure != "", tt.wantFailure)
			if tt.wantWarning == "" {
				assert.Equal(t, out.String(), "")
				return
			}
			assert.Assert(t, strings.Contains(out.String(), tt.wantWarning), out.String())
		})
	}
}
