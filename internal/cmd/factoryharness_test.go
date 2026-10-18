package cmd

import (
	"encoding/json"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"gotest.tools/v3/assert"

	"github.com/smeltdev/smelt/internal/config"
	"github.com/smeltdev/smelt/internal/iostream"
	"github.com/smeltdev/smelt/internal/smeltd"
)

func TestDecideHarness(t *testing.T) {
	tests := []struct {
		name   string
		in     harnessInputs
		want   string
		action harnessAction
	}{
		{"flag beats config", harnessInputs{Flag: "codex", Config: "claude", CanPrompt: true}, "codex", harnessUse},
		{"config beats the prompt", harnessInputs{Config: "claude", CanPrompt: true}, "claude", harnessUse},
		{"a terminal is asked", harnessInputs{CanPrompt: true}, "", harnessAsk},
		{"no terminal and nothing set fails", harnessInputs{}, "", harnessFail},
		{"a flag needs no terminal", harnessInputs{Flag: "claude"}, "claude", harnessUse},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, action := decideHarness(tt.in)
			assert.Equal(t, got, tt.want)
			assert.Equal(t, action, tt.action)
		})
	}
}

func TestResolveHarnessFailsWithoutAnyChoiceOrTerminal(t *testing.T) {
	_, err := resolveHarness(t.Context(), iostream.Streams{Out: os.Stdout, Err: os.Stderr}, harnessChoice{Cfg: &config.ProjectConfig{}})
	var ue *userError
	assert.ErrorContains(t, err, "No coding agent is chosen")
	assert.Assert(t, asUserError(err, &ue))
	assert.Assert(t, strings.Contains(ue.suggestion, "factory.harness") && strings.Contains(ue.suggestion, "--harness"), ue.suggestion)
}

func TestResolveHarnessRejectsAnUnknownFlag(t *testing.T) {
	_, err := resolveHarness(t.Context(), iostream.Streams{Out: os.Stdout, Err: os.Stderr}, harnessChoice{Flag: "gemini"})
	assert.ErrorContains(t, err, `Unknown harness "gemini"`)
}

func TestFactoryBuildNeedsAHarnessWhenNothingIsSet(t *testing.T) {
	project := factoryProject(t)
	cfg := `{"orgID":"org-1","factory":{"backend":"circleci"},"commands":[{"name":"test","run":"go test ./..."}]}`
	assert.NilError(t, os.WriteFile(filepath.Join(project, ".smelt", "config.json"), []byte(cfg), 0o644))
	_, _, err := runFactoryBuildCmd(t, "add a flag")
	assert.ErrorContains(t, err, "No coding agent is chosen")
}

func TestFallbackFlagOverridesConfigAndPreservesOrder(t *testing.T) {
	cfg := &config.FactoryConfig{Fallback: []string{"codex", "claude", "codex"}}
	got, err := resolveFallback("claude", fallbackChoice{}, cfg)
	assert.NilError(t, err)
	assert.DeepEqual(t, got, []string{"codex"})
	got, err = resolveFallback("claude", fallbackChoice{Names: []string{}, Explicit: true}, cfg)
	assert.NilError(t, err)
	assert.Equal(t, len(got), 0)
	_, err = resolveFallback("claude", fallbackChoice{Names: []string{"grok"}, Explicit: true}, cfg)
	assert.ErrorContains(t, err, "unknown fallback harness")
}

func TestFactoryBuildForwardsFallbackToDaemon(t *testing.T) {
	factoryProject(t) // No configured fallback.
	dir, err := os.MkdirTemp("", "wd")
	assert.NilError(t, err)
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	t.Setenv("SMELT_DAEMON_DIR", dir)
	t.Setenv("SMELT_DAEMON_REMOTE_ADDR", "")
	assert.NilError(t, os.WriteFile(filepath.Join(dir, "daemon.pid"), []byte(strconv.Itoa(os.Getpid())), 0o600))
	reqs := make(chan smeltd.FactoryRequest, 1)
	mux := http.NewServeMux()
	mux.HandleFunc("/ping", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, smeltd.BuildID())
	})
	mux.HandleFunc("/factory", func(w http.ResponseWriter, r *http.Request) {
		var req smeltd.FactoryRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		reqs <- req
		http.Error(w, "captured factory request", http.StatusBadRequest)
	})
	ln, err := net.Listen("unix", filepath.Join(dir, "daemon.sock"))
	assert.NilError(t, err)
	srv := &http.Server{Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Close() })
	_, _, err = runFactoryBuildCmd(t, "--harness", "codex", "--fallback", "claude", "add a flag")
	assert.ErrorContains(t, err, "captured factory request")
	select {
	case req := <-reqs:
		assert.Equal(t, req.Harness, "codex")
		assert.DeepEqual(t, req.Fallback, []string{"claude"})
		assert.Assert(t, req.FallbackSet)
	default:
		t.Fatal("factory request was not sent")
	}
}
