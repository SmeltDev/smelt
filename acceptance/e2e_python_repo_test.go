//go:build e2e

package acceptance

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"gotest.tools/v3/assert"

	"github.com/smeltdev/smelt/internal/config"
	"github.com/smeltdev/smelt/internal/iostream"
	"github.com/smeltdev/smelt/internal/sidecar"
	"github.com/smeltdev/smelt/internal/sidecar/docker"
	"github.com/smeltdev/smelt/internal/testing/binary"
	testenv "github.com/smeltdev/smelt/internal/testing/env"
	"github.com/smeltdev/smelt/internal/validate"
)

const (
	e2eImageTimeout = 20 * time.Minute
	e2eOrigin       = "git@github.com:example-owner/music_app.git"
)

var imageReadyLine = regexp.MustCompile(`Factory image ready: (\S+)`)

// e2eGit runs git in dir with the built binary's directory on PATH, because
// the pre-commit hook calls smelt from PATH.
func e2eGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	c := exec.Command("git", args...)
	c.Dir = dir
	c.Env = []string{
		"HOME=" + dir,
		"PATH=" + filepath.Dir(binary.Path()) + string(os.PathListSeparator) + os.Getenv("PATH"),
		"GIT_AUTHOR_NAME=Test", "GIT_AUTHOR_EMAIL=test@test.com",
		"GIT_COMMITTER_NAME=Test", "GIT_COMMITTER_EMAIL=test@test.com",
		"GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null",
	}
	var out bytes.Buffer
	c.Stdout, c.Stderr = &out, &out
	assert.NilError(t, c.Run(), "git %v: %s", args, out.String())
	return out.String()
}

func e2eWrite(t *testing.T, dir, name, body string) {
	t.Helper()
	p := filepath.Join(dir, name)
	assert.NilError(t, os.MkdirAll(filepath.Dir(p), 0o755))
	assert.NilError(t, os.WriteFile(p, []byte(body), 0o644))
}

// e2eMusicApp builds a repo shaped like music_app: no tests, ignored .env and
// venv/ on disk, master as the default branch, a GitHub SSH origin and no
// origin/HEAD.
func e2eMusicApp(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	e2eWrite(t, dir, "pyproject.toml", "[build-system]\nrequires = [\"setuptools\"]\nbuild-backend = \"setuptools.build_meta\"\n\n"+
		"[project]\nname = \"music_app\"\nversion = \"0.1.0\"\ndependencies = [\"fastapi\", \"uvicorn\"]\n")
	e2eWrite(t, dir, "app/__init__.py", "")
	e2eWrite(t, dir, "app/main.py", "from fastapi import FastAPI\n\napp = FastAPI()\n")
	e2eWrite(t, dir, ".gitignore", ".env\nvenv/\n")
	e2eWrite(t, dir, ".env", "SECRET=1\n")
	e2eWrite(t, dir, "venv/pyvenv.cfg", "home = /usr/bin\n")
	e2eGit(t, dir, "init", "-b", "master")
	e2eGit(t, dir, "add", ".")
	e2eGit(t, dir, "commit", "-m", "initial")
	e2eGit(t, dir, "remote", "add", "origin", e2eOrigin)
	return dir
}

// e2eInitPaths parses the paths of the "git add" line in init's commit block.
func e2eInitPaths(t *testing.T, stderr string) []string {
	t.Helper()
	_, block, ok := strings.Cut(stderr, "Commit these so runs start from them:\n")
	assert.Assert(t, ok, "no commit block in init output: %s", stderr)
	var words []string
	for _, line := range strings.Split(block, "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "git commit") {
			break
		}
		line = strings.TrimSuffix(strings.TrimPrefix(line, "git add"), "\\")
		words = append(words, strings.Fields(line)...)
	}
	assert.Assert(t, len(words) > 0, "no paths in commit block: %s", stderr)
	return words
}

// e2eImage runs factory image, schedules the cleanup of its tag and returns it.
func e2eImage(t *testing.T, env *testenv.TestEnv, dir string) string {
	t.Helper()
	res := binary.RunCLIWithTimeout(t, []string{"factory", "image"}, env, dir, e2eImageTimeout)
	m := imageReadyLine.FindStringSubmatch(res.Stderr)
	if m != nil {
		tag := m[1]
		t.Cleanup(func() { _ = exec.Command("docker", "rmi", "-f", tag).Run() })
	}
	t.Cleanup(func() {
		_ = exec.Command("sh", "-c", "docker ps -aq --filter name=smelt-verify | xargs -r docker rm -f").Run()
	})
	assert.Equal(t, res.ExitCode, 0, "stdout: %s\nstderr: %s", res.Stdout, res.Stderr)
	assert.Assert(t, m != nil, "no 'Factory image ready' line: %s", res.Stderr)
	return m[1]
}

