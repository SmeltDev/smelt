package cmd

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"gotest.tools/v3/assert"

	"github.com/smeltdev/smelt/internal/config"
	"github.com/smeltdev/smelt/internal/factory"
	"github.com/smeltdev/smelt/internal/smeltd"
	"github.com/smeltdev/smelt/internal/smeltd/server"
	"github.com/smeltdev/smelt/internal/version"
)

// fakeDaemonControl is a daemon whose answers a test scripts. It exits after
// exitAfter pings once drained, 0 meaning never.
type fakeDaemonControl struct {
	reachable   bool
	revision    string
	draining    bool
	active      int
	activeErr   error
	drainErr    error
	exitAfter   int
	cancelDrain bool // CancelDrain was called
	drains      int
	stopped     []int
	pings       int
	statuses    []smeltd.DaemonStatus
	handover    bool
}

func (f *fakeDaemonControl) Handover() (smeltd.DrainResponse, error) {
	if f.handover {
		return smeltd.DrainResponse{Draining: true, Active: f.active}, nil
	}
	return smeltd.DrainResponse{}, smeltd.ErrHandoverUnsupported
}
func (f *fakeDaemonControl) Daemons() ([]smeltd.DaemonStatus, error) { return f.statuses, nil }

func (f *fakeDaemonControl) Ping() (smeltd.PingInfo, error) {
	if f.draining && f.exitAfter > 0 {
		if f.pings++; f.pings > f.exitAfter {
			f.reachable = false
		}
	}
	return smeltd.PingInfo{Reachable: f.reachable, Revision: f.revision, Draining: f.draining}, nil
}
func (f *fakeDaemonControl) Pid() (int, bool, error) { return 4242, f.reachable, nil }
func (f *fakeDaemonControl) Active() (int, error)    { return f.active, f.activeErr }
func (f *fakeDaemonControl) Stop(pid int) error      { f.stopped = append(f.stopped, pid); return nil }
func (f *fakeDaemonControl) Drain() (smeltd.DrainResponse, error) {
	if f.drainErr != nil {
		return smeltd.DrainResponse{}, f.drainErr
	}
	f.drains++
	f.draining = true
	return smeltd.DrainResponse{Draining: true, Active: f.active}, nil
}
func (f *fakeDaemonControl) CancelDrain() (smeltd.DrainResponse, error) {
	f.cancelDrain = true
	f.draining = false
	return smeltd.DrainResponse{Active: f.active}, nil
}

func fakeOpts(f *fakeDaemonControl, rev string) daemonOptions {
	return daemonOptions{Daemon: f, Build: version.Build{Revision: rev}, PollInterval: time.Millisecond}
}

func runDaemonGroup(t *testing.T, opts daemonOptions, args ...string) (string, error) {
	t.Helper()
	cmd := newDaemonGroupCmdWith(opts)
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	cmd.SetArgs(args)
	cmd.SetContext(t.Context())
	err := cmd.Execute()
	return out.String(), err
}

func TestDaemonStatus(t *testing.T) {
	f := &fakeDaemonControl{statuses: []smeltd.DaemonStatus{
		{PID: 91234, Revision: "339d682", ActiveRuns: 2, Socket: "/tmp/daemon.sock"},
		{PID: 86001, Revision: "9bab973", Draining: true, ActiveRuns: 1, Socket: "/tmp/smeltd-86001.sock"},
	}}
	out, err := runDaemonGroup(t, fakeOpts(f, "339d682"), "status")
	assert.NilError(t, err)
	assert.Equal(t, out, "running (pid 91234)  revision 339d682 (this binary) · active runs: 2\ndraining (pid 86001) revision 9bab973 · active runs: 1 · exits when they finish\n")
	out, err = runDaemonGroup(t, fakeOpts(f, "339d682"), "status", "--json")
	assert.NilError(t, err)
	assert.Equal(t, out, `[
  {
    "pid": 91234,
    "revision": "339d682",
    "draining": false,
    "active_runs": 2,
    "socket": "/tmp/daemon.sock"
  },
  {
    "pid": 86001,
    "revision": "9bab973",
    "draining": true,
    "active_runs": 1,
    "socket": "/tmp/smeltd-86001.sock"
  }
]
`)
	f.statuses = []smeltd.DaemonStatus{}
	out, err = runDaemonGroup(t, fakeOpts(f, "x"), "status", "--json")
	assert.NilError(t, err)
	assert.Equal(t, out, "[]\n")
	out, err = runDaemonGroup(t, fakeOpts(f, "x"), "status")
	assert.NilError(t, err)
	assert.Equal(t, out, "not running\n")
}

