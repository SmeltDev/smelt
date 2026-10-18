package server

import (
	"context"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"gotest.tools/v3/assert"

	"github.com/smeltdev/smelt/internal/factory"
	"github.com/smeltdev/smelt/internal/smeltd"
)

// The helper owns signals and the logger in a separate process. Each fake run
// has its own release file, so ending one cannot accidentally release both.
func TestDrainDaemonProcess(t *testing.T) {
	if os.Getenv("SMELT_TEST_DRAIN_PROCESS") != "1" {
		return
	}
	cfg := (&fakeBackend{}).config()
	cfg.RunFactory = func(ctx context.Context, opts factory.RunOptions) (factory.Report, error) {
		opts.OnStart(opts.Prompt, factory.Worktree{Branch: "smelt/" + opts.Prompt})
		path := filepath.Join(os.Getenv("SMELT_TEST_RELEASE_DIR"), opts.Prompt)
		assert.NilError(t, os.WriteFile(path+".started", nil, 0o600))
		ticker := time.NewTicker(10 * time.Millisecond)
		defer ticker.Stop()
		for {
			if _, err := os.Stat(path); err == nil {
				assert.NilError(t, os.WriteFile(path+".finished", nil, 0o600))
				return factory.Report{}, nil
			}
			select {
			case <-ctx.Done():
				return factory.Report{}, ctx.Err()
			case <-ticker.C:
			}
		}
	}
	assert.NilError(t, RunDaemon(context.Background(), nil, "", nil, nil, WithReview(cfg)))
}

type drainProcess struct {
	cmd     *exec.Cmd
	exited  chan error
	logPath string
}

// Bound every subprocess, including CLI builds, even when a test has no deadline.
func drainCommand(t *testing.T, name string, args ...string) *exec.Cmd {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
	t.Cleanup(cancel)
	command := exec.CommandContext(ctx, name, args...)
	command.WaitDelay = 2 * time.Second
	return command
}

func startDrainProcess(t *testing.T) *drainProcess {
	t.Helper()
	exe, err := os.Executable()
	assert.NilError(t, err)
	logPath := filepath.Join(t.TempDir(), "daemon.log")
	logFile, err := os.Create(logPath)
	assert.NilError(t, err)
	t.Cleanup(func() { _ = logFile.Close() })
	cmd := drainCommand(t, exe, "-test.run=^TestDrainDaemonProcess$")
	cmd.Env = append(os.Environ(), "SMELT_TEST_DRAIN_PROCESS=1", "GORACE=atexit_sleep_ms=0")
	cmd.Stdout, cmd.Stderr = logFile, logFile
	assert.NilError(t, cmd.Start())
	p := &drainProcess{cmd: cmd, exited: make(chan error, 1), logPath: logPath}
	go func() { p.exited <- cmd.Wait(); close(p.exited) }()
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		<-p.exited
		data, _ := os.ReadFile(logPath)
		t.Logf("daemon log:\n%s", data)
	})
	sock, err := smeltd.SocketPath()
	assert.NilError(t, err)
	waitUntil(t, "daemon socket", func() bool { ok, _ := ping(sock); return ok })
	return p
}

func drainHTTP(t *testing.T, method string) {
	t.Helper()
	sock, err := smeltd.SocketPath()
	assert.NilError(t, err)
	transport := &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "unix", sock)
	}}
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport, Timeout: time.Second}
	req, err := http.NewRequestWithContext(t.Context(), method, "http://daemon/drain", nil)
	assert.NilError(t, err)
	resp, err := client.Do(req)
	assert.NilError(t, err)
	defer func() { _ = resp.Body.Close() }()
	_, err = io.Copy(io.Discard, resp.Body)
	assert.NilError(t, err)
	assert.Equal(t, resp.StatusCode, http.StatusOK)
}

func drainProject(t *testing.T) string {
	t.Helper()
	_, root := newFactoryDaemon(t, nil)
	git(t, root, "remote", "add", "origin", "https://github.com/example/drain-test.git")
	assert.NilError(t, os.WriteFile(filepath.Join(root, ".smelt", "config.json"), []byte(`{"orgID":"org-1","commands":[{"name":"test","run":"true"}],"factory":{"maxConcurrent":2}}`), 0o600))
	return root
}

