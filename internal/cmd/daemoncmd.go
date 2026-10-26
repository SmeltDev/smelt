package cmd

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/spf13/cobra"

	"github.com/smeltdev/smelt/internal/iostream"
	"github.com/smeltdev/smelt/internal/smeltd"
	"github.com/smeltdev/smelt/internal/version"
)

// daemonGroupName is the visible command group that manages the daemon. The
// daemon itself runs as the hidden smeltd.DaemonSubcommand.
const daemonGroupName = "daemon"

// drainPollInterval is how often restart looks for the draining daemon to go.
const drainPollInterval = time.Second

// daemonControl is what the daemon commands need of a daemon. The real one
// talks to the local daemon; tests supply a fake.
type daemonControl interface {
	// Ping reports what the daemon says about itself.
	Ping() (smeltd.PingInfo, error)
	Handover() (smeltd.DrainResponse, error)
	Daemons() ([]smeltd.DaemonStatus, error)
	// Pid is the daemon's process ID, false when none is running.
	Pid() (int, bool, error)
	// Active is the number of factory runs in progress.
	Active() (int, error)
	Drain() (smeltd.DrainResponse, error)
	CancelDrain() (smeltd.DrainResponse, error)
	Stop(pid int) error
}

// localDaemon is the daemon on this machine.
type localDaemon struct{}

func (localDaemon) Ping() (smeltd.PingInfo, error) { return smeltd.Status() }

func (localDaemon) Pid() (int, bool, error) {
	path, err := smeltd.PIDPath()
	if err != nil {
		return 0, false, err
	}
	running, pid, err := smeltd.IsRunning(path)
	return pid, running, err
}

func (localDaemon) Handover() (smeltd.DrainResponse, error) { return smeltd.Handover() }
func (localDaemon) Daemons() ([]smeltd.DaemonStatus, error) { return smeltd.DiscoverDaemons() }

func (localDaemon) Active() (int, error) {
	sessions, err := smeltd.MainSessions("")
	if err != nil {
		return 0, err
	}
	n := 0
	for _, s := range sessions {
		if s.Factory != nil && !s.State.Finished() {
			n++
		}
	}
	return n, nil
}

func (localDaemon) Drain() (smeltd.DrainResponse, error)       { return smeltd.Drain() }
func (localDaemon) CancelDrain() (smeltd.DrainResponse, error) { return smeltd.CancelDrain() }
func (localDaemon) Stop(pid int) error                         { return smeltd.Stop(pid) }

// daemonOptions are the dependencies of the daemon commands.
type daemonOptions struct {
	Daemon daemonControl
	// Build is this binary's build.
	Build version.Build
	// PollInterval is how often restart checks on a draining daemon.
	PollInterval time.Duration
}

func newDaemonGroupCmd() *cobra.Command {
	return newDaemonGroupCmdWith(daemonOptions{Daemon: localDaemon{}, Build: version.Current(), PollInterval: drainPollInterval})
}

// newDaemonGroupCmdWith is the group over opts, so tests can supply a daemon.
func newDaemonGroupCmdWith(opts daemonOptions) *cobra.Command {
	cmd := &cobra.Command{
		Use:   daemonGroupName,
		Short: "Inspect and restart the smelt daemon",
		Long: `Inspect and restart the smelt daemon.

A running daemon keeps the code it started with. After upgrading smelt,
'smelt daemon restart --when-idle' releases the main listener while existing
runs finish on their original daemon. The next command starts the new build.`,
		RunE: groupRunE,
	}
	cmd.AddCommand(newDaemonStatusCmd(opts), newDaemonRestartCmd(opts), newDaemonStopCmd(opts))
	return cmd
}

