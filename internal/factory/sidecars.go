package factory

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/smeltdev/smelt/internal/config"
	"github.com/smeltdev/smelt/internal/harness"
	"github.com/smeltdev/smelt/internal/iostream"
	"github.com/smeltdev/smelt/internal/review"
	"github.com/smeltdev/smelt/internal/sidecar"
)

// reviewHead tells each reviewer how the run's request and its reusable review
// prompt relate. The original request remains the specification in every round;
// later implementer prompts contain only feedback from the preceding checks.
const reviewHead = `Review the implementation against the original requested change below. Treat the requested change as the specification and the review instructions as the lens for evaluating it. Do not implement changes yourself.

## Requested change

%s

` + changeUnderReview

// changeUnderReview tells a reviewer where the change is.
const changeUnderReview = `## Change under review

The change under review is the uncommitted work in this repository: run ` +
	"`git diff HEAD`" + ` to see it, including new files. Committed history is the baseline and is not under review.`

const reviewTail = "## Review instructions\n\n%s"

// PoolName names a run's sidecar pool. Each run has a pool of its own, rather
// than one per project reused as `smelt review` does: every member is synced
// from the run's worktree and gets a baseline commit on top of it, which a
// later run's sync does not expect. Two runs in one project must not share
// sidecars either.
func PoolName(runID string) string {
	return "factory-" + runID
}

// PoolSize is how many sidecars a run needs: the implementer, which runs the
// validation commands too, and one per reviewer.
func PoolSize(reviewers int) int {
	return 1 + reviewers
}

// Members describes every sidecar in a pool other than the implementer, the
// ones its tree is relayed to. ids are the pool's, once it is synced.
func Members(impl *sidecar.PoolEntry, ids []string) []*sidecar.PoolEntry {
	var out []*sidecar.PoolEntry
	for _, id := range ids {
		if id != impl.ID {
			out = append(out, &sidecar.PoolEntry{ID: id, RepoPath: impl.RepoPath, Backend: impl.Backend})
		}
	}
	return out
}

