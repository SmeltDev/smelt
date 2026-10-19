package factory

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/smeltdev/smelt/internal/harness"
	"github.com/smeltdev/smelt/internal/harness/claude"
	"github.com/smeltdev/smelt/internal/harness/codex"
	"gotest.tools/v3/assert"
)

func failoverCandidates() []Candidate {
	return []Candidate{{Harness: codex.Harness{}}, {Harness: claude.Harness{}}}
}

func TestFailoverSameTurnAndExhaustion(t *testing.T) {
	early := time.Date(2026, 10, 19, 2, 31, 0, 0, time.UTC)
	late := early.Add(time.Hour)
	f := NewFailover(failoverCandidates(), 0, nil)
	var calls []harness.Name
	err := f.Do(context.Background(), 2, "implementer", func(c Candidate) (*harness.RateLimit, error) {
		calls = append(calls, c.Harness.Name())
		reset := &early
		if c.Harness.Name() == harness.Claude {
			reset = &late
		}
		return nil, &harness.Failure{Kind: harness.RateLimited, ResetAt: reset}
	})
	var exhausted *Exhaustion
	assert.Assert(t, errors.As(err, &exhausted))
	assert.Equal(t, err.Error(), "no harness available")
	assert.Assert(t, exhausted.ResetAt.Equal(early))
	assert.DeepEqual(t, calls, []harness.Name{harness.Codex, harness.Claude})
	assert.Equal(t, f.Swaps()[0].String(), "swapped codex → claude (rate_limited) in round 2")
}

func TestFailoverTimeoutStreakAndCodeFailures(t *testing.T) {
	for _, tc := range []struct {
		name     string
		failures int
		kind     harness.FailureKind
		want     []harness.Name
	}{
		{"single timeout", 1, harness.Timeout, []harness.Name{harness.Codex, harness.Codex}},
		{"two timeouts", 2, harness.Timeout, []harness.Name{harness.Codex, harness.Codex, harness.Claude}},
		{"auth", 1, harness.Auth, []harness.Name{harness.Codex, harness.Claude}},
		{"unavailable", 1, harness.Unavailable, []harness.Name{harness.Codex, harness.Claude}},
		{"code failure", 1, "", []harness.Name{harness.Codex}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := NewFailover(failoverCandidates(), 0, nil)
			var calls []harness.Name
			err := f.Do(context.Background(), 1, "implementer", func(c Candidate) (*harness.RateLimit, error) {
				calls = append(calls, c.Harness.Name())
				if len(calls) <= tc.failures {
					if tc.kind == "" {
						return nil, errors.New("gate failed")
					}
					return nil, &harness.Failure{Kind: tc.kind}
				}
				return nil, nil
			})
			if tc.kind != "" {
				assert.NilError(t, err)
			} else {
				assert.Error(t, err, "gate failed")
			}
			assert.DeepEqual(t, calls, tc.want)
		})
	}
}

func TestFailoverProactiveUsesPreciseClaudeWindows(t *testing.T) {
	for _, utilization := range []string{"0.9499", "0.96"} {
		t.Run(utilization, func(t *testing.T) {
			p := claude.Harness{}.NewImplementParser(nil)
			p.Write([]byte(`{"type":"rate_limit_event","rate_limit_info":{"status":"allowed","unifiedWindows":{"five_hour":{"utilization":` + utilization + `,"resetsAt":1792365600},"seven_day":{"utilization":0.47,"resetsAt":1792432800}}}}` + "\n"))
			cs := failoverCandidates()
			cs[0], cs[1] = cs[1], cs[0]
			f := NewFailover(cs, 0, nil)
			assert.NilError(t, f.Do(context.Background(), 1, "implementer", func(c Candidate) (*harness.RateLimit, error) { return p.Outcome().Turn.RateLimit, nil }))
			var got harness.Name
			assert.NilError(t, f.Do(context.Background(), 2, "implementer", func(c Candidate) (*harness.RateLimit, error) { got = c.Harness.Name(); return nil, nil }))
			want := harness.Claude
			if utilization == "0.96" {
				want = harness.Codex
			}
			assert.Equal(t, got, want)
		})
	}
}

