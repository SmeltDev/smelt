package patterns

import (
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"

	"gotest.tools/v3/assert"
	"gotest.tools/v3/golden"

	"github.com/smeltdev/smelt/internal/quality/lizard"
)

var kebab = regexp.MustCompile(`^[a-z0-9]+(-[a-z0-9]+)*$`)

func loadAll(t *testing.T) []Pattern {
	t.Helper()
	all, err := Load()
	assert.NilError(t, err)
	return all
}

func names(ps []Pattern) []string {
	var out []string
	for _, p := range ps {
		out = append(out, p.Name)
	}
	return out
}

func TestCatalogueIsComplete(t *testing.T) {
	known := lizard.LanguageNames()
	seen := map[string]bool{}
	counts := map[Area]int{}
	for _, p := range loadAll(t) {
		t.Run(p.Name, func(t *testing.T) {
			assert.Assert(t, kebab.MatchString(p.Name), "name %q is not kebab-case", p.Name)
			assert.Assert(t, !seen[p.Name], "duplicate name %q", p.Name)
			seen[p.Name] = true
			counts[p.Area]++
			assert.Assert(t, slices.Contains(Areas(), p.Area))
			assert.Assert(t, strings.TrimSpace(p.Definition) != "")
			assert.Assert(t, slices.Contains([]string{"high", "medium", "low", "info"}, p.Severity), "severity %q", p.Severity)
			assert.Assert(t, len(p.Examples) >= 2, "needs examples in at least two languages")
			for lang, ex := range p.Examples {
				assert.Assert(t, slices.Contains(known, lang), "example language %q is not a lizard name", lang)
				assert.Assert(t, strings.TrimSpace(ex.Is) != "" && strings.TrimSpace(ex.Isnt) != "", "%s example is incomplete", lang)
			}
			for _, lang := range p.AppliesTo {
				assert.Assert(t, lang == AppliesToAll || slices.Contains(known, lang), "applies_to %q", lang)
			}
			if slices.Contains(p.AppliesTo, AppliesToAll) {
				assert.Equal(t, len(p.AppliesTo), 1, "all cannot be combined with languages")
			}
			for lang := range p.Examples {
				assert.Assert(t, appliesTo(p, []string{lang}), "has an example for %s, to which it does not apply", lang)
			}
			for _, lang := range []string{"go", "python", "typescript", "rust"} {
				if appliesTo(p, []string{lang}) {
					assert.Assert(t, p.Examples[lang].Is != "", "no %s example", lang)
				}
			}
		})
	}
	assert.DeepEqual(t, counts, map[Area]int{Requirements: 3, Correctness: 5, Maintainability: 8, Testing: 4})
}

func TestParseArea(t *testing.T) {
	for in, want := range map[string]Area{
		"correctness": Correctness, "Testing": Testing, "adversarial": Requirements, "requirements": Requirements,
	} {
		got, err := ParseArea(in)
		assert.NilError(t, err)
		assert.Equal(t, got, want)
	}
	_, err := ParseArea("style")
	assert.ErrorContains(t, err, "requirements, correctness, maintainability, testing")
}

func TestSelect(t *testing.T) {
	all := loadAll(t)
	tests := []struct {
		name      string
		area      Area
		languages []string
		want      []string
		hasNone   []string // languages whose examples must be absent
	}{
		{name: "one area, python", area: Correctness, languages: []string{"python"},
			want:    []string{"silent-fallback", "retry-hides-failure", "error-cause-dropped", "lenient-parse", "ignored-error"},
			hasNone: []string{"go", "typescript", "rust"}},
		{name: "two languages", area: Testing, languages: []string{"Go", "rust", "go"},
			want: []string{"fake-only-test", "silent-skip", "assertion-free-test", "real-timeout-in-test"}, hasNone: []string{"python", "typescript"}},
		{name: "language without interfaces", area: Maintainability, languages: []string{"lua"},
			want: []string{"single-case-dispatch", "trivial-wrapper", "growing-dispatch", "narration-comment", "echo-doc-comment", "stringly-state", "bool-for-error"}},
		{name: "language with interfaces but no examples", area: Maintainability, languages: []string{"java"},
			want: names(Select(all, Maintainability, nil))},
		{name: "no languages keeps every example", area: Requirements,
			want: []string{"unrequested-feature", "excluded-case-handling", "stub-remnant"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := Select(all, tt.area, tt.languages)
			assert.DeepEqual(t, names(got), tt.want)
			for _, p := range got {
				for _, lang := range tt.hasNone {
					_, ok := p.Examples[lang]
					assert.Assert(t, !ok, "%s has a %s example", p.Name, lang)
				}
			}
		})
	}
}

