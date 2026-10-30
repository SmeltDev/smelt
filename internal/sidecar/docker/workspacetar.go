package docker

import (
	"archive/tar"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/smeltdev/smelt/internal/gitexec"
)

// This file is the host side of a volume workspace: choosing which files a sync
// sends, packing them as a tar stream, and unpacking a pull. The in-container
// side is in workspace.go. Both emulate `rsync --archive --delete` with
// `.gitignore` filters, which is what a bind workspace uses.

// normalizeEntry turns a path from a tar header or a listing into a clean
// slash-separated path relative to the workspace root. ok is false for names
// that are empty, absolute, or climb out of the root.
func normalizeEntry(name string) (clean string, ok bool) {
	if name == "" || path.IsAbs(name) {
		return "", false
	}
	clean = path.Clean(name)
	if clean == "." || clean == ".." || strings.HasPrefix(clean, "../") {
		return "", false
	}
	return clean, true
}

// parseListing reads the NUL-separated names `git ls-files -z` or `find -print0`
// printed. Directory entries (a nested repository is listed as "dir/") are
// dropped: a listing is of files.
func parseListing(out []byte) []string {
	var names []string
	for _, raw := range strings.Split(string(out), "\x00") {
		if strings.HasSuffix(raw, "/") {
			continue
		}
		if name, ok := normalizeEntry(raw); ok {
			names = append(names, name)
		}
	}
	return names
}

// staleFiles are the container files a sync did not send: what rsync --delete
// would remove. Container files git ignores are not in the listing and so are
// kept, as rsync kept excluded files.
func staleFiles(inContainer, sent []string) []string {
	keep := make(map[string]bool, len(sent))
	for _, name := range sent {
		keep[name] = true
	}
	var stale []string
	for _, name := range inContainer {
		if !keep[name] {
			stale = append(stale, name)
		}
	}
	return stale
}

// pruneScript removes the stale files in repoPath, then the directories they
// leave empty, deepest first. rmdir refuses a directory that still has content,
// so a directory holding something kept stays.
func pruneScript(repoPath string, stale []string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "cd %s || exit 1\n", shellQuote(repoPath))
	dirs := map[string]bool{}
	for _, name := range stale {
		fmt.Fprintf(&b, "rm -f -- %s\n", shellQuote(name))
		for dir := path.Dir(name); dir != "."; dir = path.Dir(dir) {
			dirs[dir] = true
		}
	}
	ordered := make([]string, 0, len(dirs))
	for dir := range dirs {
		ordered = append(ordered, dir)
	}
	sort.Slice(ordered, func(i, j int) bool {
		if di, dj := strings.Count(ordered[i], "/"), strings.Count(ordered[j], "/"); di != dj {
			return di > dj
		}
		return ordered[i] < ordered[j]
	})
	for _, dir := range ordered {
		fmt.Fprintf(&b, "rmdir -- %s 2>/dev/null\n", shellQuote(dir))
	}
	b.WriteString("exit 0\n")
	return b.String()
}

// listLocalFiles lists the files under dir that git would not ignore, relative
// to dir. It asks git against a throwaway empty repository, so that every file
// is untracked and only .gitignore decides: the filter rsync applied, and one
// that works in a directory that is not a repository. dir's own .git is never
// listed. Nested repositories, which git lists as a directory, are walked.
func listLocalFiles(ctx context.Context, dir string) ([]string, error) {
	scratch, err := os.MkdirTemp("", "smelt-ls-")
	if err != nil {
		return nil, err
	}
	defer func() { _ = os.RemoveAll(scratch) }()
	if err := (gitexec.Runner{}).Run(ctx, "init", "-q", "--bare", scratch); err != nil {
		return nil, fmt.Errorf("git init: %w", err)
	}
	out, err := gitexec.Runner{Dir: dir}.Output(ctx, "--git-dir="+scratch, "--work-tree="+dir,
		"-c", "core.excludesFile=/dev/null", "ls-files", "-z", "--others", "--exclude-standard")
	if err != nil {
		return nil, fmt.Errorf("git ls-files in %s: %w", dir, err)
	}
	var files []string
	for _, raw := range strings.Split(string(out), "\x00") {
		name, ok := normalizeEntry(raw)
		if !ok {
			continue
		}
		if !strings.HasSuffix(raw, "/") {
			files = append(files, name)
			continue
		}
		nested, err := walkFiles(dir, name)
		if err != nil {
			return nil, err
		}
		files = append(files, nested...)
	}
	sort.Strings(files)
	return slices.Compact(files), nil
}

// walkFiles lists the files and symlinks under dir/sub, relative to dir.
func walkFiles(dir, sub string) ([]string, error) {
	var files []string
	err := filepath.WalkDir(filepath.Join(dir, filepath.FromSlash(sub)), func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		rel, err := filepath.Rel(dir, p)
		if err != nil {
			return err
		}
		files = append(files, filepath.ToSlash(rel))
		return nil
	})
	return files, err
}

// writeWorkspaceTar packs the named files under dir as a tar stream, with the
// directories above them. A file that vanishes while the list is walked is
// skipped; anything that is neither a regular file nor a symlink is too.
func writeWorkspaceTar(w io.Writer, dir string, files []string) error {
	tw := tar.NewWriter(w)
	madeDirs := map[string]bool{}
	for _, name := range files {
		if err := writeParentDirs(tw, dir, name, madeDirs); err != nil {
			return err
		}
		if err := writeEntry(tw, dir, name); err != nil {
			return err
		}
	}
	return tw.Close()
}

