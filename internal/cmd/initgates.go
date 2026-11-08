package cmd

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"
	"sync"

	"github.com/smeltdev/smelt/internal/brand"
	"github.com/smeltdev/smelt/internal/config"
	"github.com/smeltdev/smelt/internal/iostream"
	"github.com/smeltdev/smelt/internal/ui"
	"github.com/smeltdev/smelt/internal/validate"
)

// gateTailLines is how many trailing output lines init shows for a failing gate.
const gateTailLines = 15

// gateFailure is the first gate that fails today, with the end of its output.
type gateFailure struct {
	Name string
	Run  string
	Tail string
}

// gatesOnly copies cfg keeping just the gate commands: the dry-run must not
// run install (setup) or autofix commands (which rewrite files).
func gatesOnly(cfg *config.ProjectConfig) *config.ProjectConfig {
	out := *cfg
	out.Commands = slices.DeleteFunc(slices.Clone(cfg.Commands), func(c config.Command) bool {
		return c.Role != config.RoleGate || c.IsInstall()
	})
	return &out
}

// tailWriter keeps the last bytes written to it, so a failing gate's final
// lines can be shown without buffering a whole test run.
type tailWriter struct {
	mu  sync.Mutex
	buf []byte
}

const tailWriterCap = 16 * 1024

func (t *tailWriter) Write(p []byte) (int, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.buf = append(t.buf, p...)
	if len(t.buf) > tailWriterCap {
		t.buf = t.buf[len(t.buf)-tailWriterCap:]
	}
	return len(p), nil
}

// lines returns the last n non-empty lines written.
func (t *tailWriter) lines(n int) string {
	t.mu.Lock()
	defer t.mu.Unlock()
	all := strings.Split(strings.TrimRight(string(bytes.TrimSpace(t.buf)), "\n"), "\n")
	return strings.Join(all[max(0, len(all)-n):], "\n")
}

// dryRunGates runs each gate once locally, with its own timeout, and returns
// the first that fails. It returns nil when every gate passes or there are none.
// Gate output is captured, not echoed; only the status lines reach the user.
func dryRunGates(ctx context.Context, workDir string, cfg *config.ProjectConfig, streams iostream.Streams) *gateFailure {
	gates := gatesOnly(cfg)
	if !gates.HasCommands() {
		return nil
	}
	tail := &tailWriter{}
	captured := iostream.Streams{Out: io.MultiWriter(tail), Err: tail}
	_, err := validate.RunAll(ctx, workDir, gates, nil, newStatusFunc(streams), captured)
	if err == nil {
		return nil
	}
	var failed *validate.CheckFailedError
	if !errors.As(err, &failed) {
		// A timeout or a command that could not start is still a gate that
		// cannot pass today; the error text stands in for its output.
		name := failedGateName(gates, err)
		return &gateFailure{Name: name, Run: commandRun(gates, name), Tail: joinNonEmpty(tail.lines(gateTailLines), err.Error())}
	}
	return &gateFailure{Name: failed.Name, Run: commandRun(gates, failed.Name), Tail: tail.lines(gateTailLines)}
}

func joinNonEmpty(parts ...string) string {
	return strings.Join(slices.DeleteFunc(parts, func(s string) bool { return s == "" }), "\n")
}

// failedGateName finds the gate an error names; errors from validate start with it.
func failedGateName(cfg *config.ProjectConfig, err error) string {
	for _, c := range cfg.Commands {
		if strings.HasPrefix(err.Error(), c.Name) {
			return c.Name
		}
	}
	return ""
}

func commandRun(cfg *config.ProjectConfig, name string) string {
	if c := cfg.FindCommand(name); c != nil {
		return c.Run
	}
	return ""
}

// hooksNotInstalledMessage is what init prints when a gate fails today.
func hooksNotInstalledMessage(gate string) string {
	return fmt.Sprintf("hooks not installed: %s fails today; fix it, then run %s init --hooks-only", gate, brand.Command)
}

// printGateFailure shows the failing command, the end of its output, and why
// no hook was installed.
func printGateFailure(f *gateFailure, streams iostream.Streams) {
	streams.ErrPrintf("%s\n", ui.Warning(fmt.Sprintf("Gate %s failed: %s", f.Name, f.Run)))
	if f.Tail != "" {
		streams.ErrPrintln(ui.Dim(f.Tail))
	}
	streams.ErrPrintln(ui.Warning(hooksNotInstalledMessage(f.Name)))
}

// hookInstall says what init writes once the gates pass.
type hookInstall struct {
	GitCommonDir string
	SkipGitHook  bool
}

// installHooksIfGatesPass dry-runs the gates and, only when they pass, writes
// the agent hook files and the git pre-commit hook. A hook that blocks every
// commit is worse than none, so a failing gate leaves them all uninstalled.
func installHooksIfGatesPass(ctx context.Context, workDir string, cfg *config.ProjectConfig, opts hookInstall, streams iostream.Streams) error {
	// The pre-commit hook runs smelt validate, which errors with nothing to
	// run, so without a gate it would block every commit too.
	if !gatesOnly(cfg).HasCommands() {
		streams.ErrPrintln(ui.Dim("hooks not installed: no gate commands to run"))
		return nil
	}
	if failure := dryRunGates(ctx, workDir, cfg, streams); failure != nil {
		printGateFailure(failure, streams)
		return nil
	}
	if err := writeAllHookFiles(workDir, cfg.Commands, streams); err != nil {
		return err
	}
	if opts.SkipGitHook {
		return nil
	}
	return writeGitHook(opts.GitCommonDir, streams)
}

// runInitHooksOnly installs the hooks for a config that init already wrote,
// for the second run after a failing gate was fixed.
func runInitHooksOnly(ctx context.Context, workDir string, cfg *config.ProjectConfig, loadErr error, opts hookInstall, streams iostream.Streams) error {
	if loadErr != nil || !cfg.HasCommands() {
		return &userError{msg: fmt.Sprintf("No commands in .smelt/config.json; run %s init first", brand.Command)}
	}
	return installHooksIfGatesPass(ctx, workDir, cfg, opts, streams)
}
