package factory

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"gotest.tools/v3/assert"

	"github.com/smeltdev/smelt/internal/config"
	"github.com/smeltdev/smelt/internal/quality"
)

// fakeSteps scripts a loop: each Implement call takes the next change, each
// Check call the next round of checks.
type fakeSteps struct {
	changes []Change
	checks  [][]Check
	prompts []string
	checked int
	current Change
	implErr error
	summary string
}

func (f *fakeSteps) Implement(_ context.Context, prompt string) (Turn, error) {
	f.prompts = append(f.prompts, prompt)
	if f.implErr != nil {
		return Turn{}, f.implErr
	}
	f.current = f.changes[0]
	f.changes = f.changes[1:]
	if f.summary != "" {
		return Turn{Summary: f.summary}, nil
	}
	return Turn{Summary: "done"}, nil
}

func (f *fakeSteps) Collect(context.Context) (Change, error) { return f.current, nil }

func (f *fakeSteps) Check(context.Context, int) ([]Check, error) {
	c := f.checks[f.checked]
	f.checked++
	return c, nil
}

func change(n int) Change {
	return Change{Fingerprint: fmt.Sprintf("fp%d", n), Stat: fmt.Sprintf("%d files changed", n)}
}

var (
	pass    = Check{Name: "lint", Kind: KindValidate, Status: StatusPassed}
	fail    = Check{Name: "tests", Kind: KindReview, Status: StatusFailed, Feedback: "- [high] a.go:3: nil deref"}
	errored = Check{Name: "style", Kind: KindReview, Status: StatusErrored, Error: "timed out"}
)

func TestLoopPassesFirstRound(t *testing.T) {
	steps := &fakeSteps{changes: []Change{change(1)}, checks: [][]Check{{pass}}}
	out, err := Loop{Attempts: 3}.Run(context.Background(), steps, "add a flag")
	assert.NilError(t, err)
	assert.Equal(t, out.Result, ResultPassed)
	assert.Equal(t, out.Rounds, 1)
	assert.DeepEqual(t, steps.prompts, []string{"add a flag"})
}

type freshFakeSteps struct {
	fakeSteps
	// events records, in order, each FreshSession call and each Implement prompt.
	events    []string
	handoffs  int
	badInputs int
}

func (f *freshFakeSteps) FreshSession() { f.events = append(f.events, "fresh") }

func (f *freshFakeSteps) Implement(ctx context.Context, prompt string) (Turn, error) {
	f.events = append(f.events, "implement:"+prompt)
	return f.fakeSteps.Implement(ctx, prompt)
}

func (f *freshFakeSteps) Handoff(_ context.Context, ticket, feedback string, _ *Plan, prior Turn, change Change, checks []Check) (string, error) {
	f.handoffs++
	if ticket != "ticket" || feedback == "" || prior.Summary == "" || change.Stat == "" || len(checks) == 0 {
		f.badInputs++
	}
	return fmt.Sprintf("handoff %d", f.handoffs), nil
}

func TestFreshRoundsStartSessionAndUseHandoff(t *testing.T) {
	steps := &freshFakeSteps{fakeSteps: fakeSteps{changes: []Change{change(1), change(2), change(3)}, checks: [][]Check{{fail}, {fail}, {pass}}}}
	out, err := (Loop{Attempts: 3, FreshRounds: true}).Run(context.Background(), steps, "ticket")
	assert.NilError(t, err)
	assert.Equal(t, out.Result, ResultPassed)
	assert.Equal(t, out.Rounds, 3)
	assert.Equal(t, steps.badInputs, 0)
	// Round 1 keeps the ticket; rounds 2 and 3 each reset the session first.
	assert.DeepEqual(t, steps.events, []string{"implement:ticket", "fresh", "implement:handoff 1", "fresh", "implement:handoff 2"})
}

