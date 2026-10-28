package factory

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"time"
)

// FormatRunID is the second-resolution form of now that run IDs, default log
// names and other per-second names start from. It is not unique by itself; use
// MintRunID for a run's ID.
func FormatRunID(now time.Time) string { return now.UTC().Format(runIDLayout) }

// MintRunID returns a run ID for the second now falls in, reserved by creating
// the run's directory under dataDir. The directory is created atomically, so
// two starts at the same instant, in one process or two, never share an ID:
// the later one gets a numeric suffix (-2, -3, ...). A fresh run's worktree
// is made in the reserved directory; a continued run leaves it empty.
func MintRunID(dataDir string, now time.Time) (string, error) {
	base := FormatRunID(now)
	dir := filepath.Join(dataDir, "factory")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", fmt.Errorf("create the run store: %w", err)
	}
	for n := 1; ; n++ {
		id := base
		if n > 1 {
			id = base + "-" + strconv.Itoa(n)
		}
		err := os.Mkdir(filepath.Join(dir, id), 0o700)
		if err == nil {
			return id, nil
		}
		if !errors.Is(err, fs.ErrExist) {
			return "", fmt.Errorf("reserve run ID %s: %w", id, err)
		}
	}
}

// ParseRunStart is the start time of the run with this ID, with or without
// the numeric suffix MintRunID adds.
func ParseRunStart(id string) (time.Time, error) {
	if len(id) > len(runIDLayout) && id[len(runIDLayout)] == '-' {
		if _, err := strconv.Atoi(id[len(runIDLayout)+1:]); err == nil {
			id = id[:len(runIDLayout)]
		}
	}
	return time.Parse(runIDLayout, id)
}
