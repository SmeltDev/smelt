package factory

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/smeltdev/smelt/internal/brand"
	"github.com/smeltdev/smelt/internal/config"
	"github.com/smeltdev/smelt/internal/gitexec"
	"github.com/smeltdev/smelt/internal/gitutil"
)

// branchPrefix is where factory branches live.
const branchPrefix = brand.BranchPrefix

// maxBranchSlugLen keeps branch names short enough to read in listings.
const maxBranchSlugLen = 48

// Worktree is a run's local checkout: a git worktree of the developer's
// repository on a branch of its own. The implementer's work is pulled into it
// each round, so the developer's own checkout is never touched and they can
// look at the work while the run goes.
type Worktree struct {
	Path   string
	Branch string
	// Baseline is the commit the branch starts from: the developer's HEAD, or
	// their uncommitted work committed on top of it.
	Baseline string
	// Head is the developer's HEAD when the run started. It differs from
	// Baseline when their uncommitted work was committed as the baseline.
	Head string
	// BaseBranch is the branch the developer was on when the run started, or
	// the one --from named; "" for a detached HEAD or a commit. DefaultBranch is the repository's default branch,
	// "" when none is recorded. Publishing reads both to pick the PR's base.
	BaseBranch    string
	DefaultBranch string
	// PRBase is the branch the run's PR targets, decided once when the run
	// starts: the branch --from named (an integrate run's --against), else the
	// default branch. Starting from a stray checkout branch does not change it.
	PRBase string
	// From is the ref the run was started from with --from, nil when it started
	// from the developer's files. FromWarning is a problem resolving it that
	// did not stop the run, such as a failed fetch.
	From        *FromRef
	FromWarning string
	// UpdatedFrom is the base the branch was updated from, nil when it never was.
	UpdatedFrom *UpdatedFrom
	// Author is who the branch's commits are attributed to; an incomplete one
	// means smelt.
	Author config.CommitAuthor
}

// WorktreeOptions describes the worktree CreateWorktree makes.
type WorktreeOptions struct {
	// Root is the developer's repository; Path is where the worktree goes.
	Root string
	Path string
	// RunID and Prompt name the branch.
	RunID  string
	Prompt string
	// Author is who the branch's commits are attributed to; see ResolveAuthor.
	Author config.CommitAuthor
	// From, when set, is a branch or commit to start from instead of the
	// developer's files; see ResolveFrom.
	From string
	// Against, with From, makes an integrate worktree: it is checked out at
	// the tip of From while its baseline is the merge base with Against, so
	// the branch's own commits are the change under review.
	Against string
	// Host, when set, names the repository's default branch; the git remote's
	// HEAD answers when it is nil or fails.
	Host DefaultBranchHost
}

// defaultBranchTimeout bounds the host's answer to the default branch, so a
// slow network never holds up the start of a run.
const defaultBranchTimeout = 10 * time.Second

// DefaultBranchHost names a repository's default branch, as the code host
// records it.
type DefaultBranchHost interface {
	DefaultBranch(ctx context.Context) (string, error)
}

// defaultBranch is the repository's default branch, "" when none is recorded:
// the host's answer, else the remote HEAD git recorded.
func (o WorktreeOptions) defaultBranch(ctx context.Context) string {
	if o.Host != nil {
		hctx, cancel := context.WithTimeout(ctx, defaultBranchTimeout)
		defer cancel()
		if b, err := o.Host.DefaultBranch(hctx); err == nil && b != "" {
			return b
		}
	}
	// A repository may record no default branch.
	b, _ := gitutil.DefaultBranchIn(o.Root)
	return b
}

// prBase is the branch the run's PR targets; see Worktree.PRBase.
func prBase(w Worktree) string {
	if w.From != nil && w.BaseBranch != "" {
		return w.BaseBranch
	}
	return w.DefaultBranch
}

// CreateWorktree checks out a new worktree of the repository at root into path,
// on a branch named smelt/<prompt slug>/<runID>. By default it starts from the
// developer's files as they are now: uncommitted work, untracked files
// included, is committed on the branch first, so it is the baseline and not
// part of the change under review. With From set it starts from that commit
// and takes no snapshot.
func CreateWorktree(ctx context.Context, o WorktreeOptions) (Worktree, error) {
	var w Worktree
	var err error
	if o.From != "" {
		w, err = o.startFromRef(ctx)
	} else {
		w, err = o.startFromCheckout(ctx)
	}
	if err != nil {
		return Worktree{}, err
	}
	w.DefaultBranch = o.defaultBranch(ctx)
	w.PRBase = prBase(w)
	// The guard runs before the worktree exists, so no implementer turn can
	// ever happen on a branch that must not be published.
	if err := CheckHeadBranch(w.Branch, w.DefaultBranch); err != nil {
		return Worktree{}, err
	}
	git := gitexec.Runner{Dir: o.Root}
	start := w.Baseline
	if o.Against != "" {
		start = w.Head
	}
	if out, err := git.CombinedOutput(ctx, "worktree", "add", "--quiet", "-b", w.Branch, o.Path, start); err != nil {
		return Worktree{}, fmt.Errorf("create worktree: %w: %s", err, strings.TrimSpace(string(out)))
	}
	return w, nil
}

