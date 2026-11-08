package factory

import (
	"testing"

	"gotest.tools/v3/assert"

	"github.com/smeltdev/smelt/internal/envspec"
	"github.com/smeltdev/smelt/internal/sidecar"
)

func TestPrepareSidecarsPassesTheStackToTheImplementer(t *testing.T) {
	for _, stack := range []string{envspec.StackGo, "python", ""} {
		opts := RunOptions{Stack: stack}
		steps := opts.prepareSidecars("ticket", Worktree{}, &sidecar.Pool{}, &sidecar.PoolEntry{}, nil)
		assert.Equal(t, steps.Implementer.Stack, stack)
	}
}
