package quality

import (
	"archive/tar"
	"bytes"
	"errors"
	"fmt"
	"io"
	"path"
	"slices"
	"strings"
)

// File is one source file. Path is slash-separated and relative to the project
// root.
type File struct {
	Path    string
	Content []byte
}

// sortFiles orders files by path.
func sortFiles(files []File) {
	slices.SortFunc(files, func(a, b File) int { return strings.Compare(a.Path, b.Path) })
}

// MaxSourceBytes is how much source is read back from the sandbox for one
// measurement. Files past it are not read, and what they hold is reported as
// not measured.
const MaxSourceBytes = 24 << 20

// ReadTar reads the regular files of a tar stream, up to limit bytes of
// content in all. A file that would pass the limit and every one after it are
// skipped, and their paths are returned as skipped.
func ReadTar(data []byte, limit int) (files []File, skipped []string, err error) {
	tr := tar.NewReader(bytes.NewReader(data))
	total := 0
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			sortFiles(files)
			return files, skipped, nil
		}
		if err != nil {
			return nil, nil, fmt.Errorf("read the source archive: %w", err)
		}
		if hdr.Typeflag != tar.TypeReg {
			continue
		}
		name := path.Clean(hdr.Name)
		if total+int(hdr.Size) > limit {
			skipped = append(skipped, name)
			continue
		}
		content, err := io.ReadAll(tr)
		if err != nil {
			return nil, nil, fmt.Errorf("read %s: %w", name, err)
		}
		total += len(content)
		files = append(files, File{Path: name, Content: content})
	}
}
