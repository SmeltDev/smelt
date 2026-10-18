package sarif

import (
	"testing"

	"gotest.tools/v3/assert"
)

type lines map[string][]string

func (l lines) Line(file string, n int) (string, bool) {
	ls := l[file]
	if n < 1 || n > len(ls) {
		return "", false
	}
	return ls[n-1], true
}

func f(rule, file string, line int, text string) Finding {
	return Finding{RuleID: rule, File: file, Line: line, Message: rule + " msg", Text: text}
}

func TestCompare(t *testing.T) {
	withPrint := func(x Finding, p string) Finding { x.Fingerprints = []string{p}; return x }
	for name, tc := range map[string]struct {
		base, change []Finding
		want         Counts
	}{
		"moved down three lines": {
			[]Finding{f("r", "a.go", 4, "x := 1")}, []Finding{f("r", "a.go", 7, "x := 1")}, Counts{0, 0, 1},
		},
		"same rule and text twice stay distinct": {
			[]Finding{f("r", "a.go", 4, "x"), f("r", "a.go", 9, "x")},
			[]Finding{f("r", "a.go", 4, "x"), f("r", "a.go", 9, "x"), f("r", "a.go", 12, "x")}, Counts{1, 0, 2},
		},
		"one of two identical removed": {
			[]Finding{f("r", "a.go", 4, "x"), f("r", "a.go", 9, "x")}, []Finding{f("r", "a.go", 9, "x")}, Counts{0, 1, 1},
		},
		"fingerprints match across text and line": {
			[]Finding{withPrint(f("r", "a.go", 4, "old"), "h=1")},
			[]Finding{withPrint(f("r", "b.go", 40, "new"), "h=1")}, Counts{0, 0, 1},
		},
		"different fingerprint is new": {
			[]Finding{withPrint(f("r", "a.go", 4, "x"), "h=1")},
			[]Finding{withPrint(f("r", "a.go", 4, "x"), "h=2")}, Counts{1, 1, 0},
		},
		"changed line text is new": {
			[]Finding{f("r", "a.go", 4, "x := 1")}, []Finding{f("r", "a.go", 4, "x := 2")}, Counts{1, 1, 0},
		},
		"different rule is new": {
			[]Finding{f("r", "a.go", 4, "x")}, []Finding{f("s", "a.go", 4, "x")}, Counts{1, 1, 0},
		},
		"removals only": {
			[]Finding{f("r", "a.go", 1, "a"), f("r", "a.go", 2, "b"), f("r", "a.go", 3, "c")}, nil, Counts{0, 3, 0},
		},
	} {
		t.Run(name, func(t *testing.T) {
			assert.Equal(t, Compare(tc.base, tc.change).Counts(), tc.want)
		})
	}
}

func TestAnnotate(t *testing.T) {
	fs := []Finding{{File: "a.go", Line: 2}, {File: "gone.go", Line: 1, Snippet: "  snip \nmore"}, {Line: 0}}
	Annotate(fs, lines{"a.go": {"one", "\t two  "}})
	assert.Equal(t, fs[0].Text, "two")
	assert.Equal(t, fs[1].Text, "snip")
	assert.Equal(t, fs[2].Text, "")
}

func TestDiffRendering(t *testing.T) {
	d := Compare([]Finding{f("r", "a.go", 1, "gone")},
		[]Finding{f("z", "b.go", 9, "n"), {RuleID: "q", Message: "repo-wide"}, f("r", "a.go", 3, "m")})
	assert.Equal(t, d.Summary("lint"), "lint: 3 new, 1 fixed, 0 existing")
	assert.Equal(t, d.Feedback(), "q repo-wide\na.go:3 r r msg\nb.go:9 z z msg")
}
