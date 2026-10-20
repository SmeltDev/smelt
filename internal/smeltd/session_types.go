package smeltd

import (
	"fmt"
	"time"

	"github.com/smeltdev/smelt/internal/config"
	"github.com/smeltdev/smelt/internal/harness"
	"github.com/smeltdev/smelt/internal/iostream"
)

// SessionState is where a whole session stands.
type SessionState string

// Session states.
const (
	SessionRunning   SessionState = "running"
	SessionDone      SessionState = "done"
	SessionFailed    SessionState = "failed"
	SessionCancelled SessionState = "cancelled"
)

// Finished reports whether the session has ended for good.
func (s SessionState) Finished() bool {
	return s != SessionRunning
}

// StageID names one stage of a session.
type StageID string

// The stages.
const (
	// StageFactoryLoop is a factory run's loop: implement, then review and
	// validate until the checks pass.
	StageFactoryLoop StageID = "factory_loop"
)

// StageState is where one stage stands.
type StageState string

// Stage states.
const (
	StageRunning StageState = "running"
	StageDone    StageState = "done"
	StageFailed  StageState = "failed"
)

// Stage is one stage of a session and how it is going.
type Stage struct {
	ID    StageID    `json:"id"`
	State StageState `json:"state"`
	// Note says more about the state: why a loop ended, what failed.
	Note string `json:"note,omitempty"`
}

// RoundState is where one round stands.
type RoundState string

// Round states.
const (
	// RoundStarted is a round that has begun and not yet said which half it
	// is in.
	RoundStarted RoundState = "started"
	// RoundImplementing and RoundChecking are a factory round's halves: the
	// implementer's turn, then the reviews and validation commands.
	RoundImplementing RoundState = "implementing"
	RoundChecking     RoundState = "checking"
	RoundDone         RoundState = "done"
	RoundFailed       RoundState = "failed"
)

// ImplementState is where a factory round's implementer turn stands.
type ImplementState string

// Implement states.
const (
	ImplementRunning ImplementState = "running"
	ImplementApplied ImplementState = "applied"
	// ImplementEmpty means the agent made no changes.
	ImplementEmpty  ImplementState = "empty"
	ImplementFailed ImplementState = "failed"
)

// RoundImplement is a factory round's implementer turn.
type RoundImplement struct {
	State ImplementState `json:"state"`
	// Fix is set for the one fix turn of an integrate run.
	Fix        bool    `json:"fix,omitempty"`
	DurationMS int64   `json:"duration_ms,omitempty"`
	CostUSD    float64 `json:"cost_usd,omitempty"`
	// Harness and Model are the agent that ran the turn.
	Harness string `json:"harness,omitempty"`
	Model   string `json:"model,omitempty"`
	// The tokens the turn spent, when the agent reports them.
	InputTokens  int `json:"input_tokens,omitempty"`
	CachedTokens int `json:"cached_tokens,omitempty"`
	OutputTokens int `json:"output_tokens,omitempty"`
	// ContextPeak is the largest prompt of the turn in tokens, and ModelCalls
	// how many model calls it made; both absent when the agent does not report
	// them.
	ContextPeak int `json:"context_peak,omitempty"`
	ModelCalls  int `json:"model_calls,omitempty"`
	// RateLimit is the last rate-limit signal the agent emitted during the turn.
	RateLimit *harness.RateLimit `json:"rate_limit,omitempty"`
	// Summary is the implementer's own account of the turn.
	Summary string `json:"summary,omitempty"`
	// Stat summarizes the work so far against the run's baseline.
	Stat  string `json:"stat,omitempty"`
	Error string `json:"error,omitempty"`
	// Unproven are the gates the summary claimed to pass without running them
	// after its last edit. They are only noted; the round's own gates decide.
	Unproven []string `json:"unproven_gates,omitempty"`
}

