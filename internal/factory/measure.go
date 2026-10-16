package factory

import (
	"cmp"
	"context"
	"encoding/base64"
	"fmt"
	"strings"
	"time"

	"github.com/smeltdev/smelt/internal/brand"
	"github.com/smeltdev/smelt/internal/quality"
	"github.com/smeltdev/smelt/internal/quality/lizard"
	"github.com/smeltdev/smelt/internal/review"
	"github.com/smeltdev/smelt/internal/sidecar"
)

// ProviderFor is the metrics provider cfg selects: the project's own command,
// or lizard.
func ProviderFor(cfg quality.Config) quality.Provider {
	if cfg.Command != "" {
		return quality.CommandProvider(cfg.Command)
	}
	// An invalid glob was rejected when the config loaded; the defaults still
	// apply if one gets here.
	ex, _ := quality.NewExcluder(cfg.Exclude)
	return lizard.Provider(ex)
}

const (
	// providerTimeout bounds one run of a provider.
	providerTimeout = 10 * time.Minute
	// providerOutputLimit bounds what a provider may print.
	providerOutputLimit = 64 << 20
	// sourceOutputLimit bounds the base64 archive of sources: the content cap,
	// room for tar's headers, and base64's growth.
	sourceOutputLimit = (quality.MaxSourceBytes + 8<<20) * 4 / 3
	// worktreeCleanupTimeout bounds removing the temporary worktree, which runs
	// even when the measurement was cancelled.
	worktreeCleanupTimeout = 30 * time.Second
	// revWorktree is where a revision is checked out to be measured.
	revWorktree = "/tmp/" + brand.Command + "-quality-rev"
)

// SandboxMeasurer measures a tree in a sandbox: it runs the provider there
// and reads back the sources the contract names, which only the sandbox is
// sure to have. It implements quality.Measurer.
type SandboxMeasurer struct {
	Exec     review.Execer
	Entry    *sidecar.PoolEntry
	Provider quality.Provider
	Config   quality.Config
	// Worktree is where a revision is checked out; revWorktree when empty.
	Worktree string
}

// Measure measures the sandbox's working tree, or with a rev the tree at that
// commit, which is checked out in a temporary git worktree that is removed on
// every path.
func (m SandboxMeasurer) Measure(ctx context.Context, rev string) (quality.Measurement, error) {
	if rev == "" {
		return m.measureIn(ctx, m.Entry.RepoPath)
	}
	dir := cmp.Or(m.Worktree, revWorktree)
	repo := sidecar.ShellEscape(m.Entry.RepoPath)
	wt := sidecar.ShellEscape(dir)
	defer func() {
		cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), worktreeCleanupTimeout)
		defer cancel()
		_, _ = review.RunScript(cleanup, m.Exec, m.Entry, removeWorktreeScript(repo, wt), 4<<10)
	}()
	setup := removeWorktreeScript(repo, wt) + "git worktree add --detach -q " + wt + " " + sidecar.ShellEscape(rev)
	if _, err := review.RunScript(ctx, m.Exec, m.Entry, setup, 4<<10); err != nil {
		return quality.Measurement{}, fmt.Errorf("check out %s to measure it: %w", rev, err)
	}
	return m.measureIn(ctx, dir)
}

// removeWorktreeScript leaves no worktree at wt, whether or not one exists.
func removeWorktreeScript(repo, wt string) string {
	return "cd " + repo + " || exit 1\n" +
		"git worktree remove --force " + wt + " >/dev/null 2>&1\n" +
		"rm -rf " + wt + "\n" +
		"git worktree prune\n"
}

func (m SandboxMeasurer) measureIn(ctx context.Context, dir string) (quality.Measurement, error) {
	ctx, cancel := context.WithTimeout(ctx, providerTimeout)
	defer cancel()
	out, err := review.RunScript(ctx, m.Exec, m.Entry, "cd "+sidecar.ShellEscape(dir)+" || exit 1\n"+m.Provider.Script, providerOutputLimit)
	if err != nil {
		return quality.Measurement{}, fmt.Errorf("run the metrics provider: %w", err)
	}
	c, err := m.Provider.Parse(out)
	if err != nil {
		return quality.Measurement{}, err
	}
	sources, err := m.fetchSources(ctx, dir, c)
	if err != nil {
		return quality.Measurement{}, err
	}
	return quality.Measurement{Contract: c, Sources: sources}, nil
}

// fetchSources reads the files of c that are measured, up to the size cap.
func (m SandboxMeasurer) fetchSources(ctx context.Context, dir string, c quality.Contract) ([]quality.File, error) {
	ex, _ := quality.NewExcluder(m.Config.Exclude)
	var names []string
	for _, f := range c.Files() {
		if !ex.Path(f) {
			names = append(names, f)
		}
	}
	if len(names) == 0 {
		return nil, nil
	}
	out, err := review.RunScript(ctx, m.Exec, m.Entry, sourcesScript(dir, names), sourceOutputLimit)
	if err != nil {
		return nil, fmt.Errorf("read the sources back: %w", err)
	}
	data, err := base64.StdEncoding.DecodeString(strings.TrimSpace(out))
	if err != nil {
		return nil, fmt.Errorf("read the sources back: %w", err)
	}
	files, _, err := quality.ReadTar(data, quality.MaxSourceBytes)
	return files, err
}

// sourcesScript prints a base64 tar of the named files, leaving out the files
// that would take the archive past the size cap, and the ones that are gone.
func sourcesScript(dir string, names []string) string {
	return fmt.Sprintf(`cd %s || exit 1
total=0
while IFS= read -r f; do
  [ -f "$f" ] || continue
  size=$(wc -c < "$f") || continue
  if [ $((total + size)) -le %d ]; then total=$((total + size)); printf '%%s\n' "$f"; else echo "skipped $f: over the size cap" >&2; fi
done <<'SMELT_FILES' | tar -cf - -T - 2>/dev/null | base64 -w0
%s
SMELT_FILES
`, sidecar.ShellEscape(dir), quality.MaxSourceBytes, strings.Join(names, "\n"))
}