func TestDrainRealDaemonKilledRequester(t *testing.T) {
	root := drainProject(t)
	release := t.TempDir()
	t.Setenv("SMELT_TEST_RELEASE_DIR", release)
	p := startDrainProcess(t)
	cli := filepath.Join(t.TempDir(), "smelt")
	build := drainCommand(t, "go", "build", "-o", cli, "../../..")
	out, err := build.CombinedOutput()
	assert.NilError(t, err, "%s", out)
	for _, name := range []string{"first", "second"} {
		_, err := smeltd.StartFactory(smeltd.FactoryRequest{Backend: smeltd.BackendCircleCI, ProjectRoot: root, Prompt: name})
		assert.NilError(t, err)
		waitUntil(t, "run start", func() bool { _, err := os.Stat(filepath.Join(release, name+".started")); return err == nil })
	}
	requester := drainCommand(t, cli, "daemon", "restart", "--when-idle")
	assert.NilError(t, requester.Start())
	t.Cleanup(func() {
		if requester.ProcessState == nil {
			_ = requester.Process.Kill()
			_ = requester.Wait()
		}
	})
	waitUntil(t, "drain request", func() bool { info, err := smeltd.Status(); return err == nil && info.Draining })
	assert.NilError(t, requester.Process.Kill())
	assert.Assert(t, requester.Wait() != nil)
	// The CLI is a different executable from the daemon's test binary.
	status := drainCommand(t, cli, "factory", "status", "first")
	status.Dir = root
	out, err = status.CombinedOutput()
	assert.NilError(t, err, "%s", out)
	assert.Assert(t, strings.Contains(string(out), "Run first: running"), "%s", out)
	info, err := smeltd.Status()
	assert.NilError(t, err)
	assert.Equal(t, info.BuildID, smeltd.BuildID(), "status must keep the original daemon")
	pidPath, err := smeltd.PIDPath()
	assert.NilError(t, err)
	pid, err := smeltd.ReadPID(pidPath)
	assert.NilError(t, err)
	assert.Equal(t, pid, p.cmd.Process.Pid)
	for i, name := range []string{"first", "second"} {
		assert.NilError(t, os.WriteFile(filepath.Join(release, name), nil, 0o600))
		waitUntil(t, "run completion", func() bool { _, err := os.Stat(filepath.Join(release, name+".finished")); return err == nil })
		if i == 0 {
			waitUntil(t, "one remaining run", func() bool {
				sessions, err := smeltd.ListSessions(root)
				if err != nil {
					return false
				}
				n := 0
				for _, s := range sessions {
					if !s.State.Finished() {
						n++
					}
				}
				return n == 1
			})
			waitUntil(t, "drain progress", func() bool {
				data, err := os.ReadFile(p.logPath)
				return err == nil && strings.Contains(string(data), "draining: 1 runs left")
			})
			info, err := smeltd.Status()
			assert.NilError(t, err)
			assert.Assert(t, info.Reachable && info.Draining)
			// Hold the other run past the shutdown delay. A daemon that treats the
			// first completion as idle would cancel it before it is released.
			select {
			case err := <-p.exited:
				t.Fatalf("daemon exited with the second run active: %v", err)
			case <-time.After(2 * drainFlushDelay):
			}
		}
	}
	select {
	case err := <-p.exited:
		assert.NilError(t, err)
	case <-time.After(10 * time.Second):
		t.Fatal("daemon did not exit after both runs completed")
	}
	data, err := os.ReadFile(p.logPath)
	assert.NilError(t, err)
	previous := -1
	for _, line := range []string{"drain requested (2 active runs)", "draining: 1 runs left", "draining: 0 runs left", "drained: exiting"} {
		index := strings.Index(string(data), line)
		assert.Assert(t, index > previous, "missing or out of order %q in %s", line, data)
		previous = index
	}
}

// The straightforward killed-requester sequence is protected already. This
// interleaving reproduces a separate cancellation source: a previously scheduled
// idle callback survives DELETE /drain and cancels a newly admitted run.
func TestDrainRealDaemonUndrainDuringFlush(t *testing.T) {
	root := drainProject(t)
	release := t.TempDir()
	t.Setenv("SMELT_TEST_RELEASE_DIR", release)
	p := startDrainProcess(t)
	drainHTTP(t, http.MethodPost)
	drainHTTP(t, http.MethodDelete)
	_, err := smeltd.StartFactory(smeltd.FactoryRequest{Backend: smeltd.BackendCircleCI, ProjectRoot: root, Prompt: "renewed"})
	assert.NilError(t, err)
	waitUntil(t, "renewed run start", func() bool { _, err := os.Stat(filepath.Join(release, "renewed.started")); return err == nil })
	select {
	case err := <-p.exited:
		t.Fatalf("cancelled drain stopped a renewed run: %v", err)
	case <-time.After(2 * drainFlushDelay):
	}
	assert.NilError(t, os.WriteFile(filepath.Join(release, "renewed"), nil, 0o600))
	waitUntil(t, "renewed run completion", func() bool { _, err := os.Stat(filepath.Join(release, "renewed.finished")); return err == nil })
	drainHTTP(t, http.MethodPost)
	select {
	case err := <-p.exited:
		assert.NilError(t, err)
	case <-time.After(10 * time.Second):
		t.Fatal("daemon did not drain")
	}
}
