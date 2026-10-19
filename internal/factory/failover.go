package factory

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

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
}

// NewFailover preserves caller order and removes repeated harnesses.
func NewFailover(candidates []Candidate, threshold float64, onSwap func(HarnessSwap)) *Failover {
	f := &Failover{skipped: map[harness.Name]error{}, usage: map[harness.Name]*harness.RateLimit{}, streak: map[string]int{}, threshold: threshold, onSwap: onSwap}
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
func (f *Failover) Do(ctx context.Context, round int, turn string, run func(Candidate) (*harness.RateLimit, error)) error {
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
		f.mu.Lock()
		name := c.Harness.Name()
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
