package factory

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"github.com/smeltdev/smelt/internal/circleci"
	"github.com/smeltdev/smelt/internal/config"
	"github.com/smeltdev/smelt/internal/gitexec"
	"github.com/smeltdev/smelt/internal/harness"
	"github.com/smeltdev/smelt/internal/iostream"
	"github.com/smeltdev/smelt/internal/quality"
	"github.com/smeltdev/smelt/internal/review"
	"github.com/smeltdev/smelt/internal/sidecar"
)

// Cleanup deadlines. Cleanup runs on the way out, after a failure or an
// interrupt too, so it gets time of its own rather than the run's context.
const (
	cleanupTimeout   = 30 * time.Second
	poolCloseTimeout = 5 * time.Minute
	pullTimeout      = 2 * time.Minute
)

// PackOptions is the context pack of a run; the zero value is off.
type PackOptions struct {
	Enabled bool
	// MaxTokens caps the pack; zero means config.DefaultPackMaxTokens.
	MaxTokens int
}

// RunOptions is one factory run. Everything that needs a person, such as
// picking an org or finding a credential, is settled before it.
type RunOptions struct {
	// Rounds selects fresh sessions or the legacy resumed session; empty defaults to fresh.
	Rounds string
	// ContextWindow is the implementer auto-compaction threshold; zero requests agent default.
	ContextWindow int
	// Root is the developer's repository root. The run's worktree is made from
	// it, and the pool's state is kept in it, where the dashboard finds it.
	Root string
	// Prompt is what the implementer is asked to do. A continued run makes its
	// own from Continue, and ignores this.
	Prompt string
	// Continue, when set, picks up the work of an earlier run instead of
	// starting from the developer's files. It keeps that run's base, so it
	// cannot be combined with From.
	Continue *Continuation
	// From, when set, starts the run from this branch or commit instead of
	// the developer's files; see ResolveFrom. Their uncommitted changes are
	// not part of the run.
	From string
	// Attempts is the most rounds to check. A continued run that checks the
	// work first counts that round as one of them.
	Attempts int
	// Reviewers is how many reviewer sidecars to run; see ReviewerCount.
	Reviewers int
	Prompts   []review.Prompt
	Commands  []config.Command
	// Plan has the implementer write a plan, and the plan reviewed, before it
	// writes code. A continued run never plans: its plan is in the record.
	Plan bool
	// Pack makes the plan turn also build a context pack for the implementer
	// and the reviewers, and a continued run re-validate the recorded one.
	Pack PackOptions
	// OnPack, when set, hears the pack a continued run reuses; a new run's
	// pack comes with its plan, in EventPlanReviewed.
	OnPack func(Pack)
	// GatesFirst runs Commands before the reviews and skips the reviews of a
	// round whose commands failed.
	GatesFirst bool
	// Integrate, when set, checks the work already on From against the spec in
	// Prompt: no plan and no implementer turn until the checks say what is
	// wrong, then one fix turn and a focused re-check. It needs From, and
	// cannot be combined with Continue.
	Integrate *IntegrateOptions

	// Quality, when set, adds the built-in quality check to every round, with
	// these limits. Nil turns it off.
	Quality *quality.Config

	// Author is the configured author of the branch's commits (factory.author);
	// when it is empty, ResolveAuthor falls back to the developer's identity.
	Author config.CommitAuthor

	Client *circleci.Client
	OrgID  string
	Image  string
	// Backend provisions and drives the run's sidecars. When nil, the run uses
	// the CircleCI backend built from Client and OrgID. Set it to run the loop
	// on a different provider, such as local Docker.
	Backend sidecar.Backend
	// Install installs the project's dependencies on each sidecar before the
	// run starts; empty skips it.
	Install string
	// KeepSidecars leaves the sidecars running when the run ends.
	KeepSidecars bool

	// Harness is the coding agent for the implementer and the reviewers; Claude
	// Code when nil. Credential is its credential.
	Harness                 harness.Harness
	Credential              review.Credential
	BaseURL                 string
	Model                   string
	ImplementerInstructions string
	ImplementTimeout        time.Duration
	ReviewTimeout           time.Duration

	// Exec runs commands on the sidecars; review.ClientExec when nil.
	Exec review.Execer

	// Warnings are problems with the run's setup that did not stop it; they go
	// in the log.
	Warnings []string

	// Log is where to keep a plain-text log of the run, with its full context
	// whatever a display of it filters out: a path, LogDefault for
	// DefaultLogPath, or "" for none.
	Log string
	// Verbose adds the review prompts, the output of commands that passed,
	// and a check each round that every reviewer has the implementer's change
	// to the log. It implies a log.
	Verbose bool

	// Status reports progress, warnings included. It must be set.
	Status iostream.StatusFunc
	// The rest report what the run is doing, and may be nil. OnStart is
	// called once the run's worktree exists.
	OnStart          func(runID string, wt Worktree)
	OnEvent          func(Event)
	OnActivity       func(Activity)
	OnReviewProgress func(review.ProgressEvent)
	OnCheck          func(Check)
}

