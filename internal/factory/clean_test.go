package factory

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"gotest.tools/v3/assert"

	"github.com/smeltdev/smelt/internal/config"
	"github.com/smeltdev/smelt/internal/testing/gitrepo"
)

type cleanEnv struct {
	root    string
	dataDir string
}

func newCleanEnv(t *testing.T) cleanEnv {
	t.Helper()
	t.Setenv(config.EnvXDGDataHome, t.TempDir())
	root := gitrepo.SetupGitRepo(t, "org", "repo")
	root, err := filepath.EvalSymlinks(root)
	assert.NilError(t, err)
	dataDir, err := config.ProjectDataDir(root)
	assert.NilError(t, err)
	return cleanEnv{root: root, dataDir: dataDir}
}

func (e cleanEnv) git(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = gitrepo.GitEnv(e.root)
	out, err := cmd.CombinedOutput()
	assert.NilError(t, err, string(out))
	return string(out)
}

// worktree adds a worktree on a factory branch for runID and returns its path.
func (e cleanEnv) worktree(t *testing.T, runID string) (path, branch string) {
	t.Helper()
	path = filepath.Join(e.dataDir, "factory", runID)
	branch = branchPrefix + "do-it/" + runID
	e.git(t, e.root, "worktree", "add", "--quiet", "-b", branch, path)
	return path, branch
}

func (e cleanEnv) record(t *testing.T, r Record) {
	t.Helper()
	assert.NilError(t, saveRecord(e.dataDir, r))
}

func (e cleanEnv) branchExists(t *testing.T, branch string) bool {
	t.Helper()
	cmd := exec.Command("git", "rev-parse", "--verify", "--quiet", "refs/heads/"+branch)
	cmd.Dir = e.root
	cmd.Env = gitrepo.GitEnv(e.root)
	return cmd.Run() == nil
}

func (e cleanEnv) inventory(t *testing.T, active ...string) []RunEntry {
	t.Helper()
	set := map[string]bool{}
	for _, id := range active {
		set[id] = true
	}
	entries, err := Inventory(context.Background(), e.root, set)
	assert.NilError(t, err)
	return entries
}

func TestInventoryStates(t *testing.T) {
	e := newCleanEnv(t)
	wt1, br1 := e.worktree(t, "20261010-100000")
	e.record(t, Record{RunID: "20261010-100000", Prompt: "Finish it\nmore", Worktree: wt1, Branch: br1, Result: ResultPassed, Rounds: 3})
	wt2, br2 := e.worktree(t, "20261010-110000")
	e.record(t, Record{RunID: "20261010-110000", Worktree: wt2, Branch: br2})
	wt3, _ := e.worktree(t, "20261010-120000")

	entries := e.inventory(t)
	assert.Equal(t, len(entries), 3)
	MarkDirty(context.Background(), entries)
	// Newest first.
	assert.Equal(t, entries[0].ID(), "20261010-120000")
	assert.Equal(t, entries[0].State, StateOrphaned)
	// git reports an orphaned worktree by its resolved path: /private/var for
	// /var on macOS.
	assert.Assert(t, samePath(entries[0].Worktree, wt3), "%s is not %s", entries[0].Worktree, wt3)
	assert.Equal(t, entries[1].State, StateAbandoned)
	assert.Equal(t, entries[2].State, StateFinished)
	assert.Equal(t, entries[2].Result, ResultPassed)
	assert.Equal(t, entries[2].Rounds, 3)
	assert.Equal(t, entries[2].Prompt, "Finish it")
	assert.Assert(t, entries[2].WorktreeExists)
	assert.Assert(t, entries[2].Dirty != nil && !*entries[2].Dirty)
	assert.Assert(t, entries[2].Dirty != nil)
	assert.Equal(t, entries[2].StartedAt.Hour(), 10)

	assert.Equal(t, e.inventory(t, "20261010-110000")[1].State, StateRunning)
}

func TestInventoryMissingWorktreeAndBranchOnly(t *testing.T) {
	e := newCleanEnv(t)
	wt, br := e.worktree(t, "20261010-100000")
	e.record(t, Record{RunID: "20261010-100000", Worktree: wt, Branch: br, Result: ResultPassed})
	assert.NilError(t, os.RemoveAll(wt))
	e.git(t, e.root, "branch", branchPrefix+"lonely/20261009-000000")

	entries := e.inventory(t)
	assert.Equal(t, len(entries), 2)
	assert.Equal(t, entries[0].ID(), "20261010-100000")
	assert.Assert(t, !entries[0].WorktreeExists)
	assert.Equal(t, entries[1].ID(), "20261009-000000")
	assert.Equal(t, entries[1].State, StateOrphaned)
	assert.Equal(t, entries[1].Worktree, "")
}