// startFromRef is the worktree that starts at the commit From names, with no
// snapshot of the checkout.
func (o WorktreeOptions) startFromRef(ctx context.Context) (Worktree, error) {
	res, err := ResolveFrom(ctx, o.Root, o.From)
	if err != nil {
		return Worktree{}, err
	}
	w := Worktree{
		Path: o.Path, Branch: branchName(o.Prompt, o.RunID), Baseline: res.Commit, Head: res.Commit,
		BaseBranch: res.Branch, From: &FromRef{Ref: o.From, Commit: res.Commit}, FromWarning: res.Warning,
		Author: o.Author,
	}
	if o.Against == "" {
		return w, nil
	}
	in, err := ResolveIntegrate(ctx, o.Root, o.From, o.Against)
	if err != nil {
		return Worktree{}, err
	}
	// The PR goes into what the branch was compared with.
	w.Baseline, w.Head, w.BaseBranch = in.Base, in.Tip, in.AgainstBranch
	w.From.Commit = in.Tip
	return w, nil
}

// startFromCheckout is the worktree that starts at the developer's HEAD, with
// their uncommitted work committed on top of it.
func (o WorktreeOptions) startFromCheckout(ctx context.Context) (Worktree, error) {
	root := o.Root
	git := gitexec.Runner{Dir: root}
	headOut, err := git.Output(ctx, "rev-parse", "--verify", "HEAD^{commit}")
	if err != nil {
		return Worktree{}, fmt.Errorf("the repository has no commit to start from: %w", err)
	}
	head := strings.TrimSpace(string(headOut))
	headTree, err := git.Output(ctx, "rev-parse", "HEAD^{tree}")
	if err != nil {
		return Worktree{}, fmt.Errorf("resolve HEAD: %w", err)
	}
	tree, err := gitutil.SnapshotTree(root)
	if err != nil {
		return Worktree{}, fmt.Errorf("snapshot your working tree: %w", err)
	}

	start := head
	if tree != strings.TrimSpace(string(headTree)) {
		out, err := (gitexec.Runner{Dir: root, Env: commitEnv(o.Author)}).Output(ctx, "commit-tree", tree, "-p", head,
			"-m", "smelt factory baseline: uncommitted changes when run "+o.RunID+" started")
		if err != nil {
			return Worktree{}, fmt.Errorf("commit your uncommitted changes as the baseline: %w", err)
		}
		start = strings.TrimSpace(string(out))
	}

	w := Worktree{Path: o.Path, Branch: branchName(o.Prompt, o.RunID), Baseline: start, Head: head, Author: o.Author}
	// A detached HEAD has no branch, which is routine.
	w.BaseBranch, _ = gitutil.CurrentBranchInCtx(ctx, root)
	return w, nil
}

func branchName(prompt, runID string) string {
	return branchPrefix + branchSlug(prompt) + "/" + runID
}

// branchSlug makes the request recognizable in branch listings while the run
// ID remains the stable identifier used for records, logs, and continuation.
func branchSlug(prompt string) string {
	subject, _, _ := strings.Cut(strings.TrimSpace(prompt), "\n")
	words := strings.FieldsFunc(strings.ToLower(subject), func(r rune) bool {
		return (r < 'a' || r > 'z') && (r < '0' || r > '9')
	})
	if len(words) == 0 {
		return "change"
	}

	// Stop at the last whole word that fits; only a single word too long to
	// fit on its own is cut.
	slug := words[0][:min(len(words[0]), maxBranchSlugLen)]
	for _, w := range words[1:] {
		if len(slug)+1+len(w) > maxBranchSlugLen {
			break
		}
		slug += "-" + w
	}
	return slug
}

