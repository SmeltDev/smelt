package factory

import (
	"context"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"gotest.tools/v3/assert"
)

var sameSecond = time.Date(2026, 10, 25, 20, 49, 5, 0, time.UTC)

func TestMintRunIDSuffixesSameSecond(t *testing.T) {
	dataDir := t.TempDir()
	for _, want := range []string{"20261025-204905", "20261025-204905-2", "20261025-204905-3"} {
		got, err := MintRunID(dataDir, sameSecond)
		assert.NilError(t, err)
		assert.Equal(t, got, want)
	}
	next, err := MintRunID(dataDir, sameSecond.Add(time.Second))
	assert.NilError(t, err)
	assert.Equal(t, next, "20261025-204906")
}

func TestMintRunIDConcurrent(t *testing.T) {
	dataDir := t.TempDir()
	var mu sync.Mutex
	var wg sync.WaitGroup
	ids := map[string]bool{}
	for range 20 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			id, err := MintRunID(dataDir, sameSecond)
			assert.Check(t, err)
			mu.Lock()
			ids[id] = true
			mu.Unlock()
		}()
	}
	wg.Wait()
	assert.Equal(t, len(ids), 20)
	dirs, err := os.ReadDir(filepath.Join(dataDir, "factory"))
	assert.NilError(t, err)
	assert.Equal(t, len(dirs), 20)
}

// A continuation mints like a fresh run: it needs no special case, and its
// reservation does not collide with a fresh run's worktree directory.
func TestMintRunIDContinuationInSameSecond(t *testing.T) {
	e := newCleanEnv(t)
	fresh, err := MintRunID(e.dataDir, sameSecond)
	assert.NilError(t, err)
	cont, err := MintRunID(e.dataDir, sameSecond)
	assert.NilError(t, err)
	assert.Assert(t, fresh != cont)
}

func TestParseRunStart(t *testing.T) {
	for _, id := range []string{"20261025-204905", "20261025-204905-2", "20261025-204905-12"} {
		got, err := ParseRunStart(id)
		assert.NilError(t, err, id)
		assert.Assert(t, got.Equal(sameSecond), id)
	}
	_, err := ParseRunStart("nonsense")
	assert.Assert(t, err != nil)
}

func TestBranchRoundTripsSuffixedID(t *testing.T) {
	assert.Equal(t, ParseRunID(branchName("do it", "20261025-204905-2")), "20261025-204905-2")
}

// git worktree add must accept the empty directory MintRunID reserved, and
// Inventory must ignore a reservation that never got a worktree.
func TestWorktreeInReservedDirAndInventory(t *testing.T) {
	e := newCleanEnv(t)
	id, err := MintRunID(e.dataDir, sameSecond)
	assert.NilError(t, err)
	_, err = MintRunID(e.dataDir, sameSecond) // a continuation's empty reservation
	assert.NilError(t, err)
	wt, err := CreateWorktree(context.Background(), WorktreeOptions{
		Root: e.root, Path: filepath.Join(e.dataDir, "factory", id), RunID: id, Prompt: "do it",
	})
	assert.NilError(t, err)
	e.record(t, Record{RunID: id, Worktree: wt.Path, Branch: wt.Branch, Result: ResultPassed})

	entries := e.inventory(t)
	assert.Equal(t, len(entries), 1)
	assert.Equal(t, entries[0].ID(), id)
}

func TestInventorySeparatesSuffixedRuns(t *testing.T) {
	e := newCleanEnv(t)
	for _, id := range []string{"20261025-204905", "20261025-204905-2"} {
		wt, br := e.worktree(t, id)
		e.record(t, Record{RunID: id, Worktree: wt, Branch: br, Result: ResultPassed})
	}
	entries := e.inventory(t)
	assert.Equal(t, len(entries), 2)
	assert.Equal(t, entries[0].ID(), "20261025-204905-2")
	assert.Equal(t, entries[1].ID(), "20261025-204905")
	for _, en := range entries {
		assert.Assert(t, en.StartedAt.Equal(sameSecond), en.RunID)
		assert.Equal(t, len(en.RunIDs), 1)
	}
}
