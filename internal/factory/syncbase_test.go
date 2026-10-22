package factory

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gotest.tools/v3/assert"

	"github.com/smeltdev/smelt/internal/config"
	"github.com/smeltdev/smelt/internal/iostream"
	"github.com/smeltdev/smelt/internal/sidecar"
)

// mirrorBackend relays by copying directories on this machine, as rsync
// --delete --exclude=.git does, so the sync tests need neither rsync nor ssh.
// A sidecar's workspace is the repoPath of its pool entry.
type mirrorBackend struct {
	sidecar.Backend // the methods a relay does not reach stay unimplemented
}

func (mirrorBackend) Sync(_ context.Context, _, repoPath, cwd string, _ bool, _ iostream.StatusFunc) error {
	return mirrorDir(cwd, repoPath)
}

func (mirrorBackend) Pull(_ context.Context, _, repoPath, localDir string, _ iostream.StatusFunc) error {
	return mirrorDir(repoPath, localDir)
}

// mirrorDir makes dst's files, but for .git, those of src.
func mirrorDir(src, dst string) error {
	want := map[string]bool{}
	err := filepath.WalkDir(src, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(src, path)
		if rel == "." {
			return nil
		}
		if rel == ".git" {
			// A worktree's .git is a file; SkipDir on a file skips its siblings.
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		want[rel] = true
		to := filepath.Join(dst, rel)
		if d.IsDir() {
			return os.MkdirAll(to, 0o755)
		}
		content, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if old, err := os.ReadFile(to); err == nil && bytes.Equal(old, content) {
			return nil
		}
		return os.WriteFile(to, content, 0o644)
	})
	if err != nil {
		return err
	}
	var gone []string
	err = filepath.WalkDir(dst, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(dst, path)
		if rel == "." {
			return nil
		}
		if rel == ".git" {
			// A worktree's .git is a file; SkipDir on a file skips its siblings.
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if !want[rel] {
			gone = append(gone, path)
			if d.IsDir() {
				return filepath.SkipDir
			}
		}
		return nil
	})
	if err != nil {
		return err
	}
	for _, path := range gone {
		if err := os.RemoveAll(path); err != nil {
			return err
		}
	}
	return nil
}

// syncRun is a live run's steps, set up to take base syncs, with an origin
// whose main the test moves.
type syncRun struct {
	s         *Sidecars
	impl      *sidecar.PoolEntry
	reviewers []*sidecar.PoolEntry
	root      string
	wt        string
	rec       *Record
	queue     *SyncQueue
}

func newSyncRun(t *testing.T) *syncRun {
	t.Helper()
	t.Setenv(config.EnvXDGDataHome, t.TempDir())
	s, impl, reviewers, _ := newSidecarsFixtureRelaying(t, nil, nil, func(dir string) *Relay {
		return NewRelay(mirrorBackend{}, dir, nil)
	})
	wt := s.Relay.dir
	root := strings.TrimSuffix(gitOutput(t, wt, "rev-parse", "--git-common-dir"), "/.git")
	gitOutput(t, root, "branch", "-M", "main")
	origin := filepath.Join(t.TempDir(), "origin.git")
	gitOutput(t, root, "init", "-q", "--bare", origin)
	gitOutput(t, root, "remote", "set-url", "origin", origin)
	gitOutput(t, root, "push", "-q", "origin", "main")

	branch := gitOutput(t, wt, "symbolic-ref", "--short", "HEAD")
	baseline := gitOutput(t, wt, "rev-parse", "HEAD")
	rec := &Record{RunID: "run-1", Prompt: "add a flag", Worktree: wt, Branch: branch, Baseline: baseline, BaseBranch: "main", DefaultBranch: "main"}
	queue := &SyncQueue{}
	s.Base = &BaseSync{Queue: queue, Root: root, Rec: rec, Wt: &Worktree{Path: wt, Branch: branch, Baseline: baseline}}
	return &syncRun{s: s, impl: impl, reviewers: reviewers, root: root, wt: wt, rec: rec, queue: queue}
}

// baseMoves commits a change to main on origin.
func (r *syncRun) baseMoves(t *testing.T, name, content string) string {
	t.Helper()
	writeFile(t, r.root, name, content)
	gitOutput(t, r.root, "add", name)
	gitOutput(t, r.root, "-c", "user.name=t", "-c", "user.email=t@example.com", "commit", "-q", "-m", "main moves on")
	gitOutput(t, r.root, "push", "-q", "origin", "main")
	return gitOutput(t, r.root, "rev-parse", "main")
}

