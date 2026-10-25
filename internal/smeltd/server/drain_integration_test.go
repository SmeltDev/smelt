package server

import (
	"context"
	"encoding/json"
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

	"github.com/smeltdev/smelt/internal/config"
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
		data, err := config.ProjectDataDir(opts.Root)
		assert.NilError(t, err)
		assert.NilError(t, os.MkdirAll(filepath.Join(data, "factory"), 0o700))
		record, err := json.Marshal(factory.Record{RunID: opts.Prompt, Prompt: opts.Prompt})
		assert.NilError(t, err)
		assert.NilError(t, os.WriteFile(filepath.Join(data, "factory", opts.Prompt+".json"), record, 0o600))
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
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Minute)
	t.Cleanup(cancel)
	command := exec.CommandContext(ctx, name, args...)
	command.WaitDelay = 2 * time.Second
	return command
}

func startDrainProcess(t *testing.T) *drainProcess {
	t.Helper()
	exe, err := os.Executable()
	assert.NilError(t, err)
	binary := filepath.Join(t.TempDir(), "daemon-test")
	input, err := os.Open(exe)
	assert.NilError(t, err)
	output, err := os.OpenFile(binary, os.O_CREATE|os.O_WRONLY, 0o700)
	assert.NilError(t, err)
	_, err = io.Copy(output, input)
	assert.NilError(t, err)
	assert.NilError(t, input.Close())
	assert.NilError(t, output.Close())
	exe = binary
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

func TestHandoverOverlappingDaemonProcesses(t *testing.T) {
	root := drainProject(t)
	release := t.TempDir()
	t.Setenv("SMELT_TEST_RELEASE_DIR", release)
	cli := filepath.Join(t.TempDir(), "smelt")
	build := drainCommand(t, "go", "build", "-o", cli, "../../..")
	output, err := build.CombinedOutput()
	assert.NilError(t, err, "%s", output)
	command := func(args ...string) (string, error) {
		c := drainCommand(t, cli, append([]string{"--no-daemon"}, args...)...)
		c.Dir = root
		out, err := c.CombinedOutput()
		return string(out), err
	}

	old := startDrainProcess(t)
	main, err := smeltd.SocketPath()
	assert.NilError(t, err)
	private, err := smeltd.PIDSocketPath(old.cmd.Process.Pid)
	assert.NilError(t, err)
	start := func(name string) string {
		id, err := smeltd.StartFactory(smeltd.FactoryRequest{Backend: smeltd.BackendCircleCI, ProjectRoot: root, Prompt: name})
		assert.NilError(t, err)
		waitUntil(t, "run start", func() bool { _, err := os.Stat(filepath.Join(release, name+".started")); return err == nil })
		return id
	}
	first := start("first")
	// This run finishes, but remains discoverable for continuation while another is active.
	prior := start("prior")
	assert.NilError(t, os.WriteFile(filepath.Join(release, "prior"), nil, 0o600))
	waitUntil(t, "prior completion", func() bool { d, err := smeltd.FetchSession(prior); return err == nil && d.State.Finished() })
	before, err := smeltd.Status()
	assert.NilError(t, err)
	text, err := command("daemon", "restart", "--when-idle")
	assert.NilError(t, err, "%s", text)
	assert.Assert(t, strings.Contains(text, "released its listener"))
	_, err = os.Stat(main)
	assert.Assert(t, os.IsNotExist(err))
	pidPath, err := smeltd.PIDPath()
	assert.NilError(t, err)
	_, err = os.Stat(pidPath)
	assert.Assert(t, os.IsNotExist(err))
	replacement := startDrainProcess(t)
	after, err := smeltd.Status()
	assert.NilError(t, err)
	assert.Assert(t, before.BuildID != after.BuildID)
	second := start("second") // admitted before the original is released
	daemons, err := smeltd.DiscoverDaemons()
	assert.NilError(t, err)
	assert.Equal(t, len(daemons), 2)
	assert.Equal(t, daemons[0].PID, replacement.cmd.Process.Pid)
	assert.Equal(t, daemons[1].PID, old.cmd.Process.Pid)
	assert.Equal(t, daemons[1].Draining, true)
	text, err = command("factory", "status", "first")
	assert.NilError(t, err, "%s", text)
	assert.Assert(t, strings.Contains(text, "Run first: running"), "%s", text)
	text, err = command("factory", "list", "--json")
	assert.NilError(t, err, "%s", text)
	assert.Assert(t, strings.Contains(text, "first") && strings.Contains(text, "second"), "%s", text)
	text, err = command("factory", "update", "first", "--next-round", "--base", "main")
	assert.NilError(t, err, "%s", text)
	for _, args := range [][]string{{"factory", "update", "first"}, {"factory", "publish", "first", "--push"}} {
		text, err = command(args...)
		assert.Assert(t, err != nil)
		assert.Assert(t, strings.Contains(text, "Run first is still running"), "%s", text)
	}

	sessions, err := smeltd.ListSessions(root)
	assert.NilError(t, err)
	assert.Equal(t, len(sessions), 3)
	detail, err := smeltd.FetchSession(first)
	assert.NilError(t, err)
	assert.Equal(t, detail.Socket, private)
	assert.Equal(t, detail.Factory.RunID, "first") // status/follow/update/publish use this lookup
	continued, err := smeltd.StartFactory(smeltd.FactoryRequest{Backend: smeltd.BackendCircleCI, ProjectRoot: root, Continue: "prior", Prompt: "continued"})
	assert.NilError(t, err)
	waitUntil(t, "continuation start", func() bool {
		d, err := smeltd.FetchSession(continued)
		return err == nil && d.Factory.RunID == "continued"
	})
	detail, err = smeltd.FetchSession(continued)
	assert.NilError(t, err)
	assert.Equal(t, detail.Socket, private)
	text, err = command("factory", "stop", "continued")
	assert.NilError(t, err, "%s", text)
	assert.Assert(t, strings.Contains(text, "has stopped"), "%s", text)
	waitUntil(t, "continuation stopped", func() bool { d, err := smeltd.FetchSession(continued); return err == nil && d.State.Finished() })
	assert.NilError(t, os.WriteFile(filepath.Join(release, "first"), nil, 0o600))
	select {
	case err := <-old.exited:
		assert.NilError(t, err)
	case <-time.After(10 * time.Second):
		t.Fatal("old daemon did not exit")
	}
	_, err = os.Stat(private)
	assert.Assert(t, os.IsNotExist(err))
	detail, err = smeltd.FetchSession(second)
	assert.NilError(t, err)
	assert.Assert(t, !detail.State.Finished())
	pid, err := smeltd.ReadPID(pidPath)
	assert.NilError(t, err)
	assert.Equal(t, pid, replacement.cmd.Process.Pid)
	assert.NilError(t, smeltd.CancelSession(second))
}

func TestHandoverCrashSocketDiscovery(t *testing.T) {
	root := drainProject(t)
	t.Setenv("SMELT_TEST_RELEASE_DIR", t.TempDir())
	old := startDrainProcess(t)
	_, err := smeltd.StartFactory(smeltd.FactoryRequest{Backend: smeltd.BackendCircleCI, ProjectRoot: root, Prompt: "crash"})
	assert.NilError(t, err)
	_, err = smeltd.Handover()
	assert.NilError(t, err)
	private, err := smeltd.PIDSocketPath(old.cmd.Process.Pid)
	assert.NilError(t, err)
	replacement := startDrainProcess(t)
	assert.NilError(t, old.cmd.Process.Kill())
	<-old.exited
	_, err = os.Stat(private)
	assert.NilError(t, err)
	daemons, err := smeltd.DiscoverDaemons()
	assert.NilError(t, err)
	assert.Equal(t, len(daemons), 1)
	assert.Equal(t, daemons[0].PID, replacement.cmd.Process.Pid)
	_, err = os.Stat(private)
	assert.Assert(t, os.IsNotExist(err))
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
