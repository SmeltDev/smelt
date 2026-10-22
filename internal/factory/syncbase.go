package factory

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/smeltdev/smelt/internal/config"
	"github.com/smeltdev/smelt/internal/gitexec"
	"github.com/smeltdev/smelt/internal/sidecar"
)

// BaseSync is what a live run needs to merge a base branch into its branch at
// the end of a round: the queue the requests come through, and the run's own
// worktree and record, which a sync changes in place.
type BaseSync struct {
	Queue  *SyncQueue
	Root   string
	Author config.CommitAuthor
	Rec    *Record
	Wt     *Worktree
}

// BaseSyncer is the steps of a loop that can merge a queued base between
// rounds. A loop whose steps do not implement it never syncs.
type BaseSyncer interface {
	// SyncBase applies the oldest queued sync after round's checks, and returns
	// it; nil when none is queued. A failed request is returned as dropped.
	SyncBase(ctx context.Context, round int) (*Sync, error)
}

// SyncBase merges the oldest queued base into the run's branch.
//
// The implementer's work sits uncommitted in the worktree, pulled there by the
// round's checks, so the merge cannot run on it. The work is stashed, the base
// merged into the clean branch the way `smelt factory update` does, the
// sidecars re-baselined on the new baseline's files, and the work put back and
// pushed to the implementer. A merge that conflicts is committed with its
// markers, and so is a stash that does not apply cleanly on the merged branch:
// the files are left with markers in them for the next round to resolve.
func (s *Sidecars) SyncBase(ctx context.Context, round int) (*Sync, error) {
	b := s.Base
	if b == nil {
		return nil, nil
	}
	i, req, ok := b.Queue.next()
	if !ok {
		return nil, nil
	}
	done, err := s.applySync(ctx, req, round)
	if err != nil {
		dropped := Sync{Base: req.Base, State: SyncDropped, Round: round, Reason: "failed: " + err.Error()}
		b.Queue.settle(i, dropped)
		var unsafe *syncWorkspaceError
		if errors.As(err, &unsafe) {
			return &dropped, err
		}
		return &dropped, nil
	}
	b.Queue.settle(i, done)
	// Persist the settled outcome with the moved baseline, so status can
	// recover it from disk even if the daemon stops before the run ends.
	b.Rec.Syncs = b.Queue.Snapshot()
	if err := saveUpdated(b.Root, *b.Rec); err != nil {
		return &done, fmt.Errorf("save the completed base sync: %w", err)
	}
	return &done, nil
}

func (s *Sidecars) applySync(ctx context.Context, req Sync, round int) (Sync, error) {
	b := s.Base
	git := gitexec.Runner{Dir: b.Wt.Path, Env: commitEnv(b.Author)}
	stashed, err := stashWork(ctx, git, req.Base)
	if err != nil {
		return Sync{}, err
	}
	res, mergeErr := s.mergeAndRebaseline(ctx, git, req.Base)
	// The branch may already have moved even if sidecar setup failed. Keep
	// the local record consistent before returning a fatal workspace error.
	if res.changed {
		s.moveBaselineTo(res.rec.Baseline)
		*b.Rec = res.rec
		b.Wt.UpdatedFrom = res.rec.UpdatedFrom
	}
	popped, popErr := restoreWork(ctx, git, stashed)
	if err := errors.Join(mergeErr, popErr); err != nil {
		if popErr != nil || res.changed {
			return Sync{}, &syncWorkspaceError{err}
		}
		return Sync{}, err
	}
	if len(popped) > 0 {
		if err := commitConflicts(ctx, git, req.Base, b.Rec.Branch, popped); err != nil {
			return Sync{}, &syncWorkspaceError{err}
		}
	}
	res.rec.Conflicts = unionFiles(res.rec.Conflicts, popped)
	if res.changed {
		if err := s.Relay.Push(ctx, []*sidecar.PoolEntry{s.Implementer.Entry}); err != nil {
			return Sync{}, &syncWorkspaceError{fmt.Errorf("bring the work back to the implementer: %w", err)}
		}
	}
	if res.rec.Baseline != b.Rec.Baseline {
		s.moveBaselineTo(res.rec.Baseline)
	}
	*b.Rec = res.rec
	b.Wt.UpdatedFrom = res.rec.UpdatedFrom
	if res.changed || len(res.rec.Conflicts) > 0 {
		if err := saveUpdated(b.Root, res.rec); err != nil {
			return Sync{}, err
		}
	}
	return Sync{
		Base: req.Base, State: SyncDone, Round: round, Commit: res.commit, Files: res.files,
		Conflicts: unionFiles(res.merge.Conflicts, popped),
	}, nil
}

// syncMerge is what merging the base into the clean branch came to.
type syncMerge struct {
	merge   baseMerge
	rec     Record
	commit  string
	files   int
	changed bool
}

