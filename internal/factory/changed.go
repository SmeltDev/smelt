package factory

import (
	"fmt"
	"math"
	"regexp"
	"strconv"
	"strings"

	"github.com/smeltdev/smelt/internal/review"
)

// matchWindow is how many lines apart two findings of one review in one file
// can be and still count as the same finding. Reviewers cite a nearby line for
// the same problem from one round to the next as the code moves.
const matchWindow = 5

// span is a run of lines, both ends included.
type span struct{ Start, End int }

// changedLines is the new-side line spans that differ between two trees, by
// file. A file mapped to wholeFile changed as a whole (it was deleted).
type changedLines map[string][]span

var wholeFile = span{Start: 0, End: math.MaxInt}

var hunkHeader = regexp.MustCompile(`^@@ -\d+(?:,\d+)? \+(\d+)(?:,(\d+))? @@`)

// parseHunks reads the output of `git diff --unified=0 --no-renames` into the
// lines it changed. A hunk that only deletes lines changed none, so the line
// before the gap and the one after it count as changed: a finding there is
// about the code around the edit.
func parseHunks(diff string) changedLines {
	out := changedLines{}
	file := ""
	// A removed line that starts "-- " prints as "--- ", so file headers are
	// only read between a diff's start and its first hunk.
	inHunk := false
	for _, line := range strings.Split(diff, "\n") {
		switch {
		case strings.HasPrefix(line, "diff --git "):
			inHunk, file = false, ""
		case inHunk && !strings.HasPrefix(line, "@@"):
		case strings.HasPrefix(line, "--- "):
			// The old name stands in for a deleted file, whose +++ is /dev/null.
			file = strings.TrimPrefix(strings.TrimPrefix(line, "--- "), "a/")
		case strings.HasPrefix(line, "+++ "):
			name := strings.TrimPrefix(line, "+++ ")
			if name == "/dev/null" {
				out[file] = append(out[file], wholeFile)
				file = ""
				continue
			}
			file = strings.TrimPrefix(name, "b/")
			if _, ok := out[file]; !ok {
				out[file] = nil
			}
		case file != "" && strings.HasPrefix(line, "@@"):
			inHunk = true
			m := hunkHeader.FindStringSubmatch(line)
			if m == nil {
				continue
			}
			start, _ := strconv.Atoi(m[1])
			count := 1
			if m[2] != "" {
				count, _ = strconv.Atoi(m[2])
			}
			if count == 0 {
				out[file] = append(out[file], span{Start: start, End: start + 1})
				continue
			}
			out[file] = append(out[file], span{Start: start, End: start + count - 1})
		}
	}
	return out
}

// touched reports whether the finding's line is in a changed span. A finding
// with no line is about the file as a whole, so any change to it counts.
func (c changedLines) touched(file string, line int) bool {
	spans, ok := c[file]
	if !ok {
		return false
	}
	if line <= 0 {
		return true
	}
	for _, s := range spans {
		if line >= s.Start && line <= s.End {
			return true
		}
	}
	return false
}

// matchesEarlier reports whether f is one of the findings in prior again: the
// same ID when the reviewer echoes one, else the same file and a line within
// matchWindow. Two findings with no line match when they share the file.
func matchesEarlier(f review.Finding, prior []review.Finding) bool {
	for _, p := range prior {
		if f.ID != "" && f.ID == p.ID {
			return true
		}
		if f.File != p.File {
			continue
		}
		if f.Line == 0 || p.Line == 0 {
			if f.Line == p.Line {
				return true
			}
			continue
		}
		if d := f.Line - p.Line; d >= -matchWindow && d <= matchWindow {
			return true
		}
	}
	return false
}

// guardInput is what the guard knows about one re-checking review's round.
type guardInput struct {
	Round int
	// Prior are the findings the review reported in earlier rounds.
	Prior []review.Finding
	// Changed is what changed since the tree the review last saw; nil when that
	// could not be established, in which case nothing is downgraded.
	Changed changedLines
}

// guard returns f, or f downgraded when it is a medium finding that neither
// matches an earlier one nor lies on lines changed since the review last
// looked. High findings are never downgraded: a real bug missed before should
// still send the run back.
func guard(f review.Finding, in guardInput) review.Finding {
	if f.Severity != review.SeverityMedium || in.Changed == nil {
		return f
	}
	if matchesEarlier(f, in.Prior) || in.Changed.touched(f.File, f.Line) {
		return f
	}
	return f.Downgrade(fmt.Sprintf("new in round %d on unchanged code", in.Round))
}