// Report is what a run left behind.
type Report struct {
	RunID string
	// Worktree is the run's worktree. It is kept once the implementer has
	// started, and removed, with its branch, if the run failed before that.
	Worktree Worktree
	// Started reports whether the implementer started.
	Started bool
	// Committed reports whether the work was committed on Worktree.Branch.
	Committed bool
	// Stat is the committed work's diff stat against Worktree.Baseline, "" for
	// no changes. StatErr says why it could not be worked out.
	Stat    string
	StatErr error
	// Notes are the account of the committed change, nil when there was none
	// to describe. They are the fallback when they could not be written.
	Notes *Notes
	// WorktreeRemoved reports that the run ended before the implementer started
	// and its worktree, holding nothing, was removed.
	WorktreeRemoved bool
	Outcome         Outcome
	// KeptSidecars are the sidecars left running with KeepSidecars.
	KeptSidecars []string
	// Log is the path of the run's log, if it kept one.
	Log string
	// Timing is where the run's time went, once the implementer started.
	Timing Timing
}

// backend is the backend that drives the run's pool and relay: the caller's
// choice (such as Docker), else the CircleCI backend built from the client.
func (o *RunOptions) backend() sidecar.Backend {
	if o.Backend != nil {
		return o.Backend
	}
	return sidecar.NewCircleCIBackend(o.Client, o.OrgID)
}

// PrepareBackend lets a backend that needs it check its host once, before the
// pool creates any sidecar.
func PrepareBackend(ctx context.Context, backend sidecar.Backend, image string, status iostream.StatusFunc) error {
	p, ok := backend.(sidecar.Preparer)
	if !ok {
		return nil
	}
	return p.Ready(ctx, image, status)
}

// createPool readies the backend, then creates the run's sidecars from the
// worktree.
func createPool(ctx context.Context, opts RunOptions, backend sidecar.Backend, runID string, wt Worktree, status iostream.StatusFunc) (*sidecar.Pool, error) {
	if err := PrepareBackend(ctx, backend, opts.Image, status); err != nil {
		return nil, err
	}
	return sidecar.NewPool(ctx, opts.Client, sidecar.PoolOptions{
		Size:    PoolSize(opts.Reviewers),
		Name:    PoolName(runID),
		OrgID:   opts.OrgID,
		Image:   opts.Image,
		Backend: backend,
		// The sidecars start from the worktree, and its state stays in the
		// project, where the dashboard finds it.
		WorkDir:  wt.Path,
		StateDir: opts.Root,
	}, status)
}

// reviewersOf is how the steps get the pool's other members once they are
// needed. IDs lists only the members that exist, so it is read after the wait.
func reviewersOf(pool *sidecar.Pool, impl *sidecar.PoolEntry) func(context.Context) ([]*sidecar.PoolEntry, error) {
	return func(ctx context.Context) ([]*sidecar.PoolEntry, error) {
		if err := pool.WaitSynced(ctx); err != nil {
			return nil, err
		}
		return Members(impl, pool.IDs()), nil
	}
}

// ReviewerCount is how many reviewer sidecars a run with prompts needs when
// asked for requested: one per prompt unless fewer are asked for, and none
// without prompts.
func ReviewerCount(requested int, prompts []review.Prompt) int {
	if len(prompts) == 0 {
		return 0
	}
	if requested <= 0 || requested > len(prompts) {
		return len(prompts)
	}
	return requested
}

