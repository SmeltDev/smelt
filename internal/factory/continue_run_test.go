package factory

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"gotest.tools/v3/assert"

	"github.com/smeltdev/smelt/internal/circleci"
	"github.com/smeltdev/smelt/internal/config"
	"github.com/smeltdev/smelt/internal/harness"
	"github.com/smeltdev/smelt/internal/harness/claude"
	"github.com/smeltdev/smelt/internal/harness/codex"
	"github.com/smeltdev/smelt/internal/iostream"
	"github.com/smeltdev/smelt/internal/review"
	"github.com/smeltdev/smelt/internal/sidecar"
)

// localBackend provisions sidecars that are directories on this machine. A
// sidecar's workspace is a directory of its own, standing in for the repoPath
// the pool asks for; rsync copies files in and out and leaves .git alone, as
// the real syncs do.
type localBackend struct {
	sidecar.Backend // the methods a run does not reach stay unimplemented
	dir             string
}

func (b *localBackend) workspace(id string) string { return filepath.Join(b.dir, id) }

func (b *localBackend) Create(_ context.Context, name, _, _ string) (sidecar.Instance, error) {
	return sidecar.Instance{ID: name}, os.MkdirAll(b.workspace(name), 0o755)
}

func (b *localBackend) Delete(_ context.Context, id string) error {
	return os.RemoveAll(b.workspace(id))
}

func (b *localBackend) IsStale(context.Context, string) bool { return false }

func (b *localBackend) Sync(ctx context.Context, id, _, cwd string, _ bool, _ iostream.StatusFunc) error {
	dst := b.workspace(id)
	if out, err := exec.CommandContext(ctx, "rsync", "-a", "--delete", "--exclude=.git", cwd+"/", dst+"/").CombinedOutput(); err != nil {
		return fmt.Errorf("rsync: %w: %s", err, out)
	}
	if _, err := os.Stat(filepath.Join(dst, ".git")); err == nil {
		return nil
	}
	if out, err := exec.CommandContext(ctx, "git", "-C", dst, "init", "-q").CombinedOutput(); err != nil {
		return fmt.Errorf("git init: %w: %s", err, out)
	}
	return nil
}

func (b *localBackend) Pull(ctx context.Context, id, _, localDir string, _ iostream.StatusFunc) error {
	if out, err := exec.CommandContext(ctx, "rsync", "-a", "--delete", "--exclude=.git", b.workspace(id)+"/", localDir+"/").CombinedOutput(); err != nil {
		return fmt.Errorf("rsync: %w: %s", err, out)
	}
	return nil
}

// inWorkspace runs scripts in the local directory that stands for a sidecar's
// workspace.
func (b *localBackend) inWorkspace(run review.Execer) review.Execer {
	return func(ctx context.Context, e *sidecar.PoolEntry, script string, env map[string]string, onOutput circleci.OutputFn, onSubmitted func(string)) (int, error) {
		script = strings.ReplaceAll(script, e.RepoPath, b.workspace(e.ID))
		return run(ctx, e, script, env, onOutput, onSubmitted)
	}
}