// FactoryTiming is where a factory run's time went, in milliseconds, with the
// one-line account the CLI prints.
type FactoryTiming struct {
	Line        string `json:"line"`
	PlanMS      int64  `json:"plan_ms"`
	ImplementMS int64  `json:"implement_ms"`
	// CommandsMS is the part of ImplementMS spent inside tool calls.
	CommandsMS int64 `json:"commands_ms"`
	GatesMS    int64 `json:"gates_ms"`
	ReviewsMS  int64 `json:"reviews_ms"`
	NotesMS    int64 `json:"notes_ms"`
	TotalMS    int64 `json:"total_ms"`
	// ContextPeakMax is the largest context, in tokens, of any turn of the run;
	// absent when none is known.
	ContextPeakMax int `json:"context_peak_max,omitempty"`
	// GateOnlyRounds are the rounds that failed on gates alone.
	GateOnlyRounds []int `json:"gate_only_rounds,omitempty"`
	// SlowCalls are warnings for implementer tool calls over two minutes.
	SlowCalls []string `json:"slow_calls,omitempty"`
}

// Feed is the latest lines of a stream that can run long, such as a run's
// progress, and how many lines there have been, so a follower can tell which
// it has not seen. Only the latest lines are kept.
type Feed struct {
	Lines []FeedLine `json:"lines,omitempty"`
	Total int        `json:"total,omitempty"`
}

// FeedLine is one line of a Feed.
type FeedLine struct {
	Level FeedLevel `json:"level"`
	Text  string    `json:"text"`
}

// FeedLevel is how a feed line reads, as iostream.Level, spelled out so it
// reads in JSON.
type FeedLevel string

// Feed levels.
const (
	FeedStep  FeedLevel = "step"
	FeedInfo  FeedLevel = "info"
	FeedWarn  FeedLevel = "warn"
	FeedDone  FeedLevel = "done"
	FeedError FeedLevel = "error"
)

// Level is the line's iostream level. An unknown value reads as info: it
// comes from a daemon that may be newer than this client.
func (l FeedLevel) Level() iostream.Level {
	switch l {
	case FeedStep:
		return iostream.LevelStep
	case FeedWarn:
		return iostream.LevelWarn
	case FeedDone:
		return iostream.LevelDone
	case FeedError:
		return iostream.LevelError
	case FeedInfo:
	}
	return iostream.LevelInfo
}

// Since returns the lines added after the first seen, as many as are still
// kept.
func (f Feed) Since(seen int) []FeedLine {
	n := min(max(f.Total-seen, 0), len(f.Lines))
	return f.Lines[len(f.Lines)-n:]
}

// RoundCheck is one validation command a factory round ran.
type RoundCheck struct {
	Name string `json:"name"`
	// Status is one of the Check values.
	Status     string `json:"status"`
	SidecarID  string `json:"sidecar_id,omitempty"`
	DurationMS int64  `json:"duration_ms,omitempty"`
	Error      string `json:"error,omitempty"`
	// Output is the end of a failed command's output.
	Output string `json:"output,omitempty"`
	// Skipped are the tests the command's output reports as skipped.
	Skipped []string `json:"skipped,omitempty"`
	// Summary is the quality check's before → after line.
	Summary string `json:"summary,omitempty"`
	// Ratchet is what a SARIF gate found: findings the change added, fixed and
	// left untouched.
	Ratchet *RatchetCounts `json:"ratchet,omitempty"`
}

// RatchetCounts is the result of a SARIF gate's comparison with the baseline.
type RatchetCounts struct {
	New      int `json:"new"`
	Fixed    int `json:"fixed"`
	Existing int `json:"existing"`
}

// FactoryQuality is a factory run's quality measurement, before and after its
// change. Erosion is a percentage; the over-threshold counts are of functions
// above the configured complexity limit.
type FactoryQuality struct {
	// Measured is false when quality could not be measured; Reason says why.
	// Readers of stored status from older runs see false here, so key off the
	// presence of this object, not Measured alone.
	Measured bool   `json:"measured"`
	Reason   string `json:"reason,omitempty"`
	// Line is the same account as one line of text.
	Line                string  `json:"line,omitempty"`
	ErosionBefore       float64 `json:"erosion_before"`
	ErosionAfter        float64 `json:"erosion_after"`
	OverThresholdBefore int     `json:"over_threshold_before"`
	OverThresholdAfter  int     `json:"over_threshold_after"`
	CloneLinesBefore    int     `json:"clone_lines_before"`
	CloneLinesAfter     int     `json:"clone_lines_after"`
	// Worst are the most complex functions after the change.
	Worst []QualityFunction `json:"worst,omitempty"`
}

// QualityFunction is one function of a FactoryQuality.
type QualityFunction struct {
	Func string `json:"func"`
	File string `json:"file"`
	// Language is the function's language, as the metrics provider names it;
	// absent for runs from before smelt measured every language.
	Language string `json:"language,omitempty"`
	CC       int    `json:"cc"`
	Lines    int    `json:"lines"`
}

