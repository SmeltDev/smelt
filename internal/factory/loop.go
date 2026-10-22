package factory

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/smeltdev/smelt/internal/quality"
	"github.com/smeltdev/smelt/internal/review"
)

// Steps are the operations one round of the loop is made of. Sidecars
// implements them against real sidecars; tests implement them in memory.
type Steps interface {
	// Implement runs one implementer turn with prompt.
	Implement(ctx context.Context, prompt string) (Turn, error)
	// Collect describes the implementer's work so far.
	Collect(ctx context.Context) (Change, error)
	// Check reviews and validates the implementer's work so far.
	Check(ctx context.Context, round int) ([]Check, error)
}

// Result is why the loop stopped.
type Result string

// Loop results.
const (
	// ResultPassed means every check passed.
	ResultPassed Result = "passed"
	// ResultFailed means no eligible harness remains.
	ResultFailed Result = "failed"
	// ResultExhausted means checks still failed when attempts ran out.
	ResultExhausted Result = "exhausted"
	// ResultStuck means the implementer changed nothing in response to
	// feedback, so another round would review the same code again.
	ResultStuck Result = "stuck"
	// ResultNoChange means the implementer's work is empty: it changed
	// nothing, or reverted everything it had changed.
	ResultNoChange Result = "no_change"
	// ResultBlocked means the implementer finished what it could and ended its
	// summary with a BLOCKED: line saying what it could not do.
	ResultBlocked Result = "blocked"
	// ResultUnavailable means the harness stayed at capacity for the whole
	// wait. It clears by itself, so the run can be retried later.
	ResultUnavailable Result = "unavailable"
)

// StopHarnessUnavailable is Outcome.Stop for ResultUnavailable.
const StopHarnessUnavailable = "harness_unavailable"

// Outcome is the end state of a loop.
type Outcome struct {
	Reason  string
	ResetAt *time.Time
	// Stop says why a run ended without a verdict on the code, for a caller
	// that retries on its own; see smeltd.StopHarnessUnavailable.
	Stop         string
	HarnessSwaps []HarnessSwap
	Turns        []TurnAttempt
	Result       Result
	// Rounds is how many rounds were checked.
	Rounds int
	Change Change
	// Checks are the last round's checks.
	Checks []Check
	// Blocked is the reason the implementer's last summary gave after
	// BLOCKED:, or "".
	Blocked string
	// Unverified are the things the implementer's last summary listed after
	// UNVERIFIED:. They never change Result.
	Unverified []string
	// Plan is the plan the implementer was sent with, nil when the run did
	// not plan.
	Plan *Plan
	// FromEmpty is set when the loop started from an empty change and sent
	// Loop.EmptyPrompt.
	FromEmpty bool
}

// Incomplete is the findings of the last round's checks that say requested
// work is missing.
func (o Outcome) Incomplete() []review.Finding {
	return MissingWork(o.Checks)
}

// Quality is the last round's quality measurement, nil when the run did not
// measure quality or the measurement failed.
func (o Outcome) Quality() *quality.Summary {
	for _, c := range o.Checks {
		if c.Kind == KindQuality && c.Quality != nil {
			return c.Quality
		}
	}
	return nil
}

// QualityNotMeasured is why the last round's quality could not be measured, ""
// when it was measured or not checked.
func (o Outcome) QualityNotMeasured() string {
	for _, c := range o.Checks {
		if c.Kind == KindQuality && c.Status == StatusNotMeasured {
			return c.Reason
		}
	}
	return ""
}

// EventKind identifies a step of the loop for display.
type EventKind int

// Event kinds, in the order a round emits them.
const (
	EventPlanning     EventKind = iota // the plan was asked for
	EventPlanReviewed                  // the plan is settled; Plan, Findings
	EventImplementing                  // a turn started; Round, Prompt
	EventImplemented                   // a turn ended; Round, Turn
	EventCollected                     // the work was collected; Round, Change
	EventChecking                      // checks started; Round
	EventChecked                       // checks ended; Round, Checks
	EventHarnessSwap
	EventTurnAttempt
	EventNoPriorWork // the work already there is empty; the request is implemented
	EventSynced      // a base was merged between rounds; Round, Sync
	EventSyncDropped // a queued sync was dropped when the run ended; Round, Sync
)

