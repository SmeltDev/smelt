package factory

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/smeltdev/smelt/internal/config"
	"github.com/smeltdev/smelt/internal/harness"
)

// Candidate is an immutable harness, credential and model snapshot for an attempt.
type Candidate struct {
	Harness    harness.Harness
	Credential harness.Credential
	BaseURL    string
	Model      string
	// Failure explains why the daemon could not authenticate this candidate.
	Failure error
}

// HarnessSwap records a change of agent for the same logical turn.
type HarnessSwap struct {
	Round   int                 `json:"round"`
	Turn    string              `json:"turn"`
	From    harness.Name        `json:"from"`
	To      harness.Name        `json:"to"`
	Kind    harness.FailureKind `json:"kind"`
	ResetAt *time.Time          `json:"reset_at,omitempty"`
}

// String is the wording shared by logs and timing.
func (s HarnessSwap) String() string {
	return fmt.Sprintf("swapped %s → %s (%s) in round %d", s.From, s.To, s.Kind, s.Round)
}

// Exhaustion means every caller-selected harness is unavailable for this run.
type Exhaustion struct {
	ResetAt *time.Time
	Cause   error
}

const noHarnessAvailable = "no harness available"

// Error is the stable, user-facing exhaustion reason.
func (*Exhaustion) Error() string { return noHarnessAvailable }

// StopReviewPass marks exhaustion as fatal after per-review retries.
func (*Exhaustion) StopReviewPass() {}

// Unwrap retains the final provider failure.
func (e *Exhaustion) Unwrap() error { return e.Cause }

// HarnessUnavailable means the last candidate stayed at capacity for the whole
// wait. Unlike Exhaustion, it clears by itself, so a caller can retry later.
type HarnessUnavailable struct {
	Harness harness.Name
	// Waited is how long the run waited on Harness before giving up.
	Waited time.Duration
	Cause  error
}

// Error is the stable, user-facing reason.
func (e *HarnessUnavailable) Error() string {
	return fmt.Sprintf("%s at capacity for %s", e.Harness, formatWait(e.Waited))
}

// StopReviewPass marks a harness that stayed busy as fatal to a review pass.
func (*HarnessUnavailable) StopReviewPass() {}

// Unwrap retains the final provider failure.
func (e *HarnessUnavailable) Unwrap() error { return e.Cause }

// formatWait prints whole minutes as "2m" rather than "2m0s".
func formatWait(d time.Duration) string {
	if d >= time.Minute && d%time.Minute == 0 {
		return fmt.Sprintf("%dm", d/time.Minute)
	}
	return d.String()
}

const (
	// capacityRetries is how many times the same harness is retried before
	// moving to the next candidate.
	capacityRetries = 4
	capacityBase    = 30 * time.Second
	capacityMaxStep = 5 * time.Minute
)

// capacityBackoff is the wait before retry number attempt (from 0): 30s, 1m,
// 2m, 4m, then 5m apart.
func capacityBackoff(attempt int) time.Duration {
	if attempt >= 4 {
		return capacityMaxStep
	}
	return min(capacityBase<<attempt, capacityMaxStep)
}

// capacityStep is what to do after a capacity failure.
type capacityStep struct {
	Wait time.Duration
	// Move means the retries are spent and another candidate can take over.
	Move bool
	// GiveUp means the wait is spent and nothing else can take over.
	GiveUp bool
}

// nextCapacityStep decides the next move. With a later candidate the first
// capacityRetries waits are free; on the last candidate every wait counts
// toward limit, and none overshoots it.
func nextCapacityStep(attempt int, waited, limit time.Duration, hasLater bool) capacityStep {
	if hasLater && attempt >= capacityRetries {
		return capacityStep{Move: true}
	}
	wait := capacityBackoff(attempt)
	if hasLater {
		return capacityStep{Wait: wait}
	}
	remaining := limit - waited
	if remaining <= 0 {
		return capacityStep{GiveUp: true}
	}
	return capacityStep{Wait: min(wait, remaining)}
}

