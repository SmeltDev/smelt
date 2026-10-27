package server

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/smeltdev/smelt/internal/config"
	"github.com/smeltdev/smelt/internal/factory"
	"github.com/smeltdev/smelt/internal/harness"
	"github.com/smeltdev/smelt/internal/harness/all"
	"github.com/smeltdev/smelt/internal/harness/status"
	"github.com/smeltdev/smelt/internal/iostream"
	"github.com/smeltdev/smelt/internal/quality"
	"github.com/smeltdev/smelt/internal/review"
	"github.com/smeltdev/smelt/internal/sidecar"
	"github.com/smeltdev/smelt/internal/smeltd"
)

// DefaultAttempts is how many rounds a factory session checks at most when the
// request does not say.
const DefaultAttempts = 3

// harnessAuth is the credential setup of the named harness.
func (d *daemon) harnessAuth(name harness.Name) HarnessAuth {
	switch name {
	case harness.Claude:
		// Claude's credential is the default below.
	case harness.Codex:
		return d.rcfg.Codex
	case harness.Grok:
		return d.rcfg.Grok
	}
	return HarnessAuth{Credential: d.rcfg.Credential, BaseURL: d.rcfg.BaseURL, AuthError: d.rcfg.AuthError}
}

// factoryAuthError explains why a factory session on the given backend with the
// named harness cannot run, or "" when it can. Every backend needs the
// harness's credential; only the CircleCI backend needs a CircleCI client.
func (d *daemon) factoryAuthError(backend string, name harness.Name) string {
	auth := d.harnessAuth(name)
	if auth.Credential.Value == "" && auth.Refresh == nil {
		if auth.AuthError != "" {
			return auth.AuthError
		}
		return fmt.Sprintf("no %s credential configured for the smelt daemon — factory runs unavailable (run: smelt auth login %s)", name, name)
	}
	if config.BackendOrDefault(backend) == smeltd.BackendDocker {
		return ""
	}
	if d.rcfg.RunFactory == nil && d.circleCIClient() == nil {
		return "not authenticated to CircleCI — factory runs unavailable (run: smelt auth login)"
	}
	return ""
}

// anyFactoryAuthError is what the snapshot reports: empty when some harness can
// run, otherwise why the first, Claude, cannot.
func (d *daemon) anyFactoryAuthError(backend string) string {
	first := ""
	for _, h := range all.All() {
		msg := d.factoryAuthError(backend, h.Name())
		if msg == "" {
			return ""
		}
		if first == "" {
			first = msg
		}
	}
	return first
}

// factoryTarget is the org and image a factory request's sidecars use: the
// request's own, filled in from the project config. The Docker backend needs no
// CircleCI org, and an empty image is fine for it: it has its own default.
func factoryTarget(root string, cfg *config.ProjectConfig, req smeltd.FactoryRequest) (orgID, image string, err error) {
	orgID, image = req.OrgID, req.Image
	if req.Backend == smeltd.BackendDocker {
		// validation.sidecarImage is a CircleCI snapshot ID, not a Docker image.
		if image == "" && cfg.Factory != nil {
			image = cfg.Factory.Image
		}
		return orgID, image, nil
	}
	if orgID != "" && image != "" {
		return orgID, image, nil
	}
	cfgOrg, cfgImage, err := poolTarget(root, cfg)
	if err != nil && orgID == "" {
		return "", "", err
	}
	if orgID == "" {
		orgID = cfgOrg
	}
	if image == "" {
		image = cfgImage
	}
	return orgID, image, nil
}

// factoryBackend builds the backend a factory request names. Nil means the run
// uses the CircleCI backend it derives from the daemon's client. It is built
// when the request arrives so a daemon that cannot reach the Docker socket
// fails the request rather than the background run.
func factoryBackend(name, root string, cfg *config.ProjectConfig, machineRuns int) (sidecar.Backend, error) {
	if name != smeltd.BackendDocker {
		return nil, nil
	}
	return factory.DockerBackend(root, cfg, machineRuns)
}

