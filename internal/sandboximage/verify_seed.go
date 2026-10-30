package sandboximage

import (
	"archive/tar"
	"context"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strings"

	"github.com/smeltdev/smelt/internal/gitexec"
	"github.com/smeltdev/smelt/internal/sidecar"
)

// seedDir is where the source repository's git directory is unpacked inside the
// container while its refs are fetched; it is removed afterwards.
const seedDir = ".smelt-src"

// uploader is implemented by a backend that can unpack a tar stream into a
// container directory, which is how the repo's history reaches a workspace the
// host can't write to.
type uploader interface {
	Upload(ctx context.Context, id, dir string, content io.Reader) error
}

// seedHistory gives the scratch copy in the container the history a factory
// run's worktree has: every local branch and remote-tracking ref of src, the
// current branch checked out, and the synced tree (uncommitted edits included)
// committed on top. Sync leaves only a bare `git init`, so without this a gate
// that diffs against origin/<base> has no base to diff against. A backend that
// can't upload (a remote backend) has nothing to seed. src is only read.
func seedHistory(ctx context.Context, b verifyBackend, inst sidecar.Instance, src string) error {
	up, ok := b.(uploader)
	if !ok {
		return nil
	}
	srcGit := gitexec.Runner{Dir: src}
	head, err := srcGit.Output(ctx, "rev-parse", "--verify", "--quiet", "HEAD^{commit}")
	if err != nil {
		return nil // no commits to carry over
	}
	gitDir, err := srcGit.Output(ctx, "rev-parse", "--path-format=absolute", "--git-common-dir")
	if err != nil {
		return fmt.Errorf("find git directory: %w", err)
	}
	pr, pw := io.Pipe()
	go func() { pw.CloseWithError(tarDir(pw, strings.TrimSpace(string(gitDir)), seedDir)) }()
	err = up.Upload(ctx, inst.ID, inst.RepoPath, pr)
	_ = pr.CloseWithError(err)
	if err != nil {
		return fmt.Errorf("upload git directory: %w", err)
	}
	var branch string
	if out, err := srcGit.Output(ctx, "symbolic-ref", "--quiet", "HEAD"); err == nil {
		branch = strings.TrimSpace(string(out))
	}
	script := seedScript(inst.RepoPath, branch, strings.TrimSpace(string(head)))
	var out strings.Builder
	code, err := b.Exec(ctx, inst.ID, script, nil, func(_ string, p []byte) { out.Write(p) }, nil)
	if err != nil {
		return err
	}
	if code != 0 {
		return fmt.Errorf("seeding exited %d: %s", code, strings.TrimSpace(out.String()))
	}
	return nil
}

// seedScript fetches the refs of the unpacked git directory, points HEAD at
// the source's branch (or its commit when detached), and commits the synced
// tree on top. branch is empty for a detached HEAD.
func seedScript(repoPath, branch, head string) string {
	headStep := "git update-ref --no-deref HEAD " + sidecar.ShellEscape(head)
	if branch != "" {
		headStep = "git symbolic-ref HEAD " + sidecar.ShellEscape(branch)
	}
	return strings.Join([]string{
		"set -e",
		"cd " + sidecar.ShellEscape(repoPath),
		"git fetch --quiet --no-tags --update-head-ok ./" + seedDir + " '+refs/heads/*:refs/heads/*' '+refs/remotes/*:refs/remotes/*'",
		"rm -rf ./" + seedDir,
		headStep,
		"git add -A",
		"git -c user.name=smelt -c user.email=smelt@localhost -c commit.gpgsign=false commit --quiet --no-verify --allow-empty -m 'synced working tree'",
	}, "\n")
}

// tarDir packs the regular files and directories under dir as a tar stream
// rooted at name.
func tarDir(w io.Writer, dir, name string) error {
	root, err := os.OpenRoot(dir)
	if err != nil {
		return err
	}
	defer func() { _ = root.Close() }()
	tw := tar.NewWriter(w)
	err = filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		info, err := d.Info()
		if err != nil || !info.IsDir() && !info.Mode().IsRegular() {
			return err
		}
		rel, err := filepath.Rel(dir, p)
		if err != nil {
			return err
		}
		hdr, err := tar.FileInfoHeader(info, "")
		if err != nil {
			return err
		}
		hdr.Name = path.Join(name, filepath.ToSlash(rel))
		if info.IsDir() {
			hdr.Name += "/"
		}
		hdr.Uid, hdr.Gid, hdr.Uname, hdr.Gname = 0, 0, "", ""
		if err := tw.WriteHeader(hdr); err != nil || info.IsDir() {
			return err
		}
		f, err := root.Open(rel)
		if err != nil {
			return err
		}
		defer func() { _ = f.Close() }()
		_, err = io.CopyN(tw, f, hdr.Size)
		return err
	})
	if err != nil {
		return err
	}
	return tw.Close()
}