// Sidecars runs the loop's steps on a pool of sidecars: the implementer writes
// code on the member it holds for the whole run, which also runs validation,
// and each review runs on another member the implementer's tree is relayed to.
type Sidecars struct {
	agentMu       sync.Mutex
	prepared      map[string]bool
	turns         []TurnAttempt
	onAttempt     func(TurnAttempt)
	Failover      *Failover
	round         int
	plan          *Plan
	prior         Turn
	reviewHandoff string
	checks        []Check
	Exec          review.Execer
	Implementer   *Implementer
	// Acquire and Release hand out reviewer sidecars: the pool's own, with the
	// implementer's member already checked out.
	Acquire   func(context.Context) (*sidecar.PoolEntry, error)
	Release   func(*sidecar.PoolEntry)
	Reviewers []*sidecar.PoolEntry
	// WaitReviewers, when set, supplies Reviewers the first time a plan review
	// or check needs them, so the implementer can start before they are ready.
	WaitReviewers func(context.Context) ([]*sidecar.PoolEntry, error)
	Relay         *Relay
	// OperatorNotes, when set, is drained once per round into the round's
	// implementer and review prompts.
	OperatorNotes *NoteLog
	// Request is the original change the implementer was asked to make. It is
	// included in every review, including rounds where the implementer is sent
	// only feedback from the preceding checks.
	Request string
	// UpdateNote, when set, is added to every review's request: the branch was
	// updated from its base, and only the ticket's own changes are under review.
	UpdateNote string
	// Scout makes the plan turn build a context pack; Plan sets Pack from it.
	Scout ScoutOptions
	// Pack, when set, is added to every review's prompt after the request.
	Pack     *Pack
	Prompts  []review.Prompt
	Review   review.Options
	Commands []config.Command
	// GatesFirst runs Commands before the reviews, and skips the reviews of a
	// round whose commands failed.
	GatesFirst bool
	// FocusedRechecks makes rounds after the first carry forward the reviews
	// that passed and re-check the rest against their own earlier findings,
	// downgrading new medium findings on code the fix did not touch. Off, every
	// round is a full review that is asked to hold back.
	FocusedRechecks bool
	// Integrate makes the reviews those of a finished branch against the whole
	// of Request: the first round is broad, later ones re-check only the
	// reviews that found something. Gates and reviews always run together.
	Integrate bool
	// Install, when set, installs the project's dependencies once on every
	// sidecar before the baseline commit. A prebuilt image holds only the
	// package manager's store, since the workspace is mounted over the repo.
	Install string
	// InstallTimeout bounds Install on each sidecar; zero means
	// config.DefaultInstallTimeout.
	InstallTimeout time.Duration
	// OnInstallOutput receives each sidecar's install output as it arrives.
	OnInstallOutput func(sidecarID string, line string)
	// Quality, when set, measures the change on the implementer's sidecar after
	// the gates of every round.
	Quality *Quality
	// OnCheck is called as each validation command and the quality check finish. Reviews report
	// their progress through Review.ProgressFn.
	OnCheck func(Check)
	// Status, when set, receives notices about a round, such as a summary whose
	// gates were not run.
	Status iostream.StatusFunc
	// OnReviewerTree, when set, is called each round for every reviewer, in
	// Reviewers order, with whether the change it is about to review is the
	// implementer's. It is a canary for the relay; a mismatch does not stop the
	// round.
	OnReviewerTree func(ReviewerTree)

	// Base, when set, lets the run merge a base branch between rounds; see
	// SyncBase.
	Base *BaseSync

	ws workspace
	// ratchet runs the SARIF gates; Prepare measures its baseline.
	ratchet *Ratchet
	// reviewersReady is set once the reviewers have their dependencies, harness
	// setup and baseline. Loop.run calls Plan, ReviewPlan and Check one after
	// the other, so it needs no lock.
	reviewersReady bool
	// notesMu guards the round's drained notes: notesRound is the round
	// notesText was taken for.
	notesMu    sync.Mutex
	notesRound int
	notesText  string
	// history is what each review found in earlier rounds, by prompt name.
	history map[string][]priorRound
	// downgraded are the findings the guard lowered, over all rounds, as the
	// PR notes list them.
	downgraded []string
	// fingerprint is the implementer's change as last collected, which is
	// what each round's reviews are of.
	fingerprint string
	// gatesTook and reviewsTook are the time Check has spent on validation
	// commands and on reviews, over all rounds.
	gatesTook, reviewsTook time.Duration
}

// ScoutOptions is how the plan turn builds the context pack. The zero value
// builds none.
type ScoutOptions struct {
	Enabled bool
	// MaxTokens caps the pack; zero means config.DefaultPackMaxTokens.
	MaxTokens int
	// Files reads the files entries are checked against: the run's baseline.
	Files FileSource
}

// reviewRequest is the request as each review is shown it: with the context
// pack after it, when there is one.
func (s *Sidecars) reviewRequest() string {
	request := s.Request
	if s.UpdateNote != "" {
		request = strings.TrimRight(request, "\n") + "\n\n" + s.UpdateNote
	}
	if s.Pack == nil {
		return request
	}
	return packedRequest(request, *s.Pack)
}

// packedRequest is request followed by the pack's section for reviewers.
func packedRequest(request string, pk Pack) string {
	section := pk.ReviewMarkdown()
	if section == "" {
		return request
	}
	return strings.TrimRight(request, "\n") + "\n\n" + section
}

// CheckTimes are the time Check has spent on validation commands and on
// reviews so far. Commands and reviews that ran at the same time each count
// their own time.
func (s *Sidecars) CheckTimes() (gates, reviews time.Duration) {
	return s.gatesTook, s.reviewsTook
}

