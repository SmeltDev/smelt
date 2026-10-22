package factory

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/smeltdev/smelt/internal/config"
	"github.com/smeltdev/smelt/internal/gitexec"
)

// UpdateResult is how bringing a run's branch up to date with its base came out.
type UpdateResult string

// The outcomes of an update.
const (
	// UpdateClean means the base merged without conflicts.
	UpdateClean UpdateResult = "clean"
	// UpdateConflicts means the merge conflicted and was committed with its
	// markers left in the files.
	UpdateConflicts UpdateResult = "conflicts"
	// UpdateUpToDate means the branch already contained the base.
	UpdateUpToDate UpdateResult = "up-to-date"
)

// UpdateReport is what UpdateBranch did. Its JSON form is what
// `smelt factory update --json` prints.
type UpdateReport struct {
	RunID         string       `json:"run_id"`
	Branch        string       `json:"branch"`
	Base          string       `json:"base"`
	Result        UpdateResult `json:"result"`
	MergedCommits int          `json:"merged_commits"`
	Conflicts     []string     `json:"conflicts"`
	// Baseline is the run's baseline after the update: the base's tip the
	// branch now contains. It is "" when the update left it alone.
	Baseline string `json:"baseline,omitempty"`
}

// UpdateOptions describes an update.
type UpdateOptions struct {
	// Base overrides the branch to merge; see ChooseBase.
	Base string
	// Author is who the merge commit is attributed to; see ResolveAuthor.
	Author config.CommitAuthor
}

// UpdateBranch merges the base branch into the branch of the finished run rec,
// in the run's worktree. The implementer cannot do this itself: its container
// has no history to merge against. A conflicted merge is committed with its
// markers in place and recorded in the run's record, so a continued run can
// resolve them and the worktree is left with nothing in progress.
func UpdateBranch(ctx context.Context, root string, rec Record, o UpdateOptions) (UpdateReport, error) {
	base := ChooseBase(o.Base, rec.BaseBranch, rec.DefaultBranch)
	if base == "" {
		return UpdateReport{}, errors.New("cannot tell which branch to merge: the run recorded no base branch; pass --base")
	}
	rep := UpdateReport{RunID: rec.RunID, Branch: rec.Branch, Base: base, Conflicts: []string{}}
	git := gitexec.Runner{Dir: rec.Worktree, Env: commitEnv(o.Author)}
	if err := checkUpdatable(ctx, git, rec); err != nil {
		return rep, err
	}
	merged, err := mergeBase(ctx, git, rec.Branch, base)
	rep.MergedCommits = merged.Commits
	if err != nil {
		return rep, err
	}
	switch {
	case merged.Commits == 0:
		rep.Result = UpdateUpToDate
	case len(merged.Conflicts) > 0:
		rep.Result, rep.Conflicts = UpdateConflicts, merged.Conflicts
		rec.Conflicts = merged.Conflicts
	default:
		rep.Result = UpdateClean
	}
	moved, err := moveBaseline(ctx, git, rec, base, merged.Target)
	if err != nil {
		return rep, err
	}
	if moved.Baseline != rec.Baseline {
		rep.Baseline = moved.Baseline
	}
	if moved.Baseline != rec.Baseline || len(moved.Conflicts) > 0 {
		err = saveUpdated(root, moved)
	}
	return rep, err
}

// baseMerge is what mergeBase did in the worktree.
type baseMerge struct {
	// Target is the remote-tracking ref that was merged.
	Target string
	// Commits is how many commits of Target the branch lacked; 0 means nothing
	// was merged.
	Commits int
	// Conflicts are the files committed with markers in them.
	Conflicts []string
}