// Event reports progress through the loop.
type Event struct {
	Swap    *HarnessSwap
	Attempt *TurnAttempt
	Kind    EventKind
	Round   int
	Prompt  string
	Turn    Turn
	Change  Change
	Checks  []Check
	// Plan and Findings are the settled plan and what its review still found
	// wrong with it.
	Plan     *Plan
	Findings []review.Finding
	// Sync is the base merged at the end of Round, for EventSynced.
	// Worktree is the current baseline and provenance after EventSynced.
	Worktree *Worktree
	Sync     *Sync
}

// Loop drives the implementer through rounds of review and validation.
type Loop struct {
	// FreshRounds starts each fix round in a new session and gives it a compact handoff.
	FreshRounds bool
	// Attempts is the most rounds to check. Each round after the first starts
	// with an implementer turn fixing the previous round's failures.
	Attempts int
	// CheckFirst checks the work already there before the implementer's first
	// turn, which is then sent the prompt with what failed. Work that passes
	// as it is ends the loop without a turn. The first round counts as one of
	// the attempts.
	CheckFirst bool
	// Planner, when set, has the implementer plan, and the plan reviewed,
	// before its first turn. A loop that checks first does not plan: there is
	// work to describe already.
	Planner Planner
	// CheckHints makes a plan whose context pack leaves out a hint of the
	// request's `## Context` section a finding, which sends the plan back
	// like any other. It is for runs whose plan turn builds a pack.
	CheckHints bool
	// EmptyPrompt, when set, makes the loop collect the work already there
	// before the first turn. If it is empty, there is nothing to check or build
	// on, so the loop sends EmptyPrompt instead of the prompt it was given and
	// does not check first. Without it, checking first an empty change ends
	// the loop as no_change.
	EmptyPrompt string
	OnEvent     func(Event)
}

// Run implements prompt, then checks the work and feeds failures back until
// the checks pass, the implementer stops changing anything, or attempts run
// out. The returned error is for a step that could not run, not for checks
// that failed; the outcome so far is returned with it.
func (l Loop) Run(ctx context.Context, steps Steps, prompt string) (Outcome, error) {
	out, err := l.run(ctx, steps, prompt)
	var unavailable *HarnessUnavailable
	if errors.As(err, &unavailable) {
		out.Result, out.Reason, out.Stop = ResultUnavailable, unavailable.Error(), StopHarnessUnavailable
		return out, err
	}
	var exhausted *Exhaustion
	if errors.As(err, &exhausted) {
		out.Result, out.Reason, out.ResetAt = ResultFailed, noHarnessAvailable, exhausted.ResetAt
		return out, err
	}
	// A block outranks why the loop stopped: the implementer said what it
	// cannot do, which no further round changes.
	if out.Blocked != "" && out.Result != "" && out.Result != ResultNoChange {
		out.Result = ResultBlocked
	}
	return out, err
}

func (l Loop) run(ctx context.Context, steps Steps, prompt string) (Outcome, error) {
	if l.Attempts < 1 {
		return Outcome{}, errors.New("attempts must be at least 1")
	}
	if prompt == "" {
		return Outcome{}, errors.New("prompt is empty")
	}
	emit := l.OnEvent
	if emit == nil {
		emit = func(Event) {}
	}
	var out Outcome
	state, err := l.prepare(ctx, steps, emit, prompt, &out)
	if err != nil || state.done {
		return out, err
	}
	prompt, lead := state.prompt, state.lead
	var previous Turn
	for round := 1; ; round++ {
		// An empty prompt means the last round failed only on checks that could
		// not run: there is nothing to fix, so the same code is checked again.
		if prompt != "" {
			var done bool
			previous, done, err = l.implementRound(ctx, steps, emit, round, prompt, state.request, previous, &out)
			if err != nil || done {
				return out, err
			}
		}
		done, err := l.checkRound(ctx, steps, emit, round, &out)
		if err != nil || done {
			return out, err
		}
		prompt = Feedback(out.Checks)
		sync, err := l.syncBase(ctx, steps, emit, round)
		if err != nil {
			return out, err
		}
		prompt = withSyncNote(prompt, sync)
		prompt, lead = combineLead(lead, prompt)
	}
}