// Prepare sets up the implementer: dependencies, the agent's own setup, and
// the tree the pool synced to its member, the worktree's, committed as the
// baseline its work is measured against. Without WaitReviewers the fixed
// Reviewers are set up too.
func (s *Sidecars) Prepare(ctx context.Context) error {
	if err := s.installDependencies(ctx, []*sidecar.PoolEntry{s.Implementer.Entry}); err != nil {
		return err
	}
	if err := s.prepareHarness(ctx, s.Implementer.Entry); err != nil {
		return err
	}
	s.ws = workspace{exec: s.Exec, entry: s.Implementer.Entry}
	if err := s.ws.commitBaseline(ctx); err != nil {
		return err
	}
	if s.Quality != nil {
		s.Quality.MeasureBaseline(ctx, s.ws.baseline)
	}
	if len(SarifCommands(s.Commands)) > 0 {
		s.sarifRatchet().MeasureBaseline(ctx, s.ws.baseline, s.Commands)
	}
	if s.WaitReviewers != nil {
		return nil
	}
	return s.prepareReviewers(ctx)
}

// sarifRatchet is the run's ratchet, made on first use.
func (s *Sidecars) sarifRatchet() *Ratchet {
	if s.ratchet == nil {
		s.ratchet = &Ratchet{Exec: s.Exec, Entry: s.Implementer.Entry, Warn: s.Status}
	}
	return s.ratchet
}

// validate runs the round's validation commands on the implementer's sandbox;
// the ones that print SARIF are judged against the baseline.
func (s *Sidecars) validate(ctx context.Context) []Check {
	return runEach(s.Commands, s.OnCheck, func(c config.Command) Check {
		if c.Format == config.FormatSARIF {
			return s.sarifRatchet().Check(ctx, c)
		}
		return runCommand(ctx, s.Exec, s.Implementer.Entry, c)
	})
}

// prepareReviewers makes the reviewers ready, once per run: it waits for them
// if WaitReviewers is set, then installs dependencies, runs the agent's setup
// and commits a baseline on each. Reviewers are sent the implementer's files
// but never its .git, so `git diff HEAD` on one needs a commit of the same
// tree to diff against. A failed attempt leaves them unready, so a retry waits
// again.
func (s *Sidecars) prepareReviewers(ctx context.Context) error {
	if s.reviewersReady {
		return nil
	}
	if s.WaitReviewers != nil {
		members, err := s.WaitReviewers(ctx)
		if err != nil {
			return fmt.Errorf("get the reviewer sidecars ready: %w", err)
		}
		s.Reviewers = members
	}
	if err := s.setUpReviewers(ctx); err != nil {
		return fmt.Errorf("set up the reviewers' workspaces: %w", err)
	}
	s.reviewersReady = true
	return nil
}

func (s *Sidecars) setUpReviewers(ctx context.Context) error {
	if err := s.installDependencies(ctx, s.Reviewers); err != nil {
		return err
	}
	if err := s.onReviewers(func(e *sidecar.PoolEntry) error { return s.prepareHarness(ctx, e) }); err != nil {
		return err
	}
	return s.onReviewers(func(e *sidecar.PoolEntry) error {
		rw := workspace{exec: s.Exec, entry: e}
		if err := rw.commitBaseline(ctx); err != nil {
			return fmt.Errorf("%s: %w", e.ID, err)
		}
		return nil
	})
}

// Implement runs one implementer turn.
func (s *Sidecars) Implement(ctx context.Context, prompt string) (Turn, error) {
	var turn Turn
	notes := s.roundNotes(s.round)
	prompt = withNotes(notes, prompt)
	run := func(c Candidate) (*harness.RateLimit, error) {
		im := s.Implementer
		if im.agent().Name() != c.Harness.Name() {
			change, err := s.Collect(ctx)
			if err != nil {
				return nil, err
			}
			handoff, err := s.Handoff(ctx, s.Request, prompt, s.plan, handoffPrior(turn, s.prior), change, s.checks)
			if err != nil {
				return nil, err
			}
			prompt = withNotes(notes, handoff)
			im.FreshSession()
		}
		s.useCandidate(c)
		if err := s.prepareCandidate(ctx, im.Entry, c); err != nil {
			s.recordAttempt(TurnAttempt{Round: s.round, Turn: turnImplementer}, c, err)
			return nil, err
		}
		var err error
		turn, err = im.Run(ctx, prompt)
		s.recordAttempt(TurnAttempt{Round: s.round, Turn: turnImplementer, RateLimit: turn.RateLimit, DurationMS: turn.Duration.Milliseconds(), CostUSD: turn.CostUSD, InputTokens: turn.InputTokens, CachedTokens: turn.CachedTokens, OutputTokens: turn.OutputTokens}, c, err)
		return turn.RateLimit, err
	}
	var err error
	if s.Failover != nil {
		err = s.Failover.Do(ctx, s.round, turnImplementer, run)
	} else {
		turn, err = s.Implementer.Run(ctx, prompt)
	}
	if err != nil {
		return turn, err
	}
	s.prior = turn
	turn = s.prove(turn)
	s.answer(turn.Summary)
	return turn, nil
}

