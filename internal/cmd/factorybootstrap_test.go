package cmd

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"gotest.tools/v3/assert"

	"github.com/smeltdev/smelt/internal/config"
	"github.com/smeltdev/smelt/internal/harness"
	"github.com/smeltdev/smelt/internal/harness/claude"
	"github.com/smeltdev/smelt/internal/iostream"
	"github.com/smeltdev/smelt/internal/review"
	"github.com/smeltdev/smelt/internal/sidecar"
)

// bootstrapBackend is a sidecar backend that answers every script with a fixed
// stream, and records what was created and deleted.
type bootstrapBackend struct {
	mu       sync.Mutex
	stdout   string
	images   []string
	created  []string
	deleted  []string
	scripts  []string
	createFn func() error
}

func (b *bootstrapBackend) Create(_ context.Context, name, image, _ string) (sidecar.Instance, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.createFn != nil {
		if err := b.createFn(); err != nil {
			return sidecar.Instance{}, err
		}
	}
	b.created = append(b.created, name)
	b.images = append(b.images, image)
	return sidecar.Instance{ID: "sc-" + name}, nil
}

func (b *bootstrapBackend) Delete(_ context.Context, id string) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.deleted = append(b.deleted, id)
	return nil
}

func (*bootstrapBackend) Snapshot(context.Context, string, string) (string, error) { return "", nil }
func (*bootstrapBackend) IsStale(context.Context, string) bool                     { return false }
func (*bootstrapBackend) Sync(context.Context, string, string, string, bool, iostream.StatusFunc) error {
	return nil
}

func (*bootstrapBackend) Pull(context.Context, string, string, string, iostream.StatusFunc) error {
	return nil
}

func (b *bootstrapBackend) Exec(_ context.Context, _, script string, _ map[string]string, onOutput iostream.OutputFn, _ func(string)) (int, error) {
	b.mu.Lock()
	b.scripts = append(b.scripts, script)
	b.mu.Unlock()
	onOutput(iostream.StreamStdout, []byte(b.stdout))
	return 0, nil
}

// bootstrapAgent is Claude with a script of its own, so a test can tell which
// harness ran.
type bootstrapAgent struct{ claude.Harness }

func (bootstrapAgent) ReviewScript(harness.ReviewInput) string { return "fake-review-script" }

const bootstrapStream = `{"type":"assistant","message":{"content":[{"type":"tool_use","name":"Read","input":{"file_path":"AGENTS.md"}}]}}
{"type":"result","subtype":"success","is_error":false,"total_cost_usd":0.5,"structured_output":{"summary":"A Go CLI.","reviews":[{"name":"testing","body":"# Testing"},{"name":"core-logic","body":"# Core"},{"name":"Bad","body":"x"}]}}
`

func bootstrapRepo(t *testing.T) string {
	t.Helper()
	dir := initGitRepo(t)
	c := exec.Command("git", "remote", "add", "origin", "https://github.com/example/proj.git")
	c.Dir = dir
	out, err := c.CombinedOutput()
	assert.NilError(t, err, string(out))
	return dir
}

func bootstrapTestConfig() *config.ProjectConfig {
	return &config.ProjectConfig{
		Commands: []config.Command{{Name: "test", Run: "go test ./..."}},
		Factory:  &config.FactoryConfig{Harness: "claude", Image: "proj-image:1"},
	}
}

func bootstrapTestEnv(be *bootstrapBackend) bootstrapEnv {
	return bootstrapEnv{
		harness: func(harness.Name) (harness.Harness, error) { return bootstrapAgent{}, nil },
		credential: func(context.Context, harness.Name) (agentCredential, error) {
			return agentCredential{Credential: harness.Credential{EnvVar: "ANTHROPIC_API_KEY", Value: "k"}}, nil
		},
		target: func(_ context.Context, s factorySettings) (sandboxTarget, error) {
			return sandboxTarget{Backend: be, Image: s.Image}, nil
		},
	}
}

func bootstrapReq(root string) bootstrapRequest {
	return bootstrapRequest{Root: root, Cfg: bootstrapTestConfig(), Timeout: time.Minute}
}

func TestBootstrapRefusesExistingPromptsBeforeAnySidecar(t *testing.T) {
	root := bootstrapRepo(t)
	dir := filepath.Join(root, review.DefaultDir)
	assert.NilError(t, os.MkdirAll(dir, 0o755))
	assert.NilError(t, os.WriteFile(filepath.Join(dir, "mine.md"), []byte("mine"), 0o644))

	be := &bootstrapBackend{stdout: bootstrapStream}
	streams, _, _ := testStreams()
	err := runBootstrap(context.Background(), streams, bootstrapReq(root), bootstrapTestEnv(be))

	var ue *userError
	assert.Assert(t, errors.As(err, &ue), "%v", err)
	assert.Equal(t, ue.Suggestion(), "Pass --output to write elsewhere and compare.")
	assert.Equal(t, len(be.created), 0, "no sidecar is created")
	assert.Equal(t, len(be.scripts), 0)
}

func TestBootstrapRefusesExistingPromptsInOutput(t *testing.T) {
	root := bootstrapRepo(t)
	out := t.TempDir()
	assert.NilError(t, os.WriteFile(filepath.Join(out, "x.md"), []byte("x"), 0o644))
	be := &bootstrapBackend{stdout: bootstrapStream}
	streams, _, _ := testStreams()
	req := bootstrapReq(root)
	req.Output = out
	err := runBootstrap(context.Background(), streams, req, bootstrapTestEnv(be))
	var ue *userError
	assert.Assert(t, errors.As(err, &ue), "%v", err)
	assert.Assert(t, strings.Contains(ue.UserMessage(), "already has review prompts"), ue.UserMessage())
	assert.Equal(t, len(be.created), 0)
}

