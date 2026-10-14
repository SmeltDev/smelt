package factory

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"unicode/utf8"

	"gotest.tools/v3/assert"

	"github.com/smeltdev/smelt/internal/config"
	"github.com/smeltdev/smelt/internal/harness"
	"github.com/smeltdev/smelt/internal/harness/claude"
	"github.com/smeltdev/smelt/internal/harness/codex"
	"github.com/smeltdev/smelt/internal/iostream"
	"github.com/smeltdev/smelt/internal/review"
	"github.com/smeltdev/smelt/internal/sidecar"
)

func TestBootstrapPrompt(t *testing.T) {
	cmds := []config.Command{{Name: "test", Run: "go test ./..."}, {Name: "lint", Run: "golangci-lint run"}}
	tests := []struct {
		name      string
		commands  []config.Command
		standards string
		has, not  []string
	}{
		{
			name: "neither", has: []string{"setting up code review", "Write 2 to 4 prompts"},
			not: []string{"validation commands also run", "<standards>"},
		},
		{
			name: "commands", commands: cmds,
			has: []string{"validation commands also run", "  - test: `go test ./...`", "  - lint: `golangci-lint run`"},
			not: []string{"<standards>"},
		},
		{
			name: "standards", standards: " Prefer small functions. \n",
			has: []string{"<standards>\nPrefer small functions.\n</standards>", StandardsFile},
			not: []string{"validation commands also run"},
		},
		{name: "blank standards are none", standards: " \n", not: []string{"<standards>"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := bootstrapPrompt(tt.commands, tt.standards)
			for _, s := range tt.has {
				assert.Assert(t, strings.Contains(got, s), "missing %q in:\n%s", s, got)
			}
			for _, s := range tt.not {
				assert.Assert(t, !strings.Contains(got, s), "unexpected %q in:\n%s", s, got)
			}
		})
	}
}

func TestBootstrapPromptCapsStandardsOnACharacterBoundary(t *testing.T) {
	// Each é is two bytes, so a cap at an odd offset falls inside one.
	standards := "x" + strings.Repeat("é", maxStandardsBytes)
	got := bootstrapPrompt(nil, standards)
	assert.Assert(t, utf8.ValidString(got))
	start := strings.Index(got, "<standards>\n") + len("<standards>\n")
	end := strings.Index(got, "\n</standards>")
	assert.Equal(t, end-start, maxStandardsBytes-1, "cut back to the boundary before the cap")
}

func TestCapUTF8(t *testing.T) {
	assert.Equal(t, capUTF8("abc", 10), "abc")
	assert.Equal(t, capUTF8("abc", 2), "ab")
	assert.Equal(t, capUTF8("aé", 2), "a", "é does not fit in one byte")
	assert.Equal(t, capUTF8("aé", 3), "aé")
}

func TestCheckBootstrap(t *testing.T) {
	good := func(name string) review.Prompt { return review.Prompt{Name: name, Body: "Review " + name} }
	many := make([]review.Prompt, 8)
	for i := range many {
		many[i] = good("area-" + string(rune('a'+i)))
	}
	tests := []struct {
		name        string
		in          []review.Prompt
		wantNames   []string
		wantDropped int
		wantErr     string
	}{
		{name: "good", in: []review.Prompt{good("testing"), good("core-logic")}, wantNames: []string{"testing", "core-logic"}},
		{name: "bad names", in: []review.Prompt{good("Testing"), good("../evil"), good("a_b"), good("-a"), good("a--b"), good("")}, wantErr: "no usable"},
		{name: "bad name beside good", in: []review.Prompt{good("testing"), good("Bad Name")}, wantNames: []string{"testing"}, wantDropped: 1},
		{name: "repeat keeps the first", in: []review.Prompt{good("testing"), {Name: "testing", Body: "second"}}, wantNames: []string{"testing"}, wantDropped: 1},
		{name: "empty body", in: []review.Prompt{good("a"), {Name: "b", Body: " \n"}}, wantNames: []string{"a"}, wantDropped: 1},
		{name: "oversized body", in: []review.Prompt{good("a"), {Name: "b", Body: strings.Repeat("x", maxBootstrapPromptBytes+1)}}, wantNames: []string{"a"}, wantDropped: 1},
		{name: "body at the cap", in: []review.Prompt{{Name: "a", Body: strings.Repeat("x", maxBootstrapPromptBytes)}}, wantNames: []string{"a"}},
		{name: "more than six", in: many, wantNames: []string{"area-a", "area-b", "area-c", "area-d", "area-e", "area-f"}, wantDropped: 2},
		{name: "none", wantErr: "no usable review prompts"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := checkBootstrap(bootstrapAnswer{Summary: " learned things ", Reviews: tt.in})
			if tt.wantErr != "" {
				assert.ErrorContains(t, err, tt.wantErr)
				return
			}
			assert.NilError(t, err)
			var names []string
			for _, p := range got.Prompts {
				names = append(names, p.Name)
			}
			assert.DeepEqual(t, names, tt.wantNames)
			assert.Equal(t, got.Dropped, tt.wantDropped)
			assert.Equal(t, got.Summary, "learned things")
		})
	}
}

