package factory

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"gotest.tools/v3/assert"

	"github.com/smeltdev/smelt/internal/gitexec"
	"github.com/smeltdev/smelt/internal/iostream"
	"github.com/smeltdev/smelt/internal/review"
)

// fakeFiles is a FileSource of in-memory files.
type fakeFiles map[string]string

func (f fakeFiles) ReadFile(_ context.Context, p string) ([]byte, error) {
	c, ok := f[p]
	if !ok {
		return nil, fmt.Errorf("%s: %w", p, os.ErrNotExist)
	}
	return []byte(c), nil
}

// numbered is a file of n lines, "line 1" to "line n".
func numbered(n int) string {
	lines := make([]string, n)
	for i := range lines {
		lines[i] = fmt.Sprintf("line %d", i+1)
	}
	return strings.Join(lines, "\n") + "\n"
}

func TestValidatePack(t *testing.T) {
	files := fakeFiles{"a.go": numbered(10), "b.go": numbered(5), "empty.go": ""}
	tests := []struct {
		name        string
		entry       PackEntry
		wantExcerpt string
		wantDropped string
	}{
		{"edit is a pointer", PackEntry{Kind: PackEdit, Path: "a.go", Start: 2, End: 4, Reason: "r", Priority: 1}, "", ""},
		{"reference is inlined from the commit", PackEntry{Kind: PackReference, Path: "a.go", Start: 2, End: 3, Reason: "r", Priority: 1}, "line 2\nline 3", ""},
		{"whole file reference", PackEntry{Kind: PackTestPattern, Path: "b.go", Reason: "r", Priority: 2}, strings.TrimSuffix(numbered(5), "\n"), ""},
		{"missing file", PackEntry{Kind: PackEdit, Path: "gone.go", Start: 1, End: 2, Reason: "r", Priority: 1}, "", "gone.go:1-2 — no such file at the baseline commit"},
		{"range past the end", PackEntry{Kind: PackReference, Path: "a.go", Start: 900, End: 950, Reason: "r", Priority: 1}, "", "a.go:900-950 — beyond end of file (10 lines)"},
		{"backwards range", PackEntry{Kind: PackEdit, Path: "a.go", Start: 5, End: 2, Reason: "r", Priority: 1}, "", "a.go:5 — invalid range 5-2"},
		{"outside the repository", PackEntry{Kind: PackReference, Path: "../x", Reason: "r", Priority: 1}, "", "../x — not a path inside the repository"},
		{"unknown kind", PackEntry{Kind: "guess", Path: "a.go", Reason: "r", Priority: 1}, "", `a.go — unknown kind "guess"`},
		{"a new file must not exist", PackEntry{Kind: PackNewFile, Path: "a.go", Reason: "follows b.go", Priority: 1}, "", "a.go — the file already exists"},
		{"a new file", PackEntry{Kind: PackNewFile, Path: "c.go", Start: 3, End: 4, Reason: "follows b.go", Priority: 1}, "", ""},
		{"a rule", PackEntry{Kind: PackRule, Path: "AGENTS.md", Reason: "Use gitexec.Runner", Priority: 1}, "", ""},
		{"an empty rule", PackEntry{Kind: PackRule, Path: "AGENTS.md", Priority: 1}, "", "AGENTS.md — a rule with no text"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Whatever excerpt the model sent is ignored.
			tt.entry.Excerpt = "invented by the model"
			p := ValidatePack(context.Background(), []PackEntry{tt.entry}, files, 1000)
			if tt.wantDropped != "" {
				assert.Equal(t, len(p.Entries), 0)
				assert.Equal(t, len(p.Dropped), 1)
				assert.Equal(t, p.Dropped[0].String(), tt.wantDropped)
				return
			}
			assert.Equal(t, len(p.Dropped), 0)
			assert.Equal(t, len(p.Entries), 1)
			assert.Equal(t, p.Entries[0].Excerpt, tt.wantExcerpt)
		})
	}
}

func TestValidatePackEmpty(t *testing.T) {
	p := ValidatePack(context.Background(), nil, fakeFiles{}, 0)
	assert.Assert(t, p.Empty())
	assert.Equal(t, p.Tokens, 0)
	assert.Equal(t, p.Markdown(), "")
	assert.Equal(t, p.ReviewMarkdown(), "")
}

