package factory

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/smeltdev/smelt/internal/brand"
	"github.com/smeltdev/smelt/internal/config"
	"github.com/smeltdev/smelt/internal/iostream"
	"github.com/smeltdev/smelt/internal/quality"
	"github.com/smeltdev/smelt/internal/review"
	"github.com/smeltdev/smelt/internal/sarif"
	"github.com/smeltdev/smelt/internal/sidecar"
)

const (
	// sarifOutputLimit bounds the SARIF a tool may print; truncated JSON would
	// not parse, so output past it fails the gate instead.
	sarifOutputLimit = 64 << 20
	// sarifReadTimeout bounds reading back and removing a SARIF file.
	sarifReadTimeout = 2 * time.Minute
	// ratchetWorktree is where the baseline is checked out when the in-place
	// baseline run failed and has to be made again.
	ratchetWorktree = "/tmp/" + brand.Command + "-ratchet-rev"
)

// invalidOutputError means a tool ran but its output is not usable SARIF.
type invalidOutputError struct{ err error }

func (e invalidOutputError) Error() string { return e.err.Error() }
func (e invalidOutputError) Unwrap() error { return e.err }

// Ratchet runs the gates that print SARIF in the implementer's sandbox. It
// compares each round's findings with those of the baseline, so a gate fails
// only on what the change added.
type Ratchet struct {
	Exec  review.Execer
	Entry *sidecar.PoolEntry
	// Warn, when set, hears of a baseline that could not be made.
	Warn iostream.StatusFunc

	mu sync.Mutex
	// rev is the baseline commit on Entry.
	rev string
	// base is the baseline's annotated findings by command name.
	base map[string][]sarif.Finding
}

// SarifCommands picks the commands that print SARIF.
func SarifCommands(cmds []config.Command) []config.Command {
	return slices.DeleteFunc(slices.Clone(cmds), func(c config.Command) bool { return c.Format != config.FormatSARIF })
}

// MeasureBaseline runs each SARIF command on the sandbox as it is now, which
// is the baseline commit rev, and keeps the findings for every round. It must
// run before the implementer edits anything, so dependencies installed in the
// repo are there. A failure is warned and not kept: the first round tries
// again from a worktree of rev.
func (r *Ratchet) MeasureBaseline(ctx context.Context, rev string, cmds []config.Command) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.rev = rev
	// Findings kept from an earlier commit must not stand in for this one.
	r.base = nil
	for _, c := range SarifCommands(cmds) {
		found, err := r.baselineInPlace(ctx, c)
		if err != nil {
			r.warn(fmt.Sprintf("the baseline findings of %s were not collected: %v (it is tried again after the first round)", c.Name, err))
			continue
		}
		r.store(c.Name, found)
	}
}

// baselineInPlace collects c's findings in the repo, within the gate's timeout
// so a hung tool cannot hold up Prepare.
func (r *Ratchet) baselineInPlace(ctx context.Context, c config.Command) ([]sarif.Finding, error) {
	ctx, cancel := context.WithTimeout(ctx, commandTimeout(c))
	defer cancel()
	found, _, err := r.collect(ctx, c, r.Entry.RepoPath)
	return found, err
}

func (r *Ratchet) store(name string, found []sarif.Finding) {
	if r.base == nil {
		r.base = map[string][]sarif.Finding{}
	}
	r.base[name] = found
}

func (r *Ratchet) warn(msg string) {
	if r.Warn != nil {
		r.Warn(iostream.LevelWarn, msg)
	}
}

// Check runs c on the sandbox's current tree and judges it by the findings the
// baseline did not have. The command's exit code is not looked at.
func (r *Ratchet) Check(ctx context.Context, c config.Command) Check {
	timeout := commandTimeout(c)
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	start := time.Now()
	id := r.Entry.ID

	after, stderr, err := r.collect(ctx, c, r.Entry.RepoPath)
	var invalid invalidOutputError
	switch {
	case ctx.Err() == context.DeadlineExceeded:
		return FromCommand(c.Name, id, -1, stderr+fmt.Sprintf("\n(timed out after %s)", timeout), time.Since(start), nil)
	case errors.As(err, &invalid):
		return FromSarifError(c.Name, id, err, stderr, time.Since(start))
	case err != nil:
		return FromCommand(c.Name, id, 0, "", time.Since(start), err)
	}
	before, err := r.baseline(ctx, c)
	if err != nil {
		return FromCommand(c.Name, id, 0, "", time.Since(start),
			fmt.Errorf("no baseline findings to compare with: %w", err))
	}
	return FromSarif(c.Name, id, sarif.Compare(before, after), time.Since(start))
}

// baseline is the findings of the commit the run started from. They are made
// once, normally by MeasureBaseline; ones that failed are made here, from the
// commit in a temporary worktree, since the tree has moved on.
func (r *Ratchet) baseline(ctx context.Context, c config.Command) ([]sarif.Finding, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if found, ok := r.base[c.Name]; ok {
		return found, nil
	}
	if r.rev == "" {
		return nil, errors.New("no baseline commit")
	}
	found, err := r.collectAt(ctx, c)
	if err != nil {
		return nil, err
	}
	r.store(c.Name, found)
	return found, nil
}