// cpusDescription is the CPU share a backend gives run containers, "" when it
// has none to report.
func cpusDescription(backend sidecar.Backend) string {
	d, ok := backend.(interface{ CPUsDescription(context.Context) string })
	if !ok {
		return ""
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	return d.CPUsDescription(ctx)
}

// recordRequest copies what the request says about the run onto its record and
// returns the model to use.
func recordRequest(record *smeltd.FactoryRun, req smeltd.FactoryRequest, cfg *config.ProjectConfig, agent harness.Harness) (string, error) {
	model := req.Model
	if model == "" {
		model = cfg.Factory.ModelFor(string(agent.Name()))
	}
	record.Harness, record.Model = string(agent.Name()), model
	record.KitOutdated = req.KitWarning != ""
	if req.Sandbox != nil {
		if err := checkSandbox(*req.Sandbox); err != nil {
			return "", err
		}
		sandbox := *req.Sandbox
		record.Sandbox = &sandbox
	}
	return model, nil
}

// startFactory validates a request, records a factory session and starts it in
// the background. It returns as soon as the session is accepted.
func (d *daemon) startFactory(req smeltd.FactoryRequest) (smeltd.Session, error) {
	if !config.ValidBackend(req.Backend) {
		return smeltd.Session{}, apiErr(http.StatusBadRequest, "unknown backend %q (want %s or %s)",
			req.Backend, smeltd.BackendCircleCI, smeltd.BackendDocker)
	}
	req.Backend = config.BackendOrDefault(req.Backend)
	if req.Harness == "" {
		req.Harness = config.HarnessClaude
	}
	agent, err := all.ByName(harness.Name(req.Harness))
	if err != nil {
		return smeltd.Session{}, apiErr(http.StatusBadRequest, "%v", err)
	}
	prompt := strings.TrimSpace(req.Prompt)
	if prompt == "" && req.Continue == "" {
		return smeltd.Session{}, apiErr(http.StatusBadRequest, "prompt required")
	}
	if req.From != "" && req.Continue != "" {
		return smeltd.Session{}, apiErr(http.StatusBadRequest, "from cannot be combined with continue: a continued run keeps its base")
	}
	if err := checkIntegrateRequest(req); err != nil {
		return smeltd.Session{}, err
	}
	ps := d.lookupProject(req.ProjectRoot)
	if ps == nil {
		ps = d.adoptProject(req.ProjectRoot)
	}
	if ps == nil {
		return smeltd.Session{}, apiErr(http.StatusNotFound, "the smelt daemon is not tracking %q, and it is not the top of a git repository", req.ProjectRoot)
	}
	var cont *factory.Continuation
	record := &smeltd.FactoryRun{Prompt: prompt}
	if req.Continue != "" {
		from, err := factory.LoadRecord(ps.root, factory.ParseRunID(req.Continue))
		if errors.Is(err, factory.ErrNoRecord) {
			return smeltd.Session{}, apiErr(http.StatusNotFound, "%v", err)
		}
		if err != nil {
			return smeltd.Session{}, apiErr(http.StatusBadRequest, "%v", err)
		}
		cont = &factory.Continuation{From: from, Guidance: prompt}
		record = &smeltd.FactoryRun{Prompt: from.Prompt, Guidance: prompt, ContinuesRunID: from.RunID}
	}
	integrate, err := integrateOf(req, ps.root)
	if err != nil {
		return smeltd.Session{}, err
	}
	if integrate != nil {
		record.Mode, record.Against = factory.ModeIntegrate, integrate.Against
	}
	cfg, err := loadProjectConfig(ps.root)
	if err != nil {
		return smeltd.Session{}, apiErr(http.StatusBadRequest, "%v", err)
	}
	agents, err := d.resolveFactoryHarnesses(req, cfg, agent)
	if err != nil {
		return smeltd.Session{}, err
	}
	auth := agents.Auth
	dir, err := sessionPromptsDir(ps.root, req.ReviewsDir)
	if err != nil {
		return smeltd.Session{}, apiErr(http.StatusBadRequest, "%v", err)
	}
	prompts, commands, err := factory.LoadChecks(dir, req.ReviewsDir == "", cfg, req.NoValidate)
	if err != nil {
		return smeltd.Session{}, apiErr(http.StatusBadRequest, "%v", err)
	}
	orgID, image, err := factoryTarget(ps.root, cfg, req)
	if err != nil {
		return smeltd.Session{}, apiErr(http.StatusBadRequest, "%v", err)
	}
	backend, err := factoryBackend(req.Backend, ps.root, cfg, d.sessions.limits.MaxRuns)
	if err != nil {
		return smeltd.Session{}, apiErr(http.StatusServiceUnavailable, "%v", err)
	}
	cpus := cpusDescription(backend)
	record.CPUs = cpus
	author, err := cfg.Factory.CommitAuthor()
	if err != nil {
		return smeltd.Session{}, apiErr(http.StatusBadRequest, "factory.author: %v", err)
	}
	// The daemon's working directory is not the caller's, so a relative path
	// would put the log somewhere the caller did not mean.
	if err := checkFactoryLog(req.Log); err != nil {
		return smeltd.Session{}, err
	}
	attempts := factoryAttempts(req)
	record.Attempts = attempts
	model, err := recordRequest(record, req, cfg, agent)
	if err != nil {
		return smeltd.Session{}, err
	}

	entry, ctx, busy := d.sessions.add(smeltd.Session{
		ProjectRoot: ps.root,
		Branch:      currentBranch(ps.root),
		HeadSHA:     headRef(ps.root),
		Factory:     record,
	}, cfg.Factory.ConcurrentRuns())
	if busy != nil {
		return smeltd.Session{}, admissionRefusal(busy)
	}
	opts := factory.RunOptions{
		Root:                    ps.root,
		Prompt:                  prompt,
		Continue:                cont,
		From:                    req.From,
		Integrate:               integrate,
		Attempts:                attempts,
		Rounds:                  cfg.Factory.RoundPolicy(),
		ContextWindow:           cfg.Factory.ImplementContextWindow(),
		Reviewers:               factory.ReviewerCount(req.Reviewers, prompts),
		Prompts:                 prompts,
		Commands:                commands,
		Plan:                    !req.NoPlan && cfg.Factory.PlanEnabled(),
		Pack:                    packOptions(req, cfg),
		GatesFirst:              cfg.Factory.GatesFirstEnabled(),
		FocusedRechecks:         cfg.Factory.FocusedRechecksEnabled(),
		Quality:                 factory.QualityConfig(cfg.Factory),
		Client:                  d.factoryClient(req.Backend),
		OrgID:                   orgID,
		Image:                   image,
		Backend:                 backend,
		CPUs:                    cpus,
		Install:                 imageInstallCommand(req.Backend, cfg),
		Author:                  author,
		KeepSidecars:            req.KeepSidecars,
		Harness:                 agent,
		Fallback:                agents.Fallback,
		InitialFailure:          agents.InitialFailure,
		SwapAtUtilization:       cfg.Factory.SwapThreshold(),
		CapacityWait:            cfg.Factory.CapacityWaitDuration(),
		Credential:              auth.Credential,
		BaseURL:                 auth.BaseURL,
		Model:                   model,
		ImplementerInstructions: req.ImplementerInstructions,
		ImplementTimeout:        time.Duration(req.ImplementTimeoutSeconds) * time.Second,
		ReviewTimeout:           time.Duration(req.ReviewTimeoutSeconds) * time.Second,
		Warnings:                warnings(req),
		Log:                     req.Log,
		Verbose:                 req.Verbose,
	}
	opts.Syncs = &factory.SyncQueue{}
	entry.mu.Lock()
	entry.syncs = opts.Syncs
	entry.mu.Unlock()
	go d.executeFactory(ctx, entry, opts)
	return entry.snapshot(), nil
}

// admissionRefusal maps the session store's refusal to the API's: a draining
// daemon or a full machine is a retryable 503 carrying its code, a full project
// a 409 with its code, anything else a 409.
func admissionRefusal(err error) *apiError {
	var handover *handoverError
	if errors.As(err, &handover) {
		return &apiError{status: http.StatusServiceUnavailable, msg: handover.Error(), code: smeltd.CodeDaemonHandover}
	}
	var drain *drainingError
	if errors.As(err, &drain) {
		return &apiError{status: http.StatusServiceUnavailable, msg: drain.Error(), code: smeltd.CodeDaemonDraining, active: drain.active}
	}
	var busy *busyError
	if errors.As(err, &busy) {
		status := http.StatusConflict
		if busy.code == smeltd.CodeDaemonBusy {
			status = http.StatusServiceUnavailable
		}
		return &apiError{status: status, msg: busy.Error(), code: busy.code, active: busy.active}
	}
	return apiErr(http.StatusConflict, "%s", err)
}

// executeFactory runs a factory session and settles it. It always closes
// entry.done.
func (d *daemon) executeFactory(ctx context.Context, entry *sessionEntry, opts factory.RunOptions) {
	// Deferred first so it runs last, after done has closed.
	defer d.sessions.ended()
	defer close(entry.done)
	defer entry.cancel()

	rec := &factoryRecorder{entry: entry, prompts: opts.Prompts, rounds: map[int]int{}, harness: opts.Harness.Name(), integrate: opts.Integrate != nil}
	root := opts.Root
	opts.Status = func(level iostream.Level, msg string) {
		log.Printf("smeltd: factory (%s): %s", root, msg)
		rec.progress(level, msg)
	}
	// execerFor wires each command into the dashboard's live output pane via the
	// CircleCI client. The Docker backend runs commands itself, so it uses the
	// backend's own Exec (review.ClientExec); its live output pane lands in a
	// later phase.
	if opts.Backend == nil {
		opts.Exec = d.execerFor(root, rec.attribute)
	} else {
		opts.Exec = review.ClientExec
	}
	opts.OnStart = rec.started
	opts.OnEvent = rec.event
	opts.OnActivity = rec.activity
	opts.OnPack = rec.setPack
	opts.OnReviewProgress = rec.reviewProgress
	opts.OnCheck = rec.checked

	run := d.rcfg.RunFactory
	if run == nil {
		run = factory.Run
	}
	rep, err := run(ctx, opts)
	rec.finish(rep, err)
	d.settleSession(entry, err)
}

// checkIntegrateRequest refuses an integrate request that cannot run, and
// --against without --integrate, which would be silently ignored.
func checkIntegrateRequest(req smeltd.FactoryRequest) error {
	switch {
	case !req.Integrate && req.Against != "":
		return apiErr(http.StatusBadRequest, "against needs integrate")
	case !req.Integrate:
		return nil
	case req.Continue != "":
		return apiErr(http.StatusBadRequest, "integrate cannot be combined with continue: it checks a branch against a spec")
	case req.From == "":
		return apiErr(http.StatusBadRequest, "integrate needs from: the branch to check")
	}
	return nil
}

// checkFactoryLog refuses a relative log path.
func checkFactoryLog(log string) error {
	if log != "" && log != factory.LogDefault && !filepath.IsAbs(log) {
		return apiErr(http.StatusBadRequest, "log must be an absolute path, got %q", log)
	}
	return nil
}

// integrateOf is the integrate options of a request, nil for any other run.
// Against defaults to the project's default branch.
func integrateOf(req smeltd.FactoryRequest, root string) (*factory.IntegrateOptions, error) {
	if !req.Integrate {
		return nil, nil
	}
	against, err := factory.ResolveAgainst(root, req.Against)
	if err != nil {
		return nil, apiErr(http.StatusBadRequest, "%v", err)
	}
	return &factory.IntegrateOptions{Against: against}, nil
}

// factoryAttempts is the rounds a request checks. An integrate run's are
// fixed whatever it says: the broad check, one fix turn and one re-check.
func factoryAttempts(req smeltd.FactoryRequest) int {
	switch {
	case req.Integrate:
		return factory.IntegrateAttempts
	case req.Attempts <= 0:
		return DefaultAttempts
	}
	return req.Attempts
}

// integrateRoundKind is what round n of an integrate run is: the broad check,
// then, after the one fix turn, the focused re-check.
func integrateRoundKind(n int) smeltd.RoundKind {
	if n == 1 {
		return smeltd.RoundBroad
	}
	return smeltd.RoundRecheck
}

// factoryResultNote says how a factory loop ended, for its stage.
func factoryResultNote(o factory.Outcome) string {
	switch o.Result {
	case factory.ResultFailed, factory.ResultUnavailable:
		return o.Reason
	case factory.ResultPassed:
		return fmt.Sprintf("all checks passed after %d round(s)", o.Rounds)
	case factory.ResultNoChange:
		return "the implementer made no changes"
	case factory.ResultBlocked:
		return "the implementer is blocked: " + o.Blocked
	case factory.ResultStuck:
		return fmt.Sprintf("the implementer stopped changing the code after round %d, with checks still failing", o.Rounds)
	case factory.ResultExhausted:
		return fmt.Sprintf("checks still failed after %d round(s)", o.Rounds)
	}
	return string(o.Result)
}

// factoryRecorder keeps a factory session's record as its run goes. Each
// factory round is a round of the record: its implementer turn, then its
// reviews and validation commands.
type factoryRecorder struct {
	entry   *sessionEntry
	prompts []review.Prompt

	// rounds maps a factory round to the record's round index, and cur is the
	// round in progress. Both are only touched from the loop's goroutine;
	// attribute reads cur through the entry's lock.
	rounds map[int]int
	cur    int
	// activityRound is the factory round the implementer is in; guarded by the
	// entry's lock, as activity runs on the exec goroutine.
	activityRound int

	// harness is the agent the run uses.
	harness harness.Name
	// integrate is set for an integrate run, whose rounds are broad, then a
	// fix and a re-check.
	integrate bool
}

func (r *factoryRecorder) started(runID string, wt factory.Worktree) {
	e := r.entry
	e.mu.Lock()
	defer e.mu.Unlock()
	f := e.s.Factory
	f.RunID, f.Worktree, f.Branch, f.Baseline, f.Head = runID, wt.Path, wt.Branch, wt.Baseline, wt.Head
	f.BaseBranch, f.DefaultBranch, f.PRBase = wt.BaseBranch, wt.DefaultBranch, wt.PRBase
	if wt.From != nil {
		f.From = &smeltd.FromRef{Ref: wt.From.Ref, Commit: wt.From.Commit}
	}
	if wt.UpdatedFrom != nil {
		f.UpdatedFrom = &smeltd.UpdatedFrom{Branch: wt.UpdatedFrom.Branch, Commit: wt.UpdatedFrom.Commit}
	}
	// The work is on the run's branch, not the one checked out.
	e.s.Branch = wt.Branch
}

// progress records something the run said. It is called from the pool's and
// the relay's goroutines as well as the loop's.
func (r *factoryRecorder) progress(level iostream.Level, msg string) {
	e := r.entry
	e.mu.Lock()
	defer e.mu.Unlock()
	addFeedLine(&e.s.Factory.Progress, level, msg)
}

// activity records an implementer tool call, or sets the outcome of the call
// with the same ID when it is an Update. It runs on
// the exec goroutine.
func (r *factoryRecorder) activity(a harness.Activity) {
	if a.Tool == "" || a.Tool == harness.ReasoningTool {
		return
	}
	e := r.entry
	e.mu.Lock()
	defer e.mu.Unlock()
	f := e.s.Factory
	rec := smeltd.ToolActivity{Round: r.activityRound, ID: a.ID, Tool: a.Tool, Detail: a.Detail,
		Outcome: string(a.Outcome), ExitCode: a.ExitCode, Error: a.Error}
	if a.Update {
		for i := len(f.Activity) - 1; i >= 0; i-- {
			if got := f.Activity[i]; got.ID != "" && got.ID == rec.ID {
				f.Activity[i] = rec
				return
			}
		}
		return
	}
	f.Activity = append(f.Activity, rec)
	if over := len(f.Activity) - smeltd.MaxToolActivity; over > 0 {
		f.Activity = slices.Delete(f.Activity, 0, over)
	}
}

// round returns the record's index for a factory round, beginning it if it
// has not begun: a round that only checks again has no implementer turn.
func (r *factoryRecorder) round(n int) int {
	if ridx, ok := r.rounds[n]; ok {
		return ridx
	}
	ridx := r.entry.beginRound(r.prompts)
	r.rounds[n] = ridx
	r.entry.mu.Lock()
	if r.integrate {
		r.entry.s.Rounds[ridx].Kind = integrateRoundKind(n)
	}
	r.cur = ridx
	r.entry.mu.Unlock()
	return ridx
}

func (r *factoryRecorder) event(ev factory.Event) {
	if r.beforeRound(ev) {
		return
	}

	if ev.Kind == factory.EventImplemented {
		for _, item := range factory.UnverifiedItems(ev.Turn.Summary) {
			r.progress(iostream.LevelInfo, "unverified: "+item)
		}
	}
	ridx := r.round(ev.Round)
	e := r.entry
	e.mu.Lock()
	defer e.mu.Unlock()
	round := &e.s.Rounds[ridx]
	switch ev.Kind {
	case factory.EventPlanning, factory.EventPlanReviewed, factory.EventNoPriorWork, factory.EventHarnessSwap, factory.EventTurnAttempt, factory.EventSyncDropped:
		// Recorded above, or kept by the sync queue.
	case factory.EventSynced:
		r.syncedLocked(ev)
	case factory.EventImplementing:
		round.State = smeltd.RoundImplementing
		r.activityRound = ev.Round
		round.Implement = &smeltd.RoundImplement{State: smeltd.ImplementRunning, Fix: r.integrate}
	case factory.EventImplemented:
		r.implementedLocked(round, ev)
	case factory.EventCollected:
		if impl := round.Implement; impl != nil {
			impl.Stat = ev.Change.Stat
			if ev.Change.Empty() {
				impl.State = smeltd.ImplementEmpty
			}
		}
	case factory.EventChecking:
		round.State = smeltd.RoundChecking
	case factory.EventChecked:
		r.checkedLocked(ridx, ev)
	}
}

// syncedLocked moves the session baseline after a successful between-round sync.
func (r *factoryRecorder) syncedLocked(ev factory.Event) {
	if ev.Worktree != nil && ev.Sync != nil && ev.Sync.State == factory.SyncDone {
		updateFactoryBaseline(r.entry.s.Factory, *ev.Worktree)
	}
}

// implementedLocked records the completed implementer turn. The entry lock is held.
func (r *factoryRecorder) implementedLocked(round *smeltd.Round, ev factory.Event) {
	if impl := round.Implement; impl != nil {
		impl.State = smeltd.ImplementApplied
		impl.DurationMS = ev.Turn.Duration.Milliseconds()
		impl.CostUSD = ev.Turn.CostUSD
		impl.Harness, impl.Model = string(ev.Turn.Harness), ev.Turn.Model
		round.Harness = string(ev.Turn.Harness)
		impl.InputTokens, impl.CachedTokens, impl.OutputTokens = ev.Turn.InputTokens, ev.Turn.CachedTokens, ev.Turn.OutputTokens
		impl.ContextPeak, impl.ModelCalls = ev.Turn.ContextPeak, ev.Turn.ModelCalls
		impl.RateLimit = ev.Turn.RateLimit
		r.recordHarnessRateLimit(ev.Turn.Harness, ev.Turn.RateLimit)
		impl.Summary = ev.Turn.Summary
		impl.Unproven = ev.Turn.Unproven
	}
}

// recordChecksLocked stores what a round's checks say about the run as a whole:
// the tests they skipped and the quality they measured.
func recordChecksLocked(f *smeltd.FactoryRun, checks []factory.Check) {
	out := factory.Outcome{Checks: checks}
	f.SkippedTests = out.Skipped()
	f.Quality = wireQualityOutcome(out, f.Quality)
}

// wireQualityOutcome is the status JSON's quality for out's checks: the
// measurement, or why there is none. It keeps prev when the checks say nothing.
func wireQualityOutcome(out factory.Outcome, prev *smeltd.FactoryQuality) *smeltd.FactoryQuality {
	if q := out.Quality(); q != nil {
		return wireQuality(q)
	}
	if reason := out.QualityNotMeasured(); reason != "" {
		return &smeltd.FactoryQuality{Measured: false, Reason: reason}
	}
	return prev
}

// planReviewed records the settled plan. It reads as a line of the run's
// progress, so a follower sees the plan before the first turn.
func (r *factoryRecorder) planReviewed(ev factory.Event) {
	msg := "plan reviewed"
	if n := len(ev.Findings); n > 0 {
		msg += fmt.Sprintf(" (%d finding(s) still open, sent with the plan)", n)
	}
	r.progress(iostream.LevelDone, msg)
	e := r.entry
	e.mu.Lock()
	defer e.mu.Unlock()
	e.s.Factory.Plan = wirePlan(ev.Plan)
	if ev.Plan != nil {
		e.s.Factory.Pack = wirePack(ev.Plan.ContextPack())
	}
}

// setPack records the pack a continued run reuses.
func (r *factoryRecorder) setPack(p factory.Pack) {
	e := r.entry
	e.mu.Lock()
	defer e.mu.Unlock()
	e.s.Factory.Pack = wirePack(p)
}

// packOptions is the context pack a request asks for.
func packOptions(req smeltd.FactoryRequest, cfg *config.ProjectConfig) factory.PackOptions {
	return factory.PackOptions{Enabled: !req.NoPack && cfg.Factory.PackEnabled(), MaxTokens: cfg.Factory.PackMaxTokens()}
}

// wirePack is a pack as the session API carries it, nil when the run has none.
func wirePack(p factory.Pack) *smeltd.Pack {
	if p.Empty() && len(p.Dropped) == 0 && len(p.RejectedHints) == 0 {
		return nil
	}
	out := &smeltd.Pack{Entries: []smeltd.PackEntry{}, Dropped: []smeltd.PackDropped{}, RejectedHints: []smeltd.RejectedHint{}, Tokens: p.Tokens}
	for _, e := range p.Entries {
		out.Entries = append(out.Entries, smeltd.PackEntry{
			Kind: string(e.Kind), Path: e.Path, Start: e.Start, End: e.End, Reason: e.Reason, Priority: e.Priority, Inlined: e.Excerpt != "",
		})
	}
	for _, d := range p.Dropped {
		out.Dropped = append(out.Dropped, smeltd.PackDropped(d))
	}
	for _, h := range p.RejectedHints {
		out.RejectedHints = append(out.RejectedHints, smeltd.RejectedHint(h))
	}
	return out
}

// wirePlan is a plan as the session API carries it.
func wirePlan(p *factory.Plan) *smeltd.Plan {
	if p == nil {
		return nil
	}
	out := &smeltd.Plan{Summary: p.Summary, Risks: p.Risks, Questions: p.Questions, CannotVerifyHere: p.CannotVerifyHere}
	for _, f := range p.Files {
		out.Files = append(out.Files, smeltd.PlanFile(f))
	}
	for _, a := range p.Acceptance {
		out.Acceptance = append(out.Acceptance, smeltd.PlanItem(a))
	}
	return out
}

// recordReviewsLocked stores the findings of a round's reviews.
func (r *factoryRecorder) recordReviewsLocked(ridx int, checks []factory.Check) {
	e := r.entry
	rd := &e.details[ridx]
	rd.Results = rd.Results[:0]
	var all []review.Finding
	for _, c := range checks {
		if c.Kind != factory.KindReview {
			continue
		}
		name := c.Harness
		if name == "" {
			name = r.harness
		}
		res := smeltd.ReviewResult{Prompt: c.Name, SidecarID: c.SidecarID, Error: c.Error, DurationMS: c.Duration.Milliseconds(), Status: string(c.Status), Harness: string(name), Model: c.Model, ContextPeak: c.Context.Peak, Mode: string(c.Mode)}
		for i, f := range c.Findings {
			f.Prompt = c.Name
			f.ID = fmt.Sprintf("%s-%d", c.Name, i+1)
			res.Findings = append(res.Findings, wireFinding(f))
			all = append(all, f)
		}
		if i := e.reviewIndexLocked(ridx, c.Name); i >= 0 {
			p := &e.s.Rounds[ridx].Reviews[i]
			p.Findings, p.Worth, p.Status = len(res.Findings), len(worthChanging(c.Findings)), string(c.Status)
			p.ContextPeak = c.Context.Peak
			p.Harness, p.Model = string(c.Harness), c.Model
			r.recordHarnessRateLimit(c.Harness, c.RateLimit)
			if c.Status == factory.StatusSkipped {
				p.State = smeltd.PromptSkipped
			}
		}
		rd.Results = append(rd.Results, res)
	}
	unique := review.DedupeFindings(all)
	e.s.Rounds[ridx].Findings = len(unique)
	e.s.Rounds[ridx].Worth = len(worthChanging(unique))
}

// wireFinding is a review's finding as the session API carries it.
func wireFinding(f review.Finding) smeltd.Finding {
	return smeltd.Finding{ID: f.ID, Prompt: f.Prompt, File: f.File, Line: f.Line, Severity: f.Severity, Body: f.Body, Patch: f.Patch, Downgraded: f.Downgraded != "", DowngradedReason: f.Downgraded}
}

func (r *factoryRecorder) reviewProgress(ev review.ProgressEvent) {
	r.entry.mu.Lock()
	ridx := r.cur
	r.entry.mu.Unlock()
	r.entry.applyProgress(ridx, ev)
}

// attribute names a command for the output store when it is a review's, and
// ties it to that review. Anything else run on the sidecars is bookkeeping, or
// the implementer's turn, whose raw output does not read as a log.
func (r *factoryRecorder) attribute(sidecarID, commandID string) string {
	e := r.entry
	e.mu.Lock()
	defer e.mu.Unlock()
	if r.cur >= len(e.s.Rounds) {
		return ""
	}
	round := &e.s.Rounds[r.cur]
	for i := range round.Reviews {
		p := &round.Reviews[i]
		if p.SidecarID == sidecarID && p.State == smeltd.PromptRunning {
			p.CommandID = commandID
			return fmt.Sprintf("round %d review: %s", round.Number, p.Name)
		}
	}
	return ""
}

// checked records a validation command as it finishes.
func (r *factoryRecorder) checked(c factory.Check) {
	e := r.entry
	e.mu.Lock()
	defer e.mu.Unlock()
	if r.cur >= len(e.s.Rounds) {
		return
	}
	round := &e.s.Rounds[r.cur]
	rc := smeltd.RoundCheck{
		Name:       c.Name,
		Status:     string(c.Status),
		SidecarID:  c.SidecarID,
		DurationMS: c.Duration.Milliseconds(),
		Error:      c.Error,
		Output:     c.Output,
		Skipped:    c.Skipped,
	}
	if c.Ratchet != nil {
		rc.Ratchet = &smeltd.RatchetCounts{New: c.Ratchet.New, Fixed: c.Ratchet.Fixed, Existing: c.Ratchet.Existing}
	}
	if c.Status == factory.StatusNotMeasured {
		rc.Error = c.Reason
	}
	if c.Kind == factory.KindQuality {
		rc.Output = c.Feedback
		if c.Quality != nil {
			rc.Summary = c.Quality.Line()
		}
	}
	round.Checks = append(round.Checks, rc)
}

// wireQuality is a quality measurement as the status JSON carries it.
func wireQuality(q *quality.Summary) *smeltd.FactoryQuality {
	out := &smeltd.FactoryQuality{
		Measured:      true,
		Line:          q.Line(),
		ErosionBefore: q.ErosionBefore, ErosionAfter: q.ErosionAfter,
		OverThresholdBefore: q.OverThresholdBefore, OverThresholdAfter: q.OverThresholdAfter,
		CloneLinesBefore: q.CloneLinesBefore, CloneLinesAfter: q.CloneLinesAfter,
	}
	for _, f := range q.Worst {
		out.Worst = append(out.Worst, smeltd.QualityFunction{Func: f.Name, File: f.File, Language: f.Language, CC: f.Complexity, Lines: f.Lines})
	}
	return out
}

// finish records what the run left behind. It runs before the session is
// settled, which closes whatever was still in flight.
func (r *factoryRecorder) finish(rep factory.Report, err error) {
	e := r.entry
	e.mu.Lock()
	defer e.mu.Unlock()
	f := e.s.Factory
	f.Committed = rep.Committed
	f.Stat = rep.Stat
	if rep.StatErr != nil {
		f.StatError = rep.StatErr.Error()
	}
	if rep.Notes != nil {
		n := smeltd.Notes(*rep.Notes)
		f.Notes = &n
	}
	f.WorktreeRemoved = rep.WorktreeRemoved
	f.KeptSidecars = rep.KeptSidecars
	f.Log = rep.Log
	if rep.Started {
		updateFactoryBaseline(f, rep.Worktree)
		f.Timing = wireTiming(rep.Timing)
		f.Result = string(rep.Outcome.Result)
		f.Rounds = rep.Outcome.Rounds
		f.Reason, f.ResetAt, f.Stop = rep.Outcome.Reason, rep.Outcome.ResetAt, rep.Outcome.Stop
		f.Blocked = rep.Outcome.Blocked
		f.Unverified = rep.Outcome.Unverified
		f.ContinuedFromEmpty = rep.Outcome.FromEmpty
		f.SkippedTests = rep.Outcome.Skipped()
		f.Quality = wireQualityOutcome(rep.Outcome, f.Quality)
		for _, fd := range rep.Outcome.Incomplete() {
			f.Incomplete = append(f.Incomplete, factory.FindingTitle(fd))
		}
	}
	if err == nil {
		e.loopNote = factoryResultNote(rep.Outcome)
		e.loopFailed = rep.Outcome.Result != factory.ResultPassed
	}
	now := time.Now()
	for i := range e.s.Rounds {
		round := &e.s.Rounds[i]
		if impl := round.Implement; impl != nil && impl.State == smeltd.ImplementRunning {
			impl.State = smeltd.ImplementFailed
			if err != nil {
				impl.Error = friendlyReviewError(err)
			}
		}
		// A round the loop stopped in after the implementer's turn, finding
		// nothing new to check, ended there: its reviews never ran.
		if err == nil && round.State == smeltd.RoundImplementing {
			round.State, round.EndedAt = smeltd.RoundDone, &now
			round.Note = "not checked: " + e.loopNote
			round.Reviews = nil
		}
	}
}

// wireTiming is a run's timing as the status JSON carries it.
func wireTiming(t factory.Timing) *smeltd.FactoryTiming {
	return &smeltd.FactoryTiming{
		Line:   t.Format(),
		PlanMS: t.Plan.Milliseconds(), ImplementMS: t.Implement.Milliseconds(), CommandsMS: t.Commands.Milliseconds(),
		GatesMS: t.Gates.Milliseconds(), ReviewsMS: t.Reviews.Milliseconds(), NotesMS: t.Notes.Milliseconds(),
		TotalMS: t.Total.Milliseconds(), ContextPeakMax: t.PeakMax(), GateOnlyRounds: t.GateOnlyRounds, SlowCalls: t.Warnings(),
	}
}

// imageInstallCommand is the dependency install to run on each sidecar. Only
// the docker backend needs it: its image bakes the package manager's store, not
// an installed tree.
func imageInstallCommand(backend string, cfg *config.ProjectConfig) string {
	if backend != smeltd.BackendDocker {
		return ""
	}
	return cfg.InstallCommand()
}

func (r *factoryRecorder) recordHarnessRateLimit(name harness.Name, rl *harness.RateLimit) {
	if rl == nil {
		return
	}
	if name == "" {
		name = r.harness
	}
	if dir, err := status.Dir(); err == nil {
		status.RecordSignal(dir, name, *rl, time.Now())
	}
}

// warnings are what the client flagged about the run's setup, for its log.
func warnings(req smeltd.FactoryRequest) []string {
	var result []string
	if req.KitWarning != "" {
		result = append(result, req.KitWarning)
	}
	if req.Sandbox != nil && req.Sandbox.KitNotice != "" {
		result = append(result, req.Sandbox.KitNotice)
	}
	return result
}

type factoryHarnesses struct {
	Auth           HarnessAuth
	Fallback       []factory.Candidate
	InitialFailure error
}

// resolveFactoryHarnesses snapshots each agent's own credential and model.
func (d *daemon) resolveFactoryHarnesses(req smeltd.FactoryRequest, cfg *config.ProjectConfig, agent harness.Harness) (factoryHarnesses, error) {
	names := req.Fallback
	if !req.FallbackSet && names == nil && cfg.Factory != nil {
		names = cfg.Factory.Fallback
	}
	fallback := make([]factory.Candidate, 0, len(names))
	seen := map[string]bool{req.Harness: true}
	for _, name := range names {
		h, err := all.ByName(harness.Name(name))
		if err != nil || name == "" {
			return factoryHarnesses{}, apiErr(http.StatusBadRequest, "unknown fallback harness %q", name)
		}
		if seen[name] {
			continue
		}
		seen[name] = true
		fallback = append(fallback, d.fallbackCandidate(h, cfg))
	}
	auth := d.harnessAuth(agent.Name())
	var initialFailure error
	if msg := d.factoryAuthError(req.Backend, agent.Name()); msg != "" {
		initialFailure = errors.New(msg)
	}
	if auth.Refresh != nil {
		if cred, err := auth.Refresh(context.Background()); err != nil {
			initialFailure = err
		} else {
			auth.Credential = cred
			initialFailure = nil
		}
	}
	if initialFailure != nil {
		if len(fallback) == 0 {
			return factoryHarnesses{}, apiErr(http.StatusServiceUnavailable, "%v", initialFailure)
		}
		initialFailure = harness.Classified(harness.Auth, initialFailure, nil)
	}

	return factoryHarnesses{Auth: auth, Fallback: fallback, InitialFailure: initialFailure}, nil
}

func (r *factoryRecorder) turnAttempt(a factory.TurnAttempt) {
	ridx := -1
	if a.Turn == "implementer" && a.Round > 0 {
		ridx = r.round(a.Round)
	}
	r.entry.mu.Lock()
	defer r.entry.mu.Unlock()
	f := r.entry.s.Factory
	f.Turns = append(f.Turns, smeltd.HarnessTurn(a))
	if a.Turn == "implementer" || a.Turn == "plan" || a.Turn == "notes" {
		f.Harness, f.Model = string(a.Harness), a.Model
	}
	if ridx >= 0 {
		round := &r.entry.s.Rounds[ridx]
		round.Harness = string(a.Harness)
		if round.Implement != nil {
			round.Implement.Harness, round.Implement.Model = string(a.Harness), a.Model
		}
	}
	r.recordHarnessRateLimit(a.Harness, a.RateLimit)
}

func (r *factoryRecorder) checkedLocked(ridx int, ev factory.Event) {
	e := r.entry
	round := &e.s.Rounds[ridx]
	r.recordReviewsLocked(ridx, ev.Checks)
	passed, skipped := 0, 0
	for _, c := range ev.Checks {
		switch c.Status {
		case factory.StatusPassed, factory.StatusNotMeasured:
			passed++
		case factory.StatusSkipped:
			skipped++
		case factory.StatusFailed, factory.StatusErrored:
		}
	}
	now := time.Now()
	round.GatesOnlyFailed = factory.GatesOnlyFailed(ev.Checks)
	round.State, round.EndedAt = smeltd.RoundDone, &now
	round.Note = fmt.Sprintf("%d of %d checks passed", passed, len(ev.Checks))
	if skipped > 0 {
		round.Note += fmt.Sprintf("; %d review(s) skipped (gates failed)", skipped)
	}
	if impl := round.Implement; impl != nil && len(impl.Unproven) > 0 {
		round.Note += "; the implementer claimed gates it did not run after its last edit: " + strings.Join(impl.Unproven, ", ")
	}
	recordChecksLocked(e.s.Factory, ev.Checks)
}

func (d *daemon) fallbackCandidate(h harness.Harness, cfg *config.ProjectConfig) factory.Candidate {
	a := d.harnessAuth(h.Name())
	var authErr error
	if a.Refresh != nil {
		a.Credential, authErr = a.Refresh(context.Background())
	}
	if authErr == nil && a.Credential.Value == "" {
		authErr = fmt.Errorf("no %s credential configured", string(h.Name()))
	}
	if authErr != nil {
		authErr = harness.Classified(harness.Auth, authErr, nil)
	}
	return factory.Candidate{Harness: h, Credential: a.Credential, BaseURL: a.BaseURL, Model: cfg.Factory.ModelFor(string(h.Name())), Failure: authErr}
}

func (r *factoryRecorder) beforeRound(ev factory.Event) bool {
	if ev.Kind == factory.EventHarnessSwap && ev.Swap != nil {
		r.entry.mu.Lock()
		defer r.entry.mu.Unlock()
		r.entry.s.Factory.HarnessSwaps = append(r.entry.s.Factory.HarnessSwaps, smeltd.HarnessSwap(*ev.Swap))
		return true
	}
	if ev.Kind == factory.EventTurnAttempt && ev.Attempt != nil {
		r.turnAttempt(*ev.Attempt)
		return true
	}

	// The plan comes before the first round.
	if ev.Kind == factory.EventPlanning {
		r.progress(iostream.LevelStep, "planning")
		return true
	}
	if ev.Kind == factory.EventPlanReviewed {
		r.planReviewed(ev)
		return true
	}
	if ev.Kind == factory.EventNoPriorWork {
		// Shown through the factory's status function, which feeds the progress
		// lines; it belongs to no round.
		return true
	}
	return false
}

// updateFactoryBaseline keeps live status consistent with the saved record.
func updateFactoryBaseline(f *smeltd.FactoryRun, wt factory.Worktree) {
	if wt.Baseline != "" {
		f.Baseline = wt.Baseline
	}
	if wt.UpdatedFrom != nil {
		f.UpdatedFrom = &smeltd.UpdatedFrom{Branch: wt.UpdatedFrom.Branch, Commit: wt.UpdatedFrom.Commit}
	}
}

// checkSandbox refuses a sandbox record whose source is not one the daemon
// knows, so status never shows a made-up one.
func checkSandbox(s smeltd.FactorySandbox) error {
	switch s.Source {
	case smeltd.SandboxFromCaller, smeltd.SandboxFromConfig, smeltd.SandboxFromRepo:
		return nil
	}
	return apiErr(http.StatusBadRequest, "sandbox source %q must be caller, config or repo", s.Source)
}