// FreshSession resets the implementer before a fresh fix round.
func (s *Sidecars) FreshSession() { s.Implementer.FreshSession() }

// Handoff assembles a concise round prompt from the request and current work.
func (s *Sidecars) Handoff(ctx context.Context, ticket, feedback string, plan *Plan, prior Turn, change Change, checks []Check) (string, error) {
	findings := MissingWork(checks)
	if len(findings) == 0 {
		findings = append(findings, reviewFindings(checks)...)
	}
	pack := s.Pack
	if pack != nil {
		rec := *pack
		rec.Entries = append([]PackEntry(nil), pack.Entries...)
		root := s.Implementer.Entry.RepoPath
		if s.Relay != nil {
			if err := s.Pull(ctx); err != nil {
				return "", err
			}
			root = s.Relay.dir
		}
		current := workingFiles{root: root}
		for i := range rec.Entries {
			if rec.Entries[i].Kind == PackNewFile {
				if _, err := current.ReadFile(ctx, rec.Entries[i].Path); err == nil {
					rec.Entries[i].Kind = PackEdit
				}
			}
		}
		revalidated := Reanchor(ctx, rec, s.Scout.Files, current, s.Scout.MaxTokens)
		pack = &revalidated
		s.Pack = pack
	}
	return BuildHandoff(HandoffOptions{
		Ticket: ticket + "\n\nCurrent round feedback:\n" + feedback, Plan: plan, Pack: pack,
		Change: change.Stat, Summary: prior.Summary, Findings: findings,
	}), nil
}

func reviewFindings(checks []Check) []review.Finding {
	var out []review.Finding
	for _, c := range checks {
		for _, f := range c.Findings {
			if f.WorthChanging() {
				out = append(out, f)
			}
		}
	}
	return out
}

// prove checks a summary that claims passing gates against what the turn ran.
// A claim with no gate run after the last edit is noted on the turn and
// reported, and nothing more: the round's own gates run right after and tell
// the truth, so asking the implementer to run them again would only add a turn.
func (s *Sidecars) prove(turn Turn) Turn {
	missing := unproven(turn.Activity, gateCommands(s.Commands))
	if len(missing) == 0 || !claimsGatesPass(turn.Summary) {
		return turn
	}
	turn.Unproven = missing
	s.notify(iostream.LevelWarn, "the summary says the gates pass but "+strings.Join(missing, ", ")+" were not run after the last edit")
	return turn
}

// notify reports a notice, if anyone listens.
func (s *Sidecars) notify(level iostream.Level, msg string) {
	if s.Status != nil {
		s.Status(level, msg)
	}
}

// answer records the implementer's summary as the reply to every review's latest
// findings, since one turn answers all the feedback of the round just checked.
func (s *Sidecars) answer(reply string) {
	for name, rounds := range s.history {
		if n := len(rounds); n > 0 && rounds[n-1].Reply == "" {
			s.history[name][n-1].Reply = strings.TrimSpace(reply)
		}
	}
}

// Collect describes the implementer's work so far.
func (s *Sidecars) Collect(ctx context.Context) (Change, error) {
	c, err := s.ws.collect(ctx)
	if err == nil {
		s.fingerprint = c.Fingerprint
	}
	return c, err
}

// Check brings the implementer's work into the worktree, then validates it and
// has it reviewed. With GatesFirst the validation commands run first and the
// reviews only when none failed; otherwise both run at the same time.
func (s *Sidecars) Check(ctx context.Context, round int) ([]Check, error) {
	s.round = round
	if err := s.Relay.Pull(ctx, s.Implementer.Entry.ID, s.Implementer.Entry.RepoPath); err != nil {
		return nil, err
	}
	if len(s.Prompts) > 0 {
		if err := s.captureReviewHandoff(ctx); err != nil {
			return nil, err
		}
	}
	if s.GatesFirst && !s.Integrate && len(s.Prompts) > 0 {
		return s.checkGatesFirst(ctx, round)
	}
	return s.checkTogether(ctx, round)
}