// Remove deletes the worktree and its branch, for a run that ended before the
// implementer did anything. Nothing is lost: the baseline is the developer's
// own files, still in their checkout, or the commit --from named.
func (w Worktree) Remove(ctx context.Context, root string) error {
	git := gitexec.Runner{Dir: root}
	if out, err := git.CombinedOutput(ctx, "worktree", "remove", "--force", w.Path); err != nil {
		return fmt.Errorf("remove worktree: %w: %s", err, strings.TrimSpace(string(out)))
	}
	if out, err := git.CombinedOutput(ctx, "branch", "-D", w.Branch); err != nil {
		return fmt.Errorf("delete branch %s: %w: %s", w.Branch, err, strings.TrimSpace(string(out)))
	}
	return nil
}

// Commit commits everything in the worktree to its branch and returns the
// commit, or "" when nothing changed. The worktree is left in place for the
// developer to carry on in. Hooks and signing are skipped: the run cannot
// answer a prompt, and the work has already been checked.
func (w Worktree) Commit(ctx context.Context, message string) (string, error) {
	git := gitexec.Runner{Dir: w.Path, Env: commitEnv(w.Author)}
	if out, err := git.CombinedOutput(ctx, "add", "-A"); err != nil {
		return "", fmt.Errorf("stage the work: %w: %s", err, strings.TrimSpace(string(out)))
	}
	if git.Run(ctx, "diff", "--cached", "--quiet") == nil {
		return "", nil
	}
	if out, err := git.CombinedOutput(ctx, "-c", "commit.gpgsign=false", "commit", "--quiet", "--no-verify", "-m", message); err != nil {
		return "", fmt.Errorf("commit the work: %w: %s", err, strings.TrimSpace(string(out)))
	}
	out, err := git.Output(ctx, "rev-parse", "HEAD")
	if err != nil {
		return "", fmt.Errorf("resolve the work's commit: %w", err)
	}
	return strings.TrimSpace(string(out)), nil
}

// Stat is git's one-line summary of the branch's work since the baseline, such
// as "3 files changed, 40 insertions(+)", or "" when there is none.
func (w Worktree) Stat(ctx context.Context) (string, error) {
	out, err := (gitexec.Runner{Dir: w.Path}).Output(ctx, "diff", "--shortstat", w.Baseline, "HEAD")
	if err != nil {
		return "", fmt.Errorf("summarize the work: %w", err)
	}
	return strings.TrimSpace(string(out)), nil
}

// Amend rewrites the message of the work's commit, once the notes that make
// it up are written. It refuses a commit the branch's upstream already has:
// the new hash could only be pushed by forcing.
func (w Worktree) Amend(ctx context.Context, message string) error {
	git := gitexec.Runner{Dir: w.Path, Env: commitEnv(w.Author)}
	if up := w.upstreamHolding(ctx); up != "" {
		return fmt.Errorf("the commit is already on %s and will not be rewritten", up)
	}
	if out, err := git.CombinedOutput(ctx, "-c", "commit.gpgsign=false", "commit", "--amend", "--quiet", "--no-verify", "-m", message); err != nil {
		return fmt.Errorf("rewrite the commit message: %w: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

// upstreamHolding names the upstream ref that already contains HEAD, or ""
// when the branch has none or HEAD is not on it. The remote-tracking ref of
// the same name stands in for an upstream that was never set.
func (w Worktree) upstreamHolding(ctx context.Context) string {
	git := gitexec.Runner{Dir: w.Path}
	var candidates []string
	if out, err := git.Output(ctx, "rev-parse", "--abbrev-ref", "--symbolic-full-name", w.Branch+"@{upstream}"); err == nil {
		candidates = append(candidates, strings.TrimSpace(string(out)))
	}
	if out, err := git.Output(ctx, "config", "--get", "branch."+w.Branch+".remote"); err == nil {
		candidates = append(candidates, strings.TrimSpace(string(out))+"/"+w.Branch)
	}
	candidates = append(candidates, "origin/"+w.Branch)
	for _, ref := range candidates {
		if git.Run(ctx, "rev-parse", "--verify", "--quiet", "refs/remotes/"+ref) != nil {
			continue
		}
		if git.Run(ctx, "merge-base", "--is-ancestor", "HEAD", "refs/remotes/"+ref) == nil {
			return ref
		}
	}
	return ""
}

// DiffStat is git's per-file diff stat of the branch's work since the
// baseline.
func (w Worktree) DiffStat(ctx context.Context) (string, error) {
	out, err := (gitexec.Runner{Dir: w.Path}).Output(ctx, "diff", "--stat", w.Baseline, "HEAD")
	if err != nil {
		return "", fmt.Errorf("summarize the work: %w", err)
	}
	return strings.TrimSpace(string(out)), nil
}