func TestBootstrapSchema(t *testing.T) {
	type node struct {
		Type                 string          `json:"type"`
		Properties           map[string]node `json:"properties"`
		Items                *node           `json:"items"`
		Required             []string        `json:"required"`
		AdditionalProperties *bool           `json:"additionalProperties"`
		MinItems             *int            `json:"minItems"`
		MaxItems             *int            `json:"maxItems"`
	}
	var schema node
	assert.NilError(t, json.Unmarshal([]byte(BootstrapSchema), &schema))
	var check func(path string, n node)
	check = func(path string, n node) {
		assert.Assert(t, n.MinItems == nil && n.MaxItems == nil, "%s sets an item count, which strict mode refuses", path)
		if n.Type == "object" {
			var props []string
			for k := range n.Properties {
				props = append(props, k)
			}
			slices.Sort(props)
			req := slices.Sorted(slices.Values(n.Required))
			assert.DeepEqual(t, req, props)
			assert.Assert(t, n.AdditionalProperties != nil && !*n.AdditionalProperties, "%s allows extra properties", path)
		}
		for k, p := range n.Properties {
			check(path+"."+k, p)
		}
		if n.Items != nil {
			check(path+"[]", *n.Items)
		}
	}
	check("answer", schema)
	assert.Equal(t, schema.Properties["reviews"].Items.Properties["name"].Type, "string")
}

func TestCheckPromptsDir(t *testing.T) {
	write := func(t *testing.T, dir, name string) {
		t.Helper()
		assert.NilError(t, os.MkdirAll(dir, 0o755))
		assert.NilError(t, os.WriteFile(filepath.Join(dir, name), []byte("x"), 0o644))
	}
	t.Run("missing", func(t *testing.T) {
		assert.NilError(t, CheckPromptsDir(filepath.Join(t.TempDir(), "nope")))
	})
	t.Run("empty", func(t *testing.T) {
		assert.NilError(t, CheckPromptsDir(t.TempDir()))
	})
	t.Run("other files", func(t *testing.T) {
		dir := t.TempDir()
		write(t, dir, "notes.json")
		assert.NilError(t, os.Mkdir(filepath.Join(dir, "sub.md"), 0o755))
		assert.NilError(t, CheckPromptsDir(dir))
	})
	t.Run("a prompt", func(t *testing.T) {
		dir := t.TempDir()
		write(t, dir, "testing.md")
		assert.Assert(t, errors.Is(CheckPromptsDir(dir), ErrPromptsExist))
	})
	t.Run("a txt prompt", func(t *testing.T) {
		dir := t.TempDir()
		write(t, dir, "testing.txt")
		assert.Assert(t, errors.Is(CheckPromptsDir(dir), ErrPromptsExist))
	})
}

func TestWritePrompts(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "reviews")
	paths, err := WritePrompts(dir, []review.Prompt{{Name: "a", Body: "body a"}, {Name: "b", Body: "body b"}})
	assert.NilError(t, err)
	assert.DeepEqual(t, paths, []string{filepath.Join(dir, "a.md"), filepath.Join(dir, "b.md")})
	got, err := os.ReadFile(paths[0])
	assert.NilError(t, err)
	assert.Equal(t, string(got), "body a\n")
}