// checkGatesFirst runs the validation commands, and the reviews if they all
// passed. A round whose gates failed is going back to the implementer anyway,
// so its reviews are reported as skipped rather than spent on code that is
// about to change.
func (s *Sidecars) checkGatesFirst(ctx context.Context, round int) ([]Check, error) {
	start := time.Now()
	gates := s.validate(ctx)
	s.gatesTook += time.Since(start)
	if gatesFailed(gates) {
		skipped := make([]Check, len(s.Prompts))
		for i, p := range s.Prompts {
			skipped[i] = skippedReview(p.Name)
		}
		return slices.Concat(skipped, gates, s.qualityChecks(ctx)), nil
	}
	start = time.Now()
	defer func() { s.reviewsTook += time.Since(start) }()
	return s.reviewAndConclude(ctx, round, gates)
}

// reviewAndConclude runs the round's reviews after its gates passed, and
// returns every check of the round.
func (s *Sidecars) reviewAndConclude(ctx context.Context, round int, gates []Check) ([]Check, error) {
	scope := s.reviewSet(round)
	var reviews []Check
	if len(scope.Prompts) > 0 {
		if err := s.prepareReviewers(ctx); err != nil {
			return nil, err
		}
		if err := s.relayToReviewers(ctx, round); err != nil {
			return nil, err
		}
		var err error
		if reviews, err = s.runReviews(ctx, round, scope); err != nil {
			return nil, err
		}
	}
	return slices.Concat(scope.Carried, reviews, gates, s.qualityChecks(ctx)), nil
}

// checkTogether runs the reviews on the reviewers and validation on the
// implementer at the same time.
func (s *Sidecars) checkTogether(ctx context.Context, round int) ([]Check, error) {
	scope := s.reviewSet(round)
	prompts := scope.Prompts
	if len(prompts) > 0 {
		if err := s.prepareReviewers(ctx); err != nil {
			return nil, err
		}
		if err := s.relayToReviewers(ctx, round); err != nil {
			return nil, err
		}
	}
	var (
		wg         sync.WaitGroup
		reviews    []Check
		reviewErr  error
		validation []Check
		// Each goroutine owns its duration; they are added up after Wait.
		reviewsTook, gatesTook time.Duration
	)
	if len(prompts) > 0 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			start := time.Now()
			reviews, reviewErr = s.runReviews(ctx, round, scope)
			reviewsTook = time.Since(start)
		}()
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		start := time.Now()
		validation = s.validate(ctx)
		gatesTook = time.Since(start)
	}()
	wg.Wait()
	s.gatesTook += gatesTook
	s.reviewsTook += reviewsTook
	if reviewErr != nil {
		return nil, reviewErr
	}
	return slices.Concat(scope.Carried, reviews, validation, s.qualityChecks(ctx)), nil
}

// qualityChecks is the round's quality check, none when the run does not
// measure quality. It runs after the gates, on the tree they ran against.
func (s *Sidecars) qualityChecks(ctx context.Context) []Check {
	if s.Quality == nil {
		return nil
	}
	c := s.Quality.Check(ctx)
	if s.OnCheck != nil {
		s.OnCheck(c)
	}
	return []Check{c}
}

// reviewSet is the reviews to run in round, as each reviewer is sent them, and
// the checks of reviews that are not run again.
func (s *Sidecars) reviewSet(round int) roundScope {
	rs := s.scopeReviews(round)
	if notes := s.roundNotes(round); notes != "" {
		for i := range rs.Prompts {
			rs.Prompts[i].Body += "\n\n" + notes
		}
	}
	return rs
}