// Run makes the run's worktree and sidecars, runs the loop, and commits the
// implementer's work on the run's branch. A failure before the loop starts
// says which step failed; one in the loop is returned as Loop.Run returns it,
// with the report so far. The work is committed either way once the implementer has
// started, so it is not lost with the sidecars.
func Run(ctx context.Context, opts RunOptions) (rep Report, err error) {
	runStart := time.Now()
	rep.RunID = runStart.UTC().Format("20060102-150405")
	cont := opts.Continue
	if cont != nil && opts.From != "" {
		return rep, errors.New("a continued run keeps its base, so it cannot start from another ref")
	}
	if err := opts.checkIntegrate(); err != nil {
		return rep, err
	}
	integ := opts.Integrate
	request := opts.Prompt
	if cont != nil {
		opts.Prompt, request = cont.prompt(), cont.request()
	}
	if opts.Verbose && opts.Log == "" {
		opts.Log = LogDefault
	}
	lg, err := openLog(opts.Log, rep.RunID, opts.Verbose)
	if err != nil {
		return rep, fmt.Errorf("open the run's log: %w", err)
	}
	if lg != nil {
		rep.Log = lg.path
	}
	// Deferred first, so it runs last: the other defers report as they clean
	// up, and that goes in the log too.
	defer func() { lg.close(rep, err) }()
	opts = lg.wrap(opts)
	status := opts.Status
	lg.start(rep.RunID, opts, request)

	dataDir, err := config.ProjectDataDir(opts.Root)
	if err != nil {
		return rep, fmt.Errorf("find smelt's data directory for the project: %w", err)
	}
	author := ResolveAuthor(ctx, opts.Root, opts.Author).CommitAuthor
	wt, err := startWorktree(ctx, opts, rep.RunID, dataDir, author)
	if err != nil {
		return rep, err
	}
	rep.Worktree = wt
	if opts.OnStart != nil {
		opts.OnStart(rep.RunID, wt)
	}
	rec := newRecord(rep.RunID, request, wt)
	rec.Harness, rec.Model = opts.harnessName(), opts.Model
	opts.markRecord(&rec)
	// Until the implementer starts, a new worktree holds nothing the developer
	// does not already have. A continued run's worktree holds the earlier
	// run's work, so it stays. The defers set rep, so it is a named result.
	defer func() {
		if !rep.Started && cont == nil {
			rep.WorktreeRemoved = removeWorktree(ctx, opts.Root, wt, status)
		}
	}()
	defer func() {
		if rep.Started {
			keepEndedRecord(dataDir, rec, rep, status)
			cont.resolved(dataDir, status)
		}
	}()
	// A continued run's sidecars are synced from the baseline's files, and
	// the work is laid on the implementer's afterwards, so the reviewers see
	// the whole change rather than only what this run adds. An integrate run's
	// worktree holds the branch's tip over the merge base the same way.
	showingBaseline := false
	if cont != nil || integ != nil {
		if err := wt.showBaseline(ctx); err != nil {
			return rep, err
		}
		showingBaseline = true
		defer func() {
			if showingBaseline {
				restoreWorktree(ctx, wt, status)
			}
		}()
	}

	status(iostream.LevelInfo, opts.summary())
	status(iostream.LevelStep, fmt.Sprintf("Preparing an implementer sidecar and %d reviewer sidecar(s)...", opts.Reviewers))
	// One backend drives both the pool and the relay.
	backend := opts.backend()
	pool, err := createPool(ctx, opts, backend, rep.RunID, wt, status)
	if err != nil {
		return rep, fmt.Errorf("create the run's sidecars: %w", err)
	}
	defer func() { rep.KeptSidecars = closePool(ctx, pool, opts.KeepSidecars, status) }()
	// A new pool has synced the worktree to its first member by now, so the work
	// can be laid back and the implementer can start without the other members.
	if showingBaseline {
		showingBaseline = false
		if err := wt.restoreWork(ctx); err != nil {
			return rep, err
		}
	}
	// The implementer holds its member for the whole run; reviews are handed
	// the rest.
	impl, err := pool.Acquire(ctx)
	if err != nil {
		return rep, fmt.Errorf("check out the implementer's sidecar: %w", err)
	}

	if !pool.Synced() {
		status(iostream.LevelInfo, "implementer started (reviewers still starting)")
	}

	exec := opts.Exec
	if exec == nil {
		exec = review.ClientExec
	}
	steps := &Sidecars{
		Exec: exec,
		Implementer: &Implementer{
			Harness: opts.Harness, Exec: exec, Entry: impl, Credential: opts.Credential, BaseURL: opts.BaseURL,
			Model: opts.Model, Timeout: opts.ImplementTimeout, Instructions: opts.ImplementerInstructions, ContextWindow: opts.ContextWindow,
			OnActivity: opts.OnActivity, Gates: gateCommands(opts.Commands),
		},
		Acquire:       pool.Acquire,
		Release:       pool.Release,
		WaitReviewers: reviewersOf(pool, impl),
		Relay:         NewRelay(backend, wt.Path, status),
		Request:       request,
		Scout:         ScoutOptions{Enabled: opts.Pack.Enabled, MaxTokens: opts.Pack.MaxTokens, Files: GitFiles{Git: gitexec.Runner{Dir: wt.Path}, Rev: wt.Baseline}},
		Prompts:       opts.Prompts,
		Review: review.Options{
			Harness: opts.Harness, Credential: opts.Credential, BaseURL: opts.BaseURL, Model: opts.Model, Timeout: opts.ReviewTimeout,
			StructuredFindings: true,
			ProgressFn:         opts.OnReviewProgress,
		},
		Commands:   opts.Commands,
		GatesFirst: opts.GatesFirst,
		Integrate:  integ != nil,
		Quality:    opts.qualityCheck(exec, impl, status),
		Status:     status,
		OnCheck:    opts.OnCheck,
		Install:    opts.Install,
		OnInstallOutput: func(id, line string) {
			status(iostream.LevelInfo, "install "+id+": "+line)
		},
	}
	if opts.Verbose {
		steps.OnReviewerTree = func(t ReviewerTree) { lg.reviewerTree(status, t) }
	}
	if err := steps.Prepare(ctx); err != nil {
		return rep, fmt.Errorf("set up the implementer's workspace: %w", err)
	}
	steps.continueRun(ctx, &opts, wt, &rec, status)
	// The implementer's last turn goes into the notes.
	lastSummary := ""
	tm := timer{now: time.Now}
	loop, loopPrompt := opts.newLoop(steps, keepSummary(&lastSummary, chain(tm.event, opts.OnEvent)))
	if cont != nil || integ != nil {
		if err := steps.Relay.Push(ctx, []*sidecar.PoolEntry{impl}); err != nil {
			return rep, fmt.Errorf("bring the work already on the branch to the implementer: %w", err)
		}
	}

	// The record is kept once there is work to continue, and again with how
	// the run ended, so a run whose daemon died can still be continued.
	keepRecord(dataDir, rec, status)
	rep.Started = true
	rep.Outcome, err = loop.Run(ctx, steps, loopPrompt)
	rep.Timing = tm.timing
	concludeRun(ctx, steps, wt, &rep, NotesInput{Request: request, Summary: lastSummary, Log: lg.block, Integrate: integ != nil}, status)
	settleTiming(&rep.Timing, steps, runStart, status)
	return rep, err
}