// A continued run whose branch holds no work implements the request: the
// implementer is sent it with the context line, the run ends with a real
// result, and the status and the saved record say where it started from.
func TestRunContinuingAnEmptyBranchImplementsTheRequest(t *testing.T) {
	for _, bin := range []string{"rsync", "git"} {
		if _, err := exec.LookPath(bin); err != nil {
			t.Skipf("%s is not installed", bin)
		}
	}
	t.Setenv(config.EnvHome, t.TempDir())
	ctx := context.Background()
	root := newProject(t)
	wt, err := CreateWorktree(ctx, WorktreeOptions{Root: root, Path: filepath.Join(t.TempDir(), "wt"), RunID: "run-1", Prompt: "add a flag"})
	assert.NilError(t, err)
	rec := Record{
		RunID: "run-1", Prompt: "add a flag",
		Worktree: wt.Path, Branch: wt.Branch, Baseline: wt.Baseline, Head: wt.Head,
		BaseBranch: wt.BaseBranch, DefaultBranch: "main", Result: ResultNoChange,
	}
	assert.Equal(t, gitOutput(t, root, "rev-parse", rec.Branch), rec.Baseline, "the branch holds work")

	home := t.TempDir()
	installFakeClaude(t, home, "echo 'package main' > flag.go\n"+streamOK)
	backend := &localBackend{dir: t.TempDir()}
	var (
		mu       sync.Mutex
		statuses []string
	)
	rep, err := Run(ctx, RunOptions{
		Root:       root,
		Continue:   &Continuation{From: rec},
		Attempts:   2,
		Backend:    backend,
		Exec:       backend.inWorkspace(withoutPkill(localExec(home))),
		Credential: review.Credential{EnvVar: "ANTHROPIC_API_KEY", Value: "k"},
		Status: func(_ iostream.Level, msg string) {
			mu.Lock()
			defer mu.Unlock()
			statuses = append(statuses, msg)
		},
	})
	assert.NilError(t, err)

	assert.Assert(t, rep.Started)
	assert.Equal(t, rep.Outcome.Result, ResultPassed)
	assert.Assert(t, rep.Outcome.FromEmpty)
	prompts := readFile(t, home, "stdin")
	assert.Equal(t, rep.Outcome.Rounds, 1)
	assert.Equal(t, strings.Count(prompts, "start from the request."), 1, "one implementer turn with the request")
	assert.Assert(t, strings.Contains(prompts, "add a flag\n\nAn earlier run on this branch produced no work; start from the request."), prompts)
	assert.Assert(t, !strings.Contains(prompts, "Build on it"), prompts)
	assert.Assert(t, strings.Contains(strings.Join(statuses, "\n"), "continuing with no prior work: implementing from the request"), "%v", statuses)

	saved, err := LoadRecord(root, rep.RunID)
	assert.NilError(t, err)
	assert.Assert(t, saved.ContinuedFromEmpty)
	assert.Equal(t, saved.Result, ResultPassed)
	assert.Equal(t, gitOutput(t, root, "show", "--format=", "--name-only", rec.Branch), "flag.go")
}

// isolatedHarness keeps provider parsing and command generation while avoiding
// host-wide credential preparation in local process tests.
type isolatedHarness struct{ harness.Harness }

func (isolatedHarness) Prepare(harness.Credential) (string, map[string]string) { return "", nil }

