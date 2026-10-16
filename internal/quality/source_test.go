package quality

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"gotest.tools/v3/assert"

	"github.com/smeltdev/smelt/internal/gitexec"
)

func git(t *testing.T, dir string, args ...string) string {
	t.Helper()
	out, err := (gitexec.Runner{Dir: dir}).CombinedOutput(context.Background(),
		append([]string{"-c", "user.name=t", "-c", "user.email=t@example.com", "-c", "commit.gpgsign=false"}, args...)...)
	assert.NilError(t, err, string(out))
	return string(out)
}

func write(t *testing.T, dir, name, content string) {
	t.Helper()
	p := filepath.Join(dir, filepath.FromSlash(name))
	assert.NilError(t, os.MkdirAll(filepath.Dir(p), 0o755))
	assert.NilError(t, os.WriteFile(p, []byte(content), 0o644))
}

func paths(files []File) []string {
	out := make([]string, len(files))
	for i, f := range files {
		out[i] = f.Path
	}
	return out
}

// repo is a git repository with a committed baseline and a changed working
// tree.
func repo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	git(t, dir, "init", "-q")
	write(t, dir, "a.go", "package p\nfunc a() {}\n")
	write(t, dir, "sub/b.go", "package p\nfunc b() {}\n")
	write(t, dir, "a_test.go", "package p\n")
	write(t, dir, "vendor/v.go", "package v\n")
	write(t, dir, "notes.md", "# notes\n")
	write(t, dir, ".gitignore", "ignored.go\n")
	git(t, dir, "add", "-A")
	git(t, dir, "commit", "-q", "-m", "base")
	return dir
}

func TestReadRevReadsTheCommittedGoFiles(t *testing.T) {
	dir := repo(t)
	// The working tree moves on; the revision does not.
	write(t, dir, "a.go", "package p\nfunc a() { println() }\n")
	files, err := ReadRev(context.Background(), dir, "HEAD")
	assert.NilError(t, err)
	assert.DeepEqual(t, paths(files), []string{"a.go", "sub/b.go"})
	assert.Equal(t, string(files[0].Content), "package p\nfunc a() {}\n")
}

func TestReadRevRejectsUnknownRevisions(t *testing.T) {
	_, err := ReadRev(context.Background(), repo(t), "no-such-rev")
	assert.ErrorContains(t, err, "no-such-rev")
}

func TestReadTreeReadsTheWorkingTreeAsItIsOnDisk(t *testing.T) {
	dir := repo(t)
	write(t, dir, "a.go", "package p\nfunc a() { println() }\n")
	write(t, dir, "new.go", "package p\nfunc n() {}\n")
	write(t, dir, "ignored.go", "package p\n")
	assert.NilError(t, os.Remove(filepath.Join(dir, "sub/b.go")))
	files, err := ReadTree(context.Background(), dir)
	assert.NilError(t, err)
	assert.DeepEqual(t, paths(files), []string{"a.go", "new.go"})
	assert.Equal(t, string(files[0].Content), "package p\nfunc a() { println() }\n")
}

func TestAssessComparesTheTreeWithABase(t *testing.T) {
	dir := repo(t)
	write(t, dir, "c.go", "package p\n\n"+funcWithIfs("busy", 16))
	a, err := Assess(context.Background(), dir, "HEAD", Config{})
	assert.NilError(t, err)
	assert.Assert(t, a.Before != nil)
	assert.Equal(t, a.After.OverThreshold, 1)
	// The busy function, and the erosion it adds to a tree that had none.
	assert.Equal(t, len(a.Findings), 2)
	assert.Equal(t, a.Findings[0].Func, "busy")
	assert.Equal(t, a.Findings[1].File, "")

	alone, err := Assess(context.Background(), dir, "", Config{})
	assert.NilError(t, err)
	assert.Assert(t, alone.Before == nil)
	assert.Equal(t, len(alone.Findings), 0)
}