// mergeAndRebaseline merges base into the clean worktree and, when the
// baseline moved, puts the sidecars on the new baseline's files.
func (s *Sidecars) mergeAndRebaseline(ctx context.Context, git gitexec.Runner, base string) (syncMerge, error) {
	rec := *s.Base.Rec
	res := syncMerge{rec: rec}
	if err := checkUpdatable(ctx, git, rec); err != nil {
		return res, err
	}
	before, err := git.Output(ctx, "rev-parse", "HEAD")
	if err != nil {
		return res, fmt.Errorf("resolve the branch: %w", err)
	}
	res.merge, err = mergeBase(ctx, git, rec.Branch, base)
	if err != nil {
		return res, err
	}
	out, err := git.Output(ctx, "rev-parse", res.merge.Target)
	if err != nil {
		return res, fmt.Errorf("resolve %s: %w", res.merge.Target, err)
	}
	res.commit = strings.TrimSpace(string(out))
	if res.merge.Commits > 0 {
		names, err := git.Output(ctx, "diff", "--name-only", strings.TrimSpace(string(before)), "HEAD")
		if err != nil {
			return res, fmt.Errorf("list the files the merge changed: %w", err)
		}
		res.files = len(strings.Fields(string(names)))
	}
	rec.Conflicts = unionFiles(rec.Conflicts, res.merge.Conflicts)
	if res.rec, err = moveBaseline(ctx, git, rec, base, res.merge.Target); err != nil {
		return res, err
	}
	moved := res.rec.Baseline != s.Base.Rec.Baseline
	res.changed = res.merge.Commits > 0 || moved
	if moved {
		err = s.rebaseline(ctx, res.rec.Baseline)
	}
	return res, err
}

// rebaseline puts the implementer and the reviewers on the files of baseline,
// committed as the commit their changes are measured against, so the base's
// files are not part of the change under review. The worktree is left showing
// the baseline's files; restoreWork puts the branch's back.
func (s *Sidecars) rebaseline(ctx context.Context, baseline string) error {
	if len(s.Prompts) > 0 {
		if err := s.prepareReviewers(ctx); err != nil {
			return err
		}
	}
	if err := (Worktree{Path: s.Base.Wt.Path, Baseline: baseline}).showBaseline(ctx); err != nil {
		return err
	}
	// A review that timed out keeps running on its sidecar and would read the
	// tree as it is replaced; see relayToReviewers.
	_ = s.onReviewers(s.script(ctx, "pkill -f '[c]laude -p' || true"))
	entries := slices.Concat([]*sidecar.PoolEntry{s.Implementer.Entry}, s.Reviewers)
	if err := s.Relay.Push(ctx, entries); err != nil {
		return err
	}
	if err := s.ws.commitBaseline(ctx); err != nil {
		return err
	}
	err := s.onReviewers(func(e *sidecar.PoolEntry) error {
		rw := workspace{exec: s.Exec, entry: e}
		if err := rw.commitBaseline(ctx); err != nil {
			return fmt.Errorf("%s: %w", e.ID, err)
		}
		return nil
	})
	if err != nil {
		return err
	}
	if s.Quality != nil {
		s.Quality.MeasureBaseline(ctx, s.ws.baseline)
	}
	if len(SarifCommands(s.Commands)) > 0 {
		s.sarifRatchet().MeasureBaseline(ctx, s.ws.baseline, s.Commands)
	}
	return nil
}

// moveBaselineTo points everything that reads the run's baseline from the
// worktree at baseline.
func (s *Sidecars) moveBaselineTo(baseline string) {
	s.Base.Wt.Baseline = baseline
	s.Scout.Files = GitFiles{Git: gitexec.Runner{Dir: s.Base.Wt.Path}, Rev: baseline}
}

// stashWork sets the work in the worktree aside so the branch can be merged,
// and reports whether there was any.
func stashWork(ctx context.Context, git gitexec.Runner, base string) (bool, error) {
	status, err := git.Output(ctx, "status", "--porcelain")
	if err != nil {
		return false, fmt.Errorf("check the worktree: %w", err)
	}
	if strings.TrimSpace(string(status)) == "" {
		return false, nil
	}
	msg := "smelt: the implementer's work, set aside while syncing " + base
	if out, err := git.CombinedOutput(ctx, "stash", "push", "--include-untracked", "--quiet", "-m", msg); err != nil {
		return false, fmt.Errorf("set the work aside: %w: %s", err, strings.TrimSpace(string(out)))
	}
	return true, nil
}