// implement is a round: the implementer's edit, collected and checked.
func (r *syncRun) implement(t *testing.T, round int, name, content string) {
	t.Helper()
	writeFile(t, r.impl.RepoPath, name, content)
	_, err := r.s.Collect(context.Background())
	assert.NilError(t, err)
	_, err = r.s.Check(context.Background(), round)
	assert.NilError(t, err)
}

func TestSyncBaseMergesAtTheEndOfTheRound(t *testing.T) {
	ctx := context.Background()
	r := newSyncRun(t)
	r.implement(t, 1, "feature.txt", "mine\n")
	tip := r.baseMoves(t, "base.txt", "from main\n")

	got, err := r.s.SyncBase(ctx, 1)
	assert.NilError(t, err)
	assert.Assert(t, got == nil, "nothing was queued")

	_, err = r.queue.Enqueue("main")
	assert.NilError(t, err)
	got, err = r.s.SyncBase(ctx, 1)
	assert.NilError(t, err)
	assert.DeepEqual(t, *got, Sync{Base: "main", State: SyncDone, Round: 1, Commit: tip, Files: 1})
	assert.DeepEqual(t, r.queue.Snapshot(), []Sync{*got})

	// The branch has the base's file, and the work is still uncommitted on it.
	assert.Equal(t, gitOutput(t, r.wt, "show", "HEAD:base.txt"), "from main")
	assert.Equal(t, gitOutput(t, r.wt, "status", "--porcelain"), "?? feature.txt")
	assert.Equal(t, gitOutput(t, r.wt, "stash", "list"), "")

	// Everything is on the new baseline, so the change under review is the
	// implementer's alone, on the implementer and on every reviewer.
	assert.Equal(t, r.s.Base.Wt.Baseline, tip)
	assert.Equal(t, r.rec.Baseline, tip)
	assert.Equal(t, r.rec.UpdatedFrom.Commit, tip)
	change, err := r.s.Collect(ctx)
	assert.NilError(t, err)
	assert.Equal(t, change.Stat, "1 file changed, 1 insertion(+)")
	assert.Equal(t, gitOutput(t, r.impl.RepoPath, "diff", "HEAD", "--name-status"), "A\tfeature.txt")
	assert.Equal(t, readFile(t, r.impl.RepoPath, "base.txt"), "from main\n")
	// The reviewers are on the new baseline too, and get the work with the next
	// round's checks.
	_, err = r.s.Check(ctx, 2)
	assert.NilError(t, err)
	for _, rev := range r.reviewers {
		assert.Equal(t, readFile(t, rev.RepoPath, "base.txt"), "from main\n", rev.ID)
		assert.Equal(t, gitOutput(t, rev.RepoPath, "diff", "HEAD", "--name-status"), "A\tfeature.txt", rev.ID)
	}

	saved, err := LoadRecord(r.root, "run-1")
	assert.NilError(t, err)
	assert.Equal(t, saved.Baseline, tip)
	assert.Equal(t, saved.OriginalBaseline, r.rec.OriginalBaseline)
	assert.DeepEqual(t, saved.Syncs, []Sync{*got}) // the completed sync survives a daemon restart before the run ends
}

func TestSyncBaseCommitsAConflictWithItsMarkers(t *testing.T) {
	ctx := context.Background()
	r := newSyncRun(t)
	r.implement(t, 1, "README.md", "# mine\n")
	tip := r.baseMoves(t, "README.md", "# main's\n")
	_, err := r.queue.Enqueue("main")
	assert.NilError(t, err)

	got, err := r.s.SyncBase(ctx, 1)
	assert.NilError(t, err)
	assert.DeepEqual(t, *got, Sync{Base: "main", State: SyncDone, Round: 1, Commit: tip, Files: 1, Conflicts: []string{"README.md"}})
	assert.DeepEqual(t, r.rec.Conflicts, []string{"README.md"})
	assert.Assert(t, strings.Contains(got.Note(), "README.md"), got.Note())

	// The markers are in the implementer's tree, and the markers gate holds
	// until they are resolved.
	assert.Assert(t, strings.Contains(readFile(t, r.impl.RepoPath, "README.md"), "<<<<<<<"))
	assert.Assert(t, strings.Contains(gitOutput(t, r.wt, "show", "HEAD:README.md"), "<<<<<<<"), "markers are committed")
	check := runCommand(ctx, r.s.Exec, r.impl, MarkersGate)
	assert.Equal(t, check.Status, StatusFailed, check.Output)

	writeFile(t, r.impl.RepoPath, "README.md", "# both\n")
	check = runCommand(ctx, r.s.Exec, r.impl, MarkersGate)
	assert.Equal(t, check.Status, StatusPassed, check.Output)
}