func TestDaemonRestartHandoverReturnsImmediately(t *testing.T) {
	f := &fakeDaemonControl{reachable: true, active: 2, handover: true}
	out, err := runDaemonGroup(t, fakeOpts(f, "x"), "restart", "--when-idle", "--timeout", "1ns")
	assert.NilError(t, err)
	assert.Equal(t, f.drains, 0)
	assert.Equal(t, f.pings, 0)
	assert.Assert(t, strings.Contains(out, "released its listener"))
}

func TestRevisionsDiffer(t *testing.T) {
	assert.Equal(t, revisionsDiffer("a", "a"), false)
	assert.Equal(t, revisionsDiffer("a", "b"), true)
	assert.Equal(t, revisionsDiffer("", ""), true, "unknown is not a match")
}

func TestDrainProgress(t *testing.T) {
	tests := []struct {
		name     string
		info     smeltd.PingInfo
		timedOut bool
		want     drainState
	}{
		{"still draining", smeltd.PingInfo{Reachable: true, Draining: true}, false, drainWaiting},
		{"gone", smeltd.PingInfo{}, false, drainExited},
		{"gone as the timeout lapses", smeltd.PingInfo{}, true, drainExited},
		{"timed out", smeltd.PingInfo{Reachable: true, Draining: true}, true, drainTimedOut},
		{"no longer draining", smeltd.PingInfo{Reachable: true}, false, drainCancelled},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, drainProgress(tc.info, tc.timedOut), tc.want)
		})
	}
}

func TestDaemonRestartWhenIdle(t *testing.T) {
	f := &fakeDaemonControl{reachable: true, exitAfter: 2}
	out, err := runDaemonGroup(t, fakeOpts(f, "x"), "restart", "--when-idle")
	assert.NilError(t, err)
	assert.Equal(t, f.drains, 1)
	assert.Assert(t, strings.Contains(out, "next smelt command starts it"), out)
}

func TestDaemonRestartWaitsForActiveRuns(t *testing.T) {
	f := &fakeDaemonControl{reachable: true, active: 2, exitAfter: 3}
	out, err := runDaemonGroup(t, fakeOpts(f, "x"), "restart", "--when-idle")
	assert.NilError(t, err)
	assert.Assert(t, strings.Contains(out, "waiting for 2 active run(s)"), out)
	assert.Assert(t, !f.cancelDrain)
}

func TestDaemonRestartTimeoutStopsDraining(t *testing.T) {
	f := &fakeDaemonControl{reachable: true, active: 1}
	_, err := runDaemonGroup(t, fakeOpts(f, "x"), "restart", "--when-idle", "--timeout", "20ms")
	var ue *userError
	assert.Assert(t, errors.As(err, &ue), "got %v", err)
	assert.Equal(t, ue.ErrorCode(), "daemon.restart_timeout")
	assert.Equal(t, ue.UserExitCode(), ExitTempFail)
	assert.Assert(t, f.cancelDrain, "draining was not stopped")
	assert.Assert(t, f.reachable, "the daemon must stay")
}

func TestDaemonRestartTwiceDrainsOnce(t *testing.T) {
	// The second call finds the daemon already draining and the server answers
	// the same; the command waits for it just as the first did.
	f := &fakeDaemonControl{reachable: true, active: 1, draining: true, exitAfter: 2}
	_, err := runDaemonGroup(t, fakeOpts(f, "x"), "restart", "--when-idle")
	assert.NilError(t, err)
	assert.Equal(t, f.drains, 1)
}

func TestDaemonRestartNotRunning(t *testing.T) {
	f := &fakeDaemonControl{}
	out, err := runDaemonGroup(t, fakeOpts(f, "x"), "restart", "--when-idle")
	assert.NilError(t, err)
	assert.Equal(t, f.drains, 0)
	assert.Assert(t, strings.Contains(out, "not running"), out)
}

func TestDaemonRestartWithoutWhenIdleIsRefused(t *testing.T) {
	f := &fakeDaemonControl{reachable: true}
	_, err := runDaemonGroup(t, fakeOpts(f, "x"), "restart")
	var ue *userError
	assert.Assert(t, errors.As(err, &ue), "got %v", err)
	assert.Equal(t, ue.UserExitCode(), ExitBadArgs)
	assert.Equal(t, f.drains, 0)
}