// e2eSnapshotListing copies the repo into a container of the image, as
// sandboximage.Verify does, and lists the workspace.
func e2eSnapshotListing(t *testing.T, dir, tag string) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	provider, err := docker.New()
	assert.NilError(t, err)
	t.Cleanup(func() { _ = provider.Close() })
	provider.ProjectPath = dir
	provider.ManagedDockerfile = "Dockerfile.smelt"
	noop := func(iostream.Level, string) {}
	assert.NilError(t, provider.Ready(ctx, tag, noop))

	repoPath, err := sidecar.RepoWorkspace(ctx, dir)
	assert.NilError(t, err)
	inst, err := provider.Create(ctx, "smelt-verify-snapshot", tag, repoPath)
	assert.NilError(t, err)
	t.Cleanup(func() {
		c, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		_ = provider.Delete(c, inst.ID)
	})
	assert.NilError(t, provider.Sync(ctx, inst.ID, repoPath, dir, false, noop))

	var out bytes.Buffer
	code, err := provider.Exec(ctx, inst.ID, "ls -A "+repoPath, nil, func(_ string, data []byte) { out.Write(data) }, nil)
	assert.NilError(t, err)
	assert.Equal(t, code, 0, out.String())
	return out.String()
}

func TestPythonRepo(t *testing.T) {
	dir := e2eMusicApp(t)
	env := testenv.NewTestEnv(t)
	env.AnthropicKey = ""
	env.CircleToken = ""
	initArgs := []string{"init", "--factory-backend", config.BackendDocker, "--skip-image-build", "--skip-auth", "--skip-completions", "--skip-skills"}

	// Pass 1: no tests.
	init1 := binary.RunCLI(t, initArgs, env, dir)
	assert.Equal(t, init1.ExitCode, 0, "stdout: %s\nstderr: %s", init1.Stdout, init1.Stderr)

	cfg, err := config.LoadProjectConfig(dir)
	assert.NilError(t, err)
	assert.Assert(t, cfg.Factory != nil, "no factory section")
	assert.Equal(t, cfg.Factory.Backend, config.BackendDocker)
	assert.Equal(t, cfg.Factory.Dockerfile, "Dockerfile.smelt")
	_, err = os.Stat(filepath.Join(dir, "Dockerfile.smelt"))
	assert.NilError(t, err)

	for _, c := range cfg.Commands {
		assert.Assert(t, c.Name != "test" && c.Role != config.RoleGate, "unexpected gate: %+v", c)
	}
	assert.Assert(t, strings.Contains(init1.Stderr, validate.NoTestGateNote), init1.Stderr)

	// Without a gate init installs no hook, so the commit below cannot be
	// blocked by smelt validate having nothing to run.
	assert.Assert(t, strings.Contains(init1.Stderr, "hooks not installed: no gate commands to run"), init1.Stderr)
	_, err = os.Stat(filepath.Join(dir, ".git", "hooks", "pre-commit"))
	assert.Assert(t, os.IsNotExist(err), "unexpected pre-commit hook without a gate: %v", err)
	e2eGit(t, dir, append([]string{"add"}, e2eInitPaths(t, init1.Stderr)...)...)
	e2eGit(t, dir, "commit", "-m", "Set up smelt")

	tag := e2eImage(t, env, dir)

	val := binary.RunCLIWithTimeout(t, []string{"validate"}, env, dir, e2eImageTimeout)
	// With no commands configured validate has nothing to pass; it refuses
	// rather than succeeding vacuously.
	assert.Assert(t, val.ExitCode != 0, "validate passed with no commands: %s%s", val.Stdout, val.Stderr)

	listing := strings.Fields(e2eSnapshotListing(t, dir, tag))
	assert.Assert(t, !slices.Contains(listing, ".env") && !slices.Contains(listing, "venv"), "ignored paths in snapshot: %v", listing)
	assert.Assert(t, slices.Contains(listing, "pyproject.toml") && slices.Contains(listing, "app"), "tracked paths missing: %v", listing)

	// Pass 2: tests and pytest in a dev group.
	e2eWrite(t, dir, "tests/test_ok.py", "def test_ok():\n    pass\n")
	e2eWrite(t, dir, "pyproject.toml", "[build-system]\nrequires = [\"setuptools\"]\nbuild-backend = \"setuptools.build_meta\"\n\n"+
		"[project]\nname = \"music_app\"\nversion = \"0.1.0\"\ndependencies = [\"fastapi\", \"uvicorn\"]\n\n"+
		"[dependency-groups]\ndev = [\"pytest\"]\n")
	e2eGit(t, dir, "add", "tests/test_ok.py", "pyproject.toml")

	init2 := binary.RunCLI(t, append(initArgs, "--force"), env, dir)
	assert.Equal(t, init2.ExitCode, 0, "stdout: %s\nstderr: %s", init2.Stdout, init2.Stderr)
	cfg, err = config.LoadProjectConfig(dir)
	assert.NilError(t, err)
	var gate *config.Command
	for i := range cfg.Commands {
		if cfg.Commands[i].Name == "test" {
			gate = &cfg.Commands[i]
		}
	}
	assert.Assert(t, gate != nil, "no test command after pass 2: %+v", cfg.Commands)
	assert.Equal(t, gate.Role, config.RoleGate)
	assert.Equal(t, gate.Run, "pytest")

	// The container verify runs the install step and then the gate.
	e2eImage(t, env, dir)
}