func TestLoopFeedsFailuresBackUntilPass(t *testing.T) {
	steps := &fakeSteps{changes: []Change{change(1), change(2)}, checks: [][]Check{{pass, fail}, {pass, pass}}}
	out, err := Loop{Attempts: 3}.Run(context.Background(), steps, "add a flag")
	assert.NilError(t, err)
	assert.Equal(t, out.Result, ResultPassed)
	assert.Equal(t, out.Rounds, 2)
	assert.Equal(t, len(steps.prompts), 2)
	assert.Assert(t, strings.Contains(steps.prompts[1], "a.go:3: nil deref"), steps.prompts[1])
	assert.Assert(t, !strings.Contains(steps.prompts[1], "lint"), "passing checks are not fed back")
}

func TestLoopStopsWhenAttemptsRunOut(t *testing.T) {
	steps := &fakeSteps{changes: []Change{change(1), change(2)}, checks: [][]Check{{fail}, {fail}}}
	out, err := Loop{Attempts: 2}.Run(context.Background(), steps, "p")
	assert.NilError(t, err)
	assert.Equal(t, out.Result, ResultExhausted)
	assert.Equal(t, out.Rounds, 2)
	// No turn is spent on fixes that would never be checked.
	assert.Equal(t, len(steps.prompts), 2)
}

// builtInAttempts mirrors the CLI's default round limit.
const builtInAttempts = 10

func TestLoopStopsBeforeTheDefaultLimitWhenARoundChangesNothing(t *testing.T) {
	steps := &fakeSteps{changes: []Change{change(1), change(1)}, checks: [][]Check{{fail}}}
	out, err := Loop{Attempts: builtInAttempts}.Run(context.Background(), steps, "p")
	assert.NilError(t, err)
	assert.Equal(t, out.Result, ResultStuck)
	assert.Assert(t, out.Rounds < builtInAttempts, "rounds: %d", out.Rounds)
	assert.Equal(t, steps.checked, 1)
}

func TestLoopStopsWhenImplementerChangesNothing(t *testing.T) {
	steps := &fakeSteps{changes: []Change{change(1), change(1)}, checks: [][]Check{{fail}}}
	out, err := Loop{Attempts: 5}.Run(context.Background(), steps, "p")
	assert.NilError(t, err)
	assert.Equal(t, out.Result, ResultStuck)
	assert.Equal(t, steps.checked, 1, "the same code is not reviewed twice")
}

func TestLoopReportsNoChangeOnFirstRound(t *testing.T) {
	steps := &fakeSteps{changes: []Change{{Fingerprint: "empty"}}}
	out, err := Loop{Attempts: 3}.Run(context.Background(), steps, "p")
	assert.NilError(t, err)
	assert.Equal(t, out.Result, ResultNoChange)
	assert.Equal(t, steps.checked, 0)
}

// TestLoopReportsNoChangeWhenWorkIsReverted guards against passing a later
// round that reverted everything: its checks would run on the bare baseline.
func TestLoopReportsNoChangeWhenWorkIsReverted(t *testing.T) {
	steps := &fakeSteps{changes: []Change{change(1), {Fingerprint: "empty"}}, checks: [][]Check{{fail}}}
	out, err := Loop{Attempts: 3}.Run(context.Background(), steps, "p")
	assert.NilError(t, err)
	assert.Equal(t, out.Result, ResultNoChange)
	assert.Equal(t, steps.checked, 1)
}

// TestLoopRechecksWithoutImplementingAfterOnlyErrors guards against prompting
// the implementer with nothing to fix: it would change nothing, and the loop
// would then call it stuck instead of retrying the checks that could not run.
func TestLoopRechecksWithoutImplementingAfterOnlyErrors(t *testing.T) {
	steps := &fakeSteps{changes: []Change{change(1)}, checks: [][]Check{{pass, errored}, {pass, pass}}}
	out, err := Loop{Attempts: 3}.Run(context.Background(), steps, "p")
	assert.NilError(t, err)
	assert.Equal(t, out.Result, ResultPassed)
	assert.Equal(t, out.Rounds, 2)
	assert.Equal(t, len(steps.prompts), 1)
}

