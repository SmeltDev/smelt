package factory

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gotest.tools/v3/assert"

	"github.com/smeltdev/smelt/internal/iostream"
	"github.com/smeltdev/smelt/internal/testing/gitrepo"
)

// newProject returns a repository with one commit and, on top of it, the
// developer's work in progress: an edit and an untracked file.
func newProject(t *testing.T) string {
	t.Helper()
	root := gitrepo.SetupGitRepo(t, "my-org", "my-repo")
	writeFile(t, root, "main.go", "package main\n")
	gitrepo.AddFile(t, root, "main.go")
	gitOutput(t, root, "commit", "-m", "base")
	writeFile(t, root, "main.go", "package main\n\n// in progress\n")
	writeFile(t, root, "mine.go", "package main\n")
	return root
}

func TestCreateWorktreeStartsFromTheDevelopersFiles(t *testing.T) {
	ctx := context.Background()
	root := newProject(t)
	before := gitOutput(t, root, "status", "--porcelain")
	branch := gitOutput(t, root, "rev-parse", "--abbrev-ref", "HEAD")

	wt, err := CreateWorktree(ctx, WorktreeOptions{Root: root, Path: filepath.Join(t.TempDir(), "wt"), RunID: "run-1", Prompt: "Add a --verbose flag"})
	assert.NilError(t, err)

	assert.Equal(t, wt.Branch, "smelt/add-a-verbose-flag/run-1")
	assert.Equal(t, gitOutput(t, wt.Path, "rev-parse", "HEAD"), wt.Baseline)
	assert.Equal(t, wt.Head, gitOutput(t, root, "rev-parse", "HEAD"))
	assert.Assert(t, wt.Baseline != wt.Head, "uncommitted work was not committed as the baseline")
	// Uncommitted work is in the baseline, untracked files included, so it is
	// not part of the change under review.
	assert.Equal(t, readFile(t, wt.Path, "main.go"), "package main\n\n// in progress\n")
	assert.Equal(t, readFile(t, wt.Path, "mine.go"), "package main\n")
	assert.Equal(t, gitOutput(t, wt.Path, "status", "--porcelain"), "")
	// The developer's checkout is left exactly as it was.
	assert.Equal(t, gitOutput(t, root, "status", "--porcelain"), before)
	assert.Equal(t, gitOutput(t, root, "rev-parse", "--abbrev-ref", "HEAD"), branch)
}

func TestCreateWorktreeNamesTheUncommittedFiles(t *testing.T) {
	root := newProject(t)
	writeFile(t, root, "pkg/deep/new.go", "package deep\n")

	wt, err := CreateWorktree(context.Background(), WorktreeOptions{Root: root, Path: filepath.Join(t.TempDir(), "wt"), RunID: "run-1", Prompt: "x"})
	assert.NilError(t, err)
	assert.DeepEqual(t, wt.Uncommitted, []string{"main.go", "mine.go", "pkg/deep/new.go"})
}

func TestCreateWorktreeCleanBaseLeavesUncommittedWorkOut(t *testing.T) {
	root := newProject(t)

	wt, err := CreateWorktree(context.Background(), WorktreeOptions{Root: root, Path: filepath.Join(t.TempDir(), "wt"), RunID: "run-1", Prompt: "x", CleanBase: true})
	assert.NilError(t, err)
	assert.Equal(t, wt.Baseline, wt.Head)
	assert.Equal(t, len(wt.Uncommitted), 0)
	assert.Equal(t, readFile(t, wt.Path, "main.go"), "package main\n")
	_, statErr := os.Stat(filepath.Join(wt.Path, "mine.go"))
	assert.Assert(t, os.IsNotExist(statErr), "mine.go is in the branch")
}

func TestUncommittedNote(t *testing.T) {
	files := []string{"a", "b", "c", "d", "e", "f"}
	assert.Equal(t, UncommittedNote(Worktree{Baseline: "b", Head: "h", Uncommitted: files}),
		"starting from uncommitted work: 6 files (a, b, c, d, e, …)")
	assert.Equal(t, UncommittedNote(Worktree{Baseline: "b", Head: "h", Uncommitted: files[:2]}),
		"starting from uncommitted work: 2 files (a, b)")
	assert.Equal(t, UncommittedNote(Worktree{Baseline: "h", Head: "h"}), "")
	assert.Equal(t, UncommittedNote(Worktree{Baseline: "b", Head: "h", Uncommitted: files, From: &FromRef{}}), "")
}