// FailoverOptions are the seams of the capacity wait; the zero value waits in
// real time, logs nothing and gives up after config.DefaultCapacityWait.
type FailoverOptions struct {
	// Sleep waits d or returns ctx's error.
	Sleep func(ctx context.Context, d time.Duration) error
	// Log reports each wait.
	Log func(msg string)
	// CapacityWait bounds the wait on the last candidate.
	CapacityWait time.Duration
}

func sleepContext(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// Failover coordinates the run's independent turn streams. It never ranks agents.
type Failover struct {
	mu         sync.Mutex
	candidates []Candidate
	current    int
	skipped    map[harness.Name]error
	usage      map[harness.Name]*harness.RateLimit
	streak     map[string]int
	threshold  float64
	swaps      []HarnessSwap
	exhausted  *Exhaustion
	onSwap     func(HarnessSwap)
	opts       FailoverOptions
}

// Configure sets the capacity wait options; call it before the first Do.
func (f *Failover) Configure(o FailoverOptions) *Failover {
	if o.Sleep == nil {
		o.Sleep = sleepContext
	}
	if o.CapacityWait <= 0 {
		o.CapacityWait = config.DefaultCapacityWait
	}
	f.opts = o
	return f
}

// NewFailover preserves caller order and removes repeated harnesses.
func NewFailover(candidates []Candidate, threshold float64, onSwap func(HarnessSwap)) *Failover {
	f := &Failover{opts: FailoverOptions{Sleep: sleepContext, CapacityWait: config.DefaultCapacityWait}, skipped: map[harness.Name]error{}, usage: map[harness.Name]*harness.RateLimit{}, streak: map[string]int{}, threshold: threshold, onSwap: onSwap}
	if threshold == 0 {
		f.threshold = 0.95
	}
	seen := map[harness.Name]bool{}
	for _, c := range candidates {
		if c.Harness != nil && !seen[c.Harness.Name()] {
			f.candidates = append(f.candidates, c)
			seen[c.Harness.Name()] = true
		}
	}
	return f
}

// Swaps returns a detached history safe to serialize while reviews are running.
func (f *Failover) Swaps() []HarnessSwap {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]HarnessSwap(nil), f.swaps...)
}

// Exhausted returns the terminal failure, including failures during notes.
func (f *Failover) Exhausted() *Exhaustion {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.exhausted
}

func (f *Failover) exhaust(cause error) error {
	e := &Exhaustion{Cause: cause}
	for _, err := range f.skipped {
		var failure *harness.Failure
		if errors.As(err, &failure) && failure.Kind == harness.RateLimited && failure.ResetAt != nil && (e.ResetAt == nil || failure.ResetAt.Before(*e.ResetAt)) {
			t := *failure.ResetAt
			e.ResetAt = &t
		}
	}
	f.exhausted = e
	return e
}

// hasLaterCandidate reports whether a candidate after the current one has not
// failed, so a utilization-based skip never strands the run on nothing.
// Callers hold f.mu.
func (f *Failover) hasLaterCandidate() bool {
	for _, c := range f.candidates[f.current+1:] {
		if _, skipped := f.skipped[c.Harness.Name()]; !skipped && c.Failure == nil {
			return true
		}
	}
	return false
}

// skipIfOverThreshold marks name failed when its last reported utilization is
// at or over the threshold and another candidate can take over. Callers hold
// f.mu.
func (f *Failover) skipIfOverThreshold(name harness.Name) {
	rl := f.usage[name]
	if rl == nil || !f.hasLaterCandidate() {
		return
	}
	var highest float64
	reset := rl.ResetsAt
	if rl.Utilization != nil {
		highest = float64(*rl.Utilization) / 100
	}
	for _, w := range rl.Windows {
		if w.Utilization >= highest {
			highest, reset = w.Utilization, w.ResetsAt
		}
	}
	if highest >= f.threshold {
		f.skipped[name] = &harness.Failure{Kind: harness.RateLimited, ResetAt: reset}
	}
}

// recordSwap notes the move from one harness to another. Callers hold f.mu.
func (f *Failover) recordSwap(round int, turn string, from, to harness.Name) {
	var failure *harness.Failure
	errors.As(f.skipped[from], &failure)
	swap := HarnessSwap{Round: round, Turn: turn, From: from, To: to}
	if failure != nil {
		swap.Kind, swap.ResetAt = failure.Kind, failure.ResetAt
	}
	f.swaps = append(f.swaps, swap)
	if f.onSwap != nil {
		f.onSwap(swap)
	}
}

