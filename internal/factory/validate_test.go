package factory

import (
	"context"
	"strings"
	"testing"

	"gotest.tools/v3/assert"

	"github.com/smeltdev/smelt/internal/config"
	"github.com/smeltdev/smelt/internal/iostream"
	"github.com/smeltdev/smelt/internal/sidecar"
)

func TestValidationCommandsDropsInstall(t *testing.T) {
	got := ValidationCommands([]config.Command{
		{Name: config.CmdInstall, Run: "pip install -e ."},
		{Name: "test", Run: "pytest"},
		{Name: "format", Run: "ruff format", Role: config.RoleAutofix},
	})
	assert.Equal(t, len(got), 1)
	assert.Equal(t, got[0].Name, "test")
}

func TestRunCommandWrapsPytest(t *testing.T) {
	var script string
	exec := func(_ context.Context, _ *sidecar.PoolEntry, s string, _ map[string]string, _ iostream.OutputFn, _ func(string)) (int, error) {
		script = s
		return 0, nil
	}
	entry := &sidecar.PoolEntry{ID: "impl", RepoPath: "/work/repo"}

	runCommand(context.Background(), exec, entry, config.Command{Name: "test", Run: "pytest"})
	assert.Assert(t, strings.HasPrefix(script, "cd '/work/repo' && "), script)
	assert.Assert(t, strings.Contains(script, "-eq 5"), script)

	runCommand(context.Background(), exec, entry, config.Command{Name: "lint", Run: "ruff check"})
	assert.Equal(t, script, "cd '/work/repo' && ruff check")
}