type loopState struct {
	request, prompt, lead string
	done                  bool
}

func (l Loop) prepare(ctx context.Context, steps Steps, emit func(Event), prompt string, out *Outcome) (loopState, error) {
	state := loopState{request: prompt, prompt: prompt}
	if l.Planner != nil && !l.CheckFirst {
		plan, preamble, err := l.plan(ctx, emit, prompt)
		if err != nil {
			return state, fmt.Errorf("plan: %w", err)
		}
		out.Plan, state.prompt = &plan, preamble+prompt
	}
	if !l.CheckFirst && l.EmptyPrompt == "" {
		return state, nil
	}
	change, err := steps.Collect(ctx)
	if err != nil {
		return state, fmt.Errorf("round 1: %w", err)
	}
	emit(Event{Kind: EventCollected, Round: 1, Change: change})
	if change.Empty() && l.EmptyPrompt != "" {
		emit(Event{Kind: EventNoPriorWork})
		out.FromEmpty, state.prompt = true, l.EmptyPrompt
		return state, nil
	}
	if change.Empty() {
		out.Result, out.Change, state.done = ResultNoChange, change, true
		return state, nil
	}
	if l.CheckFirst {
		out.Change, state.lead, state.prompt = change, state.prompt, ""
	}
	return state, nil
}

func (l Loop) implementRound(ctx context.Context, steps Steps, emit func(Event), round int, prompt, request string, previous Turn, out *Outcome) (Turn, bool, error) {
	if round > 1 && l.FreshRounds {
		if fresh, ok := steps.(interface{ FreshSession() }); ok {
			fresh.FreshSession()
		}
		if handoff, ok := steps.(interface {
			Handoff(context.Context, string, string, *Plan, Turn, Change, []Check) (string, error)
		}); ok {
			var err error
			prompt, err = handoff.Handoff(ctx, request, prompt, out.Plan, previous, out.Change, out.Checks)
			if err != nil {
				return previous, false, fmt.Errorf("round %d handoff: %w", round, err)
			}
		}
	}
	emit(Event{Kind: EventImplementing, Round: round, Prompt: prompt})
	if s, ok := steps.(*Sidecars); ok {
		s.round, s.prior, s.checks = round, previous, out.Checks
		if out.Plan != nil {
			s.plan = out.Plan
		}
	}
	turn, err := steps.Implement(ctx, prompt)
	if err != nil {
		return previous, false, fmt.Errorf("round %d: implement: %w", round, err)
	}
	emit(Event{Kind: EventImplemented, Round: round, Turn: turn})
	out.Blocked, out.Unverified = BlockedReason(turn.Summary), UnverifiedItems(turn.Summary)
	change, err := steps.Collect(ctx)
	if err != nil {
		return turn, false, fmt.Errorf("round %d: %w", round, err)
	}
	emit(Event{Kind: EventCollected, Round: round, Change: change})
	if change.Empty() {
		out.Result, out.Change = ResultNoChange, change
		return turn, true, nil
	}
	if round > 1 && change.Fingerprint == out.Change.Fingerprint {
		out.Result = ResultStuck
		return turn, true, nil
	}
	out.Change = change
	return turn, false, nil
}

