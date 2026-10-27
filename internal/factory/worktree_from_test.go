package factory

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"gotest.tools/v3/assert"

	"github.com/smeltdev/smelt/internal/config"
	"github.com/smeltdev/smelt/internal/iostream"
	"github.com/smeltdev/smelt/internal/testing/gitrepo"
)

// fromProject is a project on main with an epic/x branch one commit ahead, and
// the developer's uncommitted work on main. It returns the root and epic/x's tip.
func fromProject(t *testing.T) (root, epicTip string) {
	t.Helper()
	root = newProject(t)
	gitOutput(t, root, "stash", "--include-untracked")
	gitOutput(t, root, "checkout", "-b", "epic/x")
	writeFile(t, root, "epic.go", "package main\n")
	gitrepo.AddFile(t, root, "epic.go")
	gitOutput(t, root, "commit", "-m", "epic")
	epicTip = gitOutput(t, root, "rev-parse", "HEAD")
	gitOutput(t, root, "checkout", "-")
	gitOutput(t, root, "stash", "pop")
	// The helper's origin is a GitHub URL; tests must not reach for the network.
	gitOutput(t, root, "remote", "remove", "origin")
	return root, epicTip
}

func fromWorktree(t *testing.T, root, from string) (Worktree, error) {
	t.Helper()
	return CreateWorktree(context.Background(), WorktreeOptions{
		Root: root, Path: filepath.Join(t.TempDir(), "wt"), RunID: "run-1", Prompt: "add a flag", From: from,
	})
}

func TestCreateWorktreeFromALocalBranchLeavesTheCheckoutAlone(t *testing.T) {
	root, tip := fromProject(t)
	status := gitOutput(t, root, "status", "--porcelain")
	head := gitOutput(t, root, "rev-parse", "HEAD")
	index := readFile(t, root, ".git/index")
	mine := readFile(t, root, "main.go")

	wt, err := fromWorktree(t, root, "epic/x")
	assert.NilError(t, err)

	assert.Equal(t, wt.Branch, "smelt/add-a-flag/run-1")
	assert.Equal(t, gitOutput(t, wt.Path, "rev-parse", "HEAD"), tip)
	assert.Equal(t, wt.Baseline, tip)
	assert.Equal(t, wt.Head, tip)
	assert.Equal(t, wt.BaseBranch, "epic/x")
	assert.DeepEqual(t, wt.From, &FromRef{Ref: "epic/x", Commit: tip})
	// Neither the edit nor the untracked file is in the worktree.
	assert.Assert(t, readFile(t, wt.Path, "main.go") != mine)
	assert.Equal(t, gitOutput(t, wt.Path, "status", "--porcelain"), "")
	assert.Equal(t, gitOutput(t, root, "status", "--porcelain"), status)
	assert.Equal(t, gitOutput(t, root, "rev-parse", "HEAD"), head)
	assert.Equal(t, readFile(t, root, ".git/index"), index)
	assert.Equal(t, readFile(t, root, "main.go"), mine)
}

func TestCreateWorktreeFromACommitHasNoBaseBranch(t *testing.T) {
	root, tip := fromProject(t)
	wt, err := fromWorktree(t, root, tip[:10])
	assert.NilError(t, err)
	assert.Equal(t, wt.Baseline, tip)
	assert.Equal(t, wt.BaseBranch, "")
	assert.Equal(t, wt.From.Ref, tip[:10])
}

// remoteOnlyProject has epic/x only on a bare origin.
func remoteOnlyProject(t *testing.T) (root, tip string) {
	t.Helper()
	root, tip = fromProject(t)
	origin := filepath.Join(t.TempDir(), "origin.git")
	gitOutput(t, root, "init", "--bare", "--quiet", origin)
	gitOutput(t, root, "remote", "add", "origin", origin)
	gitOutput(t, root, "push", "--quiet", "origin", "epic/x")
	gitOutput(t, root, "branch", "-D", "epic/x")
	return root, tip
}

func TestCreateWorktreeFromARemoteOnlyBranchFetchesIt(t *testing.T) {
	root, tip := remoteOnlyProject(t)
	for _, ref := range []string{"epic/x", "origin/epic/x"} {
		gitOutput(t, root, "update-ref", "-d", "refs/remotes/origin/epic/x")
		wt, err := CreateWorktree(context.Background(), WorktreeOptions{
			Root: root, Path: filepath.Join(t.TempDir(), "wt"), RunID: "run-" + strings.ReplaceAll(ref, "/", "-"), Prompt: "x", From: ref,
		})
		assert.NilError(t, err, ref)
		assert.Equal(t, wt.Baseline, tip, ref)
		assert.Equal(t, wt.BaseBranch, "epic/x", ref)
		assert.Equal(t, wt.FromWarning, "", ref)
	}
}