func TestLoopReturnsStepErrorsWithOutcomeSoFar(t *testing.T) {
	boom := errors.New("sidecar gone")
	steps := &fakeSteps{implErr: boom}
	_, err := Loop{Attempts: 3}.Run(context.Background(), steps, "p")
	assert.Assert(t, errors.Is(err, boom))
	assert.ErrorContains(t, err, "round 1: implement")
}

func TestLoopRejectsBadInput(t *testing.T) {
	_, err := Loop{Attempts: 0}.Run(context.Background(), &fakeSteps{}, "p")
	assert.ErrorContains(t, err, "attempts")
	_, err = Loop{Attempts: 1}.Run(context.Background(), &fakeSteps{}, "")
	assert.ErrorContains(t, err, "prompt is empty")
}

func TestLoopEmitsEventsInOrder(t *testing.T) {
	var kinds []EventKind
	steps := &fakeSteps{changes: []Change{change(1)}, checks: [][]Check{{pass}}}
	_, err := Loop{Attempts: 1, OnEvent: func(e Event) { kinds = append(kinds, e.Kind) }}.Run(context.Background(), steps, "p")
	assert.NilError(t, err)
	assert.DeepEqual(t, kinds, []EventKind{EventImplementing, EventImplemented, EventCollected, EventChecking, EventChecked})
}

// Work that already passes ends a loop that checks first without a turn.
func TestLoopCheckFirstPassesWithoutATurn(t *testing.T) {
	steps := &fakeSteps{current: change(1), checks: [][]Check{{pass}}}
	out, err := Loop{Attempts: 3, CheckFirst: true}.Run(context.Background(), steps, "pick up the work")
	assert.NilError(t, err)
	assert.Equal(t, out.Result, ResultPassed)
	assert.Equal(t, out.Rounds, 1)
	assert.Equal(t, len(steps.prompts), 0)
}

// The first turn after checking first is sent the prompt with what failed.
func TestLoopCheckFirstSendsThePromptWithWhatFailed(t *testing.T) {
	steps := &fakeSteps{current: change(1), changes: []Change{change(2)}, checks: [][]Check{{fail}, {pass}}}
	out, err := Loop{Attempts: 3, CheckFirst: true}.Run(context.Background(), steps, "pick up the work")
	assert.NilError(t, err)
	assert.Equal(t, out.Result, ResultPassed)
	assert.Equal(t, out.Rounds, 2)
	assert.DeepEqual(t, steps.prompts, []string{"pick up the work\n\n" + Feedback([]Check{fail})})
}

// A check that could not run is checked again before the prompt is sent, and
// the prompt still goes with the first feedback.
func TestLoopCheckFirstHoldsThePromptUntilThereIsFeedback(t *testing.T) {
	steps := &fakeSteps{current: change(1), changes: []Change{change(2)}, checks: [][]Check{{errored}, {fail}, {pass}}}
	out, err := Loop{Attempts: 4, CheckFirst: true}.Run(context.Background(), steps, "pick up the work")
	assert.NilError(t, err)
	assert.Equal(t, out.Result, ResultPassed)
	assert.DeepEqual(t, steps.prompts, []string{"pick up the work\n\n" + Feedback([]Check{fail})})
}

// Without a way to start from the request, checking first an empty change
// has nothing to do.
func TestLoopCheckFirstWithNoWorkAndNoEmptyPromptIsNoChange(t *testing.T) {
	steps := &fakeSteps{}
	out, err := Loop{Attempts: 3, CheckFirst: true}.Run(context.Background(), steps, "pick up the work")
	assert.NilError(t, err)
	assert.Equal(t, out.Result, ResultNoChange)
	assert.Equal(t, steps.checked, 0, "nothing to check")
	assert.Assert(t, !out.FromEmpty)
}