// restoreWork puts the branch's files back in the worktree and the stashed
// work on top of them. Work that does not apply cleanly is left with conflict
// markers in the files, which it returns, and nothing staged. The caller
// commits those markers before resuming the run.
func restoreWork(ctx context.Context, git gitexec.Runner, stashed bool) ([]string, error) {
	if out, err := git.CombinedOutput(ctx, "read-tree", "-u", "--reset", "HEAD"); err != nil {
		return nil, fmt.Errorf("put the branch's files back: %w: %s", err, strings.TrimSpace(string(out)))
	}
	if !stashed {
		return nil, nil
	}
	out, err := git.CombinedOutput(ctx, "stash", "pop", "--quiet")
	if err == nil {
		return nil, nil
	}
	collisions, collisionErr := untrackedCollisions(ctx, git)
	if collisionErr != nil {
		return nil, collisionErr
	}
	conflicts, cerr := conflictedFiles(ctx, git)
	conflicts = unionFiles(conflicts, collisions)
	if cerr != nil || len(conflicts) == 0 {
		return nil, fmt.Errorf("put the work back after the merge: %w: %s (the work is kept in git stash)", err, strings.TrimSpace(string(out)))
	}
	if out, err := git.CombinedOutput(ctx, "reset", "--quiet"); err != nil {
		return nil, fmt.Errorf("unstage the conflicted files: %w: %s", err, strings.TrimSpace(string(out)))
	}
	if out, err := git.CombinedOutput(ctx, "stash", "drop", "--quiet"); err != nil {
		return nil, fmt.Errorf("drop the applied stash: %w: %s", err, strings.TrimSpace(string(out)))
	}
	return conflicts, nil
}

// untrackedCollisions preserves both versions of paths added independently by
// the implementer and the base. Git cannot restore these from the stash's
// untracked tree and does not produce index conflicts for them.
func untrackedCollisions(ctx context.Context, git gitexec.Runner) ([]string, error) {
	// A stash without untracked files has no third parent.
	if _, err := git.Output(ctx, "rev-parse", "--verify", "stash@{0}^3"); err != nil {
		return nil, nil
	}
	out, err := git.Output(ctx, "ls-tree", "-r", "--name-only", "-z", "stash@{0}^3")
	if err != nil {
		return nil, fmt.Errorf("list stashed untracked files: %w", err)
	}
	var conflicts []string
	for _, path := range strings.Split(strings.TrimSuffix(string(out), "\x00"), "\x00") {
		if path == "" {
			continue
		}
		tracked, err := git.Output(ctx, "ls-tree", "HEAD", "--", path)
		if err != nil {
			return nil, fmt.Errorf("check added path %s: %w", path, err)
		}
		if len(tracked) == 0 {
			continue
		}
		base, err := git.Output(ctx, "show", "HEAD:"+path)
		if err != nil {
			return nil, fmt.Errorf("read base addition %s: %w", path, err)
		}
		work, err := git.Output(ctx, "show", "stash@{0}^3:"+path)
		if err != nil {
			return nil, fmt.Errorf("read implementer addition %s: %w", path, err)
		}
		marked := fmt.Sprintf("<<<<<<< updated base\n%s\n=======\n%s\n>>>>>>> implementer\n", base, work)
		if err := os.WriteFile(filepath.Join(git.Dir, path), []byte(marked), 0o644); err != nil {
			return nil, fmt.Errorf("mark add/add conflict %s: %w", path, err)
		}
		conflicts = append(conflicts, path)
	}
	return conflicts, nil
}

// unionFiles is a followed by the files of b it lacks.
func unionFiles(a, b []string) []string {
	out := slices.Clone(a)
	for _, f := range b {
		if !slices.Contains(out, f) {
			out = append(out, f)
		}
	}
	return out
}

// conflictNote tells the implementer which files hold conflict markers from
// merging base.
func conflictNote(base string, files []string) string {
	return fmt.Sprintf("The branch was merged with `%s`; these files contain conflict markers (`<<<<<<<`, `=======`, `>>>>>>>`): %s. "+
		"Resolve each conflict so both sides' intent is kept, then remove the markers.", base, strings.Join(files, ", "))
}

// Note is what the next round's implementer is told about the sync: the base's
// changes are in the working tree and are not part of the request, and which
// files to resolve, if any.
func (s Sync) Note() string {
	note := fmt.Sprintf("Between rounds the branch was updated from `%s` at %s: %d file(s) from it are now in the working tree. They are not part of the request: keep them.",
		s.Base, shortSHA(s.Commit), s.Files)
	if len(s.Conflicts) > 0 {
		note += " " + conflictNote(s.Base, s.Conflicts)
	}
	return note
}

// Line is the sync as the round log records it.
func (s Sync) Line() string {
	if s.State == SyncDropped {
		return fmt.Sprintf("sync of %s dropped: %s", s.Base, s.Reason)
	}
	return fmt.Sprintf("synced %s at %s (%d files, %d conflicts)", s.Base, shortSHA(s.Commit), s.Files, len(s.Conflicts))
}

// syncWorkspaceError stops the run without pulling a partially replaced
// implementer workspace over the recovered local work. Any unapplied work
// remains in the worktree's stash for recovery.
type syncWorkspaceError struct{ error }

func (e *syncWorkspaceError) Unwrap() error { return e.error }