func TestSelectFallsBackToTheDefinitionAlone(t *testing.T) {
	got := Select(loadAll(t), Correctness, []string{"java"})
	assert.Equal(t, len(got), 5)
	for _, p := range got {
		assert.Equal(t, len(p.Examples), 0, p.Name)
		assert.Assert(t, p.Definition != "")
	}
}

func TestSelectDoesNotModifyTheCatalogue(t *testing.T) {
	all := loadAll(t)
	_ = Select(all, Correctness, []string{"python"})
	assert.Equal(t, len(all[3].Examples), 4)
}

func TestSelectNothingIsAnEmptyList(t *testing.T) {
	got := Select(loadAll(t), Correctness, []string{"lua", "go"})
	assert.Equal(t, len(got), 5)
	assert.Assert(t, Select(nil, Testing, nil) != nil)
}

func TestSelectAllAreas(t *testing.T) {
	assert.Equal(t, len(Select(loadAll(t), "", nil)), 20)
}

func TestMarkdownHasOnlyTheSelectedLanguages(t *testing.T) {
	md := Markdown(Select(loadAll(t), Testing, []string{"python"}))
	assert.Assert(t, strings.Contains(md, "### `fake-only-test`\nA test that checks a fake"))
	assert.Assert(t, strings.Contains(md, "- Is (python): "))
	assert.Assert(t, !strings.Contains(md, "(go)"))
	assert.Assert(t, strings.Contains(md, "- Usual severity: medium"))
}

func TestJSONContract(t *testing.T) {
	for name, sel := range map[string][]Pattern{
		"python-correctness":   Select(loadAll(t), Correctness, []string{"python"}),
		"java-maintainability": Select(loadAll(t), Maintainability, []string{"java"}),
	} {
		t.Run(name, func(t *testing.T) {
			got, err := json.MarshalIndent(Document{Version: Version, Patterns: sel}, "", "  ")
			assert.NilError(t, err)
			golden.Assert(t, string(got)+"\n", name+".golden.json")
		})
	}
}

// namedRef is a pattern name in brackets, as the prompts tell reviewers to
// write it in a finding.
var namedRef = regexp.MustCompile(`\[([a-z0-9]+(?:-[a-z0-9]+)+)\]`)

var heading = regexp.MustCompile("^### `([^`]+)`$")

func TestOwnReviewPromptsMatchTheCatalogue(t *testing.T) {
	byName := map[string]Pattern{}
	for _, p := range loadAll(t) {
		byName[p.Name] = p
	}
	files, err := filepath.Glob("../../../.smelt/reviews/*.md")
	assert.NilError(t, err)
	assert.Assert(t, len(files) > 0)
	for _, f := range files {
		raw, err := os.ReadFile(f)
		assert.NilError(t, err)
		lines := strings.Split(string(raw), "\n")
		for i, line := range lines {
			if m := heading.FindStringSubmatch(line); m != nil {
				p, ok := byName[m[1]]
				assert.Assert(t, ok, "%s: %q is not in the catalogue", f, m[1])
				assert.Assert(t, i+1 < len(lines))
				assert.Equal(t, lines[i+1], p.Definition, "%s: definition of %s", f, m[1])
			}
			for _, m := range namedRef.FindAllStringSubmatch(line, -1) {
				_, ok := byName[m[1]]
				assert.Assert(t, ok, "%s: [%s] is not in the catalogue", f, m[1])
			}
		}
	}
}