// An empty branch has nothing to check, so the request is implemented.
func TestLoopCheckFirstWithNoWorkImplementsTheRequest(t *testing.T) {
	steps := &fakeSteps{changes: []Change{change(1)}, checks: [][]Check{{pass}}}
	var kinds []EventKind
	loop := Loop{Attempts: 3, CheckFirst: true, EmptyPrompt: "the request", OnEvent: func(e Event) { kinds = append(kinds, e.Kind) }}
	out, err := loop.Run(context.Background(), steps, "pick up the work")
	assert.NilError(t, err)
	assert.Equal(t, out.Result, ResultPassed)
	assert.Assert(t, out.FromEmpty)
	assert.DeepEqual(t, steps.prompts, []string{"the request"})
	assert.Equal(t, steps.checked, 1)
	assert.DeepEqual(t, kinds, []EventKind{EventCollected, EventNoPriorWork, EventImplementing, EventImplemented, EventCollected, EventChecking, EventChecked})
}

// Guidance does not check first, but an empty branch still has no work to
// build on, so the prompt for it is used.
func TestLoopWithGuidanceAndNoWorkImplementsTheEmptyPrompt(t *testing.T) {
	steps := &fakeSteps{changes: []Change{change(1)}, checks: [][]Check{{pass}}}
	out, err := Loop{Attempts: 3, EmptyPrompt: "the request"}.Run(context.Background(), steps, "build on the work")
	assert.NilError(t, err)
	assert.Equal(t, out.Result, ResultPassed)
	assert.Assert(t, out.FromEmpty)
	assert.DeepEqual(t, steps.prompts, []string{"the request"})
}

// Work that is there keeps the prompt built for it.
func TestLoopWithGuidanceAndWorkImplementsFirst(t *testing.T) {
	steps := &fakeSteps{current: change(1), changes: []Change{change(2)}, checks: [][]Check{{pass}}}
	out, err := Loop{Attempts: 3, EmptyPrompt: "the request"}.Run(context.Background(), steps, "build on the work")
	assert.NilError(t, err)
	assert.Assert(t, !out.FromEmpty)
	assert.DeepEqual(t, steps.prompts, []string{"build on the work"})
}

func TestLoopCheckFirstWithWorkAndEmptyPromptStillChecksFirst(t *testing.T) {
	steps := &fakeSteps{current: change(1), checks: [][]Check{{pass}}}
	out, err := Loop{Attempts: 3, CheckFirst: true, EmptyPrompt: "the request"}.Run(context.Background(), steps, "pick up the work")
	assert.NilError(t, err)
	assert.Equal(t, out.Result, ResultPassed)
	assert.Equal(t, len(steps.prompts), 0)
	assert.Assert(t, !out.FromEmpty)
}

// An implementer that changes nothing after checking first is stuck, as in
// any other round.
func TestLoopCheckFirstThenNoChangeIsStuck(t *testing.T) {
	steps := &fakeSteps{current: change(1), changes: []Change{change(1)}, checks: [][]Check{{fail}}}
	out, err := Loop{Attempts: 3, CheckFirst: true}.Run(context.Background(), steps, "pick up the work")
	assert.NilError(t, err)
	assert.Equal(t, out.Result, ResultStuck)
	assert.Equal(t, out.Rounds, 1)
}

func TestLoopPassesWhenQualityCannotBeMeasured(t *testing.T) {
	nm := FromQuality(quality.Summary{}, 0, errors.New("run the metrics provider: exited 127: lizard is not installed in this image"))
	steps := &fakeSteps{changes: []Change{change(1)}, checks: [][]Check{{pass, nm}}}
	out, err := Loop{Attempts: 3}.Run(context.Background(), steps, "add a flag")
	assert.NilError(t, err)
	assert.Equal(t, out.Result, ResultPassed)
	assert.Equal(t, out.Rounds, 1)
	assert.Equal(t, steps.checked, 1)
	assert.Assert(t, out.Quality() == nil)
	assert.Assert(t, strings.Contains(out.QualityNotMeasured(), "lizard is not installed in this image"))
}

