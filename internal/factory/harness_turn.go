package factory

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/smeltdev/smelt/internal/harness"
	"github.com/smeltdev/smelt/internal/iostream"
	"github.com/smeltdev/smelt/internal/review"
	"github.com/smeltdev/smelt/internal/sidecar"
)

const turnImplementer = "implementer"

// TurnAttempt attributes every attempted agent call, including failed attempts.
type TurnAttempt struct {
	DurationMS   int64               `json:"duration_ms,omitempty"`
	CostUSD      float64             `json:"cost_usd,omitempty"`
	InputTokens  int                 `json:"input_tokens,omitempty"`
	CachedTokens int                 `json:"cached_tokens,omitempty"`
	OutputTokens int                 `json:"output_tokens,omitempty"`
	Round        int                 `json:"round"`
	Turn         string              `json:"turn"`
	Harness      harness.Name        `json:"harness"`
	Model        string              `json:"model,omitempty"`
	Kind         harness.FailureKind `json:"kind,omitempty"`
	Error        string              `json:"error,omitempty"`
	ResetAt      *time.Time          `json:"reset_at,omitempty"`
	RateLimit    *harness.RateLimit  `json:"rate_limit,omitempty"`
}

func (s *Sidecars) useCandidate(c Candidate) {
	im := s.Implementer
	im.Harness, im.Credential, im.BaseURL, im.Model = c.Harness, c.Credential, c.BaseURL, c.Model
}

func (s *Sidecars) prepareCandidate(ctx context.Context, entry *sidecar.PoolEntry, c Candidate) error {
	s.agentMu.Lock()
	defer s.agentMu.Unlock()
	key := entry.ID + "/" + string(c.Harness.Name())
	if s.prepared[key] {
		return nil
	}
	script, env := c.Harness.Prepare(c.Credential)
	if script != "" {
		if _, err := review.RunScriptEnv(ctx, s.Exec, entry, script, env, 64<<10); err != nil {
			return harness.Classified(harness.Unavailable, fmt.Errorf("prepare %s: %w", c.Harness.Name(), err), nil)
		}
	}
	if s.prepared == nil {
		s.prepared = map[string]bool{}
	}
	s.prepared[key] = true
	return nil
}

func (s *Sidecars) recordAttempt(a TurnAttempt, c Candidate, err error) {
	a.Harness, a.Model = c.Harness.Name(), c.Model
	if err != nil {
		a.Error = err.Error()
	}
	var failure *harness.Failure
	if errors.As(err, &failure) {
		a.Kind, a.ResetAt = failure.Kind, failure.ResetAt
	}
	s.agentMu.Lock()
	s.turns = append(s.turns, a)
	s.agentMu.Unlock()
	if s.onAttempt != nil {
		s.onAttempt(a)
	}
}

func (s *Sidecars) attempts() []TurnAttempt {
	s.agentMu.Lock()
	defer s.agentMu.Unlock()
	return append([]TurnAttempt(nil), s.turns...)
}

func (s *Sidecars) reviewOptions(round int, phase string, opts review.Options) review.Options {
	if s.Failover == nil {
		return opts
	}
	handoff := s.reviewHandoff
	if handoff == "" {
		handoff = BuildHandoff(HandoffOptions{Ticket: s.Request, Plan: s.plan, Pack: s.Pack, Summary: s.prior.Summary, Findings: reviewFindings(s.checks)})
	}
	opts.Retry = func(ctx context.Context, entry *sidecar.PoolEntry, p review.Prompt, original review.Options, run func(review.Options) (review.Result, error)) (review.Result, error) {
		var result review.Result
		err := s.Failover.Do(ctx, round, phase+":"+p.Name, func(c Candidate) (*harness.RateLimit, error) {
			attempt := original
			primary := harness.Claude
			if original.Harness != nil {
				primary = original.Harness.Name()
			}
			if c.Harness.Name() != primary {
				attempt.PromptPrefix = handoff + "\n\n"
			}
			attempt.Harness, attempt.Credential, attempt.BaseURL, attempt.Model = c.Harness, c.Credential, c.BaseURL, c.Model
			result = review.Result{Harness: c.Harness.Name(), Model: c.Model}
			err := s.prepareCandidate(ctx, entry, c)
			if err == nil {
				result, err = run(attempt)
			}
			s.recordAttempt(TurnAttempt{Round: round, Turn: phase + ":" + p.Name, RateLimit: result.RateLimit, DurationMS: result.Duration.Milliseconds()}, c, err)
			return result.RateLimit, err
		})
		return result, err
	}
	return opts
}