// collectAt collects c's findings on the baseline commit checked out in a
// temporary worktree, which is removed on every path.
func (r *Ratchet) collectAt(ctx context.Context, c config.Command) ([]sarif.Finding, error) {
	ctx, cancelRun := context.WithTimeout(ctx, commandTimeout(c))
	defer cancelRun()
	repo, wt := sidecar.ShellEscape(r.Entry.RepoPath), sidecar.ShellEscape(ratchetWorktree)
	defer func() {
		cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), worktreeCleanupTimeout)
		defer cancel()
		_, _ = review.RunScript(cleanup, r.Exec, r.Entry, removeWorktreeScript(repo, wt), 4<<10)
	}()
	setup := removeWorktreeScript(repo, wt) + "git worktree add --detach -q " + wt + " " + sidecar.ShellEscape(r.rev)
	if _, err := review.RunScript(ctx, r.Exec, r.Entry, setup, 4<<10); err != nil {
		return nil, fmt.Errorf("check out %s: %w", r.rev, err)
	}
	found, _, err := r.collect(ctx, c, ratchetWorktree)
	return found, err
}

// collect runs c in dir, parses its SARIF and annotates the findings with the
// text of the lines they flag. stderr is what the tool printed on its error
// stream, for diagnosing a failure.
func (r *Ratchet) collect(ctx context.Context, c config.Command, dir string) (found []sarif.Finding, stderr string, err error) {
	if c.SarifPath != "" {
		defer r.removeFile(ctx, dir, c.SarifPath)
	}
	stdout, stderr, err := r.runTool(ctx, c, dir)
	if err != nil {
		return nil, stderr, err
	}
	if c.SarifPath != "" {
		if stdout, err = r.readFile(ctx, dir, c.SarifPath); err != nil {
			return nil, stderr, invalidOutputError{err}
		}
	}
	found, err = sarif.Parse([]byte(stdout), dir)
	if err != nil {
		return nil, stderr, invalidOutputError{err}
	}
	src, err := r.fetchLines(ctx, dir, found)
	if err != nil {
		return nil, stderr, err
	}
	sarif.Annotate(found, src)
	return found, stderr, nil
}

// runTool runs the command with its streams kept apart: warnings on stderr
// must not corrupt the JSON on stdout. collect removes the SARIF file the tool
// writes, also when the command timed out, so the change never contains it.
func (r *Ratchet) runTool(ctx context.Context, c config.Command, dir string) (stdout, stderr string, err error) {
	var out, errOut strings.Builder
	tooBig := false
	script := "cd " + sidecar.ShellEscape(dir) + " || exit 1\n"
	if c.SarifPath != "" {
		script += "rm -f " + sidecar.ShellEscape(c.SarifPath) + "\n"
	}
	script += c.Run
	_, err = r.Exec(ctx, r.Entry, script, nil, func(stream string, data []byte) {
		if stream == iostream.StreamStderr {
			if room := maxCommandOutput - errOut.Len(); room > 0 {
				errOut.Write(data[:min(len(data), room)])
			}
			return
		}
		if out.Len()+len(data) > sarifOutputLimit {
			tooBig = true
			return
		}
		out.Write(data)
	}, nil)
	switch {
	case err != nil:
		return "", errOut.String(), err
	case tooBig:
		return "", errOut.String(), invalidOutputError{fmt.Errorf("the output is larger than %d bytes", sarifOutputLimit)}
	}
	return out.String(), errOut.String(), nil
}

// readFile prints the SARIF file at rel under dir and removes it.
func (r *Ratchet) readFile(ctx context.Context, dir, rel string) (string, error) {
	p := sidecar.ShellEscape(rel)
	script := "cd " + sidecar.ShellEscape(dir) + " && cat " + p + "; rc=$?; rm -f " + p + "; exit $rc"
	out, err := review.RunScript(ctx, r.Exec, r.Entry, script, sarifOutputLimit)
	if err != nil {
		return "", fmt.Errorf("read sarifPath %s: %w", rel, err)
	}
	return out, nil
}

func (r *Ratchet) removeFile(ctx context.Context, dir, rel string) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), sarifReadTimeout)
	defer cancel()
	script := "cd " + sidecar.ShellEscape(dir) + " && rm -f " + sidecar.ShellEscape(rel)
	_, _ = review.RunScript(ctx, r.Exec, r.Entry, script, 4<<10)
}

// fetchLines reads back the files the findings flag from dir.
func (r *Ratchet) fetchLines(ctx context.Context, dir string, found []sarif.Finding) (fileLines, error) {
	var names []string
	for _, f := range found {
		inside := f.File != "" && f.Line > 0 && !strings.HasPrefix(f.File, "/") && f.File != ".." &&
			!strings.HasPrefix(f.File, "../") && !strings.Contains(f.File, "\n")
		if inside && !slices.Contains(names, f.File) {
			names = append(names, f.File)
		}
	}
	if len(names) == 0 {
		return nil, nil
	}
	out, err := review.RunScript(ctx, r.Exec, r.Entry, sourcesScript(dir, names), sourceOutputLimit)
	if err != nil {
		return nil, fmt.Errorf("read the flagged files back: %w", err)
	}
	data, err := base64.StdEncoding.DecodeString(strings.TrimSpace(out))
	if err != nil {
		return nil, fmt.Errorf("read the flagged files back: %w", err)
	}
	files, _, err := quality.ReadTar(data, quality.MaxSourceBytes)
	if err != nil {
		return nil, fmt.Errorf("read the flagged files back: %w", err)
	}
	lines := fileLines{}
	for _, f := range files {
		lines[f.Path] = strings.Split(string(f.Content), "\n")
	}
	return lines, nil
}

// fileLines is the lines of files by path. It implements sarif.LineSource.
type fileLines map[string][]string

// Line implements sarif.LineSource.
func (l fileLines) Line(file string, n int) (string, bool) {
	lines := l[file]
	if n < 1 || n > len(lines) {
		return "", false
	}
	return strings.TrimSuffix(lines[n-1], "\r"), true
}

// commandTimeout is the time c may run: its own, else the default.
func commandTimeout(c config.Command) time.Duration {
	if c.Timeout > 0 {
		return time.Duration(c.Timeout) * time.Second
	}
	return defaultCommandTimeout
}