func newDaemonStatusCmd(opts daemonOptions) *cobra.Command {
	var jsonOut bool
	cmd := &cobra.Command{
		Use:   statusCmdName,
		Short: "Show all local daemons, their revisions and active runs",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if err := requireLocalDaemon("smelt daemon"); err != nil {
				return err
			}
			statuses, err := opts.Daemon.Daemons()
			if err != nil {
				return err
			}
			streams := iostream.FromCmd(cmd)
			if jsonOut {
				return iostream.PrintJSON(streams.Out, statuses)
			}
			if len(statuses) == 0 {
				streams.Println("not running")
			}
			for _, st := range statuses {
				streams.Printf("%s", daemonStatusText(st, opts.Build.Label()))
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&jsonOut, "json", false, "Print the status as JSON")
	return cmd
}

func newDaemonRestartCmd(opts daemonOptions) *cobra.Command {
	var (
		whenIdle bool
		timeout  time.Duration
	)
	cmd := &cobra.Command{
		Use:   "restart --when-idle",
		Short: "Release the main listener while active runs finish",
		Long: `Hand over the main listener while active runs finish on their original daemon.
Returns after ownership is released. The next command starts the new build.

Older daemons without handover support drain and wait for active runs. For those
builds, --timeout cancels draining if runs remain; the default waits indefinitely.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if !whenIdle {
				return newUserError("Restarting without waiting would cancel active runs.").
					withCode("command.invalid_flags").
					withExitCode(ExitBadArgs).
					withSuggestion("Use 'smelt daemon restart --when-idle', or 'smelt daemon stop --force' to stop it now.").
					withoutDetail()
			}
			if err := requireLocalDaemon("smelt daemon"); err != nil {
				return err
			}
			return runDaemonRestart(cmd.Context(), iostream.FromCmd(cmd), opts, timeout)
		},
	}
	cmd.Flags().BoolVar(&whenIdle, "when-idle", false, "Release the listener while existing runs finish")
	cmd.Flags().DurationVar(&timeout, "timeout", 0, "For legacy daemons, cancel draining if runs remain after this long (default: no limit)")
	return cmd
}

func newDaemonStopCmd(opts daemonOptions) *cobra.Command {
	var force bool
	cmd := &cobra.Command{
		Use:   "stop",
		Short: "Stop the daemon",
		Long: `Stop the daemon. It is left running while a factory run is active, or when that
cannot be told, because stopping it cancels its runs. --force stops it anyway.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if err := requireLocalDaemon("smelt daemon"); err != nil {
				return err
			}
			return runDaemonStop(iostream.FromCmd(cmd), opts, force)
		},
	}
	cmd.Flags().BoolVar(&force, "force", false, "Stop the daemon even if factory runs are active (cancels them)")
	return cmd
}

// revisionsDiffer treats an unknown revision as a mismatch.
func revisionsDiffer(daemonRev, binaryRev string) bool {
	return daemonRev == "" || daemonRev != binaryRev
}

func daemonStatusText(st smeltd.DaemonStatus, binary string) string {
	state, gap := "running", "  "
	if st.Draining {
		state, gap = "draining", " "
	}
	out := fmt.Sprintf("%s (pid %d)%srevision %s", state, st.PID, gap, orUnknown(st.Revision))
	if !revisionsDiffer(st.Revision, binary) {
		out += " (this binary)"
	}
	out += fmt.Sprintf(" · active runs: %d", st.ActiveRuns)
	if st.Draining {
		out += " · exits when they finish"
	}
	return out + "\n"
}

// drainState is what a poll of a draining daemon means.
type drainState int

const (
	drainWaiting drainState = iota
	// drainExited: the daemon has gone, so the next command starts a new one.
	drainExited
	// drainCancelled: a daemon answers that is not draining; someone cancelled
	// the drain or replaced the daemon.
	drainCancelled
	drainTimedOut
)

// drainProgress decides what a poll of the daemon means. A daemon that is gone
// wins over an expired timeout: that is the outcome asked for.
func drainProgress(info smeltd.PingInfo, timedOut bool) drainState {
	switch {
	case !info.Reachable:
		return drainExited
	case !info.Draining:
		return drainCancelled
	case timedOut:
		return drainTimedOut
	}
	return drainWaiting
}

func runDaemonRestart(ctx context.Context, streams iostream.Streams, opts daemonOptions, timeout time.Duration) error {
	info, err := opts.Daemon.Ping()
	if err != nil {
		return fmt.Errorf("ask the daemon: %w", err)
	}
	if !info.Reachable {
		streams.Println("The smelt daemon is not running; the next smelt command starts it from the binary on disk.")
		return nil
	}
	_, err = opts.Daemon.Handover()
	if err == nil {
		streams.Println("The daemon released its listener; the next smelt command starts it from the binary on disk. Existing runs finish on their original daemon.")
		return nil
	}
	if !errors.Is(err, smeltd.ErrHandoverUnsupported) {
		return sessionError(err)
	}
	resp, err := opts.Daemon.Drain()
	if errors.Is(err, smeltd.ErrDrainUnsupported) {
		return newUserError("This smelt daemon is too old to drain.").
			withCode("daemon.drain_unsupported").
			withSuggestion("Stop it once with 'smelt daemon stop --force' (cancels active runs); later restarts can drain.").
			withoutDetail()
	}
	if err != nil {
		return sessionError(err)
	}
	if resp.Active > 0 {
		streams.Printf("Draining: no new factory runs; waiting for %d active run(s) to finish...\n", resp.Active)
	}
	return awaitDrain(ctx, streams, opts, timeout)
}

