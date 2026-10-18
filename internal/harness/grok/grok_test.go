package grok

import (
	"context"
	"encoding/base64"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"gotest.tools/v3/assert"

	"github.com/smeltdev/smelt/internal/harness"
)

func fixture(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", "real", name))
	assert.NilError(t, err)
	return string(b)
}

func TestParserReadsARealPass(t *testing.T) {
	var acts []harness.Activity
	p := Harness{}.NewImplementParser(func(a harness.Activity) { acts = append(acts, a) })
	p.Write([]byte(fixture(t, "grok-implement-pass.jsonl")))
	p.Flush()
	out := p.Outcome()

	assert.NilError(t, out.Err)
	assert.Assert(t, out.Done)
	assert.Assert(t, !out.Failed)
	assert.Equal(t, out.Turn.Summary, "hello")
	assert.Equal(t, out.Turn.CostUSD, 0.03476)
	assert.Equal(t, out.Turn.SessionID, "01a150aa-70a3-7570-bff1-d0cffdcf32ab")
	assert.Equal(t, out.Turn.InputTokens, 11060+23936)
	assert.Equal(t, out.Turn.CachedTokens, 23936)
	assert.Equal(t, out.Turn.OutputTokens, 112)
	assert.Equal(t, out.Turn.ModelCalls, 2)
	// The larger of the two calls: 140 + 17408.
	assert.Equal(t, out.Turn.ContextPeak, 17548)
	assert.DeepEqual(t, acts, []harness.Activity{{Tool: "Read", Detail: "/tmp/repo/README.md"}, {Detail: "hello"}})
}

func TestParserReportsAFailedTurn(t *testing.T) {
	p := Harness{}.NewImplementParser(nil)
	p.Write([]byte(`{"type":"result","subtype":"error","is_error":true,"result":"boom"}` + "\n"))
	p.Flush()
	out := p.Outcome()
	assert.Assert(t, out.Done)
	assert.Assert(t, out.Failed)
	assert.Equal(t, out.Message, "boom")
}

func TestParserMapsToolNames(t *testing.T) {
	tests := map[string]string{
		`{"name":"search_replace","input":{"file_path":"a.go"}}`:   "Edit",
		`{"name":"run_terminal_command","input":{"command":"ls"}}`: "Bash",
		`{"name":"grep","input":{"pattern":"x"}}`:                  "Grep",
		`{"name":"list_dir","input":{"target_directory":"/r"}}`:    "LS",
		`{"name":"kill_command_or_subagent","input":{"id":"1"}}`:   "kill_command_or_subagent",
	}
	for tool, want := range tests {
		var acts []harness.Activity
		p := Harness{}.NewImplementParser(func(a harness.Activity) { acts = append(acts, a) })
		p.Write([]byte(`{"type":"assistant","message":{"id":"m","content":[{"type":"tool_use",` + tool[1:] + `]}}` + "\n"))
		assert.Equal(t, len(acts), 1, tool)
		assert.Equal(t, acts[0].Tool, want, tool)
	}
}

func TestParseReview(t *testing.T) {
	p, err := Harness{}.ParseReview(fixture(t, "grok-review-structured.json"))
	assert.NilError(t, err)
	assert.Assert(t, strings.HasPrefix(p.Review, "README.md is a single line"), p.Review)
	assert.Equal(t, string(p.Findings), `[{"file":"README.md","severity":"medium"}]`)

	_, err = Harness{}.ParseReview(fixture(t, "grok-review-no-structured.json"))
	assert.ErrorContains(t, err, `no structured findings (stop reason "max_tokens")`)
	_, err = Harness{}.ParseReview("not json")
	assert.ErrorContains(t, err, "read grok's result")
}

func TestReviewContext(t *testing.T) {
	// The real review made two calls, so no single call's size is known.
	assert.Equal(t, Harness{}.ReviewContext(fixture(t, "grok-review-structured.json")), harness.Context{})
	assert.Equal(t, Harness{}.ReviewContext(fixture(t, "grok-review-no-structured.json")), harness.Context{Peak: 10, Calls: 1})
	assert.Equal(t, Harness{}.ReviewContext("nope"), harness.Context{})
}