func TestDaemonRestartOfAnOldDaemonPointsToStop(t *testing.T) {
	f := &fakeDaemonControl{reachable: true, drainErr: smeltd.ErrDrainUnsupported}
	_, err := runDaemonGroup(t, fakeOpts(f, "x"), "restart", "--when-idle")
	var ue *userError
	assert.Assert(t, errors.As(err, &ue), "got %v", err)
	assert.Assert(t, strings.Contains(ue.Suggestion(), "stop --force"))
}

func TestDaemonStop(t *testing.T) {
	tests := []struct {
		name        string
		daemon      fakeDaemonControl
		args        []string
		wantStopped bool
		wantErr     bool
	}{
		{"idle", fakeDaemonControl{reachable: true}, []string{"stop"}, true, false},
		{"run active", fakeDaemonControl{reachable: true, active: 1}, []string{"stop"}, false, true},
		{"cannot tell", fakeDaemonControl{reachable: true, activeErr: errors.New("busy")}, []string{"stop"}, false, true},
		{"forced with a run active", fakeDaemonControl{reachable: true, active: 1}, []string{"stop", "--force"}, true, false},
		{"not running", fakeDaemonControl{}, []string{"stop"}, false, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := runDaemonGroup(t, fakeOpts(&tc.daemon, "x"), tc.args...)
			assert.Equal(t, err != nil, tc.wantErr, "%v", err)
			assert.Equal(t, len(tc.daemon.stopped) == 1, tc.wantStopped)
			if tc.wantErr {
				var ue *userError
				assert.Assert(t, errors.As(err, &ue))
				assert.Equal(t, ue.ErrorCode(), "daemon.stop_refused")
			}
		})
	}
}

func TestDaemonCommandsRefuseARemoteDaemon(t *testing.T) {
	t.Setenv("SMELT_DAEMON_REMOTE_ADDR", "127.0.0.1:9")
	f := &fakeDaemonControl{reachable: true}
	for _, args := range [][]string{{"status"}, {"restart", "--when-idle"}, {"stop"}} {
		_, err := runDaemonGroup(t, fakeOpts(f, "x"), args...)
		var ue *userError
		assert.Assert(t, errors.As(err, &ue), "%v: got %v", args, err)
		assert.Equal(t, ue.UserExitCode(), ExitBadArgs)
	}
	assert.Equal(t, f.drains, 0)
}

func TestDaemonGroupSkipsAutoLaunch(t *testing.T) {
	assert.Assert(t, noAutoLaunchCommands[daemonGroupName])
}

func TestRootVersionStartsWithTheVersion(t *testing.T) {
	root := newTestRootCmd()
	var out bytes.Buffer
	root.SetOut(&out)
	root.SetArgs([]string{"--version"})
	assert.NilError(t, root.Execute())
	assert.Assert(t, strings.HasPrefix(out.String(), "smelt version test"), out.String())
}