// settleTiming adds what the loop's events do not report to t: the time of the
// gates and the reviews, and the run's total. It warns of the slow calls.
func settleTiming(t *Timing, steps *Sidecars, runStart time.Time, status iostream.StatusFunc) {
	t.Gates, t.Reviews = steps.CheckTimes()
	t.Total = time.Since(runStart)
	for _, w := range t.Warnings() {
		status(iostream.LevelWarn, w)
	}
}

// noPriorWork is what is shown and logged when a continued run finds its branch
// empty.
const noPriorWork = "continuing with no prior work: implementing from the request"

// planFor is the planner a run uses, nil for none. A continued run never plans:
// the plan already exists in the record of the run it continues. An integrate
// run has no plan either: the work is done and the spec is its request.
func planFor(enabled bool, cont *Continuation, integ *IntegrateOptions, p Planner) Planner {
	if !enabled || cont != nil || integ != nil {
		return nil
	}
	return p
}

// keepSummary is an event handler that keeps the summary of the implementer's
// latest turn in last, and passes every event on to next when it is set.
func keepSummary(last *string, next func(Event)) func(Event) {
	return func(e Event) {
		if e.Kind == EventImplemented {
			*last = e.Turn.Summary
		}
		if next != nil {
			next(e)
		}
	}
}