// mergeBase fetches base and merges it into the branch checked out in git's
// worktree, which must be clean. A conflicted merge is committed with its
// markers in place.
func mergeBase(ctx context.Context, git gitexec.Runner, branch, base string) (baseMerge, error) {
	var m baseMerge
	if out, err := git.CombinedOutput(ctx, "fetch", "--quiet", DefaultRemote, base); err != nil {
		return m, fmt.Errorf("fetch %s from %s: %w: %s", base, DefaultRemote, err, strings.TrimSpace(string(out)))
	}
	m.Target = DefaultRemote + "/" + base
	count, err := git.Output(ctx, "rev-list", "--count", "HEAD.."+m.Target)
	if err != nil {
		return m, fmt.Errorf("count the commits of %s to merge: %w", m.Target, err)
	}
	if m.Commits, err = strconv.Atoi(strings.TrimSpace(string(count))); err != nil {
		return m, fmt.Errorf("read the commit count of %s: %w", m.Target, err)
	}
	if m.Commits == 0 {
		return m, nil
	}
	out, mergeErr := git.CombinedOutput(ctx, "-c", "commit.gpgsign=false", "merge", "--no-ff", "--no-edit", "--no-verify", m.Target)
	if mergeErr == nil {
		return m, nil
	}
	conflicts, err := conflictedFiles(ctx, git)
	if err != nil || len(conflicts) == 0 {
		_ = git.Run(context.WithoutCancel(ctx), "merge", "--abort")
		return m, fmt.Errorf("merge %s into %s: %w: %s", m.Target, branch, mergeErr, strings.TrimSpace(string(out)))
	}
	if err := commitConflicts(ctx, git, base, branch, conflicts); err != nil {
		return m, err
	}
	m.Conflicts = conflicts
	return m, nil
}

// moveBaseline returns rec with the merge base of the branch and target as its
// baseline, so a continued run measures only its own work and not what the
// base brought in. rec comes back unchanged when that is the baseline it
// already had (the branch was never behind a base that moved).
func moveBaseline(ctx context.Context, git gitexec.Runner, rec Record, base, target string) (Record, error) {
	out, err := git.Output(ctx, "merge-base", "HEAD", target)
	if err != nil {
		return rec, fmt.Errorf("find the merge base of %s and %s: %w", rec.Branch, target, err)
	}
	mergeBase := strings.TrimSpace(string(out))
	// A base behind the baseline, such as one the run's unpushed start was ahead
	// of, would move the baseline back and make that earlier work look new.
	if mergeBase == rec.Baseline || git.Run(ctx, "merge-base", "--is-ancestor", rec.Baseline, mergeBase) != nil {
		return rec, nil
	}
	if rec.OriginalBaseline == "" {
		rec.OriginalBaseline = rec.Baseline
	}
	rec.Baseline = mergeBase
	rec.UpdatedFrom = &UpdatedFrom{Branch: base, Commit: mergeBase}
	return rec, nil
}

// checkUpdatable refuses a worktree the merge cannot safely run in: not on the
// run's branch, or holding changes the merge would trip over.
func checkUpdatable(ctx context.Context, git gitexec.Runner, rec Record) error {
	head, err := git.Output(ctx, "symbolic-ref", "--quiet", "--short", "HEAD")
	if err != nil || strings.TrimSpace(string(head)) != rec.Branch {
		return fmt.Errorf("the worktree %s is not on the branch %s", rec.Worktree, rec.Branch)
	}
	status, err := git.Output(ctx, "status", "--porcelain")
	if err != nil {
		return fmt.Errorf("check the worktree %s: %w", rec.Worktree, err)
	}
	if strings.TrimSpace(string(status)) != "" {
		return fmt.Errorf("the worktree %s has uncommitted changes; commit or discard them first", rec.Worktree)
	}
	return nil
}

// conflictedFiles lists the paths a merge in progress left unmerged.
func conflictedFiles(ctx context.Context, git gitexec.Runner) ([]string, error) {
	out, err := git.Output(ctx, "diff", "--name-only", "--diff-filter=U")
	if err != nil {
		return nil, fmt.Errorf("list the conflicted files: %w", err)
	}
	return strings.Fields(string(out)), nil
}

// commitConflicts commits the merge in progress as it is, markers included.
func commitConflicts(ctx context.Context, git gitexec.Runner, base, branch string, conflicts []string) error {
	if out, err := git.CombinedOutput(ctx, "add", "-A"); err != nil {
		return fmt.Errorf("stage the conflicted files: %w: %s", err, strings.TrimSpace(string(out)))
	}
	msg := fmt.Sprintf("Merge %s into %s (conflicts to resolve: %s)", base, branch, strings.Join(conflicts, ", "))
	if out, err := git.CombinedOutput(ctx, "-c", "commit.gpgsign=false", "commit", "--quiet", "--no-verify", "-m", msg); err != nil {
		return fmt.Errorf("commit the conflicted merge: %w: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

// saveUpdated saves rec, which an update just changed, in the project's data
// directory.
func saveUpdated(root string, rec Record) error {
	dataDir, err := config.ProjectDataDir(root)
	if err != nil {
		return fmt.Errorf("find smelt's data directory for the project: %w", err)
	}
	return saveRecord(dataDir, rec)
}
