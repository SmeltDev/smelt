package server

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"gotest.tools/v3/assert"
	"gotest.tools/v3/assert/cmp"

	"github.com/smeltdev/smelt/internal/config"
	"github.com/smeltdev/smelt/internal/factory"
	"github.com/smeltdev/smelt/internal/harness"
	"github.com/smeltdev/smelt/internal/harness/status"
	"github.com/smeltdev/smelt/internal/review"
	"github.com/smeltdev/smelt/internal/smeltd"
)

func codexAuth(value string) HarnessAuth {
	return HarnessAuth{Credential: review.Credential{EnvVar: "SMELT_CODEX_AUTH_JSON", Value: value}}
}

func TestFactoryRunUsesTheRequestedHarnessAndItsOwnCredential(t *testing.T) {
	var got factory.RunOptions
	run := scriptedRun(factory.ResultPassed)
	d, root := newFactoryDaemon(t, func(ctx context.Context, opts factory.RunOptions) (factory.Report, error) {
		got = opts
		return run(ctx, opts)
	})
	d.rcfg.Codex = codexAuth("codex-secret")
	cfg := `{"orgID":"org-1","factory":{"models":{"codex":"gpt-5-codex","claude":"opus"}},"commands":[{"name":"test","run":"go test ./..."}]}`
	assert.NilError(t, os.WriteFile(filepath.Join(root, ".smelt", "config.json"), []byte(cfg), 0o644))

	sess, err := d.startFactory(smeltd.FactoryRequest{Backend: smeltd.BackendCircleCI, ProjectRoot: root, Prompt: "go", Harness: "codex"})
	assert.NilError(t, err)
	detail := waitForSessionEnd(t, d, sess.ID)

	assert.Equal(t, got.Harness.Name(), harness.Codex)
	assert.Equal(t, got.Credential.Value, "codex-secret")
	assert.Equal(t, got.Model, "gpt-5-codex")
	assert.Equal(t, detail.Factory.Harness, "codex")
	assert.Equal(t, detail.Factory.Model, "gpt-5-codex")

	// An explicit model wins over the harness's configured one.
	sess, err = d.startFactory(smeltd.FactoryRequest{Backend: smeltd.BackendCircleCI, ProjectRoot: root, Prompt: "go", Model: "o3"})
	assert.NilError(t, err)
	waitForSessionEnd(t, d, sess.ID)
	assert.Equal(t, got.Harness.Name(), harness.Claude)
	assert.Equal(t, got.Model, "o3")
}

func TestFactoryRunRefusesAHarnessWithoutACredential(t *testing.T) {
	d, root := newFactoryDaemon(t, scriptedRun(factory.ResultPassed))
	_, err := d.startFactory(smeltd.FactoryRequest{Backend: smeltd.BackendCircleCI, ProjectRoot: root, Prompt: "go", Harness: "codex"})
	var ae *apiError
	assert.Assert(t, errors.As(err, &ae))
	assert.Equal(t, ae.status, http.StatusServiceUnavailable)
	assert.Check(t, cmp.Contains(ae.msg, "smelt auth login codex"))

	_, err = d.startFactory(smeltd.FactoryRequest{Backend: smeltd.BackendCircleCI, ProjectRoot: root, Prompt: "go", Harness: "gemini"})
	assert.Assert(t, errors.As(err, &ae))
	assert.Equal(t, ae.status, http.StatusBadRequest)
}

func TestFactoryRunRenewsTheCredentialBeforeStarting(t *testing.T) {
	var got factory.RunOptions
	run := scriptedRun(factory.ResultPassed)
	d, root := newFactoryDaemon(t, func(ctx context.Context, opts factory.RunOptions) (factory.Report, error) {
		got = opts
		return run(ctx, opts)
	})
	d.rcfg.Codex = HarnessAuth{Refresh: func(context.Context) (review.Credential, error) {
		return review.Credential{EnvVar: "SMELT_CODEX_AUTH_JSON", Value: "renewed"}, nil
	}}
	sess, err := d.startFactory(smeltd.FactoryRequest{Backend: smeltd.BackendCircleCI, ProjectRoot: root, Prompt: "go", Harness: "codex"})
	assert.NilError(t, err)
	waitForSessionEnd(t, d, sess.ID)
	assert.Equal(t, got.Credential.Value, "renewed")

	d.rcfg.Codex.Refresh = func(context.Context) (review.Credential, error) {
		return review.Credential{}, errors.New("your Codex login expires soon; run `smelt refresh codex`")
	}
	_, err = d.startFactory(smeltd.FactoryRequest{Backend: smeltd.BackendCircleCI, ProjectRoot: root, Prompt: "go", Harness: "codex"})
	assert.ErrorContains(t, err, "smelt refresh codex")
}

