package factory

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"gotest.tools/v3/assert"

	"github.com/smeltdev/smelt/internal/review"
)

func TestParseHunks(t *testing.T) {
	diff := "diff --git a/a.go b/a.go\n--- a/a.go\n+++ b/a.go\n@@ -3,0 +4,2 @@\n+x\n+y\n@@ -10 +12 @@\n-old\n+new\n" +
		"diff --git a/b.go b/b.go\n--- a/b.go\n+++ b/b.go\n@@ -5,2 +4,0 @@\n--- a removed line that starts like a header\n-two\n" +
		"diff --git a/gone.go b/gone.go\n--- a/gone.go\n+++ /dev/null\n@@ -1,3 +0,0 @@\n-a\n-b\n-c\n" +
		"diff --git a/new.go b/new.go\n--- /dev/null\n+++ b/new.go\n@@ -0,0 +1,3 @@\n+a\n+b\n+c\n"
	got := parseHunks(diff)
	assert.DeepEqual(t, got["a.go"], []span{{4, 5}, {12, 12}})
	assert.DeepEqual(t, got["b.go"], []span{{4, 5}})
	assert.DeepEqual(t, got["gone.go"], []span{wholeFile})
	assert.DeepEqual(t, got["new.go"], []span{{1, 3}})
	assert.Equal(t, len(got), 4)
	assert.Equal(t, len(parseHunks("")), 0)
}

func TestTouched(t *testing.T) {
	c := changedLines{"a.go": {{4, 5}, {12, 12}}, "gone.go": {wholeFile}}
	for _, tc := range []struct {
		file string
		line int
		want bool
	}{
		{"a.go", 4, true}, {"a.go", 5, true}, {"a.go", 12, true},
		{"a.go", 3, false}, {"a.go", 6, false}, {"a.go", 13, false},
		{"a.go", 0, true}, {"other.go", 4, false}, {"other.go", 0, false},
		{"gone.go", 999, true},
	} {
		assert.Equal(t, c.touched(tc.file, tc.line), tc.want, "%s:%d", tc.file, tc.line)
	}
}

func TestMatchesEarlier(t *testing.T) {
	prior := []review.Finding{{ID: "bugs-1", File: "a.go", Line: 10, Severity: "medium"}, {File: "b.go", Severity: "high"}}
	for name, tc := range map[string]struct {
		f    review.Finding
		want bool
	}{
		"same line":       {review.Finding{File: "a.go", Line: 10}, true},
		"edge of window":  {review.Finding{File: "a.go", Line: 10 + matchWindow}, true},
		"past the window": {review.Finding{File: "a.go", Line: 11 + matchWindow}, false},
		"before window":   {review.Finding{File: "a.go", Line: 10 - matchWindow - 1}, false},
		"other file":      {review.Finding{File: "c.go", Line: 10}, false},
		"echoed id":       {review.Finding{ID: "bugs-1", File: "z.go", Line: 400}, true},
		"file level pair": {review.Finding{File: "b.go"}, true},
		"line vs file":    {review.Finding{File: "b.go", Line: 3}, false},
		"file vs line":    {review.Finding{File: "a.go"}, false},
		"different no id": {review.Finding{ID: "bugs-2", File: "z.go", Line: 1}, false},
	} {
		assert.Equal(t, matchesEarlier(tc.f, prior), tc.want, name)
	}
	assert.Assert(t, !matchesEarlier(review.Finding{File: "a.go", Line: 10}, nil))
}

func TestGuard(t *testing.T) {
	prior := []review.Finding{{File: "a.go", Line: 10, Severity: "medium"}}
	changed := changedLines{"a.go": {{40, 42}}}
	in := guardInput{Round: 3, Prior: prior, Changed: changed}
	for name, tc := range map[string]struct {
		f         review.Finding
		in        guardInput
		downgrade bool
	}{
		"new medium on unchanged code": {review.Finding{File: "a.go", Line: 90, Severity: "medium"}, in, true},
		"new medium on changed lines":  {review.Finding{File: "a.go", Line: 41, Severity: "medium"}, in, false},
		"medium matching an earlier":   {review.Finding{File: "a.go", Line: 12, Severity: "medium"}, in, false},
		"new high on unchanged code":   {review.Finding{File: "a.go", Line: 90, Severity: "high"}, in, false},
		"low stays low":                {review.Finding{File: "a.go", Line: 90, Severity: "low"}, in, false},
		"new medium in untouched file": {review.Finding{File: "z.go", Line: 1, Severity: "medium"}, in, true},
		"changes unknown":              {review.Finding{File: "a.go", Line: 90, Severity: "medium"}, guardInput{Round: 3, Prior: prior}, false},
	} {
		got := guard(tc.f, tc.in)
		assert.Equal(t, got.Downgraded != "", tc.downgrade, name)
		if tc.downgrade {
			assert.Equal(t, got.Severity, review.SeverityLow, name)
			assert.Equal(t, got.Downgraded, "new in round 3 on unchanged code")
			assert.Equal(t, got.Body, tc.f.Body)
			assert.Assert(t, !got.WorthChanging())
		} else {
			assert.DeepEqual(t, got, tc.f)
		}
	}
}