func TestSyncBaseUpToDateKeepsTheBaseline(t *testing.T) {
	ctx := context.Background()
	r := newSyncRun(t)
	r.implement(t, 1, "feature.txt", "mine\n")
	before := r.rec.Baseline
	_, err := r.queue.Enqueue("main")
	assert.NilError(t, err)

	got, err := r.s.SyncBase(ctx, 1)
	assert.NilError(t, err)
	assert.Equal(t, got.State, SyncDone)
	assert.Equal(t, got.Files, 0)
	assert.Equal(t, r.rec.Baseline, before)
	assert.Equal(t, gitOutput(t, r.wt, "status", "--porcelain"), "?? feature.txt")
}

func TestSyncBaseFailureDropsTheSyncAndSaysWhy(t *testing.T) {
	r := newSyncRun(t)
	_, err := r.queue.Enqueue("nosuch")
	assert.NilError(t, err)

	dropped, err := r.s.SyncBase(context.Background(), 1)
	assert.NilError(t, err)
	assert.Equal(t, dropped.State, SyncDropped)
	got := r.queue.Snapshot()
	assert.Equal(t, got[0].State, SyncDropped)
	assert.Assert(t, strings.HasPrefix(got[0].Reason, "failed: "), got[0].Reason)
}

func TestSyncQueue(t *testing.T) {
	var nilQueue *SyncQueue
	assert.Assert(t, nilQueue.Snapshot() == nil)
	assert.Assert(t, nilQueue.Close() == nil)

	q := &SyncQueue{}
	first, err := q.Enqueue("main")
	assert.NilError(t, err)
	again, err := q.Enqueue("main")
	assert.NilError(t, err)
	assert.DeepEqual(t, first, again)
	assert.Equal(t, len(q.Snapshot()), 1)
	_, err = q.Enqueue("release")
	assert.ErrorContains(t, err, "a sync of main is already queued")
	assert.DeepEqual(t, q.Snapshot(), []Sync{first})

	got := q.Close()
	assert.DeepEqual(t, got, []Sync{{Base: "main", State: SyncDropped, Reason: "the run ended before the next round"}})
	_, err = q.Enqueue("main")
	assert.ErrorIs(t, err, ErrSyncsClosed)
	assert.DeepEqual(t, q.Close(), got)
}

// failWorkPushBackend allows the baseline push but rejects the restored work.
type failWorkPushBackend struct{ mirrorBackend }

func (b failWorkPushBackend) Sync(ctx context.Context, id, repoPath, cwd string, fresh bool, status iostream.StatusFunc) error {
	if _, err := os.Stat(filepath.Join(cwd, "feature.txt")); err == nil {
		return fmt.Errorf("injected work push failure")
	}
	return b.mirrorBackend.Sync(ctx, id, repoPath, cwd, fresh, status)
}

func TestSyncBaseWorkPushFailureStopsWithLocalWorkPreserved(t *testing.T) {
	r := newSyncRun(t)
	r.implement(t, 1, "feature.txt", "mine\n")
	tip := r.baseMoves(t, "base.txt", "base\n")
	r.s.Relay.backend = failWorkPushBackend{}
	_, err := r.queue.Enqueue("main")
	assert.NilError(t, err)
	got, err := r.s.SyncBase(t.Context(), 1)
	var unsafe *syncWorkspaceError
	assert.Assert(t, errors.As(err, &unsafe), "%v", err)
	assert.Equal(t, got.State, SyncDropped)
	assert.Equal(t, readFile(t, r.wt, "feature.txt"), "mine\n")
	assert.Equal(t, r.rec.Baseline, tip)
	assert.Equal(t, r.s.Base.Wt.Baseline, tip)
}

func TestSyncBaseUntrackedCollisionCommitsMarkers(t *testing.T) {
	r := newSyncRun(t)
	r.implement(t, 1, "feature.txt", "mine\n")
	r.baseMoves(t, "feature.txt", "base\n")
	_, err := r.queue.Enqueue("main")
	assert.NilError(t, err)
	got, err := r.s.SyncBase(t.Context(), 1)
	assert.NilError(t, err)
	assert.Equal(t, got.State, SyncDone)
	assert.DeepEqual(t, got.Conflicts, []string{"feature.txt"})
	marked := gitOutput(t, r.wt, "show", "HEAD:feature.txt")
	for _, text := range []string{"<<<<<<<", "base", "=======", "mine", ">>>>>>>"} {
		assert.Assert(t, strings.Contains(marked, text), marked)
	}
	assert.Assert(t, strings.Contains(got.Note(), "feature.txt"))
	assert.Equal(t, gitOutput(t, r.wt, "stash", "list"), "")
}