func TestFactoryStatusReportsTokensAndTheRateLimitSignal(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	resets := time.Date(2026, 10, 12, 0, 0, 0, 0, time.UTC)
	pct := 80
	run := func(ctx context.Context, opts factory.RunOptions) (factory.Report, error) {
		opts.OnStart("run-1", factory.Worktree{Path: "/w", Branch: "smelt/run-1"})
		opts.OnEvent(factory.Event{Kind: factory.EventImplementing, Round: 1})
		opts.OnEvent(factory.Event{Kind: factory.EventImplemented, Round: 1, Turn: factory.Turn{
			Harness: harness.Codex, Model: "gpt-5-codex", InputTokens: 100, CachedTokens: 40, OutputTokens: 7,
			RateLimit: &harness.RateLimit{Status: "warning", Bucket: "5h", Utilization: &pct, ResetsAt: &resets},
		}})
		return factory.Report{RunID: "run-1", Outcome: factory.Outcome{Result: factory.ResultPassed, Rounds: 1}}, nil
	}
	d, root := newFactoryDaemon(t, run)
	d.rcfg.Codex = codexAuth("codex-secret")
	sess, err := d.startFactory(smeltd.FactoryRequest{Backend: smeltd.BackendCircleCI, ProjectRoot: root, Prompt: "go", Harness: "codex"})
	assert.NilError(t, err)
	detail := waitForSessionEnd(t, d, sess.ID)

	impl := detail.Rounds[0].Implement
	assert.Equal(t, impl.Harness, "codex")
	assert.Equal(t, impl.Model, "gpt-5-codex")
	assert.DeepEqual(t, []int{impl.InputTokens, impl.CachedTokens, impl.OutputTokens}, []int{100, 40, 7})
	assert.Equal(t, impl.RateLimit.Status, "warning")

	dir, err := status.Dir()
	assert.NilError(t, err)
	seen, ok := status.LastSignal(dir, harness.Codex)
	assert.Assert(t, ok)
	assert.Equal(t, seen.Bucket, "5h")
}

func TestFactoryFallbackCredentialsModelsAndOverride(t *testing.T) {
	var got factory.RunOptions
	run := scriptedRun(factory.ResultPassed)
	d, root := newFactoryDaemon(t, func(ctx context.Context, opts factory.RunOptions) (factory.Report, error) {
		got = opts
		return run(ctx, opts)
	})
	d.rcfg.Codex = codexAuth("alternate")
	cfg := `{"orgID":"org-1","factory":{"fallback":["codex","claude","codex"],"models":{"claude":"opus","codex":"own-model"},"swapAtUtilization":0.9},"commands":[{"name":"test","run":"true"}]}`
	assert.NilError(t, os.WriteFile(filepath.Join(root, ".smelt", "config.json"), []byte(cfg), 0o644))
	sess, err := d.startFactory(smeltd.FactoryRequest{Backend: smeltd.BackendCircleCI, ProjectRoot: root, Prompt: "go", Harness: "claude", Model: "primary-model"})
	assert.NilError(t, err)
	waitForSessionEnd(t, d, sess.ID)
	assert.Equal(t, len(got.Fallback), 1)
	assert.Equal(t, got.Model, "primary-model")
	assert.Equal(t, got.Fallback[0].Harness.Name(), harness.Codex)
	assert.Equal(t, got.Fallback[0].Credential.Value, "alternate")
	assert.Equal(t, got.Fallback[0].Model, "own-model")
	assert.Equal(t, got.SwapAtUtilization, 0.9)
	sess, err = d.startFactory(smeltd.FactoryRequest{Backend: smeltd.BackendCircleCI, ProjectRoot: root, Prompt: "go", FallbackSet: true})
	assert.NilError(t, err)
	waitForSessionEnd(t, d, sess.ID)
	assert.Equal(t, len(got.Fallback), 0)
	_, err = d.startFactory(smeltd.FactoryRequest{Backend: smeltd.BackendCircleCI, ProjectRoot: root, Prompt: "go", Fallback: []string{"gemini"}})
	assert.ErrorContains(t, err, "unknown fallback harness")
}

