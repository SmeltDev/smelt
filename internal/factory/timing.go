package factory

import (
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/smeltdev/smelt/internal/harness"
)

// slowCallAfter is how long one tool call may take before the run warns about
// it. A call this long is usually a command that should have been quick or a
// poll loop, and the next slow pattern is easiest to spot with its command.
const slowCallAfter = 2 * time.Minute

// ToolCall is one tool call of an implementer turn and how long it took.
type ToolCall struct {
	Tool   string
	Detail string
	Took   time.Duration
}

// toolCalls works out how long each tool call of a turn took from when its
// activity arrived. The parser reports a call when the agent issues it, so it
// ran until the next activity arrived, or until end for the last one. The gap
// includes the agent's own turnaround, which makes this a slight
// overestimate. seen[i] is when acts[i] arrived; end is when the turn ended.
func toolCalls(acts []Activity, seen []time.Time, end time.Time) []ToolCall {
	var calls []ToolCall
	for i, a := range acts {
		if a.Tool == "" || i >= len(seen) {
			continue
		}
		next := end
		if i+1 < len(seen) {
			next = seen[i+1]
		}
		calls = append(calls, ToolCall{Tool: a.Tool, Detail: a.Detail, Took: max(next.Sub(seen[i]), 0)})
	}
	return calls
}

// SlowCall is a tool call that took longer than slowCallAfter.
type SlowCall struct {
	Round int
	ToolCall
}

// slowCalls is the calls that took longer than limit.
func slowCalls(calls []ToolCall, limit time.Duration) []ToolCall {
	return slices.DeleteFunc(slices.Clone(calls), func(c ToolCall) bool { return c.Took <= limit })
}

// Warning is the line that reports a slow call, with its command.
func (c SlowCall) Warning() string {
	return fmt.Sprintf("round %d: the implementer's %s call took %s: %s", c.Round, c.Tool, c.Took.Round(time.Second), oneLine(c.Detail))
}

// Timing is where a run's time went.
type Timing struct {
	Plan time.Duration
	// Implement is the time of the implementer's turns, of which Commands was
	// spent inside tool calls.
	Implement time.Duration
	Commands  time.Duration
	// Gates and Reviews are the time of the validation commands and of the
	// reviews, whichever order they ran in.
	Gates   time.Duration
	Reviews time.Duration
	Notes   time.Duration
	Total   time.Duration
	// GateOnlyRounds are the rounds that failed on gates alone, which a
	// quick check in the turn could have caught.
	GateOnlyRounds []int
	// SlowCalls are the implementer's tool calls over slowCallAfter.
	SlowCalls []SlowCall
	// PlanContext, ImplementContext, ReviewsContext and NotesContext are how
	// large the prompts of each phase's turns got. ImplementContext has one
	// entry per implementer turn; ReviewsContext is the largest of the
	// reviews' turns. Zero values are unknown.
	PlanContext      harness.Context
	ImplementContext []RoundContext
	ReviewsContext   harness.Context
	NotesContext     harness.Context
}

// RoundContext is the context size of one round's implementer turn.
type RoundContext struct {
	Round int
	harness.Context
}

// Format is the account of the timing: the Time line, and below it the Context
// line when any turn's context size is known.
func (t Timing) Format() string {
	line := fmt.Sprintf("Time: plan %s · implement %s (commands %s) · gates %s · reviews %s · notes %s · total %s",
		minSec(t.Plan), minSec(t.Implement), minSec(t.Commands), minSec(t.Gates), minSec(t.Reviews), minSec(t.Notes), minSec(t.Total))
	if ctx := t.contextLine(); ctx != "" {
		line += "\n" + ctx
	}
	return line
}

// PeakMax is the largest context of any turn of the run, 0 when none is known.
func (t Timing) PeakMax() int {
	peak := max(t.PlanContext.Peak, t.ReviewsContext.Peak, t.NotesContext.Peak)
	for _, c := range t.ImplementContext {
		peak = max(peak, c.Peak)
	}
	return peak
}