func TestValidatePackCapsTokensDemotingLeastImportantFirst(t *testing.T) {
	big := strings.Repeat("x", 400) // 100 tokens
	files := fakeFiles{"p1.go": big, "p2.go": big, "p3a.go": big, "p3b.go": big + big}
	entries := []PackEntry{
		{Kind: PackReference, Path: "p1.go", Reason: "r", Priority: 1},
		{Kind: PackReference, Path: "p3a.go", Reason: "r", Priority: 3},
		{Kind: PackReference, Path: "p2.go", Reason: "r", Priority: 2},
		{Kind: PackReference, Path: "p3b.go", Reason: "r", Priority: 3},
	}
	inlined := func(p Pack) []string {
		var out []string
		for _, e := range p.Entries {
			if e.Excerpt != "" {
				out = append(out, e.Path)
			}
		}
		sort.Strings(out)
		return out
	}
	all := ValidatePack(context.Background(), entries, files, 100000)
	assert.DeepEqual(t, inlined(all), []string{"p1.go", "p2.go", "p3a.go", "p3b.go"})

	// Dropping the largest priority 3 excerpt is enough.
	p := ValidatePack(context.Background(), entries, files, all.Tokens-100)
	assert.DeepEqual(t, inlined(p), []string{"p1.go", "p2.go", "p3a.go"})
	assert.Assert(t, p.Tokens <= all.Tokens-100)

	// Priority 3 goes before priority 2, and 2 before 1.
	p = ValidatePack(context.Background(), entries, files, 150)
	assert.DeepEqual(t, inlined(p), []string{"p1.go"})
	assert.Equal(t, len(p.Entries), 4, "demoted entries stay as pointers")
	assert.Assert(t, p.Entries[1].Pointer)
	assert.Assert(t, contains(p.Markdown(), "- `p3a.go` — r\n"), p.Markdown())
}

func TestContextHints(t *testing.T) {
	request := "Add a thing.\n\n## What\n\nstuff\n\n## Context\n\n- the dispatch is `handleResult` in `internal/ui/app.go`\n* [ ] reuse Client.PRList\n\nplain line\n\n## Acceptance\n\n- not a hint\n"
	assert.DeepEqual(t, ContextHints(request), []string{
		"the dispatch is `handleResult` in `internal/ui/app.go`", "reuse Client.PRList", "plain line"})
	assert.Equal(t, len(ContextHints("no section here")), 0)
}

func TestMissingHints(t *testing.T) {
	pack := Pack{
		Entries:       []PackEntry{{Kind: PackEdit, Path: "internal/ui/app.go", Reason: "dispatch"}, {Kind: PackReference, Path: "internal/gh/pr.go", Reason: "reuse Client.PRList"}},
		RejectedHints: []RejectedHint{{Hint: "use the old parser", Reason: "it is gone"}},
	}
	hints := []string{
		"the dispatch is `handleResult` in `internal/ui/app.go`", // names an entry's path
		"reuse Client.PRList",  // quoted by a reason
		"Use the old parser",   // rejected
		"keep the flag hidden", // nowhere
	}
	assert.DeepEqual(t, MissingHints(hints, pack), []string{"keep the flag hidden"})
	assert.DeepEqual(t, MissingHints(nil, pack), []string(nil))
}

func TestReanchor(t *testing.T) {
	oldFiles := fakeFiles{"a.go": "head\nfunc A() {\n\tbody\n}\ntail\n", "b.go": "one\ntwo\nthree\n", "c.go": "x\ny\n", "d.go": "p\nq\n"}
	curFiles := fakeFiles{
		"a.go": "new import\nnew import 2\nhead\nfunc A() {\n\tbody\n}\ntail\n", // moved down by 2
		"b.go": "one\nTWO\nthree\n",                                             // lines changed at the range start
		"c.go": "x\ny\n",                                                        // unchanged
		// d.go is gone
	}
	rec := Pack{
		Entries: []PackEntry{
			{Kind: PackReference, Path: "a.go", Start: 2, End: 4, Reason: "A", Priority: 1},
			{Kind: PackReference, Path: "b.go", Start: 2, End: 3, Reason: "B", Priority: 1},
			{Kind: PackEdit, Path: "c.go", Start: 1, End: 2, Reason: "C", Priority: 1},
			{Kind: PackEdit, Path: "d.go", Start: 1, End: 2, Reason: "D", Priority: 1},
		},
		RejectedHints: []RejectedHint{{Hint: "h", Reason: "r"}},
	}
	got := Reanchor(context.Background(), rec, oldFiles, curFiles, 1000)

	assert.Equal(t, len(got.Entries), 3)
	a := got.Entries[0]
	assert.Equal(t, a.Label(), "a.go:4-6")
	assert.Equal(t, a.Excerpt, "func A() {\n\tbody\n}")
	b := got.Entries[1]
	assert.Equal(t, b.Label(), "b.go", "the first line is gone: a pointer to the whole file")
	assert.Assert(t, b.Pointer && b.Excerpt == "")
	assert.Equal(t, got.Entries[2].Label(), "c.go:1-2")
	assert.Equal(t, len(got.Dropped), 1)
	assert.Equal(t, got.Dropped[0].Path, "d.go")
	assert.DeepEqual(t, got.RejectedHints, rec.RejectedHints)
}