// concludeRun commits the implementer's work and describes it. The commit is
// made before the notes are written, so the implementer can read the change
// from it; its message is rewritten once they are. It sets what the run left
// behind in rep.
func concludeRun(ctx context.Context, steps *Sidecars, wt Worktree, rep *Report, in NotesInput, status iostream.StatusFunc) {
	rep.Committed = commitWork(ctx, steps, wt, Notes{Title: requestTitle(in.Request)}.CommitMessage(rep.RunID, rep.Outcome, ""), status)
	if !rep.Committed {
		return
	}
	rep.Stat, rep.StatErr = workStat(ctx, wt)
	if rep.Stat != "" {
		start := time.Now()
		rep.Notes = describeWork(ctx, steps, wt, *rep, in, status)
		rep.Timing.Notes = time.Since(start)
		if rep.Notes != nil {
			rep.Timing.NotesContext = rep.Notes.Context
		}
	}
}

// describeWork writes the notes of the committed work and rewrites the
// commit's message with them. When the notes cannot be written the commit
// carries the fallback, with a warning saying why.
func describeWork(ctx context.Context, steps *Sidecars, wt Worktree, rep Report, in NotesInput, status iostream.StatusFunc) *Notes {
	in.Outcome, in.Baseline, in.Plan = rep.Outcome, wt.Baseline, rep.Outcome.Plan
	var err error
	if in.DiffStat, err = diffStat(context.WithoutCancel(ctx), wt); err != nil {
		status(iostream.LevelWarn, fmt.Sprintf("could not read the diff stat for the PR notes: %v", err))
	}
	status(iostream.LevelStep, "writing PR notes…")
	notes, err := steps.WriteNotes(ctx, in)
	notes.Skipped = rep.Outcome.Skipped()
	if q := rep.Outcome.Quality(); q != nil {
		notes.Quality = q.Line()
	}
	notes.QualityNotMeasured = rep.Outcome.QualityNotMeasured()
	if err != nil {
		status(iostream.LevelWarn, fmt.Sprintf("PR notes were not generated: %v", err))
	} else {
		status(iostream.LevelDone, "PR notes written")
	}
	if in.Integrate {
		return &notes
	}
	amendCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), cleanupTimeout)
	defer cancel()
	if err := wt.Amend(amendCtx, notes.CommitMessage(rep.RunID, rep.Outcome, rep.Stat)); err != nil {
		status(iostream.LevelWarn, fmt.Sprintf("could not put the notes in the commit message: %v", err))
	}
	return &notes
}

// diffStat is the committed work's per-file diff stat, after the run has ended.
func diffStat(ctx context.Context, wt Worktree) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, cleanupTimeout)
	defer cancel()
	return wt.DiffStat(ctx)
}

// keepRecord saves the run's record, so a later run can continue it. A run
// that cannot save one still runs; it just cannot be continued.
func keepRecord(dataDir string, rec Record, status iostream.StatusFunc) {
	if err := saveRecord(dataDir, rec); err != nil {
		status(iostream.LevelWarn, fmt.Sprintf("this run cannot be continued later: %v", err))
	}
}

// keepEndedRecord saves rec with how the run ended and the notes it wrote.
func keepEndedRecord(dataDir string, rec Record, rep Report, status iostream.StatusFunc) {
	rec.Result, rec.Rounds, rec.Notes, rec.Plan = rep.Outcome.Result, rep.Outcome.Rounds, rep.Notes, rep.Outcome.Plan
	rec.ContinuedFromEmpty = rep.Outcome.FromEmpty
	if p := rep.Outcome.Plan; p != nil {
		if pk := p.ContextPack(); !pk.Empty() {
			rec.Pack = &pk
		}
	}
	keepRecord(dataDir, rec, status)
}

// resolved forgets the conflicts a continued run was asked to resolve in the
// record it continued, now that the run has ended. It is a no-op on a nil
// Continuation.
func (c *Continuation) resolved(dataDir string, status iostream.StatusFunc) {
	if c == nil || len(c.From.Conflicts) == 0 {
		return
	}
	from := c.From
	from.Conflicts = nil
	keepRecord(dataDir, from, status)
}

// restoreWorktree puts the work's files back in a continued run's worktree
// when the run ends before its sidecars were synced from the baseline's.
func restoreWorktree(ctx context.Context, wt Worktree, status iostream.StatusFunc) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), cleanupTimeout)
	defer cancel()
	if err := wt.restoreWork(ctx); err != nil {
		status(iostream.LevelWarn, fmt.Sprintf("%v; run git read-tree -u --reset HEAD in %s to get them back", err, wt.Path))
	}
}

