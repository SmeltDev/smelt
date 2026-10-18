// Package restartscript tests scripts/restart-daemon.sh with a fake smelt
// binary, so the fallback for a daemon too old to drain is exercised without
// a real daemon.
package restartscript

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"gotest.tools/v3/assert"
	"gotest.tools/v3/assert/cmp"
)

// runScript runs a copy of restart-daemon.sh whose dist/smelt is a shell
// script with the given body. A fake pgrep reports a running daemon, and the
// daemon directory is empty, so the fallback cannot ask it about its runs.
func runScript(t *testing.T, fakeSmelt string) (string, error) {
	t.Helper()
	root := t.TempDir()
	script, err := os.ReadFile("../../scripts/restart-daemon.sh")
	assert.NilError(t, err)
	assert.NilError(t, os.MkdirAll(filepath.Join(root, "scripts"), 0o755))
	assert.NilError(t, os.MkdirAll(filepath.Join(root, "dist"), 0o755))
	assert.NilError(t, os.MkdirAll(filepath.Join(root, "bin"), 0o755))
	assert.NilError(t, os.WriteFile(filepath.Join(root, "scripts", "restart-daemon.sh"), script, 0o755))
	assert.NilError(t, os.WriteFile(filepath.Join(root, "dist", "smelt"), []byte("#!/bin/sh\n"+fakeSmelt), 0o755))
	assert.NilError(t, os.WriteFile(filepath.Join(root, "bin", "pgrep"), []byte("#!/bin/sh\nexit 0\n"), 0o755))

	cmd := exec.Command("bash", filepath.Join(root, "scripts", "restart-daemon.sh"))
	cmd.Env = append(os.Environ(),
		"PATH="+filepath.Join(root, "bin")+string(os.PathListSeparator)+os.Getenv("PATH"),
		"SMELT_DAEMON_DIR="+filepath.Join(root, "daemon"))
	out, err := cmd.CombinedOutput()
	return string(out), err
}

const fakeSmeltHead = `case "$*" in
"daemon restart --help") echo "  --when-idle"; exit 0;;
esac
`

func TestFallsBackWhenDaemonTooOldToDrain(t *testing.T) {
	out, err := runScript(t, fakeSmeltHead+`echo "This smelt daemon is too old to drain." >&2; exit 1
`)
	assert.NilError(t, err, out)
	assert.Check(t, cmp.Contains(out, "smelt daemon left running"))
	assert.Check(t, cmp.Contains(out, "task daemon:restart"))
}

func TestOtherRestartFailuresPropagate(t *testing.T) {
	out, err := runScript(t, fakeSmeltHead+`echo "boom" >&2; exit 3
`)
	var exitErr *exec.ExitError
	assert.Assert(t, errors.As(err, &exitErr), out)
	assert.Equal(t, exitErr.ExitCode(), 3)
	assert.Check(t, cmp.Contains(out, "boom"))
}

func TestSuccessfulDrainIsReported(t *testing.T) {
	out, err := runScript(t, fakeSmeltHead+`echo drained; exit 0
`)
	assert.NilError(t, err, out)
	assert.Check(t, cmp.Contains(out, "drained"))
}