// runIn runs a shell script in dir and returns its trimmed stdout.
func runIn(t *testing.T, dir, script string) string {
	t.Helper()
	cmd := exec.Command("sh", "-c", script)
	cmd.Dir = dir
	out, err := cmd.Output()
	assert.NilError(t, err, script)
	return strings.TrimSpace(string(out))
}

func TestChangedRangesBetweenTwoTreesOfARealRepo(t *testing.T) {
	root := setupRepo(t)
	numbered := func(n int) string {
		var b strings.Builder
		for i := 1; i <= n; i++ {
			fmt.Fprintf(&b, "line %d\n", i)
		}
		return b.String()
	}
	writeFile(t, root, "big.txt", numbered(30))
	gitOutput(t, root, "add", ".")
	gitOutput(t, root, "-c", "user.name=t", "-c", "user.email=t@example.com", "commit", "-q", "-m", "big")

	// The first tree: an edit, plus a new file git does not know yet.
	writeFile(t, root, "big.txt", strings.Replace(numbered(30), "line 5\n", "line five\n", 1))
	writeFile(t, root, "fresh.txt", "x\n")
	gitOutput(t, root, "add", "-A", "-N")
	indexBefore := gitOutput(t, root, "diff", "HEAD", "--name-status")
	first := runIn(t, root, treeCmd)
	assert.Equal(t, gitOutput(t, root, "diff", "HEAD", "--name-status"), indexBefore, "the real index is left alone")

	// The second: one more line edited, one deleted, a file removed, a new one.
	second := strings.Replace(numbered(30), "line 5\n", "line five\n", 1)
	second = strings.Replace(second, "line 20\n", "line twenty\n", 1)
	second = strings.Replace(second, "line 25\n", "", 1)
	writeFile(t, root, "big.txt", second)
	assert.NilError(t, os.Remove(filepath.Join(root, "main.go")))
	writeFile(t, root, "later.txt", "a\nb\n")
	secondTree := runIn(t, root, treeCmd)
	assert.Assert(t, first != secondTree)

	got := parseHunks(runIn(t, root, changedCmd(first, secondTree)))
	assert.DeepEqual(t, got["big.txt"], []span{{20, 20}, {24, 25}})
	assert.DeepEqual(t, got["main.go"], []span{wholeFile})
	assert.DeepEqual(t, got["later.txt"], []span{{1, 2}})
	_, edited := got["fresh.txt"]
	assert.Assert(t, !edited, "a file that did not change since the first tree is not changed")
	assert.Assert(t, !got.touched("big.txt", 5), "the first round's edit is not this round's")
	assert.Assert(t, got.touched("big.txt", 20))
}

// scriptedReviewer is a claude that answers every review with the findings in
// $HOME/findings.json, a JSON array.
const scriptedReviewer = `printf '{"type":"result","subtype":"success","is_error":false,"result":"","structured_output":{"review":"ok","findings":%s}}\n' "$(cat "$HOME/findings.json")"`

func findingsJSON(fs ...[3]any) string {
	parts := make([]string, len(fs))
	for i, f := range fs {
		parts[i] = fmt.Sprintf(`{"file":"big.txt","line":%d,"severity":%q,"body":%q,"patch":""}`, f[0], f[1], f[2])
	}
	return "[" + strings.Join(parts, ",") + "]"
}