func (l Loop) checkRound(ctx context.Context, steps Steps, emit func(Event), round int, out *Outcome) (bool, error) {
	emit(Event{Kind: EventChecking, Round: round})
	checks, err := steps.Check(ctx, round)
	if err != nil {
		return false, fmt.Errorf("round %d: check: %w", round, err)
	}
	out.Rounds, out.Checks = round, checks
	emit(Event{Kind: EventChecked, Round: round, Checks: checks})
	if Passed(checks) {
		out.Result = ResultPassed
		return true, nil
	}
	if round == l.Attempts {
		out.Result = ResultExhausted
		return true, nil
	}
	return false, nil
}

// syncBase merges a base queued during the round, now that its checks are in
// and the next implementer turn has not started. nil means none was queued.
func (l Loop) syncBase(ctx context.Context, steps Steps, emit func(Event), round int) (*Sync, error) {
	syncer, ok := steps.(BaseSyncer)
	if !ok {
		return nil, nil
	}
	sync, err := syncer.SyncBase(ctx, round)
	if err != nil {
		return nil, fmt.Errorf("round %d: %w", round, err)
	}
	if sync != nil {
		emit(Event{Kind: EventSynced, Round: round, Sync: sync})
	}
	return sync, nil
}

// withSyncNote adds what the sync did to the next implementer prompt. A clean
// sync changes nothing the implementer must act on, so it is told only when
// there is a turn to tell it in; conflicts always get one.
func withSyncNote(prompt string, sync *Sync) string {
	switch {
	case sync == nil || sync.State == SyncDropped:
		return prompt
	case prompt == "" && len(sync.Conflicts) == 0:
		return ""
	case prompt == "":
		return sync.Note()
	}
	return sync.Note() + "\n\n" + prompt
}

func combineLead(lead, prompt string) (string, string) {
	if lead == "" || prompt == "" {
		return prompt, lead
	}
	return lead + "\n\n" + prompt, ""
}

// plan has the implementer write a plan and has it reviewed. A plan whose
// review found something worth changing is revised once; if the revision is
// still found wanting the run goes ahead anyway, with what the review said in
// front of the implementer, so a disagreement cannot stall it. preamble is
// what goes before the request in the implementer's first prompt.
func (l Loop) plan(ctx context.Context, emit func(Event), request string) (plan Plan, preamble string, err error) {
	emit(Event{Kind: EventPlanning})
	in := PlanInput{Request: request}
	var worth []review.Finding
	// The first pass writes the plan; the second revises it.
	for range 2 {
		if plan, err = l.Planner.Plan(ctx, in); err != nil {
			return Plan{}, "", err
		}
		var found []review.Finding
		if found, err = l.Planner.ReviewPlan(ctx, request, plan); err != nil {
			return Plan{}, "", err
		}
		if l.CheckHints {
			found = append(found, hintFindings(request, plan)...)
		}
		if worth = worthChanging(found); len(worth) == 0 {
			break
		}
		prev := plan
		in = PlanInput{Request: request, Previous: &prev, Findings: worth}
	}
	emit(Event{Kind: EventPlanReviewed, Plan: &plan, Findings: worth})
	return plan, planPreamble(plan, worth), nil
}

// hintFindings are the hints of the request's `## Context` section that the
// plan's pack neither follows nor rejects, as findings about the plan.
func hintFindings(request string, plan Plan) []review.Finding {
	missing := MissingHints(ContextHints(request), plan.ContextPack())
	out := make([]review.Finding, 0, len(missing))
	for _, h := range missing {
		out = append(out, review.Finding{
			Prompt: "context-pack", File: planFile, Severity: review.SeverityMedium,
			Body: fmt.Sprintf("the request's Context hint %q is neither in the pack nor in rejected_hints; add it to the pack, or reject it with a reason", h),
		})
	}
	return out
}

// worthChanging is the findings that send work back.
func worthChanging(findings []review.Finding) []review.Finding {
	var out []review.Finding
	for _, f := range findings {
		if f.WorthChanging() {
			out = append(out, f)
		}
	}
	return out
}