func TestRunFailsOverAfterPartialCodexWork(t *testing.T) {
	skipWithoutTools(t)
	t.Setenv(config.EnvHome, t.TempDir())
	root := newProject(t)
	home := t.TempDir()
	notes := `printf '%s\n' '{"type":"result","is_error":false,"structured_output":{"title":"Add the flag","what":"Added a flag","why":"Requested","how":"Updated Go files"}}'`
	installFakeClaude(t, home, `case "$*" in
 *--json-schema*) `+notes+` ;;
 *) test -f partial.go || exit 4
 echo 'package main' > fallback.go
 `+streamOK+` ;;
esac`)
	message := "You’ve hit your usage limit. Upgrade to Pro (https://chatgpt.com/explore/pro), visit https://chatgpt.com/settings/usage to purchase more credits or try again at Oct 19th, 2026 2:31 AM."
	raw := `{"type":"error","message":"` + message + `"}` + "\n" + `{"type":"turn.failed","error":{"message":"` + message + `"}}`
	bin := filepath.Join(home, ".local", "bin", "codex")
	assert.NilError(t, os.WriteFile(bin, []byte("#!/bin/sh\ncat > \"$HOME/codex-stdin\"\necho 'package main' > partial.go\nprintf '%s\\n' '"+raw+"'\nexit 1\n"), 0o755))
	backend := &localBackend{dir: t.TempDir()}
	var events []Event
	rep, err := Run(context.Background(), RunOptions{
		Root: root, Prompt: "add a flag", Attempts: 2,
		Backend: backend, Exec: backend.inWorkspace(withoutPkill(localExec(home))),
		Harness: isolatedHarness{codex.Harness{}}, Credential: review.Credential{EnvVar: "OPENAI_API_KEY", Value: "codex"},
		Fallback: []Candidate{{Harness: claude.Harness{}, Credential: review.Credential{EnvVar: "ANTHROPIC_API_KEY", Value: "claude"}}},
		Status:   func(iostream.Level, string) {}, OnEvent: func(e Event) { events = append(events, e) },
	})
	assert.NilError(t, err)
	assert.Equal(t, rep.Outcome.Result, ResultPassed)
	assert.Equal(t, rep.Outcome.Rounds, 1)
	assert.Equal(t, len(rep.Outcome.HarnessSwaps), 1)
	swap := rep.Outcome.HarnessSwaps[0]
	assert.Equal(t, swap.Round, 1)
	assert.Equal(t, swap.Kind, harness.RateLimited)
	assert.Assert(t, swap.ResetAt.Equal(time.Date(2026, 10, 19, 2, 31, 0, 0, time.UTC)))
	prompts := readFile(t, home, "stdin")
	for _, section := range []string{"## Ticket", "add a flag", "## Change so far", "partial.go", "## Findings to fix"} {
		assert.Assert(t, strings.Contains(prompts, section), prompts)
	}
	assert.Assert(t, !strings.Contains(readFile(t, home, "args"), "--resume"))
	implemented := 0
	for _, e := range events {
		if e.Kind == EventImplemented {
			implemented++
			assert.Equal(t, e.Round, 1)
			assert.Equal(t, e.Turn.Harness, harness.Claude)
		}
	}
	assert.Equal(t, implemented, 1)
	saved, err := LoadRecord(root, rep.RunID)
	assert.NilError(t, err)
	assert.Equal(t, len(saved.HarnessSwaps), 1)
	assert.Assert(t, len(saved.Turns) >= 2)
	assert.Equal(t, saved.Turns[0].Harness, harness.Codex)
	assert.Equal(t, saved.Turns[0].Kind, harness.RateLimited)
	assert.Equal(t, saved.Turns[1].Harness, harness.Claude)
	assert.Assert(t, strings.Contains(rep.Timing.Format(), swap.String()))
}

func TestContinuationUsesAnotherHarnessAndFullHandoff(t *testing.T) {
	for _, bin := range []string{"rsync", "git"} {
		if _, err := exec.LookPath(bin); err != nil {
			t.Skipf("%s is not installed", bin)
		}
	}
	t.Setenv(config.EnvHome, t.TempDir())
	root, rec := finishedRun(t)
	rec.Harness = string(harness.Claude)
	rec.Plan = &Plan{Summary: "approved flag plan"}
	rec.Pack = &Pack{Entries: []PackEntry{{Path: "main.go", Start: 1, End: 1, Reason: "entry point"}}}
	home := t.TempDir()
	binDir := filepath.Join(home, ".local", "bin")
	assert.NilError(t, os.MkdirAll(binDir, 0o755))
	script := `#!/bin/sh
set -eu
[ "$OPENAI_API_KEY" = new-secret ]
printf '%s\n' "$*" >> "$HOME/codex-args"
cat >> "$HOME/codex-stdin"
out=
while [ "$#" -gt 0 ]; do
 case "$1" in -o) shift; out=$1;; esac
 shift
done
if [ -n "$out" ]; then
 printf '%s\n' '{"title":"flag","what":"continued","why":"requested","how":"implemented"}' > "$out"
else
 echo 'package main' > continuation.go
 printf '%s\n' '{"type":"thread.started","thread_id":"fresh"}' '{"type":"item.completed","item":{"type":"agent_message","text":"done"}}' '{"type":"turn.completed","usage":{"input_tokens":1,"output_tokens":1}}'
fi
`
	assert.NilError(t, os.WriteFile(filepath.Join(binDir, "codex"), []byte(script), 0o755))
	backend := &localBackend{dir: t.TempDir()}
	rep, err := Run(context.Background(), RunOptions{
		Root: root, Continue: &Continuation{From: rec, Guidance: "finish the flag"}, Attempts: 2,
		Harness: isolatedHarness{codex.Harness{}}, Model: "new-model", Pack: PackOptions{Enabled: true, MaxTokens: 1000},
		Credential: review.Credential{EnvVar: codex.EnvAPIKey, Value: "new-secret"},
		Backend:    backend, Exec: backend.inWorkspace(withoutPkill(localExec(home))),
		Status: func(iostream.Level, string) {},
	})
	assert.NilError(t, err)
	assert.Equal(t, rep.Outcome.Result, ResultPassed)
	prompt := readFile(t, home, "codex-stdin")
	for _, text := range []string{rec.Prompt, "approved flag plan", "finish the flag", "main.go"} {
		assert.Assert(t, strings.Contains(prompt, text), "%s absent from %s", text, prompt)
	}
	assert.Assert(t, strings.Contains(readFile(t, home, "codex-args"), "new-model"))
	assert.Equal(t, rep.Outcome.Turns[0].Harness, harness.Codex)
	saved, err := LoadRecord(root, rep.RunID)
	assert.NilError(t, err)
	assert.Equal(t, saved.Harness, string(harness.Codex))
	assert.Equal(t, saved.Plan.Summary, "approved flag plan")
}

