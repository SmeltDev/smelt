package factory

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"gotest.tools/v3/assert"

	"github.com/smeltdev/smelt/internal/harness"
	"github.com/smeltdev/smelt/internal/harness/claude"
	"github.com/smeltdev/smelt/internal/harness/codex"
	"github.com/smeltdev/smelt/internal/iostream"
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

// capacityClock is a fake clock: Sleep records the wait and returns at once.
type capacityClock struct {
	mu    sync.Mutex
	waits []time.Duration
	logs  []string
}

func (c *capacityClock) sleep(_ context.Context, d time.Duration) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.waits = append(c.waits, d)
	return nil
}

func (c *capacityClock) log(msg string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.logs = append(c.logs, msg)
}

func (c *capacityClock) total() (sum time.Duration) {
	for _, d := range c.waits {
		sum += d
	}
	return sum
}

func atCapacity() error {
	return harness.AtCapacity(errors.New("selected model is at capacity, please try a different model"))
}

func newCapacityFailover(candidates []Candidate, wait time.Duration) (*Failover, *capacityClock) {
	clock := &capacityClock{}
	f := NewFailover(candidates, 0, nil).Configure(FailoverOptions{Sleep: clock.sleep, Log: clock.log, CapacityWait: wait})
	return f, clock
}

func TestCapacityBackoff(t *testing.T) {
	for attempt, want := range []time.Duration{30 * time.Second, time.Minute, 2 * time.Minute, 4 * time.Minute, 5 * time.Minute, 5 * time.Minute} {
		assert.Equal(t, capacityBackoff(attempt), want)
	}
}

func TestFailoverRetriesTheSameHarnessAtCapacity(t *testing.T) {
	f, clock := newCapacityFailover(failoverCandidates(), 0)
	var calls []harness.Name
	err := f.Do(context.Background(), 1, "implementer", func(c Candidate) (*harness.RateLimit, error) {
		calls = append(calls, c.Harness.Name())
		if len(calls) <= 2 {
			return nil, atCapacity()
		}
		return nil, nil
	})
	assert.NilError(t, err)
	assert.DeepEqual(t, calls, []harness.Name{harness.Codex, harness.Codex, harness.Codex})
	assert.DeepEqual(t, clock.waits, []time.Duration{30 * time.Second, time.Minute})
	assert.DeepEqual(t, clock.logs, []string{
		"round 1 implementer: codex at capacity, retrying in 30s (1 of 4)",
		"round 1 implementer: codex at capacity, retrying in 1m (2 of 4)",
	})
	assert.Equal(t, len(f.Swaps()), 0)
}

func TestFailoverMovesOnAfterFourCapacityRetries(t *testing.T) {
	f, clock := newCapacityFailover(failoverCandidates(), 0)
	var calls []harness.Name
	err := f.Do(context.Background(), 1, "implementer", func(c Candidate) (*harness.RateLimit, error) {
		calls = append(calls, c.Harness.Name())
		if c.Harness.Name() == harness.Codex {
			return nil, atCapacity()
		}
		return nil, nil
	})
	assert.NilError(t, err)
	assert.Equal(t, len(calls), 6)
	assert.Equal(t, calls[5], harness.Claude)
	assert.DeepEqual(t, clock.waits, []time.Duration{30 * time.Second, time.Minute, 2 * time.Minute, 4 * time.Minute})
	assert.Equal(t, f.Swaps()[0].Kind, harness.Unavailable)
}

func TestFailoverWaitsUpToCapacityWaitThenIsUnavailable(t *testing.T) {
	f, clock := newCapacityFailover([]Candidate{{Harness: codex.Harness{}}}, 30*time.Minute)
	err := f.Do(context.Background(), 1, "implementer", func(Candidate) (*harness.RateLimit, error) {
		return nil, atCapacity()
	})
	var unavailable *HarnessUnavailable
	assert.Assert(t, errors.As(err, &unavailable))
	assert.Equal(t, err.Error(), "codex at capacity for 30m")
	assert.Equal(t, clock.total(), 30*time.Minute)
	for _, d := range clock.waits {
		assert.Assert(t, d <= 5*time.Minute)
	}
	assert.Assert(t, f.Exhausted() == nil)
	// A later success still works: giving up did not exhaust the failover.
	assert.NilError(t, f.Do(context.Background(), 2, "implementer", func(Candidate) (*harness.RateLimit, error) { return nil, nil }))
}

func TestFailoverCapacityWaitHonoursCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	f := NewFailover(failoverCandidates(), 0, nil).Configure(FailoverOptions{
		Sleep: func(ctx context.Context, _ time.Duration) error { cancel(); return ctx.Err() },
	})
	err := f.Do(ctx, 1, "implementer", func(Candidate) (*harness.RateLimit, error) { return nil, atCapacity() })
	assert.ErrorIs(t, err, context.Canceled)
	var unavailable *HarnessUnavailable
	assert.Assert(t, !errors.As(err, &unavailable))
}

func TestFailoverDoesNotRetryAMissingBinary(t *testing.T) {
	f, clock := newCapacityFailover([]Candidate{{Harness: codex.Harness{}}}, 0)
	err := f.Do(context.Background(), 1, "implementer", func(Candidate) (*harness.RateLimit, error) {
		return nil, harness.Classified(harness.Unavailable, errors.New("codex missing"), nil)
	})
	var exhausted *Exhaustion
	assert.Assert(t, errors.As(err, &exhausted))
	assert.Equal(t, len(clock.waits), 0)
}

// TestConfigureFailoverWiresCapacityWaitAndLog drives the Failover a real run
// builds, so a dropped CapacityWait or Log option shows up here.
func TestConfigureFailoverWiresCapacityWaitAndLog(t *testing.T) {
	var mu sync.Mutex
	var lines []string
	status := func(_ iostream.Level, msg string) {
		mu.Lock()
		defer mu.Unlock()
		lines = append(lines, msg)
	}
	steps := &Sidecars{Implementer: &Implementer{Harness: codex.Harness{}}}
	configureFailover(steps, RunOptions{CapacityWait: 2 * time.Millisecond}, status)

	calls := 0
	err := steps.Failover.Do(context.Background(), 3, "implementer", func(Candidate) (*harness.RateLimit, error) {
		calls++
		return nil, atCapacity()
	})
	var unavailable *HarnessUnavailable
	assert.Assert(t, errors.As(err, &unavailable), "%v", err)
	assert.Equal(t, unavailable.Waited, 2*time.Millisecond)
	assert.Equal(t, err.Error(), "codex at capacity for 2ms")
	assert.Equal(t, calls, 2)
	mu.Lock()
	defer mu.Unlock()
	assert.DeepEqual(t, lines, []string{"round 3 implementer: codex at capacity, retrying in 2ms (1 of 4)"})
}