// MaxToolActivity is how many tool calls FactoryRun.Activity keeps.
const MaxToolActivity = 500

// ToolActivity is one tool call of the implementer.
type ToolActivity struct {
	// Round is the factory round (from 1) the call was made in.
	Round int `json:"round"`
	// ID pairs an update with its call; it is not part of the record.
	ID     string `json:"-"`
	Tool   string `json:"tool"`
	Detail string `json:"detail"`
	// Outcome is "ok" or "failed", "" when the agent does not say.
	Outcome  string `json:"outcome"`
	ExitCode *int   `json:"exit_code,omitempty"`
	Error    string `json:"error,omitempty"`
}

// FactoryRun is what a factory session works on and where its work is.
type FactoryRun struct {
	HarnessSwaps []HarnessSwap `json:"harness_swaps,omitempty"`
	Turns        []HarnessTurn `json:"turns,omitempty"`
	Reason       string        `json:"reason,omitempty"`
	ResetAt      *time.Time    `json:"reset_at,omitempty"`
	// Prompt is the request. A continued run's is the earlier run's, and
	// Guidance is what the user added to it, if anything.
	Prompt   string `json:"prompt"`
	Guidance string `json:"guidance,omitempty"`
	// Mode is "integrate" for a run that checks a finished branch against the
	// spec in Prompt, and Against what the branch is compared with; both are
	// absent otherwise.
	Mode    string `json:"mode,omitempty"`
	Against string `json:"against,omitempty"`
	// ContinuesRunID is the run whose work this one picked up, if it did.
	ContinuesRunID string `json:"continues_run_id,omitempty"`
	// ContinuedFromEmpty is set when the run found no work on the branch it
	// continued and implemented the request from scratch.
	ContinuedFromEmpty bool `json:"continued_from_empty,omitempty"`
	// Harness and Model are the coding agent the run uses and the model it was
	// asked for; Model is "" for the agent's own default.
	Harness string `json:"harness,omitempty"`
	Model   string `json:"model,omitempty"`
	// Attempts is the most rounds the run checks.
	Attempts int    `json:"attempts,omitempty"`
	RunID    string `json:"run_id,omitempty"`
	// SidecarIDs are the live or retained members of this run's pool. The
	// daemon derives ownership from its pool state so clients do not have to
	// reconstruct it from sidecar names.
	SidecarIDs []string `json:"sidecar_ids,omitempty"`
	// Worktree is where the work is, on Branch. Baseline is the commit the
	// branch starts from and Head the user's HEAD when the run started; they
	// differ when the user's uncommitted work was committed as the baseline.
	Worktree string `json:"worktree,omitempty"`
	Branch   string `json:"branch,omitempty"`
	Baseline string `json:"baseline,omitempty"`
	Head     string `json:"head,omitempty"`
	// BaseBranch is the branch the user was on when the run started, or the
	// one --from named, and DefaultBranch the repository's default; either
	// may be "".
	BaseBranch    string `json:"base_branch,omitempty"`
	DefaultBranch string `json:"default_branch,omitempty"`
	// From is the ref the run was started from with --from, absent when it
	// started from the user's files.
	From *FromRef `json:"from,omitempty"`
	// Result is why the loop stopped, one of the Result values, once it has,
	// and Rounds how many rounds were checked.
	Result string `json:"result,omitempty"`
	Rounds int    `json:"rounds,omitempty"`
	// Plan is the plan the implementer was sent with, absent when the run did
	// not plan.
	Plan *Plan `json:"plan,omitempty"`
	// Pack is the context pack the agents were given, absent when the run has
	// none.
	Pack *Pack `json:"pack,omitempty"`
	// Timing is where the run's time went, once the implementer has started.
	Timing *FactoryTiming `json:"timing,omitempty"`
	// Quality is the last round's quality measurement; absent when the run does
	// not measure quality.
	Quality *FactoryQuality `json:"quality,omitempty"`
	// SkippedTests are the tests the last round's validation skipped.
	SkippedTests []string `json:"skipped_tests,omitempty"`
	// Blocked is what the implementer said it could not do, for ResultBlocked.
	Blocked string `json:"blocked,omitempty"`
	// Unverified is what is done but can only be checked outside the sandbox.
	// It does not change Result.
	Unverified []string `json:"unverified,omitempty"`
	// Incomplete are the titles of the last round's findings that say part of
	// the request is missing.
	Incomplete []string `json:"incomplete,omitempty"`
	// KitOutdated reports that the sandbox Dockerfile has an older agent kit
	// than this smelt's.
	KitOutdated bool `json:"kit_outdated,omitempty"`
	// Committed reports whether the work was committed on Branch.
	Committed bool `json:"committed,omitempty"`
	// Stat is the committed work's diff stat against Baseline, "" when there
	// were no changes. StatError says why it could not be worked out instead.
	Stat      string `json:"stat,omitempty"`
	StatError string `json:"stat_error,omitempty"`
	// Notes are the commit's and the PR's account of the change.
	Notes *Notes `json:"notes,omitempty"`
	// WorktreeRemoved reports that the run ended before the implementer started
	// and its worktree, holding nothing, was removed.
	WorktreeRemoved bool `json:"worktree_removed,omitempty"`
	// KeptSidecars are the sidecars left running at the user's request.
	KeptSidecars []string `json:"kept_sidecars,omitempty"`
	// Log is the path of the run's log, once the run has ended, if it kept one.
	Log string `json:"log,omitempty"`
	// Activity is the implementer's tool calls with how each ended, newest
	// last, capped at the latest MaxToolActivity.
	Activity []ToolActivity `json:"activity,omitempty"`
	// Progress is what the run said as it went: its sidecars being made
	// ready and synced, and anything that went wrong cleaning up.
	Progress Feed `json:"progress,omitzero"`
}