func (f *Failover) selectCandidate(round int, turn string) (Candidate, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.exhausted != nil {
		return Candidate{}, f.exhausted
	}
	if len(f.candidates) == 0 {
		return Candidate{}, f.exhaust(nil)
	}
	from := f.candidates[f.current].Harness.Name()
	for f.current < len(f.candidates) {
		c := f.candidates[f.current]
		name := c.Harness.Name()
		if c.Failure != nil {
			f.skipped[name] = c.Failure
		}
		if _, skipped := f.skipped[name]; !skipped {
			f.skipIfOverThreshold(name)
		}
		if _, skipped := f.skipped[name]; !skipped {
			if name != from {
				f.recordSwap(round, turn, from, name)
			}
			return c, nil
		}
		f.current++
	}
	// Keep current in bounds for subsequent calls.
	f.current = len(f.candidates) - 1
	return Candidate{}, f.exhaust(f.skipped[from])
}

// Do retries only classified harness failures; deadlines are counted per logical stream.
//
// A capacity refusal waits and retries the same harness first, then moves on
// like a rate limit; the last candidate keeps waiting until CapacityWait is
// spent and then gives up with *HarnessUnavailable.
func (f *Failover) Do(ctx context.Context, round int, turn string, run func(Candidate) (*harness.RateLimit, error)) error {
	var (
		onHarness harness.Name
		retries   int
		waited    time.Duration
	)
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		c, err := f.selectCandidate(round, turn)
		if err != nil {
			return err
		}
		rl, err := run(c)
		if ctx.Err() != nil {
			return ctx.Err()
		}
		name := c.Harness.Name()
		if name != onHarness {
			onHarness, retries, waited = name, 0, 0
		}
		if harness.IsAtCapacity(err) {
			wait, waitErr := f.waitForCapacity(ctx, c, err, retries, waited, capacityLabel{round, turn})
			if waitErr != nil {
				return waitErr
			}
			retries++
			waited += wait
			continue
		}
		f.mu.Lock()
		if rl != nil {
			f.usage[name] = rl
		}
		key := fmt.Sprintf("%s/%d/%s", name, round, turn)
		var failure *harness.Failure
		if !errors.As(err, &failure) {
			delete(f.streak, key)
			f.mu.Unlock()
			return err
		}
		if failure.Kind == harness.Timeout {
			f.streak[key]++
			if f.streak[key] < 2 {
				f.mu.Unlock()
				continue
			}
		} else {
			delete(f.streak, key)
		}
		f.skipped[name] = err
		f.mu.Unlock()
	}
}

type capacityLabel struct {
	round int
	turn  string
}

// waitForCapacity handles one capacity refusal from c. It returns after
// sleeping so the caller retries, or after marking c skipped so the next
// candidate is selected. A non-nil error ends the call: the context's, or
// *HarnessUnavailable once the wait is spent. It sleeps outside f.mu.
func (f *Failover) waitForCapacity(ctx context.Context, c Candidate, cause error, retries int, waited time.Duration, at capacityLabel) (time.Duration, error) {
	name := c.Harness.Name()
	f.mu.Lock()
	step := nextCapacityStep(retries, waited, f.opts.CapacityWait, f.hasLaterCandidate())
	switch {
	case step.Move:
		f.skipped[name] = cause
	case step.GiveUp:
		f.mu.Unlock()
		return 0, &HarnessUnavailable{Harness: name, Waited: waited, Cause: cause}
	}
	f.mu.Unlock()
	if step.Move {
		return 0, nil
	}
	if f.opts.Log != nil {
		progress := fmt.Sprintf("(%d of %d)", retries+1, capacityRetries)
		if retries >= capacityRetries {
			progress = fmt.Sprintf("(up to %s)", formatWait(f.opts.CapacityWait))
		}
		f.opts.Log(fmt.Sprintf("round %d %s: %s at capacity, retrying in %s %s", at.round, at.turn, name, formatWait(step.Wait), progress))
	}
	if err := f.opts.Sleep(ctx, step.Wait); err != nil {
		return 0, err
	}
	return step.Wait, nil
}