// roundNotes is the operator notes section for round: the notes no earlier
// round has read, taken once however many prompts of the round ask.
func (s *Sidecars) roundNotes(round int) string {
	if s.OperatorNotes == nil {
		return ""
	}
	s.notesMu.Lock()
	defer s.notesMu.Unlock()
	if round == s.notesRound {
		return s.notesText
	}
	notes, err := s.OperatorNotes.Take(round)
	if err != nil && s.Status != nil {
		s.Status(iostream.LevelWarn, fmt.Sprintf("operator notes: %v", err))
	}
	s.notesRound, s.notesText = round, notesSection(notes)
	return s.notesText
}

// scopeReviews is the round's reviews without the operator's notes.
func (s *Sidecars) scopeReviews(round int) roundScope {
	switch {
	case s.Integrate:
		return scopePrompts(scopeInput{Broad: broadPreamble(s.Request), Spec: s.Request}, s.Prompts, s.history, round)
	case s.FocusedRechecks:
		return scopePrompts(normalScope(s.reviewRequest()), s.Prompts, s.history, round)
	}
	return legacyScope(s.reviewRequest(), s.Prompts, s.history)
}

// normalScope is the scoping of a normal run: the request, with the context
// pack when there is one, is both the broad review's and the re-check's.
func normalScope(request string) scopeInput {
	return scopeInput{Broad: fmt.Sprintf(reviewHead, request), Spec: request}
}

// relayToReviewers relays the implementer's tree, as pulled into the
// worktree, to the reviewers.
func (s *Sidecars) relayToReviewers(ctx context.Context, round int) error {
	// A review that timed out stops being streamed but keeps running on
	// its sidecar, and would read the tree as it is replaced. Best-effort:
	// a sidecar that cannot be reached fails the push that follows anyway.
	// The bracket keeps the pattern from matching the shell running pkill.
	_ = s.onReviewers(s.script(ctx, "pkill -f '[c]laude -p' || true"))
	if err := s.Relay.Push(ctx, s.Reviewers); err != nil {
		return err
	}
	// New files are marked intent-to-add so `git diff HEAD` shows them; the
	// reset first drops marks for files a later round deleted.
	if err := s.onReviewers(s.script(ctx, "git reset -q && git add -A -N")); err != nil {
		return fmt.Errorf("prepare reviewers: %w", err)
	}
	if s.OnReviewerTree != nil {
		s.reviewerTrees(ctx, round)
	}
	return nil
}

// runReviews runs the scope's prompts on the reviewers and remembers what each
// found for the next round. In a focused re-check, a new medium finding on code
// the fix did not touch is downgraded before it is judged.
func (s *Sidecars) runReviews(ctx context.Context, round int, scope roundScope) ([]Check, error) {
	results, err := review.RunPass(ctx, s.Acquire, s.Release, s.Exec, scope.Prompts, s.reviewOptions(round, "review", s.Review))
	if err != nil {
		return nil, fmt.Errorf("reviews: %w", err)
	}
	tree := ""
	if s.FocusedRechecks && !s.Integrate {
		tree = s.recordTree(ctx)
	}
	changes := map[string]changedLines{}
	checks := make([]Check, 0, len(results))
	for _, r := range results {
		mode := scope.Modes[r.Prompt]
		if mode == ModeRecheck {
			r.Parsed.Findings = s.guardFindings(ctx, r, round, tree, changes)
		}
		c := FromReview(r)
		c.Mode = mode
		checks = append(checks, c)
		s.remember(round, c, tree)
	}
	return checks, nil
}

// recordTree is the tree the reviewers are about to be shown, "" when it
// cannot be read. Without it the next round's guard has nothing to compare.
func (s *Sidecars) recordTree(ctx context.Context) string {
	tree, err := s.ws.tree(ctx)
	if err != nil {
		s.notify(iostream.LevelWarn, "could not record the reviewed tree; later findings will not be downgraded: "+err.Error())
		return ""
	}
	return tree
}