func TestImplementScript(t *testing.T) {
	tests := []struct {
		name string
		in   harness.ImplementInput
		want []string
		not  []string
	}{
		{
			name: "new session with a model",
			in:   harness.ImplementInput{RepoPath: "/repo", Prompt: "do it", SessionID: "sid", Model: "grok-4.7"},
			want: []string{
				"grok --output-format streaming-messages-json --always-approve --max-turns 200 -m grok-4.7 --session-id sid --prompt-file \"$pf\"",
				"cd /repo && ",
				"rm -f \"$pf\"",
			},
			not: []string{"--resume"},
		},
		{
			name: "resumed session",
			in:   harness.ImplementInput{RepoPath: "/repo", Prompt: "more", SessionID: "sid", Resume: true},
			want: []string{"--resume sid --prompt-file"},
			not:  []string{"--session-id", " -m "},
		},
		{
			name: "no session",
			in:   harness.ImplementInput{RepoPath: "/repo", Prompt: "x"},
			not:  []string{"--session-id", "--resume"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := strings.ReplaceAll(Harness{}.ImplementScript(tt.in), "'", "")
			for _, w := range tt.want {
				assert.Assert(t, strings.Contains(got, w), "missing %q in:\n%s", w, got)
			}
			for _, n := range tt.not {
				assert.Assert(t, !strings.Contains(got, n), "unexpected %q in:\n%s", n, got)
			}
			assert.Assert(t, !strings.Contains(got, tt.in.Prompt+"\n"), "the prompt travels encoded")
			assert.Assert(t, strings.Contains(got, guardDir+":"), "the git guard leads PATH")
		})
	}
}

func encodedPrompt(t *testing.T, script string) string {
	t.Helper()
	for _, l := range strings.Split(script, "\n") {
		if rest, ok := strings.CutPrefix(l, "echo "); ok && strings.HasSuffix(rest, `| base64 -d > "$pf"`) {
			b, err := base64.StdEncoding.DecodeString(strings.TrimSpace(strings.TrimSuffix(rest, `| base64 -d > "$pf"`)))
			assert.NilError(t, err)
			return string(b)
		}
	}
	t.Fatalf("no prompt line in:\n%s", script)
	return ""
}

func TestImplementScriptPrependsTheRules(t *testing.T) {
	got := Harness{}.ImplementScript(harness.ImplementInput{RepoPath: "/r", Prompt: "build", SystemPrompt: "be careful; 'quoted'"})
	assert.Equal(t, encodedPrompt(t, got), "## Rules\n\nbe careful; 'quoted'\n\n## Task\n\nbuild")
	plain := Harness{}.ImplementScript(harness.ImplementInput{RepoPath: "/r", Prompt: "build"})
	assert.Equal(t, encodedPrompt(t, plain), "build")
}

func TestReviewScriptIsReadOnly(t *testing.T) {
	got := strings.ReplaceAll(Harness{}.ReviewScript(harness.ReviewInput{RepoPath: "/r", Prompt: "look", Schema: `{"type":"object"}`, Model: "m", Tools: []string{"Bash", "Edit"}}), "'", "")
	assert.Assert(t, strings.Contains(got, "--tools read_file,grep,list_dir"), got)
	assert.Assert(t, !strings.Contains(got, "Bash"), "ReviewInput.Tools is ignored: %s", got)
	assert.Assert(t, strings.Contains(got, `--json-schema {"type":"object"}`), got)
	assert.Assert(t, strings.Contains(got, "-m m "), got)
	assert.Equal(t, encodedPrompt(t, got), "look")

	in := harness.ReviewInput{RepoPath: "/r", Prompt: "look"}
	plain := Harness{}.ReviewScript(in)
	in.Stream = true
	assert.Equal(t, Harness{}.ReviewScript(in), plain)
}

