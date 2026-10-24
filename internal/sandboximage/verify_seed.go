package sandboximage

import (
	"context"
	"fmt"
	"strings"

	"github.com/smeltdev/smelt/internal/gitexec"
)

// seedHistory gives the scratch copy in hostDir the history a factory run's
// worktree has: every local branch and remote-tracking ref of src, the current
// branch checked out, and the synced tree (uncommitted edits included)
// committed on top. Sync leaves only a bare `git init`, so without this a gate
// that diffs against origin/<base> has no base to diff against. An instance
// without a host directory (a remote backend) has nothing to seed. Fetching
// only writes into hostDir; src is read.
func seedHistory(ctx context.Context, hostDir, src string) error {
	if hostDir == "" {
		return nil
	}
	srcGit := gitexec.Runner{Dir: src}
	head, err := srcGit.Output(ctx, "rev-parse", "--verify", "--quiet", "HEAD^{commit}")
	if err != nil {
		return nil // no commits to carry over
	}
	git := gitexec.Runner{Dir: hostDir}
	fetch := []string{"fetch", "--quiet", "--no-tags", "--update-head-ok", src,
		"+refs/heads/*:refs/heads/*", "+refs/remotes/*:refs/remotes/*"}
	if out, err := git.CombinedOutput(ctx, fetch...); err != nil {
		return fmt.Errorf("git fetch: %w: %s", err, strings.TrimSpace(string(out)))
	}
	if branch, err := srcGit.Output(ctx, "symbolic-ref", "--quiet", "HEAD"); err == nil {
		if out, err := git.CombinedOutput(ctx, "symbolic-ref", "HEAD", strings.TrimSpace(string(branch))); err != nil {
			return fmt.Errorf("git symbolic-ref: %w: %s", err, strings.TrimSpace(string(out)))
		}
	} else if out, err := git.CombinedOutput(ctx, "update-ref", "--no-deref", "HEAD", strings.TrimSpace(string(head))); err != nil {
		return fmt.Errorf("git update-ref: %w: %s", err, strings.TrimSpace(string(out)))
	}
	for _, args := range [][]string{
		{"add", "-A"},
		{"-c", "user.name=smelt", "-c", "user.email=smelt@localhost", "-c", "commit.gpgsign=false",
			"commit", "--quiet", "--no-verify", "--allow-empty", "-m", "synced working tree"},
	} {
		if out, err := git.CombinedOutput(ctx, args...); err != nil {
			return fmt.Errorf("git %s: %w: %s", args[len(args)-1], err, strings.TrimSpace(string(out)))
		}
	}
	return nil
}