func TestFactoryStatusRecordsParserToControllerSwap(t *testing.T) {
	var got factory.RunOptions
	run := func(ctx context.Context, opts factory.RunOptions) (factory.Report, error) {
		got = opts
		opts.OnStart("run-1", factory.Worktree{Path: "/w", Branch: "smelt/run-1"})
		opts.OnEvent(factory.Event{Kind: factory.EventImplementing, Round: 1})
		cs := append([]factory.Candidate{{Harness: opts.Harness, Credential: opts.Credential}}, opts.Fallback...)
		f := factory.NewFailover(cs, 0, func(swap factory.HarnessSwap) {
			opts.OnEvent(factory.Event{Kind: factory.EventHarnessSwap, Round: swap.Round, Swap: &swap})
		})
		err := f.Do(ctx, 1, "implementer", func(c factory.Candidate) (*harness.RateLimit, error) {
			if c.Harness.Name() == harness.Codex {
				p := c.Harness.NewImplementParser(nil)
				p.Write([]byte(`{"type":"error","message":"You’ve hit your usage limit. Try again at Oct 19th, 2026 2:31 AM."}` + "\n" + `{"type":"turn.failed","error":{"message":"You’ve hit your usage limit. Try again at Oct 19th, 2026 2:31 AM."}}` + "\n"))
				return p.Outcome().Turn.RateLimit, p.Outcome().Err
			}
			opts.OnEvent(factory.Event{Kind: factory.EventImplemented, Round: 1, Turn: factory.Turn{Harness: c.Harness.Name(), Model: c.Model, Summary: "done"}})
			return nil, nil
		})
		return factory.Report{Started: true, Outcome: factory.Outcome{Result: factory.ResultPassed, Rounds: 1, HarnessSwaps: f.Swaps()}}, err
	}
	d, root := newFactoryDaemon(t, run)
	d.rcfg.Codex = codexAuth("primary")
	sess, err := d.startFactory(smeltd.FactoryRequest{Backend: smeltd.BackendCircleCI, ProjectRoot: root, Prompt: "go", Harness: "codex", Fallback: []string{"claude"}})
	assert.NilError(t, err)
	detail := waitForSessionEnd(t, d, sess.ID)
	assert.Equal(t, got.Harness.Name(), harness.Codex)
	assert.Equal(t, detail.Factory.Result, "passed")
	assert.Equal(t, len(detail.Factory.HarnessSwaps), 1)
	swap := detail.Factory.HarnessSwaps[0]
	assert.Equal(t, swap.From, harness.Codex)
	assert.Equal(t, swap.To, harness.Claude)
	assert.Equal(t, swap.Kind, harness.RateLimited)
	assert.Assert(t, swap.ResetAt.Equal(time.Date(2026, 10, 19, 2, 31, 0, 0, time.UTC)))
	assert.Equal(t, detail.Rounds[0].Harness, "claude")
	assert.Equal(t, detail.Rounds[0].Implement.Harness, "claude")
}

func TestFactoryContinuationOverrideUsesNewCredentialAndModel(t *testing.T) {
	var got factory.RunOptions
	run := scriptedRun(factory.ResultPassed)
	d, root := newFactoryDaemon(t, func(ctx context.Context, opts factory.RunOptions) (factory.Report, error) {
		got = opts
		return run(ctx, opts)
	})
	d.rcfg.Codex = codexAuth("new-secret")
	cfg := `{"orgID":"org-1","factory":{"models":{"codex":"new-model"}},"commands":[{"name":"test","run":"true"}]}`
	assert.NilError(t, os.WriteFile(filepath.Join(root, ".smelt", "config.json"), []byte(cfg), 0o644))
	data, err := config.ProjectDataDir(root)
	assert.NilError(t, err)
	assert.NilError(t, os.MkdirAll(filepath.Join(data, "factory"), 0o700))
	raw, err := json.Marshal(factory.Record{RunID: "prior", Prompt: "original ticket", Harness: "claude", Model: "old-model", Plan: &factory.Plan{Summary: "original plan"}})
	assert.NilError(t, err)
	assert.NilError(t, os.WriteFile(filepath.Join(data, "factory", "prior.json"), raw, 0o600))
	sess, err := d.startFactory(smeltd.FactoryRequest{Backend: smeltd.BackendCircleCI, ProjectRoot: root, Continue: "prior", Harness: "codex", Prompt: "finish it"})
	assert.NilError(t, err)
	waitForSessionEnd(t, d, sess.ID)
	assert.Equal(t, got.Harness.Name(), harness.Codex)
	assert.Equal(t, got.Credential.Value, "new-secret")
	assert.Equal(t, got.Model, "new-model")
	assert.Equal(t, got.Continue.From.Prompt, "original ticket")
	assert.Equal(t, got.Continue.From.Plan.Summary, "original plan")
	assert.Equal(t, got.Continue.Guidance, "finish it")
}

