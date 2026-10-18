package factory

import (
	"strings"
	"testing"
	"time"

	"gotest.tools/v3/assert"

	"github.com/smeltdev/smelt/internal/harness"
)

func TestToolCallsTimeEachCallUntilTheNextActivity(t *testing.T) {
	t0 := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	acts := []Activity{{Tool: "Edit", Detail: "a.go"}, {Tool: "", Detail: "thinking"}, {Tool: "Bash", Detail: "task ci:test"}, {Tool: "Read", Detail: "b.go"}}
	seen := []time.Time{t0, t0.Add(10 * time.Second), t0.Add(15 * time.Second), t0.Add(2*time.Minute + 26*time.Second)}
	got := toolCalls(acts, seen, t0.Add(2*time.Minute+30*time.Second))
	assert.DeepEqual(t, got, []ToolCall{
		{Tool: "Edit", Detail: "a.go", Took: 10 * time.Second},
		{Tool: "Bash", Detail: "task ci:test", Took: 2*time.Minute + 11*time.Second},
		{Tool: "Read", Detail: "b.go", Took: 4 * time.Second},
	})
}

func TestSlowCalls(t *testing.T) {
	calls := []ToolCall{{Tool: "Bash", Took: 2 * time.Minute}, {Tool: "Bash", Detail: "sleep 400", Took: 6*time.Minute + 40*time.Second}}
	got := slowCalls(calls, slowCallAfter)
	assert.Equal(t, len(got), 1)
	assert.Equal(t, got[0].Detail, "sleep 400")
}

func TestTimerAddsPhasesUpFromEvents(t *testing.T) {
	clock := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	tm := timer{now: func() time.Time { return clock }}
	tm.event(Event{Kind: EventPlanning})
	clock = clock.Add(62 * time.Second)
	tm.event(Event{Kind: EventPlanReviewed})
	tm.event(Event{Kind: EventImplemented, Round: 1, Turn: Turn{Duration: 3 * time.Minute, Calls: []ToolCall{
		{Tool: "Bash", Detail: "go test ./x/...", Took: 70 * time.Second},
		{Tool: "Bash", Detail: "task ci:test; sleep 400", Took: 3 * time.Minute},
	}}})
	tm.event(Event{Kind: EventImplemented, Round: 2, Turn: Turn{Duration: 40 * time.Second}})
	tm.event(Event{Kind: EventChecked, Round: 1, Checks: []Check{
		{Kind: KindValidate, Status: StatusFailed}, {Kind: KindReview, Status: StatusSkipped},
	}})
	tm.event(Event{Kind: EventChecked, Round: 2, Checks: []Check{{Kind: KindValidate, Status: StatusPassed}}})

	got := tm.timing
	assert.Equal(t, got.Plan, 62*time.Second)
	assert.Equal(t, got.Implement, 3*time.Minute+40*time.Second)
	assert.Equal(t, got.Commands, 4*time.Minute+10*time.Second)
	assert.DeepEqual(t, got.GateOnlyRounds, []int{1})

	got.Gates, got.Reviews, got.Notes, got.Total = 52*time.Second, 21*time.Second, 15*time.Second, 6*time.Minute+10*time.Second
	assert.Equal(t, got.Format(), "Time: plan 1m02s · implement 3m40s (commands 4m10s) · gates 0m52s · reviews 0m21s · notes 0m15s · total 6m10s")
}

func TestTimerWarnsOfAToolCallOverTwoMinutes(t *testing.T) {
	tm := timer{now: time.Now}
	tm.event(Event{Kind: EventImplemented, Round: 2, Turn: Turn{Calls: []ToolCall{
		{Tool: "Bash", Detail: "task ci:test", Took: 90 * time.Second},
		{Tool: "Bash", Detail: "sleep 400;\n tail -30 out", Took: 6*time.Minute + 40*time.Second},
	}}})
	warnings := tm.timing.Warnings()
	assert.Equal(t, len(warnings), 1)
	assert.Equal(t, warnings[0], "round 2: the implementer's Bash call took 6m40s: sleep 400; tail -30 out")
	assert.Assert(t, !strings.Contains(warnings[0], "task ci:test"))
}