func TestRunExhaustionSurvivesNotesAndPersistsEarliestReset(t *testing.T) {
	for _, phase := range []string{"implementer", "notes"} {
		t.Run(phase, func(t *testing.T) {
			skipWithoutTools(t)
			t.Setenv(config.EnvHome, t.TempDir())
			root, home := newProject(t), t.TempDir()
			rejected := `printf '%s\n' '{"type":"rate_limit_event","rate_limit_info":{"status":"rejected","resetsAt":1792365600}}'; exit 1`
			body := "echo 'package main' > partial.go\n" + rejected
			if phase == "notes" {
				body = "case \"$*\" in *--json-schema*) " + rejected + " ;; *) echo 'package main' > partial.go\n" + streamOK + ";; esac"
			}
			installFakeClaude(t, home, body)
			assert.NilError(t, os.WriteFile(filepath.Join(home, ".local", "bin", "codex"), []byte("#!/bin/sh\ncat >/dev/null\nprintf '%s\\n' '{\"type\":\"turn.failed\",\"error\":{\"message\":\"Usage limit. Try again at 2026-10-19T02:31:00Z.\"}}'\nexit 1\n"), 0o755))
			backend := &localBackend{dir: t.TempDir()}
			rep, err := Run(context.Background(), RunOptions{Root: root, Prompt: "ticket", Attempts: 2,
				Backend: backend, Exec: backend.inWorkspace(withoutPkill(localExec(home))),
				Credential: review.Credential{EnvVar: "ANTHROPIC_API_KEY", Value: "a"},
				Fallback:   []Candidate{{Harness: isolatedHarness{codex.Harness{}}, Credential: review.Credential{Value: "b"}}}, Status: func(iostream.Level, string) {},
			})
			assert.ErrorContains(t, err, "no harness available")
			assert.Equal(t, rep.Outcome.Result, ResultFailed)
			assert.Equal(t, rep.Outcome.Reason, "no harness available")
			assert.Assert(t, rep.Outcome.ResetAt.Equal(time.Unix(1792365600, 0)))
			saved, loadErr := LoadRecord(root, rep.RunID)
			assert.NilError(t, loadErr)
			assert.Equal(t, saved.Result, ResultFailed)
			assert.Equal(t, saved.Reason, rep.Outcome.Reason)
			assert.Assert(t, saved.ResetAt.Equal(*rep.Outcome.ResetAt))
			assert.Equal(t, saved.HarnessSwaps[0].Turn, phase)
		})
	}
}

