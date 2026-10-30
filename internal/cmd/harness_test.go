package cmd

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/docker/docker/client"
	"gotest.tools/v3/assert"

	"github.com/smeltdev/smelt/internal/config"
	"github.com/smeltdev/smelt/internal/harness"
	"github.com/smeltdev/smelt/internal/harness/status"
	"github.com/smeltdev/smelt/internal/iostream"
	"github.com/smeltdev/smelt/internal/keyring/keyringtest"
	"github.com/smeltdev/smelt/internal/sandboximage"
)

func TestHarnessStatusJSONReflectsThePersistedSignal(t *testing.T) {
	isolateConfig(t)
	keyringtest.Install(t)
	t.Setenv(config.EnvXDGDataHome, t.TempDir())
	t.Setenv("SMELT_CODEX_AUTH_JSON", "")
	t.Setenv("CODEX_API_KEY", "")
	t.Setenv("XAI_API_KEY", "")
	t.Setenv("PATH", t.TempDir())
	// Turned off, so the test makes no request and reads no keychain.
	cfgDir := filepath.Join(os.Getenv(config.EnvXDGConfigHome), "smelt")
	assert.NilError(t, os.MkdirAll(cfgDir, 0o700))
	assert.NilError(t, os.WriteFile(filepath.Join(cfgDir, "config.json"), []byte(`{"harness":{"claude":{"usage_probe":false}}}`), 0o600))
	t.Chdir(t.TempDir())

	dir, err := status.Dir()
	assert.NilError(t, err)
	resets := time.Now().Add(time.Hour).UTC().Truncate(time.Second)
	status.RecordSignal(dir, harness.Claude, harness.RateLimit{Status: "warning", Bucket: "5h", ResetsAt: &resets}, time.Now())

	out, err := runRoot(t, "harness", "status", "--json")
	assert.NilError(t, err, out)
	var rep status.Report
	assert.NilError(t, json.Unmarshal([]byte(out), &rep), out)
	assert.Equal(t, len(rep.Harnesses), 3)
	claude := rep.Harnesses[0]
	assert.Equal(t, claude.Name, harness.Claude)
	assert.Equal(t, len(claude.RateLimits), 1)
	assert.Equal(t, claude.RateLimits[0].Status, "warning")
	assert.Equal(t, claude.RateLimits[0].Source, "run")
	assert.Equal(t, rep.Harnesses[1].Auth, "missing")
}

func TestHarnessStatusProbeIsOffWhenTheUserSaysSo(t *testing.T) {
	isolateConfig(t)
	cfgDir := filepath.Join(os.Getenv(config.EnvXDGConfigHome), "smelt")
	assert.NilError(t, os.MkdirAll(cfgDir, 0o700))
	t.Chdir(t.TempDir())

	assert.Assert(t, harnessStatusDeps(t.Context()).UsageProbe != nil, "on by default")
	assert.NilError(t, os.WriteFile(filepath.Join(cfgDir, "config.json"), []byte(`{"harness":{"claude":{"usage_probe":false}}}`), 0o600))
	assert.Assert(t, harnessStatusDeps(t.Context()).UsageProbe == nil)
}

func isolatedHarnessStatus(t *testing.T) {
	t.Helper()
	isolateConfig(t)
	keyringtest.Install(t)
	t.Setenv(config.EnvXDGDataHome, t.TempDir())
	t.Setenv("SMELT_CODEX_AUTH_JSON", "")
	t.Setenv("CODEX_API_KEY", "")
	t.Setenv("XAI_API_KEY", "")
	t.Setenv("PATH", t.TempDir())
	cfgDir := filepath.Join(os.Getenv(config.EnvXDGConfigHome), "smelt")
	assert.NilError(t, os.MkdirAll(cfgDir, 0o700))
	assert.NilError(t, os.WriteFile(filepath.Join(cfgDir, "config.json"), []byte(`{"harness":{"claude":{"usage_probe":false}}}`), 0o600))
	t.Chdir(t.TempDir())
}

func TestHarnessStatusGoldenCompatibility(t *testing.T) {
	before, err := os.ReadFile("../harness/status/testdata/status-before.golden.json")
	assert.NilError(t, err)
	golden, err := os.ReadFile("../harness/status/testdata/status.golden.json")
	assert.NilError(t, err)
	isolatedHarnessStatus(t)
	out, err := runRoot(t, "harness", "status", "--json")
	assert.NilError(t, err, out)
	assert.Equal(t, out, string(golden))
	var baseline, current struct{ Harnesses []json.RawMessage }
	assert.NilError(t, json.Unmarshal(before, &baseline))
	assert.NilError(t, json.Unmarshal([]byte(out), &current))
	assert.Equal(t, len(current.Harnesses), 3)
	assert.DeepEqual(t, current.Harnesses[:2], baseline.Harnesses)
}