// TestLaterRoundsRecheckAndDowngrade plays the shape of run 20261018-181908:
// round 3 raises new medium findings on code nothing touched since round 2.
func TestLaterRoundsRecheckAndDowngrade(t *testing.T) {
	for _, focused := range []bool{true, false} {
		t.Run(fmt.Sprintf("focused=%v", focused), func(t *testing.T) {
			ctx := context.Background()
			s, impl, _, home := newSidecarsFixtureWith(t, nil, func(s *Sidecars) {
				s.FocusedRechecks, s.GatesFirst = focused, true
			})
			installFakeClaude(t, home, scriptedReviewer)
			var mtime time.Time
			round := func(n int, edit func(string) string, found string) []Check {
				mtime = time.Now().Add(time.Duration(n) * time.Minute)
				name := filepath.Join(impl.RepoPath, "big.txt")
				cur, _ := os.ReadFile(name)
				assert.NilError(t, os.WriteFile(name, []byte(edit(string(cur))), 0o644))
				assert.NilError(t, os.Chtimes(name, mtime, mtime))
				assert.NilError(t, os.WriteFile(filepath.Join(home, "findings.json"), []byte(found), 0o644))
				_, err := s.Collect(ctx)
				assert.NilError(t, err)
				checks, err := s.Check(ctx, n)
				assert.NilError(t, err)
				s.answer("done")
				return checks
			}
			numbered := func(string) string {
				var b strings.Builder
				for i := 1; i <= 60; i++ {
					fmt.Fprintf(&b, "line %d\n", i)
				}
				return b.String()
			}
			statuses := func(cs []Check) map[string]Status {
				m := map[string]Status{}
				for _, c := range cs {
					m[c.Name] = c.Status
				}
				return m
			}

			c1 := round(1, numbered, findingsJSON([3]any{2, "medium", "first"}))
			assert.Equal(t, c1[0].Status, StatusFailed)
			assert.Equal(t, c1[0].Mode, ModeBroad)

			// The fix touches line 20 only; the reviewer repeats one finding,
			// flags the fixed code, and adds one far from the edit.
			c2 := round(2, func(s string) string { return strings.Replace(s, "line 20\n", "line twenty\n", 1) },
				findingsJSON([3]any{2, "medium", "first again"}, [3]any{20, "medium", "broke it"}, [3]any{50, "medium", "new, far away"}))
			if !focused {
				assert.Equal(t, c2[0].Status, StatusFailed)
				assert.Equal(t, c2[0].Mode, ModeBroad)
				assert.Equal(t, len(s.Downgraded()), 0)
				// Nothing is guarded, so round 3's new findings send the run back.
				c3 := round(3, func(s string) string { return s }, findingsJSON([3]any{50, "medium", "new, far away"}))
				assert.Equal(t, c3[0].Status, StatusFailed, "today's behaviour: exhausted")
				return
			}
			assert.Equal(t, c2[0].Mode, ModeRecheck)
			assert.Equal(t, c2[0].Status, StatusFailed)
			assert.Assert(t, !contains(c2[0].Feedback, "far away"), c2[0].Feedback)
			assert.Assert(t, contains(c2[0].Feedback, "first again") && contains(c2[0].Feedback, "broke it"), c2[0].Feedback)
			var low review.Finding
			for _, f := range c2[0].Findings {
				if f.Downgraded != "" {
					low = f
				}
			}
			assert.Equal(t, low.Body, "new, far away")
			assert.Equal(t, low.Severity, review.SeverityLow)

			// Round 3 changes nothing; the reviewers raise two new mediums.
			c3 := round(3, func(s string) string { return s },
				findingsJSON([3]any{40, "medium", "new one"}, [3]any{55, "medium", "new two"}))
			for _, c := range c3[:2] {
				assert.Equal(t, c.Mode, ModeRecheck)
				assert.Equal(t, c.Status, StatusPassed, c.Name)
				assert.Equal(t, len(c.Findings), 2)
			}
			assert.Assert(t, len(s.Downgraded()) >= 3, "%v", s.Downgraded())
			assert.Assert(t, contains(s.Downgraded()[0], "big.txt:50: new, far away (downgraded: new in round 2 on unchanged code)"), s.Downgraded()[0])

			// Both passed, so round 4 runs no review and says so.
			c4 := round(4, func(s string) string { return s }, findingsJSON([3]any{1, "high", "never read"}))
			assert.DeepEqual(t, statuses(c4), map[string]Status{"bugs": StatusPassed, "style": StatusPassed})
			for _, c := range c4 {
				assert.Equal(t, c.Mode, ModeCarried)
				assert.Equal(t, len(c.Findings), 0)
			}
		})
	}
}