// removeWorktree removes the worktree of a run that ended before the
// implementer started, and reports whether it did.
func removeWorktree(ctx context.Context, root string, wt Worktree, status iostream.StatusFunc) bool {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), cleanupTimeout)
	defer cancel()
	if err := wt.Remove(ctx, root); err != nil {
		status(iostream.LevelWarn, fmt.Sprintf("could not remove the run's worktree %s: %v", wt.Path, err))
		return false
	}
	return true
}

// workStat summarizes the committed work, after the run has ended.
func workStat(ctx context.Context, wt Worktree) (string, error) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), cleanupTimeout)
	defer cancel()
	return wt.Stat(ctx)
}

// commitWork brings the implementer's last work into the worktree and commits
// it on the run's branch. It gets one deadline for the pull and a fresh one
// for the commit, so a pull that hangs cannot use up the commit's time. What
// an earlier round pulled is committed even if the last pull fails.
func commitWork(ctx context.Context, steps *Sidecars, wt Worktree, message string, status iostream.StatusFunc) bool {
	base := context.WithoutCancel(ctx)
	pullCtx, cancelPull := context.WithTimeout(base, pullTimeout)
	err := steps.Pull(pullCtx)
	cancelPull()
	if err != nil {
		status(iostream.LevelWarn, fmt.Sprintf("could not bring back the implementer's last changes: %v", err))
	}
	ctx, cancel := context.WithTimeout(base, cleanupTimeout)
	defer cancel()
	if _, err := wt.Commit(ctx, message); err != nil {
		status(iostream.LevelWarn, fmt.Sprintf("could not commit the work in %s: %v", wt.Path, err))
		return false
	}
	return true
}

// closePool deletes the run's sidecars, or with keep leaves them running and
// returns their IDs. A kept pool stays in its state file, which is how the
// dashboard and 'smelt sidecar' still find its sidecars; no later run reuses
// it, since the name is the run's own.
func closePool(ctx context.Context, pool *sidecar.Pool, keep bool, status iostream.StatusFunc) []string {
	if !keep {
		ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), cleanupTimeout)
		defer cancel()
		if err := pool.Destroy(ctx); err != nil {
			status(iostream.LevelWarn, fmt.Sprintf("could not destroy pool: %v", err))
		}
		return nil
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), poolCloseTimeout)
	defer cancel()
	pool.Close(ctx)
	ids := pool.IDs()
	status(iostream.LevelInfo, "kept sidecars: "+strings.Join(ids, " "))
	return ids
}

// qualityCheck is the run's quality check on the implementer's sidecar, nil
// when the run does not measure quality.
func (o *RunOptions) qualityCheck(exec review.Execer, impl *sidecar.PoolEntry, status iostream.StatusFunc) *Quality {
	if o.Quality == nil {
		return nil
	}
	return &Quality{Config: *o.Quality, Exec: exec, Entry: impl,
		Warn: func(msg string) { status(iostream.LevelWarn, msg) }}
}

// harnessName is the agent the run uses, Claude Code when none is set.
func (o *RunOptions) harnessName() string {
	if o.Harness == nil {
		return config.HarnessClaude
	}
	return string(o.Harness.Name())
}

// summary describes the run's configuration; an explicit Backend means docker.
func (o *RunOptions) summary() string {
	f := summaryFields{Image: o.Image, Attempts: o.Attempts, Reviewers: o.Reviewers, Model: o.Model}
	if o.Backend != nil {
		f.Backend = config.BackendDocker
	}
	f.Harness = o.harnessName()
	return runSummary(f)
}

// summaryFields are what runSummary describes.
type summaryFields struct {
	Backend, Image, Harness, Model string
	Attempts, Reviewers            int
}

// runSummary is the one-line description of a run's configuration. Empty
// fields show a readable default, and nothing secret is included.
func runSummary(f summaryFields) string {
	return fmt.Sprintf("Factory run: backend=%s image=%s attempts=%d reviewers=%d harness=%s model=%s",
		orDefault(f.Backend, config.BackendCircleCI), orDefault(f.Image, "default"), f.Attempts, f.Reviewers,
		orDefault(f.Harness, config.HarnessClaude), orDefault(f.Model, "default"))
}

func orDefault(s, def string) string {
	if s == "" {
		return def
	}
	return s
}