func TestHarnessStatusGrokHumanAndJSON(t *testing.T) {
	for _, expired := range []bool{false, true} {
		t.Run(yesNo(expired), func(t *testing.T) {
			isolatedHarnessStatus(t)
			bin := os.Getenv("PATH")
			assert.NilError(t, os.WriteFile(filepath.Join(bin, "grok"), []byte("#!/bin/sh\n[ \"$1\" = --version ]\n"), 0o755))
			expiry := time.Now().UTC().Truncate(time.Second).Add(time.Hour)
			state := "ok"
			if expired {
				expiry = expiry.Add(-2 * time.Hour)
				state = "expired"
			}
			dir := filepath.Join(os.Getenv("HOME"), ".grok")
			assert.NilError(t, os.MkdirAll(dir, 0o700))
			assert.NilError(t, os.WriteFile(filepath.Join(dir, "auth.json"), []byte(`{"a":{"key":"access-secret","refresh_token":"refresh-secret","expires_at":"`+expiry.Format(time.RFC3339)+`"}}`), 0o600))
			out, err := runRoot(t, "harness", "status")
			assert.NilError(t, err, out)
			for _, want := range []string{"grok", "login:      " + state, "source:     oauth", expiry.Format(time.RFC3339)} {
				assert.Assert(t, strings.Contains(out, want), out)
			}
			if expired {
				assert.Assert(t, strings.Contains(out, "run `grok login`"), out)
			}
			out, err = runRoot(t, "harness", "status", "--json")
			assert.NilError(t, err, out)
			var rep status.Report
			assert.NilError(t, json.Unmarshal([]byte(out), &rep))
			assert.Equal(t, len(rep.Harnesses), 3)
			assert.Equal(t, rep.Harnesses[0].Name, harness.Claude)
			assert.Equal(t, rep.Harnesses[1].Name, harness.Codex)
			g := rep.Harnesses[2]
			assert.Equal(t, g.Name, harness.Grok)
			assert.Equal(t, g.Auth, state)
			assert.Equal(t, g.Source, "oauth")
			assert.Assert(t, g.ExpiresAt.Equal(expiry))
			assert.Assert(t, g.InstalledHost)
			assert.Assert(t, !strings.Contains(out, "secret"))
		})
	}
}

func TestHarnessStatusPrintsHostAndSandboxVersions(t *testing.T) {
	var out bytes.Buffer
	printHarnessStatus(iostream.Streams{Out: &out}, status.Report{Harnesses: []status.Entry{{Name: harness.Claude, HostVersion: "host-123", ImageVersion: "sandbox-456"}}})
	assert.Assert(t, strings.Contains(out.String(), "host-123 on this machine, sandbox-456 in the image"), out.String())
}

func TestHarnessRefreshCommand(t *testing.T) {
	previous := refreshAgentKit
	t.Cleanup(func() { refreshAgentKit = previous })
	called := false
	refreshAgentKit = func(_ context.Context, ref string) (sandboximage.KitChoice, error) {
		called = true
		assert.Equal(t, ref, sandboximage.KitRegistryRepo+":6")
		return sandboximage.KitChoice{Ref: "sha256:new", Kit: 6}, nil
	}
	out, err := runRoot(t, "harness", "refresh", "--kit-ref", sandboximage.KitRegistryRepo+":6")
	assert.NilError(t, err, out)
	assert.Assert(t, called)
	assert.Assert(t, strings.Contains(out, "Selected agent kit v6: sha256:new"), out)
	assert.Assert(t, strings.Contains(out, "smelt factory image"), out)
}