// A real daemon that is draining refuses a new run with the retryable code, and
// goes away once its last run ends.
func TestFactoryBuildAgainstADrainingDaemon(t *testing.T) {
	factoryProject(t)
	release := make(chan struct{})
	var started atomic.Bool
	cfg := fakeFactoryConfig(t, factory.ResultPassed)
	cfg.RunFactory = func(ctx context.Context, _ factory.RunOptions) (factory.Report, error) {
		started.Store(true)
		select {
		case <-release:
		case <-ctx.Done():
		}
		return factory.Report{}, ctx.Err()
	}
	startSessionDaemon(t, cfg)

	root, err := sessionProjectRoot(t.Context())
	assert.NilError(t, err)
	_, err = smeltd.StartFactory(smeltd.FactoryRequest{Backend: smeltd.BackendCircleCI, ProjectRoot: root, Prompt: "first"})
	assert.NilError(t, err)
	resp, err := smeltd.Drain()
	assert.NilError(t, err)
	assert.Equal(t, resp.Active, 1)

	_, stderr, err := runFactoryBuildCmd(t, "second")
	var ue *userError
	assert.Assert(t, errors.As(err, &ue), "got %v\n%s", err, stderr)
	assert.Equal(t, ue.ErrorCode(), smeltd.CodeDaemonDraining)
	assert.Equal(t, ue.UserExitCode(), 75)
	assert.Equal(t, ue.UserMessage(), "smelt is restarting to finish an upgrade after 1 active run(s); try again shortly.")

	_, _, err = runFactoryBuildCmd(t, "--json", "third")
	assert.Assert(t, errors.As(err, &ue), "got %v", err)
	assert.Equal(t, ue.ErrorCode(), "daemon.draining")

	close(release)
	deadline := time.Now().Add(10 * time.Second)
	for smeltd.IsDaemonRunning() {
		if time.Now().After(deadline) {
			t.Fatal("the draining daemon did not exit after its last run")
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestDaemonRestartRequesterCancellationLeavesDrainEnabled(t *testing.T) {
	f := &fakeDaemonControl{reachable: true, active: 2}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	command := newDaemonGroupCmdWith(fakeOpts(f, "new-build"))
	var out bytes.Buffer
	command.SetOut(&out)
	command.SetErr(&out)
	command.SetArgs([]string{"restart", "--when-idle"})
	command.SetContext(ctx)
	err := command.Execute()
	assert.Assert(t, errors.Is(err, context.Canceled), "got %v", err)
	assert.Equal(t, f.drains, 1)
	assert.Assert(t, f.draining && f.reachable)
	assert.Assert(t, !f.cancelDrain)
	assert.Equal(t, len(f.stopped), 0)
}

// A daemon at its machine limit, or a project at its own, refuses a new run
// with a retryable code and exit 75.
func TestFactoryBuildAgainstABusyDaemon(t *testing.T) {
	for _, tc := range []struct {
		name   string
		limits server.MachineLimits
		code   string
	}{
		{"machine", server.MachineLimits{MaxRuns: 1}, smeltd.CodeDaemonBusy},
		{"project", server.MachineLimits{}, smeltd.CodeProjectBusy},
	} {
		t.Run(tc.name, func(t *testing.T) {
			factoryProject(t)
			cfg := fakeFactoryConfig(t, factory.ResultPassed)
			cfg.Limits = tc.limits
			cfg.RunFactory = func(ctx context.Context, _ factory.RunOptions) (factory.Report, error) {
				<-ctx.Done()
				return factory.Report{}, ctx.Err()
			}
			startSessionDaemon(t, cfg)

			root, err := sessionProjectRoot(t.Context())
			assert.NilError(t, err)
			_, err = smeltd.StartFactory(smeltd.FactoryRequest{Backend: smeltd.BackendCircleCI, ProjectRoot: root, Prompt: "first"})
			assert.NilError(t, err)

			_, stderr, err := runFactoryBuildCmd(t, "second")
			var ue *userError
			assert.Assert(t, errors.As(err, &ue), "got %v\n%s", err, stderr)
			assert.Equal(t, ue.ErrorCode(), tc.code)
			assert.Equal(t, ue.UserExitCode(), 75)
		})
	}
}

func TestDaemonStatusTextShowsTheMachineLimit(t *testing.T) {
	st := smeltd.DaemonStatus{PID: 7, Revision: "abc", ActiveRuns: 5, MaxRuns: 5}
	assert.Assert(t, strings.Contains(daemonStatusText(st, "abc"), "runs: 5 of 5 (machine limit)"))
	st.MaxRuns = 0
	assert.Assert(t, !strings.Contains(daemonStatusText(st, "abc"), "machine limit"))
}

// The machine limits in the user config reach the daemon's review config.
func TestDaemonMachineLimitsComeFromUserConfig(t *testing.T) {
	dir := t.TempDir()
	t.Setenv(config.EnvXDGConfigHome, dir)
	assert.NilError(t, os.MkdirAll(filepath.Join(dir, "smelt"), 0o700))
	data := `{"factory":{"maxRuns":3}}`
	assert.NilError(t, os.WriteFile(filepath.Join(dir, "smelt", "config.json"), []byte(data), 0o600))

	// The combined builder is what RunE uses; it keeps the limits even when
	// no credential resolved.
	cfg, err := daemonFullReviewConfig(config.ResolvedConfig{}, errors.New("no config"))
	assert.NilError(t, err)
	assert.Equal(t, cfg.Limits.MaxRuns, 3)

	bad := `{"factory":{"maxRuns":-1}}`
	assert.NilError(t, os.WriteFile(filepath.Join(dir, "smelt", "config.json"), []byte(bad), 0o600))
	_, err = daemonFullReviewConfig(config.ResolvedConfig{}, nil)
	assert.ErrorContains(t, err, "maxRuns")
}