// awaitDrain polls the draining daemon until it exits or the timeout lapses.
func awaitDrain(ctx context.Context, streams iostream.Streams, opts daemonOptions, timeout time.Duration) error {
	start := time.Now()
	ticker := time.NewTicker(opts.PollInterval)
	defer ticker.Stop()
	for {
		info, err := opts.Daemon.Ping()
		if err != nil {
			return fmt.Errorf("ask the daemon: %w", err)
		}
		timedOut := timeout > 0 && time.Since(start) >= timeout
		switch drainProgress(info, timedOut) {
		case drainExited:
			streams.Println("The smelt daemon has exited; the next smelt command starts it from the binary on disk.")
			return nil
		case drainCancelled:
			return newUserError("The daemon stopped draining before it exited.").withCode("daemon.drain_cancelled").withoutDetail()
		case drainTimedOut:
			return giveUpDraining(streams, opts, timeout)
		case drainWaiting:
		}
		select {
		case <-ctx.Done():
			// Leaving the daemon draining is deliberate: the drain was asked for
			// and nothing here undoes it; DELETE /drain does.
			return fmt.Errorf("wait for the daemon: %w", ctx.Err())
		case <-ticker.C:
		}
	}
}

// giveUpDraining ends the drain after the timeout. A daemon that exits just
// before the request lands has reached the state asked for.
func giveUpDraining(streams iostream.Streams, opts daemonOptions, timeout time.Duration) error {
	if _, err := opts.Daemon.CancelDrain(); err != nil {
		if info, pingErr := opts.Daemon.Ping(); pingErr == nil && !info.Reachable {
			streams.Println("The smelt daemon has exited; the next smelt command starts it from the binary on disk.")
			return nil
		}
		return fmt.Errorf("stop draining: %w", err)
	}
	n, _ := opts.Daemon.Active()
	return newUserError(fmt.Sprintf("%d factory run(s) were still active after %s; the daemon keeps running and accepts runs again.", n, timeout)).
		withCode("daemon.restart_timeout").
		withExitCode(ExitTempFail).
		withSuggestion("Try again when they finish, or use 'smelt daemon stop --force' to cancel them.").
		withoutDetail()
}

func runDaemonStop(streams iostream.Streams, opts daemonOptions, force bool) error {
	pid, running, err := opts.Daemon.Pid()
	if err != nil {
		return fmt.Errorf("read the daemon's pid: %w", err)
	}
	if !running {
		streams.Println("The smelt daemon is not running.")
		return nil
	}
	if !force {
		if err := checkIdle(opts.Daemon); err != nil {
			return err
		}
	}
	if err := opts.Daemon.Stop(pid); err != nil {
		return fmt.Errorf("stop the daemon: %w", err)
	}
	streams.Println("The smelt daemon stopped; the next smelt command starts it from the binary on disk.")
	return nil
}

// checkIdle refuses when a factory run is active, or when that cannot be told:
// stopping the daemon cancels its runs, so a missing answer is not a yes.
func checkIdle(d daemonControl) error {
	active, err := d.Active()
	switch {
	case err != nil:
		return newUserError("The smelt daemon was left running: could not tell whether a factory run is active.").
			withCode("daemon.stop_refused").
			withExitCode(ExitTempFail).
			withSuggestion("Use 'smelt daemon restart --when-idle', or 'smelt daemon stop --force' to stop it anyway.").
			wrap(err)
	case active > 0:
		return newUserError(fmt.Sprintf("The smelt daemon was left running: %d factory run(s) active.", active)).
			withCode("daemon.stop_refused").
			withExitCode(ExitTempFail).
			withSuggestion("Use 'smelt daemon restart --when-idle', or 'smelt daemon stop --force' to cancel them.").
			withoutDetail()
	}
	return nil
}

// drainingError is the refusal of a new run by a daemon that is draining. It is
// retryable, so it has its own exit code and code for tools to act on.
func drainingError(active int) *userError {
	return newUserError(fmt.Sprintf("smelt is restarting to finish an upgrade after %d active run(s); try again shortly.", active)).
		withCode(smeltd.CodeDaemonDraining).
		withExitCode(ExitTempFail).
		withoutDetail()
}