func TestBootstrapWritesPromptsToTheDefaultDirectory(t *testing.T) {
	root := bootstrapRepo(t)
	be := &bootstrapBackend{stdout: bootstrapStream}
	streams, out, errOut := testStreams()

	err := runBootstrap(context.Background(), streams, bootstrapReq(root), bootstrapTestEnv(be))
	assert.NilError(t, err)

	got, err := os.ReadFile(filepath.Join(root, review.DefaultDir, "testing.md"))
	assert.NilError(t, err)
	assert.Equal(t, string(got), "# Testing\n")
	assert.DeepEqual(t, be.images, []string{"proj-image:1"})
	assert.Equal(t, len(be.created), 1, "one sidecar")
	assert.Equal(t, len(be.deleted), 1, "and it is not kept")
	assert.Assert(t, strings.Contains(be.scripts[0], "fake-review-script"), "the configured harness ran")
	assert.Assert(t, strings.Contains(out.String(), "A Go CLI."))
	assert.Assert(t, strings.Contains(errOut.String(), "Read AGENTS.md"), "tool calls are shown: %s", errOut.String())
	assert.Assert(t, strings.Contains(errOut.String(), "Dropped 1 prompt"), errOut.String())
	assert.Assert(t, strings.Contains(errOut.String(), "$0.50"), errOut.String())
}

func TestBootstrapJSONToOutputDirectory(t *testing.T) {
	root := bootstrapRepo(t)
	outDir := filepath.Join(t.TempDir(), "elsewhere")
	be := &bootstrapBackend{stdout: bootstrapStream}
	streams, out, _ := testStreams()
	req := bootstrapReq(root)
	req.Output, req.JSON = outDir, true

	assert.NilError(t, runBootstrap(context.Background(), streams, req, bootstrapTestEnv(be)))

	var got map[string]any
	assert.NilError(t, json.Unmarshal(out.Bytes(), &got), out.String())
	assert.Equal(t, got["dir"], outDir)
	assert.Equal(t, got["summary"], "A Go CLI.")
	assert.Equal(t, got["dropped"], float64(1))
	assert.Equal(t, got["cost_usd"], 0.5)
	assert.Equal(t, got["harness"], "claude")
	assert.DeepEqual(t, got["prompts"], []any{
		map[string]any{"name": "testing", "path": filepath.Join(outDir, "testing.md")},
		map[string]any{"name": "core-logic", "path": filepath.Join(outDir, "core-logic.md")},
	})
	_, err := os.Stat(filepath.Join(root, review.DefaultDir))
	assert.Assert(t, os.IsNotExist(err), "nothing is written to the project")
}

func TestBootstrapHarnessFlagWinsOverConfig(t *testing.T) {
	root := bootstrapRepo(t)
	be := &bootstrapBackend{stdout: bootstrapStream}
	env := bootstrapTestEnv(be)
	var asked harness.Name
	env.harness = func(n harness.Name) (harness.Harness, error) { asked = n; return bootstrapAgent{}, nil }
	streams, _, _ := testStreams()
	req := bootstrapReq(root)
	req.Harness = "codex"
	_ = runBootstrap(context.Background(), streams, req, env)
	assert.Equal(t, asked, harness.Codex)
}

func TestBootstrapTargetErrorStartsNoSidecar(t *testing.T) {
	root := bootstrapRepo(t)
	be := &bootstrapBackend{stdout: bootstrapStream}
	env := bootstrapTestEnv(be)
	env.target = func(context.Context, factorySettings) (sandboxTarget, error) {
		return sandboxTarget{}, newUserError("CircleCI token required.")
	}
	streams, _, _ := testStreams()
	err := runBootstrap(context.Background(), streams, bootstrapReq(root), env)
	assert.ErrorContains(t, err, "CircleCI token required")
	assert.Equal(t, len(be.created), 0)
}

func TestBootstrapTargetRefusesCircleCIWithoutOptIn(t *testing.T) {
	cfg := bootstrapTestConfig()
	target := bootstrapTarget(newFactoryBootstrapCmd(), iostream.Streams{}, t.TempDir(), cfg)
	_, err := target(context.Background(), factorySettings{Backend: config.BackendCircleCI})
	assert.ErrorContains(t, err, "CircleCI backend is not enabled")
}

func TestBootstrapAgentError(t *testing.T) {
	err := bootstrapAgentError(claude.ErrCredentialRejected)
	var ue *userError
	assert.Assert(t, errors.As(err, &ue))
	assert.Equal(t, ue.UserExitCode(), ExitAuthError)
	assert.ErrorContains(t, bootstrapAgentError(errors.New("boom")), "boom")
}

func TestToolLine(t *testing.T) {
	assert.Equal(t, toolLine(harness.Activity{Tool: "Read", Detail: "a.go"}), "Read a.go")
	assert.Equal(t, toolLine(harness.Activity{Tool: "Bash", Detail: "git log\n\x1b[31m--oneline"}), "Bash git log [31m--oneline")
	long := toolLine(harness.Activity{Tool: "Bash", Detail: strings.Repeat("x", 500)})
	assert.Equal(t, len([]rune(long)), maxToolLine+1)
}