// Notes is the human-readable account of a factory run's change; it mirrors
// factory.Notes, which smeltd cannot import.
type Notes struct {
	Title string `json:"title"`
	What  string `json:"what"`
	Why   string `json:"why"`
	How   string `json:"how"`
	// Fallback says why the notes could not be written, when they could not.
	Fallback string `json:"fallback,omitempty"`
	// Skipped are the tests the sandbox skipped in the run's last round.
	Skipped []string `json:"skipped,omitempty"`
	// Low are the findings downgraded to low in a later round.
	Low []string `json:"low,omitempty"`
	// Quality is the run's quality line, before → after.
	Quality string `json:"quality,omitempty"`
	// QualityNotMeasured is why quality could not be measured.
	QualityNotMeasured string `json:"qualityNotMeasured,omitempty"`
	// Context mirrors factory.Notes so the two convert into each other. It is
	// never sent: the notes' context is for the run's log, not the PR.
	Context harness.Context `json:"-"`
}

// Plan mirrors factory.Plan: what the implementer said it would do before it
// wrote code.
type Plan struct {
	Summary          string     `json:"summary"`
	Files            []PlanFile `json:"files,omitempty"`
	Acceptance       []PlanItem `json:"acceptance,omitempty"`
	Risks            []string   `json:"risks,omitempty"`
	Questions        []string   `json:"questions,omitempty"`
	CannotVerifyHere []string   `json:"cannot_verify_here,omitempty"`
}

// Pack mirrors factory.Pack: the exact code the agents were pointed to.
type Pack struct {
	Entries       []PackEntry    `json:"entries"`
	Dropped       []PackDropped  `json:"dropped"`
	RejectedHints []RejectedHint `json:"rejected_hints"`
	// Tokens is the estimated size of the pack as the agents see it.
	Tokens int `json:"tokens"`
}

// PackEntry mirrors factory.PackEntry.
type PackEntry struct {
	Kind     string `json:"kind"`
	Path     string `json:"path"`
	Start    int    `json:"start"`
	End      int    `json:"end"`
	Reason   string `json:"reason"`
	Priority int    `json:"priority"`
	// Inlined is whether the entry's code is in the agents' prompts.
	Inlined bool `json:"inlined"`
}

// PackDropped mirrors factory.DroppedEntry.
type PackDropped struct {
	Path   string `json:"path"`
	Start  int    `json:"start"`
	End    int    `json:"end"`
	Reason string `json:"reason"`
}

// RejectedHint mirrors factory.RejectedHint.
type RejectedHint struct {
	Hint   string `json:"hint"`
	Reason string `json:"reason"`
}