func TestReanchorPicksTheNearestOccurrence(t *testing.T) {
	old := "x\nfoo\nbar\n" + strings.Repeat("filler\n", 20)
	cur := "foo\nbar\n" + strings.Repeat("filler\n", 20) + "x\nfoo\nbar\n"
	got := Reanchor(context.Background(), Pack{Entries: []PackEntry{{Kind: PackEdit, Path: "f.go", Start: 2, End: 3, Reason: "r", Priority: 1}}},
		fakeFiles{"f.go": old}, fakeFiles{"f.go": cur}, 1000)
	// "foo" is at line 2 of old; the nearest "foo" in cur is line 1.
	assert.Equal(t, got.Entries[0].Label(), "f.go:1-2")
}

func TestPackMarkdown(t *testing.T) {
	files := fakeFiles{"internal/ui/app.go": numbered(30), "internal/gh/pr.go": "func PRList() {}\n```\nfenced\n```\n"}
	p := ValidatePack(context.Background(), []PackEntry{
		{Kind: PackEdit, Path: "internal/ui/app.go", Start: 10, End: 20, Reason: "add the new result case here", Priority: 1},
		{Kind: PackNewFile, Path: "internal/ui/new.go", Reason: "follows internal/ui/app.go", Priority: 2},
		{Kind: PackReference, Path: "internal/gh/pr.go", Start: 1, End: 4, Reason: "reuse Client.PRList", Priority: 1},
		{Kind: PackRule, Path: "AGENTS.md", Reason: "Use gitexec.Runner for git", Priority: 1},
	}, files, 1000)

	const wantBody = "Edit targets:\n" +
		"- `internal/ui/app.go:10-20` — add the new result case here\n" +
		"- `internal/ui/new.go` (new file) — follows internal/ui/app.go\n\n" +
		"Reuse and follow:\n" +
		"- `internal/gh/pr.go:1-4` — reuse Client.PRList\n" +
		"````\nfunc PRList() {}\n```\nfenced\n```\n````\n\n" +
		"Rules:\n" +
		"- Use gitexec.Runner for git (AGENTS.md)"
	assert.Equal(t, p.Markdown(), "## Context pack\n\nThis is where the work is; read the edit targets before changing them. You may read anything else you need.\n\n"+wantBody)
	assert.Equal(t, p.ReviewMarkdown(), "## Context pack\n\n"+reviewPackIntro+"\n\n"+wantBody)
	assert.Assert(t, !contains(p.Markdown(), "line 10"), "an edit target is never inlined")
}

func TestPlanPreambleCarriesThePack(t *testing.T) {
	plan := Plan{Summary: "Do it."}.withPack(Pack{Entries: []PackEntry{
		{Kind: PackEdit, Path: "a.go", Start: 1, End: 5, Reason: "edit here", Priority: 1},
		{Kind: PackReference, Path: "b.go", Reason: "reuse", Priority: 1, Excerpt: "func B() {}"},
	}})
	got := planPreamble(plan, nil)
	const want = "Your plan, reviewed and approved:\n\nDo it.\n\n" +
		"## Context pack\n\nThis is where the work is; read the edit targets before changing them. You may read anything else you need.\n\n" +
		"Edit targets:\n- `a.go:1-5` — edit here\n\nReuse and follow:\n- `b.go` — reuse\n```\nfunc B() {}\n```\n\n" +
		"Implement it. If you must deviate, say where and why in your summary.\n\n---\n\n"
	assert.Equal(t, got, want)
	assert.Equal(t, planPreamble(Plan{Summary: "Do it."}, nil), "Your plan, reviewed and approved:\n\nDo it.\n\nImplement it. If you must deviate, say where and why in your summary.\n\n---\n\n")
}