func TestAnnounceStartWarnsAboutUncommittedWork(t *testing.T) {
	var got []string
	status := func(_ iostream.Level, msg string) { got = append(got, msg) }
	AnnounceStart(context.Background(), t.TempDir(), Worktree{Baseline: "b", Head: "h", Uncommitted: []string{"mine.go"}}, status)
	assert.DeepEqual(t, got, []string{"starting from uncommitted work: 1 files (mine.go)"})

	got = nil
	AnnounceStart(context.Background(), t.TempDir(), Worktree{Baseline: "h", Head: "h"}, status)
	assert.Equal(t, len(got), 0)
}

func TestCreateWorktreeFromACleanTreeStartsAtHEAD(t *testing.T) {
	ctx := context.Background()
	root := gitrepo.SetupGitRepo(t, "my-org", "my-repo")
	writeFile(t, root, "main.go", "package main\n")
	gitrepo.AddFile(t, root, "main.go")
	gitOutput(t, root, "commit", "-m", "base")

	wt, err := CreateWorktree(ctx, WorktreeOptions{Root: root, Path: filepath.Join(t.TempDir(), "wt"), RunID: "run-1", Prompt: "add a flag"})
	assert.NilError(t, err)
	assert.Equal(t, wt.Baseline, gitOutput(t, root, "rev-parse", "HEAD"))
	assert.Equal(t, wt.Head, wt.Baseline)
}

func TestWorktreeCommitKeepsTheWorktree(t *testing.T) {
	ctx := context.Background()
	root := newProject(t)
	wt, err := CreateWorktree(ctx, WorktreeOptions{Root: root, Path: filepath.Join(t.TempDir(), "wt"), RunID: "run-1", Prompt: "add a flag"})
	assert.NilError(t, err)

	commit, err := wt.Commit(ctx, "nothing")
	assert.NilError(t, err)
	assert.Equal(t, commit, "", "nothing changed, so nothing is committed")

	writeFile(t, wt.Path, "main.go", "package main\n\nfunc main() {}\n")
	writeFile(t, wt.Path, "helper.go", "package main\n")
	commit, err = wt.Commit(ctx, Notes{Title: "add a main", What: "- adds main", Why: "- needed", How: "- main.go"}.CommitMessage("run-1", Outcome{Result: ResultPassed, Rounds: 2}, ""))
	assert.NilError(t, err)

	assert.Equal(t, gitOutput(t, root, "rev-parse", wt.Branch), commit)
	assert.Equal(t, gitOutput(t, root, "log", "-1", "--format=%s", wt.Branch), "add a main")
	assert.Equal(t, gitOutput(t, root, "log", "-1", "--format=%b", wt.Branch),
		"What:\n- adds main\n\nWhy:\n- needed\n\nHow:\n- main.go\n\nWritten by smelt factory run run-1: passed after 2 round(s).", "the subject is not repeated in the body")
	stat, err := wt.Stat(ctx)
	assert.NilError(t, err)
	assert.Equal(t, stat, "2 files changed, 2 insertions(+), 1 deletion(-)")
	_, err = os.Stat(wt.Path)
	assert.NilError(t, err, "the worktree is kept for the developer")
}

func TestWorktreeRemove(t *testing.T) {
	ctx := context.Background()
	root := newProject(t)
	wt, err := CreateWorktree(ctx, WorktreeOptions{Root: root, Path: filepath.Join(t.TempDir(), "wt"), RunID: "run-1", Prompt: "add a flag"})
	assert.NilError(t, err)

	assert.NilError(t, wt.Remove(ctx, root))
	_, err = os.Stat(wt.Path)
	assert.Assert(t, os.IsNotExist(err))
	assert.Equal(t, gitOutput(t, root, "branch", "--list", wt.Branch), "")
	assert.Equal(t, readFile(t, root, "mine.go"), "package main\n", "the developer's own files are untouched")
}

func TestBranchSlug(t *testing.T) {
	for _, tc := range []struct {
		prompt string
		want   string
	}{
		{prompt: "Add a factory status command", want: "add-a-factory-status-command"},
		{prompt: "# Fix --continue lookup\n\nMore detail", want: "fix-continue-lookup"},
		{prompt: "  Spaces, punctuation & CAPS!  ", want: "spaces-punctuation-caps"},
		{prompt: "!!!", want: "change"},
		{prompt: "Make this deliberately long branch name stop at a readable boundary eventually", want: "make-this-deliberately-long-branch-name-stop-at"},
		{prompt: "Make this deliberately long branch name stop atop a word", want: "make-this-deliberately-long-branch-name-stop"},
		{prompt: strings.Repeat("a", 60), want: strings.Repeat("a", 48)},
	} {
		t.Run(tc.want, func(t *testing.T) {
			assert.Equal(t, branchSlug(tc.prompt), tc.want)
		})
	}
}