// A file that appears after CheckPromptsDir is not overwritten: that prompt
// fails, loudly, and the others are still written.
func TestWritePromptsNeverOverwrites(t *testing.T) {
	dir := t.TempDir()
	assert.NilError(t, CheckPromptsDir(dir))
	mine := filepath.Join(dir, "a.md")
	assert.NilError(t, os.WriteFile(mine, []byte("mine"), 0o644))

	paths, err := WritePrompts(dir, []review.Prompt{{Name: "a", Body: "theirs"}, {Name: "b", Body: "other"}})
	assert.ErrorContains(t, err, "a.md")
	assert.Assert(t, errors.Is(err, os.ErrExist))
	assert.DeepEqual(t, paths, []string{filepath.Join(dir, "b.md")})
	got, _ := os.ReadFile(mine)
	assert.Equal(t, string(got), "mine")
}

// Fixtures below are written by hand to the shape of each CLI's output, not
// captured from a run.
const (
	claudeAnswer = `{"summary":"A Go CLI.","reviews":[{"name":"testing","body":"# Testing\nUse real git."},{"name":"Bad Name","body":"x"}]}`

	claudeStream = `{"type":"system","subtype":"init","session_id":"s1"}
{"type":"assistant","message":{"content":[{"type":"text","text":"Reading the docs."},{"type":"tool_use","name":"Read","input":{"file_path":"/repo/AGENTS.md"}}]}}
{"type":"user","message":{"content":[{"type":"tool_result","content":"big file"}]}}
{"type":"assistant","message":{"content":[{"type":"tool_use","name":"Grep","input":{"pattern":"func New"}}]}}
{"type":"result","subtype":"success","is_error":false,"result":"done","total_cost_usd":0.12,"structured_output":` + claudeAnswer + `}
`

	codexEvents = `{"type":"thread.started","thread_id":"t1"}
{"type":"item.completed","item":{"type":"reasoning","text":"thinking"}}
{"type":"item.completed","item":{"type":"command_execution","command":"git log --oneline","aggregated_output":"","exit_code":0}}
{"type":"item.completed","item":{"type":"agent_message","text":"done"}}
{"type":"turn.completed","usage":{"input_tokens":10,"cached_input_tokens":0,"output_tokens":5}}
`
)

func TestParseAnswerClaude(t *testing.T) {
	h := claude.Harness{}
	t.Run("stream-json takes the result line", func(t *testing.T) {
		got, err := parseAnswer[bootstrapAnswer](h, claudeStream, "review prompts")
		assert.NilError(t, err)
		assert.Equal(t, len(got.Reviews), 2)
	})
	t.Run("plain json result", func(t *testing.T) {
		out := `{"type":"result","is_error":false,"structured_output":` + claudeAnswer + `}`
		got, err := parseAnswer[bootstrapAnswer](h, out, "review prompts")
		assert.NilError(t, err)
		assert.Equal(t, got.Summary, "A Go CLI.")
	})
	t.Run("an error result quotes at most 500 characters", func(t *testing.T) {
		out := `{"type":"result","subtype":"error_max_turns","is_error":true,"result":"` + strings.Repeat("é", 2000) + `"}`
		_, err := parseAnswer[bootstrapAnswer](h, out, "review prompts")
		assert.ErrorContains(t, err, "error_max_turns")
		assert.Assert(t, utf8.RuneCountInString(err.Error()) < 600, "%d", utf8.RuneCountInString(err.Error()))
	})
	t.Run("no structured output", func(t *testing.T) {
		_, err := parseAnswer[bootstrapAnswer](h, `{"type":"result","subtype":"success","result":"prose"}`, "review prompts")
		assert.ErrorContains(t, err, "no structured review prompts")
	})
}

func TestParseAnswerCodex(t *testing.T) {
	got, err := parseAnswer[bootstrapAnswer](codex.Harness{}, "\n"+claudeAnswer+"\n", "review prompts")
	assert.NilError(t, err)
	assert.Equal(t, got.Summary, "A Go CLI.")
	_, err = parseAnswer[bootstrapAnswer](codex.Harness{}, "not json", "review prompts")
	assert.ErrorContains(t, err, "read the review prompts")
}

// scripted runs the sidecar's script by writing fixed streams to the callback.
type scripted struct {
	stdout, stderr string
	code           int
	script         string
	envs           []map[string]string
}