func TestRunSwapsBeforeSecondRoundAtPreciseUtilization(t *testing.T) {
	skipWithoutTools(t)
	t.Setenv(config.EnvHome, t.TempDir())
	root, home := newProject(t), t.TempDir()
	installFakeClaude(t, home, "echo 'package main' > partial.go\nprintf '%s\\n' '{\"type\":\"rate_limit_event\",\"rate_limit_info\":{\"status\":\"allowed\",\"unifiedWindows\":{\"five_hour\":{\"utilization\":0.96,\"resetsAt\":1792365600},\"seven_day\":{\"utilization\":0.47}}}}'\n"+streamOK)
	codexScript := `#!/bin/sh
cat >> "$HOME/codex-stdin"
out=
while [ "$#" -gt 0 ]; do case "$1" in -o) shift; out=$1;; esac; shift; done
if [ -n "$out" ]; then
 printf '%s\n' '{"title":"done","what":"flag","why":"ticket","how":"code"}' > "$out"
else
 echo 'package main' > fallback.go
 printf '%s\n' '{"type":"item.completed","item":{"type":"agent_message","text":"done"}}' '{"type":"turn.completed"}'
fi
`
	assert.NilError(t, os.WriteFile(filepath.Join(home, ".local", "bin", "codex"), []byte(codexScript), 0o755))
	backend := &localBackend{dir: t.TempDir()}
	rep, err := Run(context.Background(), RunOptions{Root: root, Prompt: "ticket", Attempts: 2,
		Backend: backend, Exec: backend.inWorkspace(withoutPkill(localExec(home))),
		Credential: review.Credential{EnvVar: "ANTHROPIC_API_KEY", Value: "a"},
		Fallback:   []Candidate{{Harness: isolatedHarness{codex.Harness{}}, Credential: review.Credential{Value: "b"}}},
		Commands:   []config.Command{{Name: "test", Run: "test -f fallback.go"}}, Status: func(iostream.Level, string) {},
	})
	assert.NilError(t, err)
	assert.Equal(t, rep.Outcome.Result, ResultPassed)
	assert.Equal(t, rep.Outcome.Rounds, 2)
	assert.Equal(t, len(rep.Outcome.HarnessSwaps), 1)
	assert.Equal(t, rep.Outcome.HarnessSwaps[0].Round, 2)
	assert.Equal(t, rep.Outcome.HarnessSwaps[0].Kind, harness.RateLimited)
	assert.Equal(t, strings.Count(readFile(t, home, "args"), "--end--"), 1)
	assert.Equal(t, rep.Outcome.Turns[1].Harness, harness.Codex)
}

