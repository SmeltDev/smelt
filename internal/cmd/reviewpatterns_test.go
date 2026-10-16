package cmd

import (
	"errors"
	"strings"
	"testing"

	"gotest.tools/v3/assert"
	"gotest.tools/v3/golden"
)

func TestReviewPatternsFiltersByLanguageAndArea(t *testing.T) {
	stdout, _, err := runValidateResults(t, "review", "patterns", "--language", "python", "--area", "correctness")
	assert.NilError(t, err)

	for _, name := range []string{"silent-fallback", "retry-hides-failure", "error-cause-dropped", "lenient-parse", "ignored-error"} {
		assert.Assert(t, strings.Contains(stdout, "### `"+name+"`"), "%s missing:\n%s", name, stdout)
	}
	assert.Assert(t, strings.Contains(stdout, "raise ConfigError"), stdout)
	for _, other := range []string{"(go)", "(typescript)", "(rust)", "os.WriteFile", "fmt.Errorf", ".catch(", "unwrap_or"} {
		assert.Assert(t, !strings.Contains(stdout, other), "%q is from another language:\n%s", other, stdout)
	}
	assert.Assert(t, !strings.Contains(stdout, "lonely-interface"), "other areas are left out")
}

func TestReviewPatternsTable(t *testing.T) {
	tests := []struct {
		name    string
		args    []string
		want    []string
		notWant []string
	}{
		{name: "two languages", args: []string{"--area", "testing", "--language", "go", "--language", "rust"},
			want: []string{"(go)", "(rust)"}, notWant: []string{"(python)", "(typescript)"}},
		{name: "comma separated", args: []string{"--area", "testing", "--language", "go,python"},
			want: []string{"(go)", "(python)"}, notWant: []string{"(rust)"}},
		{name: "alias of requirements", args: []string{"--area", "adversarial", "--language", "go"},
			want: []string{"## requirements", "stub-remnant"}, notWant: []string{"silent-fallback"}},
		{name: "language without examples gets definitions", args: []string{"--area", "correctness", "--language", "java"},
			want: []string{"silent-fallback", "Code that turns a failure into a plausible default"}, notWant: []string{"- Is (", "- Isn't ("}},
		{name: "language without interfaces", args: []string{"--area", "maintainability", "--language", "lua"},
			want: []string{"trivial-wrapper"}, notWant: []string{"lonely-interface"}},
		{name: "every area", want: []string{"## requirements", "## correctness", "## maintainability", "## testing"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			stdout, _, err := runValidateResults(t, append([]string{"review", "patterns"}, tt.args...)...)
			assert.NilError(t, err)
			for _, w := range tt.want {
				assert.Assert(t, strings.Contains(stdout, w), "missing %q:\n%s", w, stdout)
			}
			for _, w := range tt.notWant {
				assert.Assert(t, !strings.Contains(stdout, w), "unexpected %q:\n%s", w, stdout)
			}
		})
	}
}

func TestReviewPatternsJSON(t *testing.T) {
	stdout, _, err := runValidateResults(t, "review", "patterns", "--json", "--language", "python", "--area", "correctness")
	assert.NilError(t, err)
	golden.Assert(t, stdout, "review-patterns-python-correctness.golden.json")
}

func TestReviewPatternsBadFlags(t *testing.T) {
	for _, args := range [][]string{
		{"--area", "style"},
		{"--language", "Py thon"},
		{"extra"},
	} {
		_, _, err := runValidateResults(t, append([]string{"review", "patterns"}, args...)...)
		assert.Assert(t, err != nil, "%v", args)
	}
	_, _, err := runValidateResults(t, "review", "patterns", "--area", "style")
	var ue *userError
	assert.Assert(t, errors.As(err, &ue), "%v", err)
	assert.Equal(t, ue.exitCode, ExitBadArgs)
	assert.Assert(t, strings.Contains(ue.UserMessage(), "requirements, correctness, maintainability, testing"), ue.UserMessage())
}
