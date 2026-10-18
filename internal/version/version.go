package version

import (
	"fmt"
	"runtime/debug"
	"strings"

	"github.com/smeltdev/smelt/internal/brand"
)

// Value is set via ldflags at build time (e.g. -X main.version=...).
// main.go copies the value here at startup so internal packages can use it.
var Value = "dev"

// UserAgent is the User-Agent smelt sends.
func UserAgent() string {
	return fmt.Sprintf("%s (%s)", brand.Command, Value)
}

// revisionLen is how many characters of a commit hash are shown.
const revisionLen = 7

// Build is what the Go toolchain stamped into a binary built from a checkout.
// The zero value means no VCS information.
type Build struct {
	// Revision is the commit, shortened.
	Revision string
	// Time is the commit date, as the toolchain wrote it (RFC 3339).
	Time string
	// Modified reports uncommitted changes in the tree it was built from.
	Modified bool
}

// ReadBuild extracts the Build from read, which has debug.ReadBuildInfo's shape
// so tests can supply their own.
func ReadBuild(read func() (*debug.BuildInfo, bool)) Build {
	info, ok := read()
	if !ok || info == nil {
		return Build{}
	}
	var b Build
	for _, s := range info.Settings {
		switch s.Key {
		case "vcs.revision":
			b.Revision = s.Value
			if len(b.Revision) > revisionLen {
				b.Revision = b.Revision[:revisionLen]
			}
		case "vcs.time":
			b.Time = s.Value
		case "vcs.modified":
			b.Modified = s.Value == "true"
		}
	}
	return b
}

// Current is the Build of the running binary.
func Current() Build { return ReadBuild(debug.ReadBuildInfo) }

// Date is the day of the commit (YYYY-MM-DD), empty when unknown.
func (b Build) Date() string {
	date, _, _ := strings.Cut(b.Time, "T")
	return date
}

// Label is the revision with "-dirty" appended for an uncommitted tree, empty
// without VCS information. It is how a build is compared and displayed.
func (b Build) Label() string {
	if b.Revision != "" && b.Modified {
		return b.Revision + "-dirty"
	}
	return b.Revision
}

// Describe formats ver with the build, as "dev (dabe728, 2026-10-18)". A dirty
// tree appends "-dirty" to the revision; without VCS information it is ver.
func Describe(ver string, b Build) string {
	if b.Revision == "" {
		return ver
	}
	rev := b.Label()
	if date := b.Date(); date != "" {
		return fmt.Sprintf("%s (%s, %s)", ver, rev, date)
	}
	return fmt.Sprintf("%s (%s)", ver, rev)
}