// PlanFile mirrors factory.PlanFile.
type PlanFile struct {
	Path   string `json:"path"`
	Change string `json:"change"`
	Why    string `json:"why"`
}

// PlanItem mirrors factory.PlanItem.
type PlanItem struct {
	Item                string `json:"item"`
	How                 string `json:"how"`
	VerifiableInSandbox bool   `json:"verifiable_in_sandbox"`
}

// Why a factory run's loop stopped, as FactoryRun.Result.
const (
	// ResultPassed means every check passed.
	ResultPassed = "passed"
	// ResultFailed means no eligible harness remained.
	ResultFailed = "failed"
	// ResultExhausted means checks still failed when attempts ran out.
	ResultExhausted = "exhausted"
	// ResultStuck means the implementer stopped changing the code with checks
	// still failing.
	ResultStuck = "stuck"
	// ResultNoChange means the implementer's work is empty.
	ResultNoChange = "no_change"
	// ResultBlocked means the implementer ended its summary with a BLOCKED:
	// line; FactoryRun.Blocked says why.
	ResultBlocked = "blocked"
)

// How a check came out, as ReviewResult.Status and RoundCheck.Status.
const (
	// CheckPassed means the check ran and found nothing to fix.
	CheckPassed = "passed"
	// CheckFailed means the check ran and found something to fix.
	CheckFailed = "failed"
	// CheckErrored means the check could not run.
	CheckErrored = "errored"
	// CheckSkipped means the check was not started because the round's gates
	// failed.
	CheckSkipped = "skipped"
	// CheckNotMeasured means a quality measurement could not be made. It never
	// blocks a round; RoundCheck.Error says why.
	CheckNotMeasured = "not_measured"
)

// ReviewPrompt is one review's progress inside a round. It carries state only:
// what the review said is fetched on demand through SessionDetail, the way
// command output is fetched through /output, so a snapshot stays small however
// much Claude wrote.
type ReviewPrompt struct {
	Harness   string         `json:"harness,omitempty"`
	Model     string         `json:"model,omitempty"`
	Name      string         `json:"name"`
	State     PromptRunState `json:"state"`
	SidecarID string         `json:"sidecar_id,omitempty"`
	// CommandID names the buffered output of this review's Claude run, readable
	// through /output while it runs and after.
	CommandID  string `json:"command_id,omitempty"`
	DurationMS int64  `json:"duration_ms,omitempty"`
	Error      string `json:"error,omitempty"`
	// Findings counts the structured findings parsed from this review's output,
	// and Worth the ones worth changing (severity high or medium).
	Findings int `json:"findings,omitempty"`
	Worth    int `json:"worth,omitempty"`
	// Status is how the review came out as a check, one of the Check values:
	// passed unless it found something worth changing, failed if it did,
	// errored if it could not run. Empty until it is known.
	Status string `json:"status,omitempty"`
	// ContextPeak is the review's largest prompt in tokens, absent when
	// unknown.
	ContextPeak int `json:"context_peak,omitempty"`
}

// PromptRunState is where one review of a round stands. The values are
// spelled out so they read in JSON and survive the daemon's own enum being
// reordered.
type PromptRunState string

// Review states.
const (
	PromptQueued  PromptRunState = "queued"
	PromptRunning PromptRunState = "running"
	PromptDone    PromptRunState = "done"
	PromptFailed  PromptRunState = "failed"
	// PromptSkipped means the round's gates failed, so the review never ran.
	PromptSkipped PromptRunState = "skipped"
)

// RoundKind says what a round of an integrate run was.
type RoundKind string

// Round kinds. The implementer turn that starts a recheck round is the fix.
const (
	// RoundBroad is the first round: every review judges the whole spec.
	RoundBroad RoundKind = "broad"
	// RoundRecheck follows the fix: only reviews that found something look
	// again, at those findings.
	RoundRecheck RoundKind = "recheck"
)