func TestResolveFromPrefersOriginOnlyWhenTheLocalBranchIsBehind(t *testing.T) {
	root, tip := remoteOnlyProject(t)
	ctx := context.Background()
	gitOutput(t, root, "branch", "epic/x", tip+"~1")
	res, err := ResolveFrom(ctx, root, "epic/x")
	assert.NilError(t, err)
	assert.Equal(t, res.Commit, tip)

	// A diverged local branch wins.
	gitOutput(t, root, "checkout", "--quiet", "epic/x")
	gitOutput(t, root, "reset", "--quiet", "--hard", tip+"~1")
	writeFile(t, root, "local.go", "package main\n")
	gitrepo.AddFile(t, root, "local.go")
	gitOutput(t, root, "commit", "-m", "local")
	local := gitOutput(t, root, "rev-parse", "HEAD")
	gitOutput(t, root, "checkout", "--quiet", "-")
	res, err = ResolveFrom(ctx, root, "epic/x")
	assert.NilError(t, err)
	assert.Equal(t, res.Commit, local)
}

func TestResolveFromWarnsWhenTheFetchFailsAndUsesWhatIsLocal(t *testing.T) {
	root, tip := fromProject(t)
	gitOutput(t, root, "remote", "add", "origin", filepath.Join(t.TempDir(), "missing.git"))
	res, err := ResolveFrom(context.Background(), root, "epic/x")
	assert.NilError(t, err)
	assert.Equal(t, res.Commit, tip)
	assert.Assert(t, res.Warning != "")
}

func TestResolveFromUnknownRef(t *testing.T) {
	root, _ := fromProject(t)
	_, err := ResolveFrom(context.Background(), root, "nope/never")
	assert.Assert(t, errors.Is(err, ErrUnknownRef), "%v", err)
	assert.ErrorContains(t, err, "no branch or commit nope/never")
}

func TestAnnounceFromSaysOnceAndOnlyForADirtyCheckout(t *testing.T) {
	root, tip := fromProject(t)
	wt, err := fromWorktree(t, root, "epic/x")
	assert.NilError(t, err)

	var got []string
	status := func(_ iostream.Level, msg string) { got = append(got, msg) }
	announceFrom(context.Background(), root, wt, status)
	assert.DeepEqual(t, got, []string{"Starting from epic/x (" + tip[:7] + "); your uncommitted changes are not part of this run."})

	gitOutput(t, root, "stash", "--include-untracked")
	got = nil
	announceFrom(context.Background(), root, wt, status)
	assert.Equal(t, len(got), 0)
}

// startWorktree is what Run uses: it must pass From on and announce it.
func TestStartWorktreeFromARefAnnouncesAndIgnoresTheCheckout(t *testing.T) {
	root, tip := fromProject(t)
	var got []string
	opts := RunOptions{
		Root: root, Prompt: "add a flag", From: "epic/x",
		Status: func(_ iostream.Level, msg string) { got = append(got, msg) },
	}

	wt, err := startWorktree(context.Background(), opts, "run-1", t.TempDir(), config.CommitAuthor{})
	assert.NilError(t, err)

	assert.Equal(t, wt.Baseline, tip)
	assert.Equal(t, gitOutput(t, wt.Path, "rev-parse", "HEAD"), tip)
	assert.DeepEqual(t, got, []string{"Starting from epic/x (" + tip[:7] + "); your uncommitted changes are not part of this run."})
}

// startWorktree must say, once, that a stray checkout branch is not the PR's
// base, and stay quiet on the default branch.
func TestStartWorktreeSaysWhenTheCheckoutIsNotOnTheDefaultBranch(t *testing.T) {
	root := originProject(t)
	gitOutput(t, root, "checkout", "-B", "main")
	// DefaultBranchIn reads this symref.
	gitOutput(t, root, "remote", "set-head", "origin", "main")
	var got []string
	opts := RunOptions{Root: root, Prompt: "x", Status: func(_ iostream.Level, msg string) { got = append(got, msg) }}

	_, err := startWorktree(context.Background(), opts, "run-1", t.TempDir(), config.CommitAuthor{})
	assert.NilError(t, err)
	assert.Equal(t, len(got), 0)

	gitOutput(t, root, "checkout", "-b", "feature/x")
	_, err = startWorktree(context.Background(), opts, "run-2", t.TempDir(), config.CommitAuthor{})
	assert.NilError(t, err)
	assert.DeepEqual(t, got, []string{"starting from feature/x (not main); the PR will target main"})
}