func TestFactoryImageTagUsesDefaultToolchain(t *testing.T) {
	t.Setenv("SMELT_DOCKER_IMAGE", "")
	t.Setenv(config.EnvXDGDataHome, t.TempDir())
	root := t.TempDir()
	assert.NilError(t, os.WriteFile(filepath.Join(root, sandboximage.DefaultDockerfile), []byte("FROM alpine:3.22\n"), 0o644))
	cfg := &config.FactoryConfig{}
	assert.Equal(t, factoryImageTag(root, cfg), "")
	dir, err := config.AppData()
	assert.NilError(t, err)
	ref := sandboximage.KitRegistryRepo + "@sha256:abcd"
	assert.NilError(t, sandboximage.RememberKit(sandboximage.KitStatePath(dir), ref))
	plan, err := sandboximage.NewPlan(root, sandboximage.DefaultDockerfile)
	assert.NilError(t, err)
	assert.Equal(t, factoryImageTag(root, cfg), plan.SandboxPlan(ref).Tag)
	cfg.Image = "custom:tag"
	assert.Equal(t, factoryImageTag(root, cfg), cfg.Image)
}

// manifestDocker is a Docker API with one labeled sandbox image whose kit
// manifest can only be read by creating a container; it counts those.
func manifestDocker(t *testing.T, creates *atomic.Int32) *sandboximage.Builder {
	t.Helper()
	manifest := `{"kit":6,"tools":[{"name":"claude","version":"2.1.310","sha256":"x"}]}`
	var archive bytes.Buffer
	tw := tar.NewWriter(&archive)
	assert.NilError(t, tw.WriteHeader(&tar.Header{Name: "manifest.json", Mode: 0o644, Size: int64(len(manifest))}))
	_, err := tw.Write([]byte(manifest))
	assert.NilError(t, err)
	assert.NilError(t, tw.Close())
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.HasSuffix(r.URL.Path, "/containers/create"):
			creates.Add(1)
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`{"Id":"c1"}`))
		case strings.HasSuffix(r.URL.Path, "/containers/c1/archive"):
			w.Header().Set("X-Docker-Container-Path-Stat", base64.StdEncoding.EncodeToString([]byte(`{"name":"manifest.json"}`)))
			_, _ = w.Write(archive.Bytes())
		case r.Method == http.MethodDelete:
			w.WriteHeader(http.StatusNoContent)
		case strings.HasSuffix(r.URL.Path, "/images/json"):
			_, _ = w.Write([]byte(`[{"Id":"sha256:img"}]`))
		case strings.Contains(r.URL.Path, "/images/") && strings.HasSuffix(r.URL.Path, "/json"):
			_, _ = w.Write([]byte(`{"Id":"sha256:img","Config":{"Labels":{"` + sandboximage.LabelHarnesses + `":"claude","` + sandboximage.LabelKit + `":"6"}}}`))
		default:
			http.Error(w, "unexpected", http.StatusBadRequest)
		}
	}))
	t.Cleanup(server.Close)
	cli, err := client.NewClientWithOpts(client.WithHost(server.URL), client.WithVersion("1.47"))
	assert.NilError(t, err)
	t.Cleanup(func() { _ = cli.Close() })
	return sandboximage.NewBuilder(cli)
}

func TestHarnessStatusJSONIsUnchangedWhenTheManifestComesFromTheCache(t *testing.T) {
	project := t.TempDir()
	assert.NilError(t, exec.Command("git", "-C", project, "init", "-q").Run())
	gitPath, err := exec.LookPath("git")
	assert.NilError(t, err)
	isolatedHarnessStatus(t)
	// The project root is found with git, and no agent CLI is on the PATH.
	bin := t.TempDir()
	assert.NilError(t, os.Symlink(gitPath, filepath.Join(bin, "git")))
	t.Setenv("PATH", bin)
	t.Setenv(config.EnvXDGStateHome, t.TempDir())
	assert.NilError(t, os.MkdirAll(filepath.Join(project, ".smelt"), 0o700))
	assert.NilError(t, os.WriteFile(filepath.Join(project, ".smelt", "config.json"), []byte(`{"factory":{"image":"proj:img"}}`), 0o600))
	t.Chdir(project)

	var creates atomic.Int32
	previous := connectDocker
	t.Cleanup(func() { connectDocker = previous })
	connectDocker = func() (*sandboximage.Builder, error) { return manifestDocker(t, &creates), nil }

	first, err := runRoot(t, "harness", "status", "--json")
	assert.NilError(t, err, first)
	assert.Assert(t, strings.Contains(first, "2.1.310"), first)
	assert.Equal(t, creates.Load(), int32(1))

	for range 3 {
		again, err := runRoot(t, "harness", "status", "--json")
		assert.NilError(t, err, again)
		assert.Equal(t, again, first)
	}
	assert.Equal(t, creates.Load(), int32(1), "the cached manifest starts no container")
}