// guardFindings applies guard to a re-checking review's findings, and notes
// the ones it lowered. changes caches the diff from each tree a review last saw.
func (s *Sidecars) guardFindings(ctx context.Context, r review.Result, round int, tree string, changes map[string]changedLines) []review.Finding {
	rounds := s.history[r.Prompt]
	in := guardInput{Round: round}
	for _, p := range rounds {
		in.Prior = append(in.Prior, p.Findings...)
	}
	if base := rounds[len(rounds)-1].Tree; base != "" && tree != "" {
		cl, cached := changes[base]
		if !cached {
			var err error
			if cl, err = s.ws.changedRanges(ctx, base, tree); err != nil {
				s.notify(iostream.LevelWarn, "could not tell what changed since the last review; its findings will not be downgraded: "+err.Error())
				cl = nil
			}
			changes[base] = cl
		}
		in.Changed = cl
	}
	out := make([]review.Finding, len(r.Parsed.Findings))
	for i, f := range r.Parsed.Findings {
		out[i] = guard(f, in)
		if out[i].Downgraded != "" {
			note := fmt.Sprintf("%s: %s (downgraded: %s)", out[i].Location(), strings.Join(strings.Fields(out[i].Body), " "), out[i].Downgraded)
			s.downgraded = append(s.downgraded, note)
			s.notify(iostream.LevelInfo, "review "+r.Prompt+": "+note)
		}
	}
	return out
}

// Downgraded lists the findings the guard lowered to low in any round, for the
// run's PR notes.
func (s *Sidecars) Downgraded() []string {
	return s.downgraded
}

// Pull brings the implementer's work so far into the worktree. Check does it
// every round; this is for the way out, after a turn that failed or a round
// that stopped short of checking.
func (s *Sidecars) Pull(ctx context.Context) error {
	return s.Relay.Pull(ctx, s.Implementer.Entry.ID, s.Implementer.Entry.RepoPath)
}

// onReviewers runs fn on every reviewer at once and reports all that failed.
func (s *Sidecars) onReviewers(fn func(*sidecar.PoolEntry) error) error {
	errs := make([]error, len(s.Reviewers))
	var wg sync.WaitGroup
	for i, e := range s.Reviewers {
		wg.Add(1)
		go func(i int, e *sidecar.PoolEntry) {
			defer wg.Done()
			errs[i] = fn(e)
		}(i, e)
	}
	wg.Wait()
	return errors.Join(errs...)
}

// script runs a command in a reviewer's workspace.
func (s *Sidecars) script(ctx context.Context, cmd string) func(*sidecar.PoolEntry) error {
	return func(e *sidecar.PoolEntry) error {
		if _, err := review.RunScript(ctx, s.Exec, e, inRepo(e, cmd), maxScriptOutput); err != nil {
			return fmt.Errorf("%s: %w", e.ID, err)
		}
		return nil
	}
}

// inRepo is the script that runs cmd in e's workspace.
func inRepo(e *sidecar.PoolEntry, cmd string) string {
	return "cd " + sidecar.ShellEscape(e.RepoPath) + " && " + cmd
}

// ReviewerTree compares the change one reviewer is about to review with the
// implementer's, by fingerprint.
type ReviewerTree struct {
	Round     int
	SidecarID string
	// Fingerprint is the reviewer's change; Want is the implementer's.
	Fingerprint string
	Want        string
	// Err is why the reviewer's fingerprint could not be read.
	Err error
}

// Matches reports whether the reviewer has the implementer's change.
func (t ReviewerTree) Matches() bool {
	return t.Err == nil && t.Fingerprint != "" && t.Fingerprint == t.Want
}

// reviewerTrees fingerprints every reviewer's change at once and reports them
// in order.
func (s *Sidecars) reviewerTrees(ctx context.Context, round int) {
	trees := make([]ReviewerTree, len(s.Reviewers))
	var wg sync.WaitGroup
	for i, e := range s.Reviewers {
		wg.Add(1)
		go func(i int, e *sidecar.PoolEntry) {
			defer wg.Done()
			out, err := review.RunScript(ctx, s.Exec, e, inRepo(e, fingerprintCmd), maxScriptOutput)
			trees[i] = ReviewerTree{Round: round, SidecarID: e.ID, Fingerprint: strings.TrimSpace(out), Want: s.fingerprint, Err: err}
		}(i, e)
	}
	wg.Wait()
	for _, t := range trees {
		s.OnReviewerTree(t)
	}
}

