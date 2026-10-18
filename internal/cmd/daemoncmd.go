package cmd

import (
	"context"
	"encoding/json"
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

func (localDaemon) Active() (int, error) {
	sessions, err := smeltd.ListSessions("")
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
'smelt daemon restart --when-idle' lets it finish its factory runs, refuses new
ones meanwhile, and exits; the next smelt command starts the daemon from the
binary now on disk.`,
		RunE: groupRunE,
	}
	cmd.AddCommand(newDaemonStatusCmd(opts), newDaemonRestartCmd(opts), newDaemonStopCmd(opts))
	return cmd
}

func newDaemonStatusCmd(opts daemonOptions) *cobra.Command {
	var jsonOut bool
	cmd := &cobra.Command{
		Use:   statusCmdName,
		Short: "Show the daemon's pid, its revision against this binary's, and active runs",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if err := requireLocalDaemon("smelt daemon"); err != nil {
				return err
			}
			st, err := readDaemonStatus(opts)
			if err != nil {
				return err
			}
			streams := iostream.FromCmd(cmd)
			if jsonOut {
				return printDaemonStatusJSON(streams, st)
			}
			streams.Printf("%s", st.text())
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
		Short: "Let active runs finish, then restart the daemon on the binary on disk",
		Long: `Put the daemon in drain mode: it accepts no new factory runs, lets the active
ones finish, then exits. The next smelt command starts the daemon from the
binary now on disk. With no active runs it exits at once.

If runs are still active after --timeout, the daemon keeps running and stops
draining; runs are never killed. The default is to wait as long as it takes.`,
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
	cmd.Flags().BoolVar(&whenIdle, "when-idle", false, "Drain: finish active runs, accept no new ones, then restart")
	cmd.Flags().DurationVar(&timeout, "timeout", 0, "Stop draining and leave the daemon running if runs are still active after this long (default: no limit)")
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

// daemonStatus is what 'daemon status' reports.
type daemonStatus struct {
	Running  bool   `json:"running"`
	Pid      int    `json:"pid,omitempty"`
	Draining bool   `json:"draining,omitempty"`
	Active   *int   `json:"active_runs,omitempty"`
	Revision string `json:"revision,omitempty"`
	// BinaryRevision is this binary's.
	BinaryRevision string `json:"binary_revision,omitempty"`
	Differs        bool   `json:"differs,omitempty"`
}

// revisionsDiffer reports whether a daemon runs a different build than the
// binary. A daemon that reports no revision is one that predates the report, so
// it cannot be shown to match.
func revisionsDiffer(daemonRev, binaryRev string) bool {
	return daemonRev == "" || daemonRev != binaryRev
}

func readDaemonStatus(opts daemonOptions) (daemonStatus, error) {
	info, err := opts.Daemon.Ping()
	if err != nil {
		return daemonStatus{}, fmt.Errorf("ask the daemon: %w", err)
	}
	if !info.Reachable {
		return daemonStatus{}, nil
	}
	pid, _, err := opts.Daemon.Pid()
	if err != nil {
		return daemonStatus{}, fmt.Errorf("read the daemon's pid: %w", err)
	}
	st := daemonStatus{
		Running:        true,
		Pid:            pid,
		Draining:       info.Draining,
		Revision:       info.Revision,
		BinaryRevision: opts.Build.Label(),
		Differs:        revisionsDiffer(info.Revision, opts.Build.Label()),
	}
	// Active runs are best-effort: a busy daemon that is slow to list them still
	// has a pid and a revision worth showing.
	if n, err := opts.Daemon.Active(); err == nil {
		st.Active = &n
	}
	return st, nil
}

func (st daemonStatus) text() string {
	if !st.Running {
		return "not running\n"
	}
	out := fmt.Sprintf("running (pid %d)\n", st.Pid)
	out += fmt.Sprintf("daemon revision:  %s\nbinary revision:  %s\n", orUnknown(st.Revision), orUnknown(st.BinaryRevision))
	if st.Differs {
		out += "The daemon runs a different build than this binary; restart it with 'smelt daemon restart --when-idle'.\n"
	}
	if st.Draining {
		out += "draining: finishing active runs, then exiting\n"
	}
	if st.Active != nil {
		out += fmt.Sprintf("active runs:      %d\n", *st.Active)
	}
	return out
}

func printDaemonStatusJSON(streams iostream.Streams, st daemonStatus) error {
	// differs is always present so a tool need not infer false from absence.
	doc := struct {
		daemonStatus
		Differs bool `json:"differs"`
	}{st, st.Differs}
	raw, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return fmt.Errorf("encode status: %w", err)
	}
	streams.Println(string(raw))
	return nil
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