func TestFactoryExhaustionStatusRetainsReasonAndEarliestReset(t *testing.T) {
	early := time.Date(2026, 10, 19, 1, 0, 0, 0, time.UTC)
	d, root := newFactoryDaemon(t, func(ctx context.Context, opts factory.RunOptions) (factory.Report, error) {
		opts.OnStart("run-1", factory.Worktree{Path: "/w"})
		f := factory.NewFailover(append([]factory.Candidate{{Harness: opts.Harness}}, opts.Fallback...), 0, func(swap factory.HarnessSwap) {
			opts.OnEvent(factory.Event{Kind: factory.EventHarnessSwap, Swap: &swap})
		})
		err := f.Do(ctx, 0, "plan", func(c factory.Candidate) (*harness.RateLimit, error) {
			reset := early
			if c.Harness.Name() == harness.Claude {
				reset = early.Add(time.Hour)
			}
			return nil, &harness.Failure{Kind: harness.RateLimited, ResetAt: &reset}
		})
		var exhausted *factory.Exhaustion
		assert.Assert(t, errors.As(err, &exhausted))
		return factory.Report{Started: true, Outcome: factory.Outcome{Result: factory.ResultFailed, Reason: err.Error(), ResetAt: exhausted.ResetAt}}, err
	})
	d.rcfg.Codex = codexAuth("fallback")
	sess, err := d.startFactory(smeltd.FactoryRequest{Backend: smeltd.BackendCircleCI, ProjectRoot: root, Prompt: "ticket", Fallback: []string{"codex"}})
	assert.NilError(t, err)
	detail := waitForSessionEnd(t, d, sess.ID)
	assert.Equal(t, detail.Factory.Result, "failed")
	assert.Equal(t, detail.Factory.Reason, "no harness available")
	assert.Assert(t, detail.Factory.ResetAt.Equal(early))
	raw, err := json.Marshal(detail)
	assert.NilError(t, err)
	assert.Assert(t, strings.Contains(string(raw), `"reason":"no harness available"`))
	assert.Assert(t, strings.Contains(string(raw), `"reset_at":"2026-10-19T01:00:00Z"`))
}

func TestFactoryRunGivesGrokItsOwnCredential(t *testing.T) {
	var got factory.RunOptions
	run := scriptedRun(factory.ResultPassed)
	d, root := newFactoryDaemon(t, func(ctx context.Context, opts factory.RunOptions) (factory.Report, error) {
		got = opts
		return run(ctx, opts)
	})
	d.rcfg.Codex = codexAuth("codex-secret")
	d.rcfg.Grok = HarnessAuth{Credential: review.Credential{EnvVar: "XAI_API_KEY", Value: "grok-secret"}}

	sess, err := d.startFactory(smeltd.FactoryRequest{Backend: smeltd.BackendCircleCI, ProjectRoot: root, Prompt: "go", Harness: "grok"})
	assert.NilError(t, err)
	waitForSessionEnd(t, d, sess.ID)
	assert.Equal(t, got.Harness.Name(), harness.Grok)
	assert.Equal(t, got.Credential.Value, "grok-secret")
	assert.Equal(t, got.Credential.EnvVar, "XAI_API_KEY")

	// Without a Grok credential the run is refused, whatever Codex has.
	d.rcfg.Grok = HarnessAuth{}
	_, err = d.startFactory(smeltd.FactoryRequest{Backend: smeltd.BackendCircleCI, ProjectRoot: root, Prompt: "go", Harness: "grok"})
	var ae *apiError
	assert.Assert(t, errors.As(err, &ae))
	assert.Check(t, cmp.Contains(ae.msg, "smelt auth login grok"))
}