// runReview runs the generated review script in a git repo, with a fake grok
// on PATH that prints a valid answer and then runs body in the repo.
func runReview(t *testing.T, setup, body string) (code int, stdout, stderr string) {
	t.Helper()
	home := t.TempDir()
	repo := t.TempDir()
	bin := filepath.Join(home, ".local", "bin")
	assert.NilError(t, os.MkdirAll(bin, 0o755))
	fake := "#!/bin/sh\necho '{\"structuredOutput\":{\"review\":\"fine\",\"findings\":[]}}'\n" + body + "\n"
	assert.NilError(t, os.WriteFile(filepath.Join(bin, "grok"), []byte(fake), 0o755))
	git := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = repo
		cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
		out, err := cmd.CombinedOutput()
		assert.NilError(t, err, string(out))
	}
	git("init", "-q")
	assert.NilError(t, os.WriteFile(filepath.Join(repo, "tracked.txt"), []byte("one\n"), 0o644))
	git("add", ".")
	git("commit", "-q", "-m", "base")
	if setup != "" {
		cmd := exec.Command("sh", "-c", setup)
		cmd.Dir = repo
		out, err := cmd.CombinedOutput()
		assert.NilError(t, err, string(out))
	}

	script := Harness{}.ReviewScript(harness.ReviewInput{RepoPath: repo, Prompt: "look", Schema: `{}`})
	var so, se strings.Builder
	cmd := exec.Command("sh", "-c", script)
	cmd.Env = append(os.Environ(), "HOME="+home)
	cmd.Stdout, cmd.Stderr = &so, &se
	err := cmd.Run()
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		return ee.ExitCode(), so.String(), se.String()
	}
	assert.NilError(t, err)
	return 0, so.String(), se.String()
}

func TestReviewScriptGuardsTheWorkingTree(t *testing.T) {
	tests := []struct {
		name, setup, body string
		wantFail          bool
	}{
		{name: "tree left alone", body: "true"},
		{name: "tracked file modified", body: "echo two > tracked.txt", wantFail: true},
		{name: "untracked file created", body: "echo x > scratch.txt", wantFail: true},
		{name: "tracked file deleted", body: "rm tracked.txt", wantFail: true},
		{name: "pre-dirty tree, unchanged", setup: "echo dirty > tracked.txt; echo u > untracked.txt", body: "true"},
		{name: "pre-dirty tree, changed further", setup: "echo dirty > tracked.txt", body: "echo more >> tracked.txt", wantFail: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			code, stdout, stderr := runReview(t, tt.setup, tt.body)
			if tt.wantFail {
				assert.Assert(t, code != 0, "stdout=%s stderr=%s", stdout, stderr)
				assert.Assert(t, strings.Contains(stderr, "review changed files"), stderr)
				assert.Equal(t, strings.TrimSpace(stdout), "")
				return
			}
			assert.Equal(t, code, 0, stderr)
			p, err := Harness{}.ParseReview(stdout)
			assert.NilError(t, err)
			assert.Equal(t, p.Review, "fine")
		})
	}
}

func TestReviewScriptFailsWhenGrokFails(t *testing.T) {
	code, stdout, _ := runReview(t, "", "exit 3")
	assert.Equal(t, code, 3)
	assert.Equal(t, strings.TrimSpace(stdout), "", "a failed run's output is dropped")
}

// Prepare's script runs on the sidecar; here it runs in a temp dir by pointing
// the fixed paths at it.
func TestPrepareWritesTheAuthFileAndGuard(t *testing.T) {
	auth := `{"https://auth.x.ai::u":{"key":"jwt","refresh_token":""}}`
	cred := harness.Credential{EnvVar: EnvAuthJSON, Value: base64.StdEncoding.EncodeToString([]byte(auth))}
	script, env := Harness{}.Prepare(cred)
	assert.Equal(t, env[EnvAuthJSON], cred.Value)
	assert.Assert(t, !strings.Contains(script, cred.Value), "the secret is in the environment, not the script")
	turnEnv := Harness{}.Env(cred, "")
	_, leaked := turnEnv[EnvAuthJSON]
	assert.Assert(t, !leaked, "later turns do not get the login")
	assert.Equal(t, len(turnEnv), 0)

	dir := t.TempDir()
	home := filepath.Join(dir, "home")
	assert.NilError(t, os.MkdirAll(home, 0o755))
	script = strings.ReplaceAll(script, guardDir, dir+"/guard")
	cmd := exec.Command("sh", "-c", script)
	cmd.Env = append(os.Environ(), EnvAuthJSON+"="+cred.Value, "HOME="+home)
	out, err := cmd.CombinedOutput()
	assert.NilError(t, err, string(out))

	file := filepath.Join(home, ".grok", "auth.json")
	info, err := os.Stat(file)
	assert.NilError(t, err)
	assert.Equal(t, info.Mode().Perm(), os.FileMode(0o600))
	dirInfo, err := os.Stat(filepath.Dir(file))
	assert.NilError(t, err)
	assert.Equal(t, dirInfo.Mode().Perm(), os.FileMode(0o700))
	got, err := os.ReadFile(file)
	assert.NilError(t, err)
	assert.Equal(t, string(got), auth)
	_, err = os.Stat(filepath.Join(dir, "guard", "git"))
	assert.NilError(t, err)
}