// contextLine is the Context line, or "" when no turn it covers has a known
// size. The notes turn is measured but left off: it is for the run log.
func (t Timing) contextLine() string {
	known := t.PlanContext.Peak > 0 || t.ReviewsContext.Peak > 0
	var parts []string
	if len(t.ImplementContext) > 0 {
		rounds := make([]string, 0, len(t.ImplementContext))
		for _, c := range t.ImplementContext {
			known = known || c.Peak > 0
			label := kTokens(c.Peak)
			if len(t.ImplementContext) > 1 {
				label += fmt.Sprintf(" (round %d)", c.Round)
			}
			rounds = append(rounds, label)
		}
		parts = append(parts, "implementer peak "+strings.Join(rounds, ", "))
	}
	if t.Reviews > 0 || t.ReviewsContext.Peak > 0 {
		parts = append(parts, "reviews peak "+kTokens(t.ReviewsContext.Peak))
	}
	if t.Plan > 0 || t.PlanContext.Peak > 0 {
		parts = append(parts, "plan "+kTokens(t.PlanContext.Peak))
	}
	if !known {
		return ""
	}
	return "Context: " + strings.Join(parts, " · ")
}

// kTokens renders a token count as 88k, or n/a when it is not known.
func kTokens(n int) string {
	if n <= 0 {
		return "n/a"
	}
	return fmt.Sprintf("%dk", (n+500)/1000)
}

// contextLogLine is one turn's context size for the run log.
func contextLogLine(c harness.Context) string {
	if c.Peak <= 0 {
		return "context peak n/a (not streamed)"
	}
	line := "context peak " + commas(c.Peak) + " tokens"
	switch {
	case c.Calls == 1:
		line += " (1 model call)"
	case c.Calls > 1:
		line += fmt.Sprintf(" (%d model calls)", c.Calls)
	}
	return line
}

// commas groups n's digits in threes.
func commas(n int) string {
	s := strconv.Itoa(n)
	for i := len(s) - 3; i > 0; i -= 3 {
		s = s[:i] + "," + s[i:]
	}
	return s
}

// minSec renders d as 3m07s.
func minSec(d time.Duration) string {
	secs := int(d.Round(time.Second) / time.Second)
	return fmt.Sprintf("%dm%02ds", secs/60, secs%60)
}

// GatesOnlyFailed reports whether a round's checks failed on validation
// commands and on nothing else: no review found anything to fix.
func GatesOnlyFailed(checks []Check) bool {
	var gateFailed bool
	for _, c := range checks {
		if c.Status != StatusFailed {
			continue
		}
		if c.Kind != KindValidate {
			return false
		}
		gateFailed = true
	}
	return gateFailed
}

// timer builds a Timing from the loop's events. The phases the loop does not
// report, gates, reviews and notes, are set by the caller afterwards.
type timer struct {
	now       func() time.Time
	planStart time.Time
	timing    Timing
}

// event takes one event of the loop into the timing.
func (t *timer) event(e Event) {
	switch e.Kind {
	case EventPlanning:
		t.planStart = t.now()
	case EventPlanReviewed:
		if e.Plan != nil {
			t.timing.PlanContext = e.Plan.Context
		}
		if !t.planStart.IsZero() {
			t.timing.Plan += t.now().Sub(t.planStart)
		}
	case EventImplemented:
		t.timing.Implement += e.Turn.Duration
		t.timing.ImplementContext = append(t.timing.ImplementContext,
			RoundContext{Round: e.Round, Context: harness.Context{Peak: e.Turn.ContextPeak, Calls: e.Turn.ModelCalls}})
		for _, c := range e.Turn.Calls {
			t.timing.Commands += c.Took
		}
		for _, c := range slowCalls(e.Turn.Calls, slowCallAfter) {
			t.timing.SlowCalls = append(t.timing.SlowCalls, SlowCall{Round: e.Round, ToolCall: c})
		}
	case EventChecked:
		for _, c := range e.Checks {
			if c.Kind == KindReview && c.Context.Peak > t.timing.ReviewsContext.Peak {
				t.timing.ReviewsContext = c.Context
			}
		}
		if GatesOnlyFailed(e.Checks) {
			t.timing.GateOnlyRounds = append(t.timing.GateOnlyRounds, e.Round)
		}
	case EventImplementing, EventCollected, EventChecking, EventNoPriorWork:
	}
}

// Warnings are the lines that report the run's slow calls.
func (t Timing) Warnings() []string {
	if len(t.SlowCalls) == 0 {
		return nil
	}
	out := make([]string, 0, len(t.SlowCalls))
	for _, c := range t.SlowCalls {
		out = append(out, c.Warning())
	}
	return out
}
