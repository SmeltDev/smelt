package factory

import (
	"context"
	"fmt"
	"strings"

	"github.com/smeltdev/smelt/internal/review"
	"github.com/smeltdev/smelt/internal/sidecar"
)

// workspace is the implementer's checkout on its sidecar. The loop keeps the
// implementer's work as uncommitted changes on top of a baseline commit, so
// `git diff HEAD` there, and on every reviewer it is relayed to, is exactly
// the work under review.
type workspace struct {
	exec     review.Execer
	entry    *sidecar.PoolEntry
	baseline string
}

// Change is the implementer's work so far, relative to the baseline.
type Change struct {
	// Fingerprint identifies the content of the change, so a round in which
	// the implementer changed nothing can be detected.
	Fingerprint string
	// Stat is git's one-line summary, such as "3 files changed, 40 insertions(+)".
	Stat string
}

// Empty reports whether the implementer has changed nothing.
func (c Change) Empty() bool {
	return c.Stat == ""
}

// gitIdentity commits as smelt whatever the sidecar image's git config says:
// an image may have no identity set, require signed commits, or install hooks.
const gitIdentity = "git -c user.name=" + defaultAuthorName + " -c user.email=" + defaultAuthorEmail + " -c commit.gpgsign=false"

// commitBaseline commits whatever the developer's synced tree holds, so their
// own uncommitted work is part of the baseline rather than of the change the
// reviewers see. The commit is made even when the tree is clean, so the
// baseline is always a commit smelt made.
func (w *workspace) commitBaseline(ctx context.Context) error {
	script := fmt.Sprintf(`cd %s && git add -A && %s commit -q --no-verify --allow-empty -m "smelt factory baseline" && git rev-parse HEAD`,
		sidecar.ShellEscape(w.entry.RepoPath), gitIdentity)
	out, err := w.run(ctx, script)
	if err != nil {
		return fmt.Errorf("commit baseline: %w", err)
	}
	w.baseline = strings.TrimSpace(out)
	return nil
}

// fingerprintCmd prints a hash of the change in the current directory: its
// diff against HEAD, binary files included. Two workspaces with the same files
// on the same baseline tree print the same hash, whatever their baseline
// commits, since the diff names blobs by content.
const fingerprintCmd = "git diff HEAD --binary | sha256sum | cut -d' ' -f1"

// collect gathers the implementer's work as uncommitted changes against the
// baseline and describes it. Commits the implementer made despite being told
// not to are folded back into the working tree, and new files are marked
// intent-to-add so `git diff HEAD` shows them alongside edits.
func (w *workspace) collect(ctx context.Context) (Change, error) {
	if w.baseline == "" {
		return Change{}, fmt.Errorf("collect changes: no baseline commit")
	}
	script := fmt.Sprintf(`cd %s || exit 1
if [ "$(git rev-parse HEAD)" != %s ]; then git reset -q --soft %s || exit 1; fi
git add -A -N || exit 1
%s
git diff HEAD --shortstat`,
		sidecar.ShellEscape(w.entry.RepoPath), sidecar.ShellEscape(w.baseline), sidecar.ShellEscape(w.baseline), fingerprintCmd)
	out, err := w.run(ctx, script)
	if err != nil {
		return Change{}, fmt.Errorf("collect changes: %w", err)
	}
	lines := strings.SplitN(strings.TrimSpace(out), "\n", 2)
	c := Change{Fingerprint: strings.TrimSpace(lines[0])}
	if len(lines) == 2 {
		c.Stat = strings.TrimSpace(lines[1])
	}
	return c, nil
}

// treeCmd prints the tree object of the working tree in the current directory,
// new files included. It uses an index of its own so the intent-to-add marks
// `git diff HEAD` relies on stay as they are.
const treeCmd = `idx=$(git rev-parse --git-path smelt-tree-index) || exit 1
rm -f "$idx"
GIT_INDEX_FILE=$idx git read-tree HEAD && GIT_INDEX_FILE=$idx git add -A && GIT_INDEX_FILE=$idx git write-tree
status=$?
rm -f "$idx"
exit $status`

// changedCmd prints the line-level diff between two trees, without rename
// detection so a moved file reads as new and every changed line is named.
func changedCmd(from, to string) string {
	return "git diff --unified=0 --no-renames --no-color --no-ext-diff " + sidecar.ShellEscape(from) + " " + sidecar.ShellEscape(to)
}

// tree is the tree object of the implementer's work as it stands.
func (w *workspace) tree(ctx context.Context) (string, error) {
	out, err := w.run(ctx, "cd "+sidecar.ShellEscape(w.entry.RepoPath)+" && "+treeCmd)
	if err != nil {
		return "", fmt.Errorf("record tree: %w", err)
	}
	return strings.TrimSpace(out), nil
}

// changedRanges is the lines that differ between two trees recorded by tree.
func (w *workspace) changedRanges(ctx context.Context, from, to string) (changedLines, error) {
	out, err := review.RunScript(ctx, w.exec, w.entry, "cd "+sidecar.ShellEscape(w.entry.RepoPath)+" && "+changedCmd(from, to), maxDiffOutput)
	if err != nil {
		return nil, fmt.Errorf("diff trees: %w", err)
	}
	return parseHunks(out), nil
}

// maxScriptOutput caps what a workspace script may print: a commit, or git's
// summary of the change. Past this something has gone wrong.
const maxScriptOutput = 64 << 10

// maxDiffOutput caps the line-level diff between two rounds. A diff over it
// fails rather than being cut, and the guard then downgrades nothing.
const maxDiffOutput = 8 << 20

// run executes script on the implementer's sidecar and returns its stdout.
func (w *workspace) run(ctx context.Context, script string) (string, error) {
	return review.RunScript(ctx, w.exec, w.entry, script, maxScriptOutput)
}
