package commandutil

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"gotest.tools/v3/assert"
)

// runWrapped runs the wrapped command in a git repo whose PATH holds a fake
// pytest that exits with code, tracking testFile when it is not empty.
func runWrapped(t *testing.T, command string, code int, testFile string) int {
	t.Helper()
	dir := t.TempDir()
	bin := t.TempDir()
	script := "#!/bin/sh\nexit " + string(rune('0'+code)) + "\n"
	for _, n := range []string{"pytest", "py.test", "python", "python3"} {
		assert.NilError(t, os.WriteFile(filepath.Join(bin, n), []byte(script), 0o755))
	}
	git := func(args ...string) {
		c := exec.Command("git", args...)
		c.Dir = dir
		out, err := c.CombinedOutput()
		assert.NilError(t, err, string(out))
	}
	git("init")
	if testFile != "" {
		assert.NilError(t, os.MkdirAll(filepath.Dir(filepath.Join(dir, testFile)), 0o755))
		assert.NilError(t, os.WriteFile(filepath.Join(dir, testFile), nil, 0o644))
		git("add", testFile)
	}
	c := exec.Command("sh", "-c", WrapPytestNoTests(command))
	c.Dir = dir
	c.Env = append(os.Environ(), "PATH="+bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	err := c.Run()
	if err == nil {
		return 0
	}
	var exitErr *exec.ExitError
	assert.Assert(t, errors.As(err, &exitErr), err)
	return exitErr.ExitCode()
}

func TestWrapPytestNoTests(t *testing.T) {
	for _, command := range []string{"pytest", "python3 -m pytest -q", "py.test"} {
		t.Run(command, func(t *testing.T) {
			assert.Equal(t, runWrapped(t, command, 5, ""), 0, "exit 5 with no test files passes")
			assert.Equal(t, runWrapped(t, command, 5, "tests/test_a.py"), 5, "exit 5 with test_*.py fails")
			assert.Equal(t, runWrapped(t, command, 5, "pkg/a_test.py"), 5, "exit 5 with *_test.py fails")
			assert.Equal(t, runWrapped(t, command, 1, ""), 1)
			assert.Equal(t, runWrapped(t, command, 0, "tests/test_a.py"), 0)
		})
	}
}

func TestWrapPytestNoTestsLeavesOtherCommands(t *testing.T) {
	for _, command := range []string{"go test ./...", "task test", "echo pytest"} {
		assert.Equal(t, WrapPytestNoTests(command), command)
	}
	assert.Assert(t, strings.Contains(WrapPytestNoTests("pytest -x"), "pytest -x"))
}
