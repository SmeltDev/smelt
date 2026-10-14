// Package upstreamcheck tests scripts/upstream-check.sh and UPSTREAM.md against
// temporary repositories, running the script with bash as users do.
package upstreamcheck

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"gotest.tools/v3/assert"
)

const script = "../../scripts/upstream-check.sh"

func git(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(),
		"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@example.com",
		"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@example.com",
		"GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null")
	out, err := cmd.CombinedOutput()
	assert.NilError(t, err, string(out))
	return strings.TrimSpace(string(out))
}

func commit(t *testing.T, dir, file, content, msg string) string {
	t.Helper()
	assert.NilError(t, os.WriteFile(filepath.Join(dir, file), []byte(content), 0o644))
	git(t, dir, "add", "-A")
	git(t, dir, "commit", "-q", "-m", msg)
	return git(t, dir, "rev-parse", "HEAD")
}

type fixture struct {
	upstream, fork string
	shas           []string
}

// newFixture builds an upstream with three commits and a fork whose UPSTREAM.md
// records the first one.
func newFixture(t *testing.T, withRemote bool) fixture {
	t.Helper()
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash not available")
	}
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	up := t.TempDir()
	git(t, up, "init", "-q", "-b", "main")
	f := fixture{upstream: up}
	f.shas = append(f.shas,
		commit(t, up, "a.txt", "one\n", "first"),
		commit(t, up, "b.txt", "two\nlines\n", "second"),
		commit(t, up, "a.txt", "changed\n", "third"))

	f.fork = t.TempDir()
	git(t, f.fork, "init", "-q", "-b", "main")
	git(t, f.fork, "remote", "add", "origin", "https://example.com/fork.git")
	commit(t, f.fork, "UPSTREAM.md", "Synced through: "+f.shas[0]+"\n", "base")
	if withRemote {
		git(t, f.fork, "remote", "add", "upstream", up)
	}
	return f
}

func (f fixture) run(t *testing.T, args ...string) (string, error) {
	t.Helper()
	abs, err := filepath.Abs(script)
	assert.NilError(t, err)
	cmd := exec.Command("bash", append([]string{abs}, args...)...)
	cmd.Dir = f.fork
	cmd.Env = append(os.Environ(), "UPSTREAM_URL="+f.upstream, "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null")
	out, err := cmd.CombinedOutput()
	return string(out), err
}

func TestListsLaterCommitsOldestFirst(t *testing.T) {
	f := newFixture(t, true)
	out, err := f.run(t)
	assert.NilError(t, err, out)
	second := strings.Index(out, "second")
	third := strings.Index(out, "third")
	assert.Assert(t, second >= 0 && third > second, out)
	assert.Assert(t, !strings.Contains(out, "first"), out)
	assert.Assert(t, strings.Contains(out, "1 file changed, 2 insertions(+)"), out)
	assert.Assert(t, strings.Contains(out, "1 file changed, 1 insertion(+), 1 deletion(-)"), out)
	assert.Assert(t, strings.Contains(out, "2 new upstream commit(s) since "+f.shas[0][:7]), out)
}

func TestUpToDate(t *testing.T) {
	f := newFixture(t, true)
	out, err := f.run(t, "--since", f.shas[2])
	assert.NilError(t, err, out)
	assert.Assert(t, strings.Contains(out, "Up to date with upstream ("+f.shas[2][:7]+")"), out)
}

func TestAcceptsPrefixAndSinceOverride(t *testing.T) {
	f := newFixture(t, true)
	out, err := f.run(t, "--since", f.shas[1][:10])
	assert.NilError(t, err, out)
	assert.Assert(t, strings.Contains(out, "1 new upstream commit(s)"), out)
}

func TestMissingSyncedThrough(t *testing.T) {
	f := newFixture(t, true)
	commit(t, f.fork, "UPSTREAM.md", "nothing here\n", "edit")
	out, err := f.run(t)
	assert.Assert(t, err != nil, out)
	assert.Assert(t, strings.Contains(out, "no 'Synced through:' line"), out)
}

func TestUnknownCommit(t *testing.T) {
	f := newFixture(t, true)
	out, err := f.run(t, "--since", strings.Repeat("1", 40))
	assert.Assert(t, err != nil, out)
	assert.Assert(t, strings.Contains(out, "not a known"), out)
}

func TestCommitNotInRemoteBranch(t *testing.T) {
	f := newFixture(t, true)
	other := commit(t, f.fork, "x.txt", "x\n", "fork only")
	out, err := f.run(t, "--since", other)
	assert.Assert(t, err != nil, out)
	assert.Assert(t, strings.Contains(out, "is not in upstream/main"), out)
}

func TestAddsRemoteAndPinsGH(t *testing.T) {
	f := newFixture(t, false)
	out, err := f.run(t)
	assert.NilError(t, err, out)
	assert.Equal(t, git(t, f.fork, "remote", "get-url", "upstream"), f.upstream)
	assert.Equal(t, git(t, f.fork, "config", "remote.origin.gh-resolved"), "base")
	assert.Assert(t, strings.Contains(out, "gh-resolved"), out)
}

func TestKeepsExistingGHDefault(t *testing.T) {
	f := newFixture(t, true)
	git(t, f.fork, "config", "remote.origin.gh-resolved", "acme/app")
	out, err := f.run(t)
	assert.NilError(t, err, out)
	assert.Equal(t, git(t, f.fork, "config", "remote.origin.gh-resolved"), "acme/app")
}

func TestLeavesTreeAndBranchesAlone(t *testing.T) {
	f := newFixture(t, false)
	before := []string{git(t, f.fork, "status", "--porcelain"), git(t, f.fork, "rev-parse", "HEAD"), git(t, f.fork, "for-each-ref", "refs/heads")}
	out, err := f.run(t)
	assert.NilError(t, err, out)
	after := []string{git(t, f.fork, "status", "--porcelain"), git(t, f.fork, "rev-parse", "HEAD"), git(t, f.fork, "for-each-ref", "refs/heads")}
	assert.DeepEqual(t, before, after)
}

func TestUsageError(t *testing.T) {
	f := newFixture(t, true)
	out, err := f.run(t, "--since")
	assert.Assert(t, err != nil, out)
	assert.Assert(t, strings.Contains(out, "usage"), out)
}

func TestUpstreamMDRecordsSyncPointAndDecisions(t *testing.T) {
	b, err := os.ReadFile("../../UPSTREAM.md")
	assert.NilError(t, err)
	md := string(b)
	assert.Assert(t, regexp.MustCompile(`(?m)^Synced through: [0-9a-f]{40}$`).MatchString(md))
	for _, sha := range []string{"364f24c", "ec0fc4b", "1957045", "0a407f8", "3a1ce38"} {
		assert.Assert(t, strings.Contains(md, "| "+sha+" |"), "missing row %s", sha)
	}
}