func TestReviewersGetThePackAfterTheRequest(t *testing.T) {
	pk := Pack{Entries: []PackEntry{{Kind: PackReference, Path: "b.go", Reason: "reuse", Priority: 1, Excerpt: "func B() {}"}}}
	s := &Sidecars{Request: "Add a thing.\n", Prompts: []review.Prompt{{Name: "bugs", Body: "find bugs"}}}
	without, _ := s.reviewSet(1)
	assert.Equal(t, without[0].Body, scopePrompts("Add a thing.\n", s.Prompts, nil)[0].Body)

	s.Pack = &pk
	with, _ := s.reviewSet(1)
	body := with[0].Body
	assert.Assert(t, contains(body, "## Requested change\n\nAdd a thing.\n\n## Context pack\n\n"+reviewPackIntro), body)
	assert.Assert(t, contains(body, "```\nfunc B() {}\n```\n\n## Change under review"), body)
}

func TestContinuationPromptsCarryThePack(t *testing.T) {
	c := &Continuation{From: Record{Prompt: "Do it."}}
	assert.Assert(t, !contains(c.prompt(), "Context pack"))
	c.pack = "## Context pack\n\nstuff"
	assert.Assert(t, strings.HasSuffix(c.prompt(), "\n\n## Context pack\n\nstuff"), c.prompt())
	assert.Assert(t, strings.HasSuffix(c.emptyPrompt(), "\n\n## Context pack\n\nstuff"), c.emptyPrompt())
}

func TestHintFindings(t *testing.T) {
	request := "Do it.\n\n## Context\n\n- the dispatch is in internal/ui/app.go\n- keep the flag hidden\n"
	plan := Plan{Summary: "s"}.withPack(Pack{Entries: []PackEntry{{Kind: PackEdit, Path: "internal/ui/app.go", Priority: 1}}})
	found := hintFindings(request, plan)
	assert.Equal(t, len(found), 1)
	assert.Assert(t, found[0].WorthChanging())
	assert.Equal(t, found[0].File, planFile)
	assert.Assert(t, contains(found[0].Body, "keep the flag hidden"), found[0].Body)

	plan.RejectedHints = []RejectedHint{{Hint: "keep the flag hidden", Reason: "the request says show it"}}
	assert.Equal(t, len(hintFindings(request, plan)), 0)
	assert.Equal(t, len(hintFindings("no context section", Plan{Summary: "s"})), 0)
}

func TestLoopSendsAPlanWithAMissingHintBackOnce(t *testing.T) {
	request := "Add a flag.\n\n## Context\n\n- the dispatch is in internal/ui/app.go\n"
	planner := &hintPlanner{}
	steps := &fakeSteps{changes: []Change{change(1)}, checks: [][]Check{{pass}}}
	out, err := Loop{Attempts: 2, Planner: planner, CheckHints: true}.Run(context.Background(), steps, request)
	assert.NilError(t, err)
	assert.Equal(t, planner.calls, 2)
	assert.Assert(t, contains(planner.in[1].Findings[0].Body, "internal/ui/app.go"))
	assert.Equal(t, out.Plan.Summary, "plan 2")
	assert.Assert(t, contains(steps.prompts[0], "- `internal/ui/app.go` — dispatch"), steps.prompts[0])

	// Without the check, the hint is not enforced.
	planner = &hintPlanner{}
	_, err = Loop{Attempts: 2, Planner: planner}.Run(context.Background(), &fakeSteps{changes: []Change{change(1)}, checks: [][]Check{{pass}}}, request)
	assert.NilError(t, err)
	assert.Equal(t, planner.calls, 1)
}

// hintPlanner writes a plan without the pack entry for the hint at first, and
// with it once it is sent back.
type hintPlanner struct {
	calls int
	in    []PlanInput
}

func (h *hintPlanner) Plan(_ context.Context, in PlanInput) (Plan, error) {
	h.calls++
	h.in = append(h.in, in)
	p := Plan{Summary: fmt.Sprintf("plan %d", h.calls)}
	if h.calls > 1 {
		p = p.withPack(Pack{Entries: []PackEntry{{Kind: PackEdit, Path: "internal/ui/app.go", Reason: "dispatch", Priority: 1}}})
	}
	return p, nil
}

func (h *hintPlanner) ReviewPlan(context.Context, string, Plan) ([]review.Finding, error) {
	return nil, nil
}