func TestInventoryGroupsAChain(t *testing.T) {
	e := newCleanEnv(t)
	wt, br := e.worktree(t, "20261010-100000")
	e.record(t, Record{RunID: "20261010-100000", Worktree: wt, Branch: br, Result: ResultExhausted})
	e.record(t, Record{RunID: "20261010-130000", Worktree: wt, Branch: br, ContinuesRunID: "20261010-100000"})

	entries := e.inventory(t)
	assert.Equal(t, len(entries), 1)
	assert.DeepEqual(t, entries[0].RunIDs, []string{"20261010-100000", "20261010-130000"})
	assert.Equal(t, entries[0].State, StateAbandoned)
}

func TestInventoryDirtyWorktree(t *testing.T) {
	e := newCleanEnv(t)
	wt, br := e.worktree(t, "20261010-100000")
	e.record(t, Record{RunID: "20261010-100000", Worktree: wt, Branch: br, Result: ResultPassed})
	assert.NilError(t, os.WriteFile(filepath.Join(wt, "new.txt"), []byte("x"), 0o644))
	entries := e.inventory(t)
	MarkDirty(context.Background(), entries)
	assert.Assert(t, entries[0].IsDirty())
}

func TestInventoryRunsNoGitStatus(t *testing.T) {
	e := newCleanEnv(t)
	wt, br := e.worktree(t, "20261010-100000")
	e.record(t, Record{RunID: "20261010-100000", Worktree: wt, Branch: br, Result: ResultPassed})

	realGit, err := exec.LookPath("git")
	assert.NilError(t, err)
	shimDir := t.TempDir()
	logFile := filepath.Join(shimDir, "calls.log")
	shim := "#!/bin/sh\necho \"$@\" >> '" + logFile + "'\nexec '" + realGit + "' \"$@\"\n"
	assert.NilError(t, os.WriteFile(filepath.Join(shimDir, "git"), []byte(shim), 0o755))
	t.Setenv("PATH", shimDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	entries := e.inventory(t)
	assert.Assert(t, entries[0].WorktreeExists)
	assert.Assert(t, entries[0].Dirty == nil)
	calls, err := os.ReadFile(logFile)
	assert.NilError(t, err)
	assert.Assert(t, len(calls) > 0, "the shim saw no git call, so it proves nothing")
	assert.Assert(t, !strings.Contains(string(calls), "status"), string(calls))
}

func TestMarkDirtyCleanDirtyUnreadable(t *testing.T) {
	e := newCleanEnv(t)
	cleanWT, cleanBr := e.worktree(t, "20261010-100000")
	e.record(t, Record{RunID: "20261010-100000", Worktree: cleanWT, Branch: cleanBr, Result: ResultPassed})
	dirtyWT, dirtyBr := e.worktree(t, "20261010-110000")
	e.record(t, Record{RunID: "20261010-110000", Worktree: dirtyWT, Branch: dirtyBr, Result: ResultPassed})
	assert.NilError(t, os.WriteFile(filepath.Join(dirtyWT, "new.txt"), []byte("x"), 0o644))
	brokenWT, brokenBr := e.worktree(t, "20261010-120000")
	e.record(t, Record{RunID: "20261010-120000", Worktree: brokenWT, Branch: brokenBr, Result: ResultPassed})
	assert.NilError(t, os.WriteFile(filepath.Join(brokenWT, ".git"), []byte("garbage"), 0o644))
	e.git(t, e.root, "branch", branchPrefix+"lonely/20261009-000000")

	entries := e.inventory(t)
	MarkDirty(context.Background(), entries)
	byID := map[string]RunEntry{}
	for _, en := range entries {
		byID[en.ID()] = en
	}
	assert.Assert(t, byID["20261010-100000"].Dirty != nil && !*byID["20261010-100000"].Dirty)
	assert.Assert(t, byID["20261010-110000"].IsDirty())
	assert.Assert(t, byID["20261010-120000"].IsDirty(), "an unreadable status counts as dirty")
	assert.Assert(t, byID["20261009-000000"].Dirty == nil)
}

func TestDirtyOrder(t *testing.T) {
	for name, tc := range map[string]struct {
		entries []RunEntry
		want    []int
	}{
		"empty": {nil, nil},
		"running first": {[]RunEntry{
			{WorktreeExists: true, State: StateFinished},
			{WorktreeExists: true, State: StateRunning},
			{WorktreeExists: true, State: StateAbandoned},
			{WorktreeExists: true, State: StateRunning},
		}, []int{1, 3, 0, 2}},
		"skips missing worktrees": {[]RunEntry{
			{State: StateRunning},
			{WorktreeExists: true, State: StateFinished},
		}, []int{1}},
	} {
		t.Run(name, func(t *testing.T) {
			assert.DeepEqual(t, dirtyOrder(tc.entries), tc.want)
		})
	}
}

func TestMarkDirtyBoundsConcurrency(t *testing.T) {
	entries := make([]RunEntry, 20)
	for i := range entries {
		entries[i] = RunEntry{WorktreeExists: true, State: StateFinished, Worktree: "wt"}
	}
	var cur, peak atomic.Int32
	markDirty(context.Background(), entries, func(context.Context, string) bool {
		n := cur.Add(1)
		for {
			p := peak.Load()
			if n <= p || peak.CompareAndSwap(p, n) {
				break
			}
		}
		time.Sleep(10 * time.Millisecond)
		cur.Add(-1)
		return false
	})
	assert.Assert(t, peak.Load() <= dirtyWorkers, "peak %d", peak.Load())
	assert.Assert(t, peak.Load() > 1, "checks did not run concurrently")
	for _, en := range entries {
		assert.Assert(t, en.Dirty != nil)
	}
}

func TestRemoveRunChain(t *testing.T) {
	e := newCleanEnv(t)
	wt, br := e.worktree(t, "20261010-100000")
	e.record(t, Record{RunID: "20261010-100000", Worktree: wt, Branch: br, Result: ResultExhausted})
	e.record(t, Record{RunID: "20261010-130000", Worktree: wt, Branch: br, Result: ResultPassed})

	got, err := RemoveRun(context.Background(), e.root, e.inventory(t)[0], false)
	assert.NilError(t, err)
	assert.DeepEqual(t, got, Removal{Worktree: true, Branch: true, Records: 2})
	_, statErr := os.Stat(wt)
	assert.Assert(t, errors.Is(statErr, os.ErrNotExist))
	assert.Assert(t, !e.branchExists(t, br))
	assert.Equal(t, len(e.inventory(t)), 0)
	assert.Assert(t, !strings.Contains(e.git(t, e.root, "worktree", "list"), wt))
}

func TestRemoveRunRefusesRunning(t *testing.T) {
	e := newCleanEnv(t)
	wt, br := e.worktree(t, "20261010-100000")
	e.record(t, Record{RunID: "20261010-100000", Worktree: wt, Branch: br})

	_, err := RemoveRun(context.Background(), e.root, e.inventory(t, "20261010-100000")[0], true)
	assert.Assert(t, errors.Is(err, ErrRunning))
	assert.Assert(t, e.branchExists(t, br))
}

func TestRemoveRunDirty(t *testing.T) {
	e := newCleanEnv(t)
	wt, br := e.worktree(t, "20261010-100000")
	e.record(t, Record{RunID: "20261010-100000", Worktree: wt, Branch: br, Result: ResultPassed})
	assert.NilError(t, os.WriteFile(filepath.Join(wt, "new.txt"), []byte("x"), 0o644))
	entries := e.inventory(t)
	MarkDirty(context.Background(), entries)
	entry := entries[0]

	_, err := RemoveRun(context.Background(), e.root, entry, false)
	assert.Assert(t, errors.Is(err, ErrDirty))
	assert.ErrorContains(t, err, wt)
	assert.Assert(t, e.branchExists(t, br))

	_, err = RemoveRun(context.Background(), e.root, entry, true)
	assert.NilError(t, err)
	assert.Assert(t, !e.branchExists(t, br))
}

func TestRemoveRunWorktreeAlreadyGoneAndIdempotent(t *testing.T) {
	e := newCleanEnv(t)
	wt, br := e.worktree(t, "20261010-100000")
	e.record(t, Record{RunID: "20261010-100000", Worktree: wt, Branch: br, Result: ResultPassed})
	entry := e.inventory(t)[0]
	assert.NilError(t, os.RemoveAll(wt))

	got, err := RemoveRun(context.Background(), e.root, entry, false)
	assert.NilError(t, err)
	assert.DeepEqual(t, got, Removal{Branch: true, Records: 1})
	assert.Assert(t, !e.branchExists(t, br))

	got, err = RemoveRun(context.Background(), e.root, entry, false)
	assert.NilError(t, err)
	assert.DeepEqual(t, got, Removal{})
}