func (s *scripted) exec(_ context.Context, _ *sidecar.PoolEntry, script string, env map[string]string, onOutput iostream.OutputFn, _ func(string)) (int, error) {
	s.envs = append(s.envs, env)
	if !strings.Contains(script, "-schema") {
		return 0, nil // a harness's setup script
	}
	s.script = script
	if s.stderr != "" {
		onOutput(iostream.StreamStderr, []byte(s.stderr))
	}
	if s.stdout != "" {
		onOutput(iostream.StreamStdout, []byte(s.stdout))
	}
	return s.code, nil
}

func bootstrapOpts(h harness.Harness, ex *scripted, tools *[]string) BootstrapOptions {
	return BootstrapOptions{
		Entry: &sidecar.PoolEntry{ID: "sc", RepoPath: "/home/circleci/repo"}, Exec: ex.exec, Root: "/nonexistent",
		Harness: h, Credential: harness.Credential{EnvVar: "ANTHROPIC_API_KEY", Value: "k"},
		Commands: []config.Command{{Name: "test", Run: "go test ./..."}, {Name: "fmt", Run: "gofmt -w .", Role: config.RoleAutofix}},
		OnTool:   func(a harness.Activity) { *tools = append(*tools, a.Tool+" "+a.Detail) },
	}
}

func TestRunBootstrapClaude(t *testing.T) {
	ex := &scripted{stdout: claudeStream}
	var tools []string
	got, err := RunBootstrap(context.Background(), bootstrapOpts(claude.Harness{}, ex, &tools))
	assert.NilError(t, err)
	assert.DeepEqual(t, tools, []string{"Read /repo/AGENTS.md", "Grep func New"})
	assert.Equal(t, got.Summary, "A Go CLI.")
	assert.Equal(t, len(got.Prompts), 1)
	assert.Equal(t, got.Dropped, 1)
	assert.Equal(t, got.CostUSD, 0.12)
	assert.Assert(t, strings.Contains(ex.script, "stream-json"), ex.script)
	assert.Assert(t, strings.Contains(ex.script, "Bash(git show:*)"), ex.script)
	prompt := promptOf(t, ex.script)
	assert.Assert(t, strings.Contains(prompt, "`go test ./...`"))
	assert.Assert(t, !strings.Contains(prompt, "gofmt"), "an autofix command is not a check")
}

func TestRunBootstrapCodex(t *testing.T) {
	ex := &scripted{stdout: claudeAnswer, stderr: codexEvents}
	var tools []string
	got, err := RunBootstrap(context.Background(), bootstrapOpts(codex.Harness{}, ex, &tools))
	assert.NilError(t, err)
	assert.DeepEqual(t, tools, []string{"Bash git log --oneline"})
	assert.Equal(t, len(got.Prompts), 1)
	assert.Equal(t, got.CostUSD, 0.0)
	assert.Assert(t, len(ex.envs) == 2, "the harness is set up once, then run: %d scripts", len(ex.envs))
}

func TestRunBootstrapAgentFailures(t *testing.T) {
	var tools []string
	ex := &scripted{stdout: `{"type":"result","is_error":true,"subtype":"error_during_execution","result":"it broke"}`}
	_, err := RunBootstrap(context.Background(), bootstrapOpts(claude.Harness{}, ex, &tools))
	assert.ErrorContains(t, err, "it broke")

	ex = &scripted{code: harness.MissingExitCode}
	_, err = RunBootstrap(context.Background(), bootstrapOpts(claude.Harness{}, ex, &tools))
	assert.Assert(t, errors.Is(err, claude.ErrMissing))

	ex = &scripted{stdout: `{"type":"result","is_error":false,"structured_output":{"summary":"s","reviews":[]}}`}
	_, err = RunBootstrap(context.Background(), bootstrapOpts(claude.Harness{}, ex, &tools))
	assert.ErrorContains(t, err, "no usable review prompts")
}

func TestRunBootstrapReadsTheTeamStandards(t *testing.T) {
	root := t.TempDir()
	assert.NilError(t, os.MkdirAll(filepath.Join(root, filepath.Dir(StandardsFile)), 0o755))
	assert.NilError(t, os.WriteFile(filepath.Join(root, StandardsFile), []byte("Always wrap errors."), 0o644))
	ex := &scripted{stdout: claudeStream}
	var tools []string
	o := bootstrapOpts(claude.Harness{}, ex, &tools)
	o.Root = root
	_, err := RunBootstrap(context.Background(), o)
	assert.NilError(t, err)
	assert.Assert(t, strings.Contains(promptOf(t, ex.script), "Always wrap errors."))
}