func TestGitFilesReadsTheCommitNotTheWorkingTree(t *testing.T) {
	dir := t.TempDir()
	git := func(args ...string) string {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-c", "user.name=t", "-c", "user.email=t@example.com"}, args...)...)
		cmd.Dir = dir
		out, err := cmd.CombinedOutput()
		assert.NilError(t, err, string(out))
		return strings.TrimSpace(string(out))
	}
	git("init", "-q")
	assert.NilError(t, os.MkdirAll(filepath.Join(dir, "pkg"), 0o755))
	assert.NilError(t, os.WriteFile(filepath.Join(dir, "pkg", "a.go"), []byte(numbered(6)), 0o644))
	git("add", ".")
	git("commit", "-qm", "base")
	base := git("rev-parse", "HEAD")
	assert.NilError(t, os.WriteFile(filepath.Join(dir, "pkg", "a.go"), []byte("changed\n"), 0o644))

	files := GitFiles{Git: gitexec.Runner{Dir: dir}, Rev: base}
	p := ValidatePack(context.Background(), []PackEntry{
		{Kind: PackReference, Path: "pkg/a.go", Start: 2, End: 3, Reason: "r", Priority: 1},
		{Kind: PackEdit, Path: "pkg", Reason: "a directory is not a file", Priority: 1},
		{Kind: PackEdit, Path: "pkg/nope.go", Reason: "r", Priority: 1},
	}, files, 1000)
	assert.Equal(t, len(p.Entries), 1)
	assert.Equal(t, p.Entries[0].Excerpt, "line 2\nline 3")
	assert.Equal(t, len(p.Dropped), 2)
}

func TestPlanSchemaIsStrict(t *testing.T) {
	var schema map[string]any
	assert.NilError(t, json.Unmarshal([]byte(PlanSchema), &schema))
	var walk func(path string, node any)
	walk = func(path string, node any) {
		m, ok := node.(map[string]any)
		if !ok {
			return
		}
		if props, ok := m["properties"].(map[string]any); ok {
			assert.Equal(t, m["additionalProperties"], false, path)
			var names []string
			for k := range props {
				names = append(names, k)
			}
			var required []string
			for _, r := range m["required"].([]any) {
				required = append(required, r.(string))
			}
			sort.Strings(names)
			sort.Strings(required)
			assert.DeepEqual(t, required, names)
			for k, v := range props {
				walk(path+"."+k, v)
			}
		}
		if items, ok := m["items"]; ok {
			walk(path+"[]", items)
		}
	}
	walk("plan", schema)
	props := schema["properties"].(map[string]any)
	assert.Assert(t, props["pack"] != nil && props["rejected_hints"] != nil)
}

func TestContinuePackReanchorsAgainstTheBranch(t *testing.T) {
	dir := t.TempDir()
	git := func(args ...string) string {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-c", "user.name=t", "-c", "user.email=t@example.com"}, args...)...)
		cmd.Dir = dir
		out, err := cmd.CombinedOutput()
		assert.NilError(t, err, string(out))
		return strings.TrimSpace(string(out))
	}
	git("init", "-q")
	write := func(content string) {
		assert.NilError(t, os.WriteFile(filepath.Join(dir, "a.go"), []byte(content), 0o644))
		git("add", ".")
		git("commit", "-qm", "c")
	}
	write("one\ntwo\nthree\nfour\n")
	base := git("rev-parse", "HEAD")
	write("added\nadded too\none\ntwo\nthree\nfour\n") // the earlier run moved the lines down

	rec := Pack{Entries: []PackEntry{{Kind: PackReference, Path: "a.go", Start: 2, End: 3, Reason: "r", Priority: 1}}, Tokens: 3}
	from := Record{RunID: "run-1", Prompt: "Do it.", Baseline: base, Pack: &rec}
	var lines []string
	status := func(_ iostream.Level, msg string) { lines = append(lines, msg) }

	s := &Sidecars{}
	opts := RunOptions{Continue: &Continuation{From: from}, Pack: PackOptions{Enabled: true, MaxTokens: 1000}}
	var record Record
	c := s.continuePack(context.Background(), opts, Worktree{Path: dir}, &record, status)
	assert.Equal(t, s.Pack.Entries[0].Label(), "a.go:4-5")
	assert.Equal(t, s.Pack.Entries[0].Excerpt, "two\nthree")
	assert.Assert(t, contains(c.prompt(), "`a.go:4-5` — r\n```\ntwo\nthree\n```"), c.prompt())
	assert.Assert(t, record.Pack != nil && record.Pack.Entries[0].Start == 4)
	assert.Assert(t, contains(strings.Join(lines, "\n"), "pack: reference a.go:4-5"))
	assert.Equal(t, from.Pack.Entries[0].Start, 2, "the recorded pack is not changed")

	// With the pack off the recorded pack is ignored, and kept for later runs.
	s = &Sidecars{}
	opts.Pack.Enabled = false
	record = Record{}
	c = s.continuePack(context.Background(), opts, Worktree{Path: dir}, &record, status)
	assert.Assert(t, s.Pack == nil)
	assert.Assert(t, !contains(c.prompt(), "Context pack"))
	assert.Equal(t, record.Pack, from.Pack)
}