func writeParentDirs(tw *tar.Writer, dir, name string, made map[string]bool) error {
	var missing []string
	for parent := path.Dir(name); parent != "." && !made[parent]; parent = path.Dir(parent) {
		missing = append(missing, parent)
	}
	for _, parent := range slices.Backward(missing) {
		info, err := os.Lstat(filepath.Join(dir, filepath.FromSlash(parent)))
		if err != nil {
			return err
		}
		hdr, err := tar.FileInfoHeader(info, "")
		if err != nil {
			return err
		}
		resetOwner(hdr)
		hdr.Name = parent + "/"
		if err := tw.WriteHeader(hdr); err != nil {
			return err
		}
		made[parent] = true
	}
	return nil
}

func writeEntry(tw *tar.Writer, dir, name string) error {
	full := filepath.Join(dir, filepath.FromSlash(name))
	info, err := os.Lstat(full)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	link := ""
	switch {
	case info.Mode()&fs.ModeSymlink != 0:
		if link, err = os.Readlink(full); err != nil {
			return err
		}
	case !info.Mode().IsRegular():
		return nil
	}
	hdr, err := tar.FileInfoHeader(info, link)
	if err != nil {
		return err
	}
	resetOwner(hdr)
	hdr.Name = name
	if err := tw.WriteHeader(hdr); err != nil {
		return err
	}
	if !info.Mode().IsRegular() {
		return nil
	}
	f, err := os.Open(full)
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	// A file that grew since Lstat is cut at the size the header promised.
	if _, err := io.CopyN(tw, f, hdr.Size); err != nil {
		return fmt.Errorf("pack %s: %w", name, err)
	}
	return nil
}

// resetOwner drops what a tar header says about the host's users and times: the
// container's user owns the files, and Upload says which.
func resetOwner(hdr *tar.Header) {
	hdr.Uid, hdr.Gid = 0, 0
	hdr.Uname, hdr.Gname = "", ""
	hdr.AccessTime, hdr.ChangeTime = time.Time{}, time.Time{}
}

// chownTar copies a tar stream, giving every entry to uid:gid.
func chownTar(dst io.Writer, src io.Reader, uid, gid int) error {
	tr, tw := tar.NewReader(src), tar.NewWriter(dst)
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return tw.Close()
		}
		if err != nil {
			return err
		}
		hdr.Uid, hdr.Gid = uid, gid
		hdr.Uname, hdr.Gname = "", ""
		if err := tw.WriteHeader(hdr); err != nil {
			return err
		}
		if _, err := io.CopyN(tw, tr, hdr.Size); err != nil {
			return err
		}
	}
}

// extractWorkspace unpacks the entries of a tar stream that are in eligible
// into dir, and returns the files it wrote. Every write goes through an
// os.Root, so no entry or symlink can land outside dir. A file is written
// beside its destination and renamed into place, so a reader never sees half of
// it, and keeps its mode and modification time as rsync -a would.
func extractWorkspace(src io.Reader, dir string, eligible map[string]bool) (map[string]bool, error) {
	root, err := os.OpenRoot(dir)
	if err != nil {
		return nil, err
	}
	defer func() { _ = root.Close() }()
	written := map[string]bool{}
	tr := tar.NewReader(src)
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return written, nil
		}
		if err != nil {
			return written, err
		}
		name, ok := normalizeEntry(hdr.Name)
		if !ok || !eligible[name] {
			continue
		}
		switch hdr.Typeflag {
		case tar.TypeReg:
			err = writeRegular(root, name, hdr, tr)
		case tar.TypeSymlink:
			err = writeSymlink(root, name, hdr.Linkname)
		default:
			continue
		}
		if err != nil {
			return written, fmt.Errorf("unpack %s: %w", name, err)
		}
		written[name] = true
	}
}

func writeRegular(root *os.Root, name string, hdr *tar.Header, content io.Reader) error {
	if err := root.MkdirAll(path.Dir(name), 0o755); err != nil {
		return err
	}
	var r [6]byte
	_, _ = rand.Read(r[:])
	tmp := path.Join(path.Dir(name), "."+path.Base(name)+".tmp-"+hex.EncodeToString(r[:]))
	f, err := root.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, hdr.FileInfo().Mode().Perm())
	if err != nil {
		return err
	}
	if _, err := io.Copy(f, content); err != nil {
		_ = f.Close()
		_ = root.Remove(tmp)
		return err
	}
	if err := f.Close(); err != nil {
		_ = root.Remove(tmp)
		return err
	}
	// OpenFile applied the umask; the container's mode is what the file keeps.
	if err := root.Chmod(tmp, hdr.FileInfo().Mode().Perm()); err != nil {
		_ = root.Remove(tmp)
		return err
	}
	if err := root.Chtimes(tmp, hdr.ModTime, hdr.ModTime); err != nil {
		_ = root.Remove(tmp)
		return err
	}
	if err := root.Rename(tmp, name); err != nil {
		_ = root.Remove(tmp)
		return err
	}
	return nil
}

func writeSymlink(root *os.Root, name, target string) error {
	if err := root.MkdirAll(path.Dir(name), 0o755); err != nil {
		return err
	}
	if err := root.Remove(name); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	return root.Symlink(target, name)
}

// removeMissing deletes the local files a pull did not write, and the
// directories that leave empty: the --delete half of a mirror. local is what
// listLocalFiles found, so .git and ignored files are never touched.
func removeMissing(dir string, local []string, written map[string]bool) error {
	root, err := os.OpenRoot(dir)
	if err != nil {
		return err
	}
	defer func() { _ = root.Close() }()
	for _, name := range local {
		if written[name] {
			continue
		}
		if err := root.Remove(name); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return fmt.Errorf("remove %s: %w", name, err)
		}
		for parent := path.Dir(name); parent != "."; parent = path.Dir(parent) {
			if root.Remove(parent) != nil {
				break
			}
		}
	}
	return nil
}
