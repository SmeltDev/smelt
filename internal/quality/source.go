package quality

import (
	"archive/tar"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/smeltdev/smelt/internal/gitexec"
)

// ReadTree reads the Go files of the working tree at dir: tracked files and
// untracked ones that are not ignored, as they are on disk now. dir must be
// inside a git repository; paths are relative to dir.
func ReadTree(ctx context.Context, dir string) ([]File, error) {
	out, err := (gitexec.Runner{Dir: dir}).Output(ctx, "ls-files", "-z", "-co", "--exclude-standard", "--", "*.go")
	if err != nil {
		return nil, fmt.Errorf("list the files in %s: %w", dir, err)
	}
	var files []File
	for p := range strings.SplitSeq(string(out), "\x00") {
		if p == "" || Excluded(p) {
			continue
		}
		content, err := os.ReadFile(filepath.Join(dir, filepath.FromSlash(p)))
		if errors.Is(err, fs.ErrNotExist) {
			// Deleted from disk but still in the index.
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("read %s: %w", p, err)
		}
		files = append(files, File{Path: p, Content: content})
	}
	sortFiles(files)
	return files, nil
}

// sortFiles orders files by path, as the readers return them.
func sortFiles(files []File) {
	slices.SortFunc(files, func(a, b File) int { return strings.Compare(a.Path, b.Path) })
}

// ReadRev reads the Go files of the commit rev in the repository at dir,
// without touching the working tree.
func ReadRev(ctx context.Context, dir, rev string) ([]File, error) {
	out, err := (gitexec.Runner{Dir: dir}).Output(ctx, "archive", "--format=tar", rev, "--", "*.go")
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", rev, err)
	}
	var files []File
	tr := tar.NewReader(bytes.NewReader(out))
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			sortFiles(files)
			return files, nil
		}
		if err != nil {
			return nil, fmt.Errorf("read the archive of %s: %w", rev, err)
		}
		if hdr.Typeflag != tar.TypeReg || Excluded(hdr.Name) {
			continue
		}
		content, err := io.ReadAll(tr)
		if err != nil {
			return nil, fmt.Errorf("read %s at %s: %w", hdr.Name, rev, err)
		}
		files = append(files, File{Path: hdr.Name, Content: content})
	}
}
