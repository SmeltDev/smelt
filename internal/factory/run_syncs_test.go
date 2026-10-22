package factory

import (
	"context"
	"strings"
	"testing"

	"gotest.tools/v3/assert"

	"github.com/smeltdev/smelt/internal/config"
	"github.com/smeltdev/smelt/internal/iostream"
	"github.com/smeltdev/smelt/internal/review"
)

// syncedProject supplies the GitHub URL the sidecar pool needs for setup,
// and a local bare remote to use once the run starts syncing.
func syncedProject(t *testing.T) (root, origin string) {
	t.Helper()
	root, origin, _ = updateProject(t)
	gitOutput(t, root, "remote", "set-url", "origin", "https://github.com/o/r.git")
	return root, origin
}

// A sync queued while a run is going is applied by Run at the end of the
// round: the next round starts on the merged tree, the run's worktree holds
// the moved baseline, and the saved record says the sync is done.
func TestRunAppliesAQueuedSyncBetweenRounds(t *testing.T) {
	skipWithoutTools(t)
	t.Setenv(config.EnvHome, t.TempDir())
	ctx := context.Background()
	root, origin := syncedProject(t)
	home := t.TempDir()
	installFakeClaude(t, home, "if [ -f other.go ]; then echo 'package main' > b.go; else echo 'package main' > a.go; fi\n"+streamOK)
	backend := &localBackend{dir: t.TempDir()}
	queue := &SyncQueue{}
	rep, err := Run(ctx, RunOptions{
		Root: root, Prompt: "add work", Attempts: 3, Syncs: queue,
		Backend:    backend,
		Exec:       backend.inWorkspace(withoutPkill(localExec(home))),
		Credential: review.Credential{EnvVar: "ANTHROPIC_API_KEY", Value: "k"},
		Commands:   []config.Command{{Name: "test", Run: "test -f b.go"}},
		Status:     func(iostream.Level, string) {},
		OnEvent: func(e Event) {
			if e.Kind != EventImplementing || e.Round != 1 {
				return
			}
			gitOutput(t, root, "remote", "set-url", "origin", origin)
			landOnMain(t, root, "other.go", "package main\n")
			_, err := queue.Enqueue("main")
			assert.NilError(t, err)
		},
	})
	assert.NilError(t, err)
	assert.Equal(t, rep.Outcome.Result, ResultPassed)
	assert.Equal(t, rep.Outcome.Rounds, 2)

	tip := gitOutput(t, root, "rev-parse", "main")
	assert.Equal(t, rep.Worktree.Baseline, tip, "the report holds the moved baseline")
	saved, err := LoadRecord(root, rep.RunID)
	assert.NilError(t, err)
	assert.Equal(t, len(saved.Syncs), 1)
	assert.Equal(t, saved.Syncs[0].State, SyncDone)
	assert.Equal(t, saved.Syncs[0].Round, 1)
	assert.Equal(t, saved.Syncs[0].Commit, tip)
	assert.Equal(t, saved.Baseline, tip)
	files := gitOutput(t, root, "diff", "--name-only", tip, saved.Branch)
	assert.Equal(t, files, "a.go\nb.go", "the branch's own change excludes the base's files")
}

// A sync still queued when the run ends is dropped, and the saved record says
// so.
func TestRunSavesTheSyncsItDropped(t *testing.T) {
	skipWithoutTools(t)
	t.Setenv(config.EnvHome, t.TempDir())
	root, _ := syncedProject(t)
	home := t.TempDir()
	installFakeClaude(t, home, "echo 'package main' > a.go\n"+streamOK)
	backend := &localBackend{dir: t.TempDir()}
	queue := &SyncQueue{}
	rep, err := Run(context.Background(), RunOptions{
		Root: root, Prompt: "add work", Attempts: 2, Syncs: queue,
		Backend:    backend,
		Exec:       backend.inWorkspace(withoutPkill(localExec(home))),
		Credential: review.Credential{EnvVar: "ANTHROPIC_API_KEY", Value: "k"},
		Status:     func(iostream.Level, string) {},
		OnEvent: func(e Event) {
			if e.Kind == EventImplementing {
				_, err := queue.Enqueue("main")
				assert.NilError(t, err)
			}
		},
	})
	assert.NilError(t, err)
	assert.Equal(t, rep.Outcome.Result, ResultPassed)

	saved, err := LoadRecord(root, rep.RunID)
	assert.NilError(t, err)
	assert.Equal(t, len(saved.Syncs), 1)
	assert.Equal(t, saved.Syncs[0].State, SyncDropped)
	assert.Assert(t, strings.Contains(saved.Syncs[0].Reason, "ended"), saved.Syncs[0].Reason)
	_, err = queue.Enqueue("main")
	assert.ErrorIs(t, err, ErrSyncsClosed)
}

// Setup failures must close the queue even before a worktree or loop exists.
func TestRunDropsSyncWhenSetupFails(t *testing.T) {
	queue := &SyncQueue{}
	_, err := queue.Enqueue("main")
	assert.NilError(t, err)
	_, err = Run(t.Context(), RunOptions{
		Syncs: queue, From: "main", Continue: &Continuation{},
	})
	assert.ErrorContains(t, err, "cannot start from another ref")
	syncs := queue.Snapshot()
	assert.Equal(t, len(syncs), 1)
	assert.Equal(t, syncs[0].State, SyncDropped)
	_, err = queue.Enqueue("main")
	assert.ErrorIs(t, err, ErrSyncsClosed)
}