func TestGatesOnlyFailed(t *testing.T) {
	gate := func(s Status) Check { return Check{Kind: KindValidate, Status: s} }
	rev := func(s Status) Check { return Check{Kind: KindReview, Status: s} }
	for name, tt := range map[string]struct {
		checks []Check
		want   bool
	}{
		"gate failed, reviews skipped": {[]Check{gate(StatusFailed), rev(StatusSkipped)}, true},
		"gate and review failed":       {[]Check{gate(StatusFailed), rev(StatusFailed)}, false},
		"review failed":                {[]Check{gate(StatusPassed), rev(StatusFailed)}, false},
		"all passed":                   {[]Check{gate(StatusPassed), rev(StatusPassed)}, false},
		"gate errored":                 {[]Check{gate(StatusErrored)}, false},
	} {
		assert.Equal(t, GatesOnlyFailed(tt.checks), tt.want, name)
	}
}

func TestTimingFormatsTheContextLine(t *testing.T) {
	timeLine := "Time: plan 1m09s · implement 3m40s (commands 1m10s) · gates 0m52s · reviews 0m21s · notes 0m15s · total 6m10s"
	base := Timing{Plan: 69 * time.Second, Implement: 220 * time.Second, Commands: 70 * time.Second, Gates: 52 * time.Second,
		Reviews: 21 * time.Second, Notes: 15 * time.Second, Total: 370 * time.Second}
	assert.Equal(t, base.Format(), timeLine)

	base.ImplementContext = []RoundContext{{Round: 1, Context: harness.Context{Peak: 88412, Calls: 24}}, {Round: 2, Context: harness.Context{Peak: 52100, Calls: 9}}}
	base.ReviewsContext = harness.Context{Peak: 41000, Calls: 1}
	base.PlanContext = harness.Context{Peak: 31000, Calls: 1}
	assert.Equal(t, base.Format(), timeLine+"\nContext: implementer peak 88k (round 1), 52k (round 2) · reviews peak 41k · plan 31k")
	assert.Equal(t, base.PeakMax(), 88412)

	unknown := Timing{Plan: time.Second, Reviews: time.Second, ImplementContext: []RoundContext{{Round: 1, Context: harness.Context{Peak: 90000}}}}
	assert.Assert(t, strings.HasSuffix(unknown.Format(), "\nContext: implementer peak 90k · reviews peak n/a · plan n/a"), unknown.Format())

	// A turn with a context of its own size known only for the notes is not shown.
	notesOnly := Timing{NotesContext: harness.Context{Peak: 30700}, ImplementContext: []RoundContext{{Round: 1}}}
	assert.Assert(t, !strings.Contains(notesOnly.Format(), "Context"), notesOnly.Format())
	assert.Equal(t, notesOnly.PeakMax(), 30700)
}

func TestTimerCollectsContext(t *testing.T) {
	tm := timer{now: time.Now}
	tm.event(Event{Kind: EventPlanReviewed, Plan: &Plan{Context: harness.Context{Peak: 31000, Calls: 1}}})
	tm.event(Event{Kind: EventImplemented, Round: 1, Turn: Turn{ContextPeak: 88412, ModelCalls: 24}})
	tm.event(Event{Kind: EventChecked, Round: 1, Checks: []Check{
		{Kind: KindReview, Context: harness.Context{Peak: 20000, Calls: 1}},
		{Kind: KindReview, Context: harness.Context{Peak: 41000, Calls: 1}},
		{Kind: KindValidate},
	}})
	got := tm.timing
	assert.Equal(t, got.PlanContext.Peak, 31000)
	assert.DeepEqual(t, got.ImplementContext, []RoundContext{{Round: 1, Context: harness.Context{Peak: 88412, Calls: 24}}})
	assert.Equal(t, got.ReviewsContext.Peak, 41000)
}