// remember keeps what a review found this round for the reviews of later ones.
// A review that could not run, or was not started, has nothing to remember.
func (s *Sidecars) remember(round int, c Check, tree string) {
	if c.Kind != KindReview || c.Status == StatusErrored || c.Status == StatusSkipped {
		return
	}
	if s.history == nil {
		s.history = map[string][]priorRound{}
	}
	var worth []review.Finding
	for _, f := range c.Findings {
		if f.WorthChanging() {
			worth = append(worth, f)
		}
	}
	s.history[c.Name] = append(s.history[c.Name], priorRound{Round: round, Findings: worth, Tree: tree})
}

// legacyScope is a run's reviews with factory.focusedRechecks off: every
// review is a full one in every round, asked to hold back by what it reported
// before. Nothing is carried forward.
func legacyScope(request string, prompts []review.Prompt, history map[string][]priorRound) roundScope {
	rs := roundScope{Modes: map[string]Mode{}}
	for _, p := range prompts {
		var b strings.Builder
		fmt.Fprintf(&b, reviewHead, request)
		b.WriteString("\n\n")
		b.WriteString(severityScope + "\n\n")
		if h := legacyHistory(history[p.Name]); h != "" {
			b.WriteString(h + "\n")
		}
		fmt.Fprintf(&b, reviewTail, p.Body)
		rs.Prompts = append(rs.Prompts, review.Prompt{Name: p.Name, Body: b.String()})
		rs.Modes[p.Name] = ModeBroad
	}
	return rs
}

// installDependencies runs Install in each entry's workspace at once. What
// it creates is gitignored, so later syncs leave it alone.
func (s *Sidecars) installDependencies(ctx context.Context, entries []*sidecar.PoolEntry) error {
	if s.Install == "" {
		return nil
	}
	errs := make([]error, len(entries))
	var wg sync.WaitGroup
	for i, e := range entries {
		wg.Add(1)
		go func(i int, e *sidecar.PoolEntry) {
			defer wg.Done()
			errs[i] = s.installOn(ctx, e)
		}(i, e)
	}
	wg.Wait()
	if err := errors.Join(errs...); err != nil {
		return fmt.Errorf("install dependencies: %w", err)
	}
	return nil
}

func (s *Sidecars) installOn(ctx context.Context, e *sidecar.PoolEntry) error {
	timeout := s.InstallTimeout
	if timeout <= 0 {
		timeout = config.DefaultInstallTimeout * time.Second
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	var tail strings.Builder
	code, err := s.Exec(ctx, e, inRepo(e, s.Install), nil, func(_ string, data []byte) {
		if s.OnInstallOutput != nil {
			for _, line := range strings.Split(strings.TrimRight(string(data), "\n"), "\n") {
				s.OnInstallOutput(e.ID, line)
			}
		}
		tail.Write(data)
		if tail.Len() > 8192 {
			rest := tail.String()[tail.Len()-4096:]
			tail.Reset()
			tail.WriteString(rest)
		}
	}, nil)
	switch {
	case errors.Is(ctx.Err(), context.DeadlineExceeded):
		return fmt.Errorf("%s: %q timed out after %s: %s", e.ID, s.Install, timeout, review.Tail(strings.TrimSpace(tail.String()), 2000))
	case err != nil:
		return fmt.Errorf("%s: %w", e.ID, err)
	case code != 0:
		return fmt.Errorf("%s: %q exited %d: %s", e.ID, s.Install, code, review.Tail(strings.TrimSpace(tail.String()), 2000))
	}
	return nil
}

// prepareHarness runs the agent's own setup once on a sidecar, after the
// dependencies are installed. Its secrets are in the setup's environment only,
// so later turns never get them.
func (s *Sidecars) prepareHarness(ctx context.Context, e *sidecar.PoolEntry) error {
	if s.Failover != nil {
		return nil
	}
	script, env := s.Implementer.agent().Prepare(s.Implementer.Credential)
	if script == "" {
		return nil
	}
	if _, err := review.RunScriptEnv(ctx, s.Exec, e, script, env, 64<<10); err != nil {
		return fmt.Errorf("%s: set up %s: %w", e.ID, s.Implementer.agent().Name(), err)
	}
	return nil
}