// announceFrom tells the developer what a run started with --from ignores: a
// failed fetch, and uncommitted changes in their checkout. It says nothing for
// a run that started from the checkout.
func announceFrom(ctx context.Context, root string, wt Worktree, status iostream.StatusFunc) {
	if wt.From == nil {
		return
	}
	if wt.FromWarning != "" {
		status(iostream.LevelWarn, wt.FromWarning)
	}
	// --no-optional-locks keeps the check from refreshing the index.
	out, err := (gitexec.Runner{Dir: root}).Output(ctx, "--no-optional-locks", "status", "--porcelain")
	if err != nil || strings.TrimSpace(string(out)) == "" {
		return
	}
	status(iostream.LevelInfo, fmt.Sprintf("Starting from %s (%s); your uncommitted changes are not part of this run.",
		wt.From.Ref, wt.From.Commit[:min(len(wt.From.Commit), 7)]))
}

// startWorktree is the run's worktree: new, or the continued run's reopened.
func startWorktree(ctx context.Context, opts RunOptions, runID, dataDir string, author config.CommitAuthor) (Worktree, error) {
	cont := opts.Continue
	if cont != nil {
		opts.Status(iostream.LevelInfo, fmt.Sprintf("Continuing run %s on %s", cont.From.RunID, cont.From.Branch))
		wt, err := openWorktree(ctx, opts.Root, cont.From, runID, author)
		if err != nil {
			return Worktree{}, fmt.Errorf("open the worktree of run %s: %w", cont.From.RunID, err)
		}
		return wt, nil
	}
	wt, err := CreateWorktree(ctx, WorktreeOptions{
		Root: opts.Root, Path: filepath.Join(dataDir, "factory", runID),
		RunID: runID, Prompt: opts.Prompt, Author: author, From: opts.From, Against: opts.againstRef(),
	})
	if err != nil {
		return Worktree{}, fmt.Errorf("create the run's worktree: %w", err)
	}
	announceFrom(ctx, opts.Root, wt, opts.Status)
	return wt, nil
}

// againstRef is the ref an integrate run compares its branch with, "" for any
// other run.
func (o *RunOptions) againstRef() string {
	if o.Integrate == nil {
		return ""
	}
	return o.Integrate.Against
}

// checkIntegrate refuses the combinations of integrate options that cannot run.
func (o *RunOptions) checkIntegrate() error {
	switch {
	case o.Integrate == nil:
		return nil
	case o.Continue != nil:
		return errors.New("an integrate run checks a branch against a spec, so it cannot continue another run")
	case o.From == "":
		return errors.New("an integrate run needs the branch to check, from From")
	case o.Integrate.Against == "":
		return errors.New("an integrate run needs a ref to compare the branch with")
	}
	return nil
}

// newLoop is the loop a run drives, and the prompt it starts with. onEvent
// hears every event, and the status function hears of a continued run that
// found no work.
func (o *RunOptions) newLoop(steps *Sidecars, onEvent func(Event)) (Loop, string) {
	loop := Loop{Attempts: o.Attempts, OnEvent: onEvent, Planner: planFor(o.Plan, o.Continue, o.Integrate, steps), CheckHints: o.Pack.Enabled, FreshRounds: o.Rounds != "resume"}
	if o.Status != nil {
		loop.OnEvent = func(e Event) {
			if e.Kind == EventNoPriorWork {
				o.Status(iostream.LevelInfo, noPriorWork)
			}
			onEvent(e)
		}
	}
	prompt := o.Prompt
	if c := o.Continue; c != nil {
		loop.CheckFirst, loop.EmptyPrompt = c.checkFirst(), c.emptyPrompt()
	}
	if o.Integrate != nil {
		// The spec is the reviews' request; the implementer is only sent what
		// fails, behind the fix lead.
		loop.Attempts, loop.CheckFirst, prompt = IntegrateAttempts, true, integrateFixLead
	}
	return loop, prompt
}

// markRecord records what kind of run this is: whose request it continues, or
// that it integrates a branch.
func (o *RunOptions) markRecord(rec *Record) {
	if c := o.Continue; c != nil {
		rec.Prompt, rec.ContinuesRunID = c.From.Prompt, c.From.RunID
	}
	if o.Integrate != nil {
		rec.Mode, rec.Against = ModeIntegrate, o.Integrate.Against
	}
}