// Round is one pass of implementing the work and checking it.
type Round struct {
	Harness string     `json:"harness,omitempty"`
	Number  int        `json:"number"`
	State   RoundState `json:"state"`
	// Kind is "broad" or "recheck" for a round of an integrate run, "" for any
	// other.
	Kind    RoundKind      `json:"kind,omitempty"`
	Reviews []ReviewPrompt `json:"reviews"`
	// Findings counts every finding the round's reviews reported; Worth counts
	// the ones worth changing (severity high or medium).
	Findings int `json:"findings"`
	Worth    int `json:"worth"`
	// Implement and Checks are a factory round's implementer turn and
	// validation commands. Its reviews are in Reviews.
	Implement *RoundImplement `json:"implement,omitempty"`
	Checks    []RoundCheck    `json:"checks,omitempty"`
	// Note says why the round ended the way it did, such as why the loop stopped.
	Note string `json:"note,omitempty"`
	// GatesOnlyFailed is set when the round failed on validation commands and
	// on no review.
	GatesOnlyFailed bool       `json:"gates_only_failed,omitempty"`
	StartedAt       time.Time  `json:"started_at"`
	EndedAt         *time.Time `json:"ended_at,omitempty"`
}

// Session is the record of one factory run. It holds state only; text is in
// SessionDetail.
type Session struct {
	ID          string `json:"id"`
	ProjectRoot string `json:"project_root"`
	Branch      string `json:"branch,omitempty"`
	HeadSHA     string `json:"head_sha,omitempty"`

	State SessionState `json:"state"`
	Error string       `json:"error,omitempty"`

	// Stages is the session's stages, in order. The only one is its factory loop.
	Stages []Stage `json:"stages"`
	Rounds []Round `json:"rounds"`
	// Factory is what the run works on and where its work is.
	Factory *FactoryRun `json:"factory,omitempty"`

	StartedAt time.Time  `json:"started_at"`
	EndedAt   *time.Time `json:"ended_at,omitempty"`
	// StoppedAfterMS is the time from a stop request to the run ending, set
	// only on a run that was stopped.
	StoppedAfterMS *int64 `json:"stopped_after_ms,omitempty"`
}

// ReviewResult is what one review found.
type ReviewResult struct {
	Prompt     string `json:"prompt"`
	SidecarID  string `json:"sidecar_id,omitempty"`
	Error      string `json:"error,omitempty"`
	DurationMS int64  `json:"duration_ms,omitempty"`
	// Findings are the structured findings the review gave, each with an ID
	// unique within the round. Empty when it gave none or failed; Error tells
	// the two apart.
	Findings []Finding `json:"findings,omitempty"`
	// Status is how a factory review came out as a check, one of the Check
	// values.
	Status string `json:"status,omitempty"`
	// Harness is the agent that ran the review.
	Harness string `json:"harness,omitempty"`
	Model   string `json:"model,omitempty"`
	// ContextPeak is the largest prompt of the review in tokens, absent when
	// unknown.
	ContextPeak int `json:"context_peak,omitempty"`
	// Mode is how a factory review was run: "broad", "recheck", or "carried"
	// when an earlier pass stood in for it.
	Mode string `json:"mode,omitempty"`
}

// Finding is one problem a review reported.
type Finding struct {
	// ID is unique within a round.
	ID string `json:"id,omitempty"`
	// Prompt names the review that reported it.
	Prompt string `json:"prompt,omitempty"`
	// File is a repository-relative path with forward slashes.
	File string `json:"file"`
	// Line is the 1-based line the finding is about; zero means the file as a
	// whole or a line the reviewer did not give.
	Line     int    `json:"line,omitempty"`
	Severity string `json:"severity"`
	Body     string `json:"body"`
	// Patch is an optional unified diff that would fix the finding.
	Patch string `json:"patch,omitempty"`
	// Downgraded is set when smelt lowered the finding to low because it was
	// new in a later round on code the fix did not touch; DowngradedReason
	// says why.
	Downgraded       bool   `json:"downgraded,omitempty"`
	DowngradedReason string `json:"downgraded_reason,omitempty"`
}

// Location is where the finding is, as file:line, or the file alone when the
// finding has no line.
func (f Finding) Location() string {
	if f.Line > 0 {
		return fmt.Sprintf("%s:%d", f.File, f.Line)
	}
	return f.File
}

// RoundDetail is the text of one round.
type RoundDetail struct {
	Number  int            `json:"number"`
	Results []ReviewResult `json:"results,omitempty"`
}

// SessionDetail is a session plus the text of its reviews.
type SessionDetail struct {
	Session
	Details []RoundDetail `json:"details,omitempty"`
}

// Backends a FactoryRequest can name: the project config's, so the request and
// the config agree.
const (
	BackendCircleCI = config.BackendCircleCI
	BackendDocker   = config.BackendDocker
)