func TestLoopSendsAQualityFindingBackAndPassesTheNextRound(t *testing.T) {
	busy := FromQuality(quality.Summary{Measured: true, Findings: []quality.Finding{{
		Severity: quality.SeverityMedium, File: "a.go", Line: 12, Func: "busy",
		Message: "busy has cyclomatic complexity 16 (is new; limit 15): split it: a handler per case, or extract the decision into a pure function",
	}}}, 0, nil)
	clean := FromQuality(quality.Summary{Measured: true, ErosionBefore: 10, ErosionAfter: 10}, 0, nil)
	steps := &fakeSteps{changes: []Change{change(1), change(2)}, checks: [][]Check{{pass, busy}, {pass, clean}}}
	out, err := Loop{Attempts: 3}.Run(context.Background(), steps, "add a function")
	assert.NilError(t, err)
	assert.Equal(t, out.Result, ResultPassed)
	assert.Equal(t, out.Rounds, 2)
	assert.Assert(t, strings.Contains(steps.prompts[1], "a.go:12"), steps.prompts[1])
	assert.Assert(t, strings.Contains(steps.prompts[1], "complexity 16"), steps.prompts[1])
	assert.Assert(t, strings.Contains(steps.prompts[1], "split it"), steps.prompts[1])
	assert.Assert(t, out.Quality() != nil && out.Quality().ErosionAfter == 10, "the outcome carries the passing round's summary")
}

func TestNewLoopMapsRoundPolicyToFreshRounds(t *testing.T) {
	for _, tc := range []struct {
		rounds config.RoundPolicy
		fresh  bool
	}{
		{"", true},
		{config.RoundsFresh, true},
		{config.RoundsResume, false},
	} {
		opts := RunOptions{Attempts: 2, Rounds: tc.rounds}
		loop, _ := opts.newLoop(&Sidecars{}, func(Event) {})
		assert.Equal(t, loop.FreshRounds, tc.fresh, "rounds %q", tc.rounds)
	}
}

// blockingSteps is a loop whose implementer turn runs until it is cancelled.
type blockingSteps struct {
	fakeSteps
	started   chan struct{}
	implement int
	collected int
}

func (b *blockingSteps) Implement(ctx context.Context, _ string) (Turn, error) {
	b.implement++
	close(b.started)
	<-ctx.Done()
	return Turn{}, ctx.Err()
}

func (b *blockingSteps) Collect(ctx context.Context) (Change, error) {
	b.collected++
	return b.fakeSteps.Collect(ctx)
}

func TestLoopStopsAtACancelledImplementerTurn(t *testing.T) {
	steps := &blockingSteps{started: make(chan struct{})}
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		<-steps.started
		cancel()
	}()
	_, err := Loop{Attempts: 3}.Run(ctx, steps, "add a flag")
	assert.Assert(t, errors.Is(err, context.Canceled), "%v", err)
	assert.Equal(t, steps.implement, 1)
	assert.Equal(t, steps.collected, 0)
	assert.Equal(t, steps.checked, 0)
}

func TestLoopStopsUnavailableWithoutSpendingARound(t *testing.T) {
	steps := &fakeSteps{implErr: &HarnessUnavailable{Harness: "codex", Waited: 30 * time.Minute}}
	out, err := Loop{Attempts: 3}.Run(context.Background(), steps, "p")
	var unavailable *HarnessUnavailable
	assert.Assert(t, errors.As(err, &unavailable))
	assert.Equal(t, out.Result, ResultUnavailable)
	assert.Equal(t, out.Reason, "codex at capacity for 30m")
	assert.Equal(t, out.Stop, StopHarnessUnavailable)
	assert.Equal(t, out.Rounds, 0)
}