func TestPrepareForAnAPIKeyWritesNoAuthFile(t *testing.T) {
	cred := harness.Credential{EnvVar: EnvAPIKey, Value: "xai-secret"}
	script, env := Harness{}.Prepare(cred)
	assert.Equal(t, len(env), 0)
	assert.Assert(t, !strings.Contains(script, "auth.json"), script)
	assert.Assert(t, !strings.Contains(script, cred.Value), script)
	assert.DeepEqual(t, Harness{}.Env(cred, ""), map[string]string{EnvAPIKey: "xai-secret"})
}

func TestGitGuard(t *testing.T) {
	dir := t.TempDir()
	guard := filepath.Join(dir, "guard")
	realBin := filepath.Join(dir, "real")
	assert.NilError(t, os.MkdirAll(guard, 0o755))
	assert.NilError(t, os.MkdirAll(realBin, 0o755))
	script := harness.GitGuardScript(guard)
	assert.NilError(t, os.WriteFile(filepath.Join(guard, "git"), []byte(script), 0o755))
	assert.NilError(t, os.WriteFile(filepath.Join(realBin, "git"), []byte("#!/bin/sh\necho real git \"$@\"\n"), 0o755))

	run := func(args ...string) (string, error) {
		cmd := exec.Command(filepath.Join(guard, "git"), args...)
		cmd.Env = []string{"PATH=" + guard + ":" + realBin + ":/usr/bin:/bin"}
		out, err := cmd.CombinedOutput()
		return string(out), err
	}
	for _, sub := range harness.GuardedGit {
		out, err := run(sub, "-m", "x")
		assert.Assert(t, err != nil, "%s should be refused", sub)
		assert.Assert(t, strings.Contains(out, "not allowed"), out)
	}
	out, err := run("status", "--short")
	assert.NilError(t, err, out)
	assert.Equal(t, strings.TrimSpace(out), "real git status --short")
}

func TestRunError(t *testing.T) {
	h := Harness{}
	ctx := context.Background()
	assert.NilError(t, h.RunError(ctx, time.Minute, 0, nil, "", ""))
	assert.ErrorIs(t, h.RunError(ctx, time.Minute, harness.MissingExitCode, nil, "", ""), ErrMissing)
	assert.ErrorIs(t, h.RunError(ctx, time.Minute, 1, nil, "", "401 Unauthorized"), ErrCredentialRejected)
	assert.ErrorIs(t, h.RunError(ctx, time.Minute, 1, nil, "Not logged in", ""), ErrCredentialRejected)
	assert.ErrorContains(t, h.RunError(ctx, time.Minute, 2, nil, "", "boom"), "grok exited 2: boom")
	assert.Error(t, h.RunError(ctx, time.Minute, 2, nil, "", ""), "grok exited 2")
	assert.NilError(t, h.RunError(ctx, time.Minute, 0, nil, "", "401 Unauthorized"))
	assert.ErrorContains(t, h.RunError(ctx, time.Minute, 1, errors.New("pipe"), "", ""), "exec: pipe")
}

func TestRunErrorDeadline(t *testing.T) {
	ctx, cancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer cancel()
	tests := []struct {
		name    string
		timeout time.Duration
		want    string
	}{
		{"with a run timeout", 30 * time.Second, "timed out after 30s"},
		{"without one", 0, "timed out: context deadline exceeded"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Error(t, Harness{}.RunError(ctx, tt.timeout, -1, nil, "", ""), tt.want)
		})
	}
}

func TestIdentity(t *testing.T) {
	assert.Equal(t, Harness{}.Name(), harness.Grok)
	assert.Equal(t, Harness{}.Binary(), "grok")
}

func TestParseAnswer(t *testing.T) {
	raw, err := Harness{}.ParseAnswer(fixture(t, "grok-review-structured.json"))
	assert.NilError(t, err)
	assert.Assert(t, strings.Contains(string(raw), `"findings"`), string(raw))
	assert.Assert(t, !strings.Contains(string(raw), "stopReason"), string(raw))
	_, err = Harness{}.ParseAnswer(fixture(t, "grok-review-no-structured.json"))
	assert.Assert(t, err != nil)
	_, err = Harness{}.ParseAnswer("not json")
	assert.Assert(t, err != nil)
}