// FromRef is the ref a run was started from and the commit it resolved to.
type FromRef struct {
	Ref    string `json:"ref"`
	Commit string `json:"commit"`
}

// FactoryRequest starts a factory session.
type FactoryRequest struct {
	Fallback []string `json:"fallback,omitempty"`
	// FallbackSet distinguishes an explicitly empty list from project defaults.
	FallbackSet bool `json:"fallback_set,omitempty"`
	// ProjectRoot is a project the daemon tracks. Required.
	ProjectRoot string `json:"project_root"`
	// Prompt is what the implementer is asked to do. Required unless Continue
	// is set, when it is optional guidance added to the earlier run's request.
	Prompt string `json:"prompt"`
	// Continue is the ID of an earlier run to pick up the work of, or empty to
	// start from the project's files.
	Continue string `json:"continue,omitempty"`
	// From is a branch or commit to start the run from instead of the
	// project's files, whose uncommitted changes are then left out. It cannot
	// be combined with Continue.
	From string `json:"from,omitempty"`
	// Integrate checks the work already on From against Prompt, the spec for
	// all of it, and fixes only what fails. It needs From and a Prompt, and
	// cannot be combined with Continue. Attempts is ignored.
	Integrate bool `json:"integrate,omitempty"`
	// Against is what an integrate run compares From with; empty means the
	// project's default branch.
	Against string `json:"against,omitempty"`
	// ReviewsDir is a directory of review prompts relative to the project
	// root; empty means .smelt/reviews, which may be missing when validation
	// commands are enough.
	ReviewsDir string `json:"reviews_dir,omitempty"`
	NoValidate bool   `json:"no_validate,omitempty"`
	// NoPlan skips the plan turn that otherwise precedes the implementation.
	NoPlan bool `json:"no_plan,omitempty"`
	// NoPack skips the context pack the plan turn otherwise builds.
	NoPack bool `json:"no_pack,omitempty"`
	// Attempts is the most rounds to check; zero means DefaultAttempts.
	Attempts int `json:"attempts,omitempty"`
	// Reviewers is how many reviewer sidecars to run; zero means one per
	// review prompt.
	Reviewers int    `json:"reviewers,omitempty"`
	Model     string `json:"model,omitempty"`
	// Harness is the coding agent for the run: "claude", "codex" or "grok"; empty
	// means Claude Code.
	Harness                 string `json:"harness,omitempty"`
	ImplementerInstructions string `json:"implementer_instructions,omitempty"`
	// Zero timeouts mean the factory's defaults.
	ImplementTimeoutSeconds int  `json:"implement_timeout_seconds,omitempty"`
	ReviewTimeoutSeconds    int  `json:"review_timeout_seconds,omitempty"`
	KeepSidecars            bool `json:"keep_sidecars,omitempty"`
	// OrgID and Image override the project's configured ones.
	OrgID string `json:"org_id,omitempty"`
	Image string `json:"image,omitempty"`
	// Backend selects where the run's sidecars run: "" or BackendDocker for
	// local Docker containers, BackendCircleCI for CircleCI microVMs.
	Backend string `json:"backend,omitempty"`
	// KitWarning is set by the client when the project's Dockerfile has an older
	// agent kit than this smelt's; the run records and logs it.
	KitWarning string `json:"kit_warning,omitempty"`
	// Log is where the run keeps a plain-text log of its full context: an
	// absolute path, factory.LogDefault, or empty for none. Verbose adds more
	// to it and implies it; see factory.RunOptions.
	Log     string `json:"log,omitempty"`
	Verbose bool   `json:"verbose,omitempty"`
}

// SessionStartResponse answers an accepted POST /factory.
type SessionStartResponse struct {
	ID string `json:"id"`
}

// SessionList answers GET /factory.
type SessionList struct {
	Sessions []Session `json:"sessions"`
}

// HarnessSwap records which agent took over a logical turn and why.
type HarnessSwap struct {
	Round   int                 `json:"round"`
	Turn    string              `json:"turn"`
	From    harness.Name        `json:"from"`
	To      harness.Name        `json:"to"`
	Kind    harness.FailureKind `json:"kind"`
	ResetAt *time.Time          `json:"reset_at,omitempty"`
}

// HarnessTurn attributes a completed attempt, including failed agent calls.
type HarnessTurn struct {
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