func TestFailoverConcurrentReviewsHaveIndependentTimeoutStreaks(t *testing.T) {
	f := NewFailover(failoverCandidates(), 0, nil)
	var wg sync.WaitGroup
	for _, stream := range []string{"review:a", "review:b"} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			calls := 0
			err := f.Do(context.Background(), 1, stream, func(c Candidate) (*harness.RateLimit, error) {
				calls++
				if calls == 1 {
					return nil, &harness.Failure{Kind: harness.Timeout}
				}
				return nil, nil
			})
			assert.Check(t, err == nil)
		}()
	}
	wg.Wait()
	assert.Equal(t, len(f.Swaps()), 0)
}

func TestFailoverCancellationNeverSwaps(t *testing.T) {
	f := NewFailover(failoverCandidates(), 0, nil)
	ctx, cancel := context.WithCancel(context.Background())
	err := f.Do(ctx, 1, "implementer", func(Candidate) (*harness.RateLimit, error) {
		cancel()
		return nil, &harness.Failure{Kind: harness.Timeout}
	})
	assert.ErrorIs(t, err, context.Canceled)
	assert.Equal(t, len(f.Swaps()), 0)
}

func TestFailoverCompletedOutcomeClearsTimeoutStreak(t *testing.T) {
	f := NewFailover(failoverCandidates(), 0, nil)
	for range 2 {
		calls := 0
		assert.NilError(t, f.Do(context.Background(), 1, "review:a", func(c Candidate) (*harness.RateLimit, error) {
			calls++
			if calls == 1 {
				return nil, &harness.Failure{Kind: harness.Timeout}
			}
			return nil, nil
		}))
	}
	assert.Equal(t, len(f.Swaps()), 0)
}

func TestFailoverConfiguredThreshold(t *testing.T) {
	f := NewFailover(failoverCandidates(), 0.9, nil)
	assert.NilError(t, f.Do(t.Context(), 1, "implementer", func(c Candidate) (*harness.RateLimit, error) {
		return &harness.RateLimit{Windows: map[string]harness.UsageWindow{"five_hour": {Utilization: 0.92}}}, nil
	}))
	assert.NilError(t, f.Do(t.Context(), 2, "implementer", func(c Candidate) (*harness.RateLimit, error) {
		assert.Equal(t, c.Harness.Name(), harness.Claude)
		return nil, nil
	}))
	assert.Equal(t, len(f.Swaps()), 1)
}

func TestFailoverSkipsCredentialFailureAndPreservesExhaustion(t *testing.T) {
	cs := failoverCandidates()
	cs[1].Failure = &harness.Failure{Kind: harness.Auth, Cause: errors.New("credential unavailable")}
	f := NewFailover(cs, 0, nil)
	reset := time.Now().Add(time.Hour)
	calls := 0
	err := f.Do(t.Context(), 1, "implementer", func(c Candidate) (*harness.RateLimit, error) {
		calls++
		assert.Equal(t, c.Harness.Name(), harness.Codex)
		return nil, &harness.Failure{Kind: harness.RateLimited, ResetAt: &reset}
	})
	var exhausted *Exhaustion
	assert.Assert(t, errors.As(err, &exhausted))
	assert.Equal(t, calls, 1)
	assert.Assert(t, exhausted.ResetAt.Equal(reset))
	assert.Equal(t, f.Exhausted(), exhausted)
	err = f.Do(t.Context(), 2, "implementer", func(Candidate) (*harness.RateLimit, error) {
		t.Fatal("exhausted run must not invoke a candidate")
		return nil, nil
	})
	assert.Equal(t, err, error(exhausted))
}

func TestFailoverProactiveSkipKeepsTheLastCandidate(t *testing.T) {
	f := NewFailover([]Candidate{{Harness: claude.Harness{}}}, 0, nil)
	hot := 96
	usage := &harness.RateLimit{Utilization: &hot}
	var calls int
	for round := 1; round <= 2; round++ {
		err := f.Do(context.Background(), round, "implementer", func(Candidate) (*harness.RateLimit, error) {
			calls++
			return usage, nil
		})
		assert.NilError(t, err)
	}
	assert.Equal(t, calls, 2)
	assert.Equal(t, len(f.Swaps()), 0)
}