// agentAnswer retains both streams for harness classification. Bookkeeping
// scripts deliberately keep their existing RunScriptEnv semantics.
func (s *Sidecars) agentAnswer(ctx context.Context, turn string, in harness.ReviewInput, timeout time.Duration, limit int) (string, harness.Harness, error) {
	var output string
	h := s.Implementer.agent()
	run := func(c Candidate) (*harness.RateLimit, error) {
		if s.Implementer.agent().Name() != c.Harness.Name() {
			change, err := s.Collect(ctx)
			if err != nil {
				return nil, err
			}
			handoff, err := s.Handoff(ctx, s.Request, in.Prompt, s.plan, s.prior, change, s.checks)
			if err != nil {
				return nil, err
			}
			in.Prompt = handoff + "\n\n" + in.Prompt
			s.Implementer.FreshSession()
		}
		s.useCandidate(c)
		h = c.Harness
		if err := s.prepareCandidate(ctx, s.Implementer.Entry, c); err != nil {
			s.recordAttempt(TurnAttempt{Turn: turn}, c, err)
			return nil, err
		}
		start := time.Now()
		attemptCtx, cancel := context.WithTimeout(ctx, timeout)
		defer cancel()
		// A streamed turn prints its result last, after every event: keep the
		// end of each stream, never the start.
		stdout, stderr := harness.StreamTail{Limit: limit}, harness.StreamTail{Limit: limit}
		in.Model = c.Model
		in.Stream = true
		code, execErr := s.Exec(attemptCtx, s.Implementer.Entry, h.ReviewScript(in), h.Env(c.Credential, c.BaseURL), func(stream string, data []byte) {
			buf := &stdout
			if stream == iostream.StreamStderr {
				buf = &stderr
			}
			_, _ = buf.Write(data)
		}, nil)
		output = stdout.String()
		parser := h.NewImplementParser(nil)
		parser.Write([]byte(output + "\n" + stderr.String()))
		parser.Flush()
		outcome := parser.Outcome()
		err := h.RunError(attemptCtx, timeout, code, execErr, output, stderr.String())
		if err == nil {
			err = outcome.Err
		}
		s.recordAttempt(TurnAttempt{Turn: turn, RateLimit: outcome.Turn.RateLimit, DurationMS: time.Since(start).Milliseconds(), CostUSD: outcome.Turn.CostUSD, InputTokens: outcome.Turn.InputTokens, CachedTokens: outcome.Turn.CachedTokens, OutputTokens: outcome.Turn.OutputTokens}, c, err)
		return outcome.Turn.RateLimit, err
	}
	var err error
	if s.Failover != nil {
		err = s.Failover.Do(ctx, 0, turn, run)
	} else {
		_, err = run(Candidate{Harness: h, Credential: s.Implementer.Credential, BaseURL: s.Implementer.BaseURL, Model: s.Implementer.Model})
	}
	if h.Name() == harness.Claude {
		output = harness.ResultLine(output)
	}
	return output, h, err
}

func handoffPrior(partial, previous Turn) Turn {
	if partial.Summary != "" {
		return partial
	}
	return previous
}

// captureReviewHandoff freezes the current tree before concurrent review retries.
func (s *Sidecars) captureReviewHandoff(ctx context.Context) error {
	if s.Failover == nil {
		return nil
	}
	change, err := s.ws.collect(ctx)
	if err != nil {
		return err
	}
	s.reviewHandoff, err = s.Handoff(ctx, s.Request, "Review the current change.", s.plan, s.prior, change, s.checks)
	return err
}
