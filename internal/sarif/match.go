package sarif

import (
	"cmp"
	"fmt"
	"slices"
	"strings"
)

// LineSource gives the text of one line of a file, 1-based. ok is false when
// the file or line is not available.
type LineSource interface {
	Line(file string, line int) (text string, ok bool)
}

// Annotate sets Text on each finding to the trimmed text of its flagged line,
// from src, else from the tool's snippet.
func Annotate(fs []Finding, src LineSource) {
	for i := range fs {
		f := &fs[i]
		f.Text = strings.TrimSpace(strings.SplitN(f.Snippet, "\n", 2)[0])
		if f.File == "" || f.Line == 0 || src == nil {
			continue
		}
		if t, ok := src.Line(f.File, f.Line); ok {
			f.Text = strings.TrimSpace(t)
		}
	}
}

// Counts is how many findings a change added, removed and left as they were.
type Counts struct {
	New      int `json:"new"`
	Fixed    int `json:"fixed"`
	Existing int `json:"existing"`
}

// Diff is the comparison of a change's findings with the baseline's.
type Diff struct {
	// New are the change's findings with no match in the baseline.
	New []Finding
	// Fixed are the baseline's findings with no match in the change.
	Fixed []Finding
	// Existing are the change's findings that match one in the baseline.
	Existing []Finding
}

// Counts is the size of each part of the diff.
func (d Diff) Counts() Counts {
	return Counts{New: len(d.New), Fixed: len(d.Fixed), Existing: len(d.Existing)}
}

// Summary is the one-line result, such as "lint: 0 new, 3 fixed, 214 existing".
func (d Diff) Summary(name string) string {
	c := d.Counts()
	return fmt.Sprintf("%s: %d new, %d fixed, %d existing", name, c.New, c.Fixed, c.Existing)
}

// Feedback lists the new findings, one per line, as "file:line rule message".
func (d Diff) Feedback() string {
	fs := slices.Clone(d.New)
	sortFindings(fs)
	lines := make([]string, len(fs))
	for i, f := range fs {
		lines[i] = f.String()
	}
	return strings.Join(lines, "\n")
}

// String renders the finding as "file:line rule message".
func (f Finding) String() string {
	var parts []string
	switch {
	case f.File != "" && f.Line > 0:
		parts = append(parts, fmt.Sprintf("%s:%d", f.File, f.Line))
	case f.File != "":
		parts = append(parts, f.File)
	}
	if f.RuleID != "" {
		parts = append(parts, f.RuleID)
	}
	parts = append(parts, f.Message)
	return strings.Join(parts, " ")
}

func sortFindings(fs []Finding) {
	slices.SortStableFunc(fs, func(a, b Finding) int {
		return cmp.Or(cmp.Compare(a.File, b.File), cmp.Compare(a.Line, b.Line))
	})
}

// Compare matches the change's findings against the baseline's. A finding with
// fingerprints in both runs matches the baseline finding that shares one.
// Every other finding matches by rule, file and the text of its flagged line,
// so a finding that only moved still matches; when several share those, the
// n-th in line order matches the n-th. Findings must be annotated.
func Compare(base, change []Finding) Diff {
	base, change = slices.Clone(base), slices.Clone(change)
	sortFindings(base)
	sortFindings(change)

	byPrint := map[string][]int{}
	byKey := map[string][]int{}
	for i, f := range base {
		if len(f.Fingerprints) > 0 {
			for _, p := range f.Fingerprints {
				byPrint[p] = append(byPrint[p], i)
			}
			continue
		}
		k := textKey(f)
		byKey[k] = append(byKey[k], i)
	}
	used := make([]bool, len(base))
	claim := func(idx []int) bool {
		for _, i := range idx {
			if !used[i] {
				used[i] = true
				return true
			}
		}
		return false
	}

	var d Diff
	for _, f := range change {
		matched := false
		if len(f.Fingerprints) > 0 {
			for _, p := range f.Fingerprints {
				if matched = claim(byPrint[p]); matched {
					break
				}
			}
		} else {
			matched = claim(byKey[textKey(f)])
		}
		if matched {
			d.Existing = append(d.Existing, f)
		} else {
			d.New = append(d.New, f)
		}
	}
	for i, f := range base {
		if !used[i] {
			d.Fixed = append(d.Fixed, f)
		}
	}
	return d
}

func textKey(f Finding) string {
	return strings.Join([]string{f.RuleID, f.File, f.Text}, "\x00")
}