func TestRunPlanningAndNotesRetryOnAnotherHarness(t *testing.T) {
	for _, phase := range []string{"plan", "notes"} {
		t.Run(phase, func(t *testing.T) {
			skipWithoutTools(t)
			t.Setenv(config.EnvHome, t.TempDir())
			root, home := newProject(t), t.TempDir()
			installFakeClaude(t, home, "case \"$*\" in *--json-schema*) printf '%s\\n' '{\"type\":\"rate_limit_event\",\"rate_limit_info\":{\"status\":\"rejected\"}}'; exit 1 ;; *) echo 'package main' > partial.go\n"+streamOK+";; esac")
			script := `#!/bin/sh
cat >> "$HOME/codex-stdin"
out=
schema=
while [ "$#" -gt 0 ]; do case "$1" in -o) shift; out=$1;; --output-schema) shift; schema=$1;; esac; shift; done
if [ -n "$out" ]; then
 if grep -q 'cannot_verify_here' "$schema"; then
  printf '%s\n' '{"summary":"fallback plan","files":[],"acceptance":[]}' > "$out"
 else
  printf '%s\n' '{"title":"flag","what":"change","why":"ticket","how":"code"}' > "$out"
 fi
else
 echo 'package main' > fallback.go
 printf '%s\n' '{"type":"item.completed","item":{"type":"agent_message","text":"done"}}' '{"type":"turn.completed"}'
fi
`
			assert.NilError(t, os.WriteFile(filepath.Join(home, ".local", "bin", "codex"), []byte(script), 0o755))
			backend := &localBackend{dir: t.TempDir()}
			rep, err := Run(context.Background(), RunOptions{Root: root, Prompt: "ticket", Attempts: 2, Plan: phase == "plan",
				Backend: backend, Exec: backend.inWorkspace(withoutPkill(localExec(home))),
				Credential: review.Credential{EnvVar: "ANTHROPIC_API_KEY", Value: "a"},
				Fallback:   []Candidate{{Harness: isolatedHarness{codex.Harness{}}, Credential: review.Credential{Value: "b"}}}, Status: func(iostream.Level, string) {},
			})
			assert.NilError(t, err)
			assert.Equal(t, rep.Outcome.Result, ResultPassed)
			assert.Equal(t, rep.Outcome.HarnessSwaps[0].Turn, phase)
			assert.Equal(t, rep.Outcome.HarnessSwaps[0].Round, 0)
			prompt := readFile(t, home, "codex-stdin")
			assert.Assert(t, strings.Contains(prompt, "## Ticket"))
			assert.Assert(t, strings.Contains(prompt, "## Change so far"))
		})
	}
}

// A round of an updated run that deletes a file of the base it was updated
// from, without the request naming it, fails the deletions gate, and the work
// is not committed.
func TestRunDoesNotCommitDeletionsOfTheBasesFiles(t *testing.T) {
	skipWithoutTools(t)
	t.Setenv(config.EnvHome, t.TempDir())
	ctx := context.Background()
	root, _, rec := updateProject(t)
	landOnMain(t, root, "other.go", "package main\n")
	_, err := UpdateBranch(ctx, root, rec, UpdateOptions{Author: runAuthor})
	assert.NilError(t, err)
	saved, err := LoadRecord(root, rec.RunID)
	assert.NilError(t, err)
	saved.Result = ResultExhausted
	// The update is done; the sidecar pool needs a GitHub remote to detect the repo.
	gitOutput(t, root, "remote", "set-url", "origin", "https://github.com/o/r.git")
	before := gitOutput(t, root, "rev-parse", saved.Branch)

	home := t.TempDir()
	installFakeClaude(t, home, "rm -f other.go\n"+streamOK)
	backend := &localBackend{dir: t.TempDir()}
	var warnings []string
	rep, err := Run(ctx, RunOptions{
		Root:       root,
		Continue:   &Continuation{From: saved, Guidance: "tidy up"},
		Attempts:   2,
		Backend:    backend,
		Exec:       backend.inWorkspace(withoutPkill(localExec(home))),
		Credential: review.Credential{EnvVar: "ANTHROPIC_API_KEY", Value: "k"},
		Status: func(l iostream.Level, msg string) {
			if l == iostream.LevelWarn {
				warnings = append(warnings, msg)
			}
		},
	})
	assert.NilError(t, err)
	assert.Equal(t, rep.Outcome.Result, ResultStuck, "the fake implementer repeats the same deletion")
	assert.Assert(t, failedGate(rep.Outcome.Checks, DeletionsGateName), "%+v", rep.Outcome.Checks)
	var listed string
	for _, c := range rep.Outcome.Checks {
		if c.Name == DeletionsGateName {
			listed = c.Output
		}
	}
	assert.Assert(t, strings.Contains(listed, "other.go"), listed)
	assert.Assert(t, !rep.Committed)
	assert.Equal(t, gitOutput(t, root, "rev-parse", saved.Branch), before, "the branch did not move")
	assert.Assert(t, strings.Contains(strings.Join(warnings, "\n"), "not committing"), "%v", warnings)
	assert.Assert(t, strings.Contains(readFile(t, home, "stdin"), "updated from main at"), "the implementer is told")
}
