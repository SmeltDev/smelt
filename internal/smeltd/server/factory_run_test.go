package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"gotest.tools/v3/assert"
	"gotest.tools/v3/assert/cmp"

	"github.com/smeltdev/smelt/internal/config"
	"github.com/smeltdev/smelt/internal/envspec"
	"github.com/smeltdev/smelt/internal/factory"
	"github.com/smeltdev/smelt/internal/harness"
	"github.com/smeltdev/smelt/internal/iostream"
	"github.com/smeltdev/smelt/internal/quality"
	"github.com/smeltdev/smelt/internal/review"
	"github.com/smeltdev/smelt/internal/sarif"
	"github.com/smeltdev/smelt/internal/sidecar"
	"github.com/smeltdev/smelt/internal/sidecar/docker"
	"github.com/smeltdev/smelt/internal/smeltd"
	"github.com/smeltdev/smelt/internal/testing/gitrepo"
)

// newFactoryDaemon builds a daemon tracking a project with two review prompts
// and one validation command, whose factory runs are run by run in place of
// factory.Run.
func newFactoryDaemon(t *testing.T, run func(context.Context, factory.RunOptions) (factory.Report, error)) (*daemon, string) {
	t.Helper()
	d, root := newSessionDaemon(t, &fakeBackend{})
	d.rcfg.RunFactory = run
	cfg := `{"orgID":"org-1","commands":[{"name":"test","run":"go test ./..."}]}`
	assert.NilError(t, os.WriteFile(filepath.Join(root, ".smelt", "config.json"), []byte(cfg), 0o644))
	return d, root
}

// scriptedRun plays one round as factory.Run reports it: an implementer turn,
// both reviews on their own sidecars, with "bugs" submitting a command, the
// validation command, and the round's checks.
func scriptedRun(result factory.Result) func(context.Context, factory.RunOptions) (factory.Report, error) {
	return func(ctx context.Context, opts factory.RunOptions) (factory.Report, error) {
		wt := factory.Worktree{Path: "/data/factory/run-1", Branch: "smelt/run-1", Baseline: "base", Head: "head", BaseBranch: "feature", DefaultBranch: "main"}
		opts.OnStart("run-1", wt)
		opts.Status(iostream.LevelStep, "Preparing an implementer sidecar and 2 reviewer sidecar(s)...")

		opts.OnEvent(factory.Event{Kind: factory.EventImplementing, Round: 1, Prompt: opts.Prompt})
		opts.OnEvent(factory.Event{Kind: factory.EventImplemented, Round: 1, Turn: factory.Turn{Summary: "added it", Duration: time.Minute, CostUSD: 0.5}})
		opts.OnEvent(factory.Event{Kind: factory.EventCollected, Round: 1, Change: factory.Change{Stat: "1 file changed", Fingerprint: "f"}})
		opts.OnEvent(factory.Event{Kind: factory.EventChecking, Round: 1})

		opts.OnReviewProgress(review.ProgressEvent{Prompt: "bugs", SidecarID: "sc-2", State: review.StateRunning})
		opts.OnReviewProgress(review.ProgressEvent{Prompt: "style", SidecarID: "sc-3", State: review.StateRunning})
		if _, err := opts.Exec(ctx, &sidecar.PoolEntry{ID: "sc-2"}, "claude -p", nil, func(string, []byte) {}, nil); err != nil {
			return factory.Report{}, err
		}
		// A bookkeeping script on the implementer is not a review's.
		if _, err := opts.Exec(ctx, &sidecar.PoolEntry{ID: "sc-1"}, "git add -N .", nil, func(string, []byte) {}, nil); err != nil {
			return factory.Report{}, err
		}
		opts.OnReviewProgress(review.ProgressEvent{Prompt: "bugs", SidecarID: "sc-2", State: review.StateDone, Duration: time.Second})
		opts.OnReviewProgress(review.ProgressEvent{Prompt: "style", SidecarID: "sc-3", State: review.StateDone, Duration: time.Second})
		opts.OnCheck(factory.Check{Name: "test", Kind: factory.KindValidate, Status: factory.StatusPassed, SidecarID: "sc-1", Duration: 2 * time.Second})
		opts.OnCheck(factory.Check{Name: "lint", Kind: factory.KindValidate, Status: factory.StatusFailed, SidecarID: "sc-1", Output: "main.go:3: unused"})

		bug := review.Finding{File: "main.go", Line: 3, Severity: "high", Body: "nil deref"}
		checks := []factory.Check{
			{Name: "bugs", Kind: factory.KindReview, Status: factory.StatusFailed, SidecarID: "sc-2", Findings: []review.Finding{bug}},
			{Name: "style", Kind: factory.KindReview, Status: factory.StatusPassed, SidecarID: "sc-3"},
			{Name: "test", Kind: factory.KindValidate, Status: factory.StatusPassed, SidecarID: "sc-1"},
		}
		opts.OnEvent(factory.Event{Kind: factory.EventChecked, Round: 1, Checks: checks})

		return factory.Report{
			RunID: "run-1", Worktree: wt, Started: true, Committed: true,
			Outcome: factory.Outcome{Result: result, Rounds: 1, Checks: checks},
		}, nil
	}
}

func TestFactorySessionRecordsTheRun(t *testing.T) {
	var got factory.RunOptions
	run := scriptedRun(factory.ResultPassed)
	d, root := newFactoryDaemon(t, func(ctx context.Context, opts factory.RunOptions) (factory.Report, error) {
		got = opts
		return run(ctx, opts)
	})

	sess, err := d.startFactory(smeltd.FactoryRequest{
		Backend: smeltd.BackendCircleCI, ProjectRoot: root, Prompt: "  add a --verbose flag  ", Reviewers: 1,
		ImplementerInstructions: "keep the change small",
	})
	assert.NilError(t, err)
	detail := waitForSessionEnd(t, d, sess.ID)

	// The run was asked for what the request and the project say.
	assert.Equal(t, got.Prompt, "add a --verbose flag")
	assert.Equal(t, got.Attempts, DefaultAttempts)
	assert.Equal(t, got.Reviewers, 1)
	assert.Equal(t, got.OrgID, "org-1")
	assert.Equal(t, len(got.Prompts), 2)
	assert.Equal(t, len(got.Commands), 2) // the project's command and the markers gate
	assert.Equal(t, got.Credential.Value, testSecret)
	assert.Equal(t, got.ImplementerInstructions, "keep the change small")
	assert.Assert(t, got.Quality != nil, "quality is measured unless the project turns it off")

	assert.Equal(t, detail.State, smeltd.SessionDone)
	assert.Equal(t, detail.Stages[0].ID, smeltd.StageFactoryLoop)
	assert.Equal(t, detail.Stages[0].State, smeltd.StageDone)
	assert.Equal(t, detail.Stages[0].Note, "all checks passed after 1 round(s)")
	assert.Equal(t, detail.Branch, "smelt/run-1")
	assert.DeepEqual(t, *detail.Factory, smeltd.FactoryRun{
		Prompt: "add a --verbose flag", Harness: "claude", Attempts: DefaultAttempts, RunID: "run-1",
		Worktree: "/data/factory/run-1", Branch: "smelt/run-1", Baseline: "base", Head: "head",
		BaseBranch: "feature", DefaultBranch: "main",
		Timing: &smeltd.FactoryTiming{Line: factory.Timing{}.Format()},
		Result: "passed", Rounds: 1, Committed: true,
		Progress: smeltd.Feed{Lines: []smeltd.FeedLine{{Level: smeltd.FeedStep, Text: "Preparing an implementer sidecar and 2 reviewer sidecar(s)..."}}, Total: 1},
	})

	assert.Equal(t, len(detail.Rounds), 1)
	round := detail.Rounds[0]
	assert.Equal(t, round.State, smeltd.RoundDone)
	assert.Equal(t, round.Note, "2 of 3 checks passed")
	assert.DeepEqual(t, *round.Implement, smeltd.RoundImplement{
		State: smeltd.ImplementApplied, DurationMS: 60000, CostUSD: 0.5, Summary: "added it", Stat: "1 file changed",
	})
	assert.DeepEqual(t, round.Checks, []smeltd.RoundCheck{
		{Name: "test", Status: "passed", SidecarID: "sc-1", DurationMS: 2000},
		{Name: "lint", Status: "failed", SidecarID: "sc-1", Output: "main.go:3: unused"},
	})
	assert.Equal(t, round.Findings, 1)
	assert.Equal(t, round.Worth, 1)
	byName := map[string]smeltd.ReviewPrompt{}
	for _, p := range round.Reviews {
		byName[p.Name] = p
	}
	assert.Equal(t, byName["bugs"].State, smeltd.PromptDone)
	assert.Equal(t, byName["bugs"].Findings, 1)
	// How each review came out as a check is in the snapshot, not only the
	// detail, so a dashboard can tell a review that failed the round from one
	// that found nothing worth changing.
	assert.Equal(t, byName["bugs"].Worth, 1)
	assert.Equal(t, byName["bugs"].Status, "failed")
	assert.Equal(t, byName["style"].Status, "passed")
	assert.Assert(t, byName["bugs"].CommandID != "", "the review's command was not tied to it")
	assert.Equal(t, byName["style"].CommandID, "")
	assert.Equal(t, len(detail.Details[0].Results), 2)
	for _, res := range detail.Details[0].Results {
		want := map[string]string{"bugs": "failed", "style": "passed"}[res.Prompt]
		assert.Equal(t, res.Status, want, res.Prompt)
	}

	// Only the review's command is in the output store, named for its review.
	cmds := d.out.commandsFor(root)
	assert.Equal(t, len(cmds), 1)
	assert.Equal(t, cmds[0].Name, "round 1 review: bugs")
}

func TestFactorySessionWhoseChecksStillFailIsDoneWithItsStageFailed(t *testing.T) {
	d, root := newFactoryDaemon(t, scriptedRun(factory.ResultExhausted))

	sess, err := d.startFactory(smeltd.FactoryRequest{Backend: smeltd.BackendCircleCI, ProjectRoot: root, Prompt: "add a flag", Attempts: 1})
	assert.NilError(t, err)
	detail := waitForSessionEnd(t, d, sess.ID)

	assert.Equal(t, detail.State, smeltd.SessionDone)
	assert.Equal(t, detail.Factory.Result, "exhausted")
	assert.Equal(t, detail.Stages[0].State, smeltd.StageFailed)
	assert.Equal(t, detail.Stages[0].Note, "checks still failed after 1 round(s)")
}

// A run that stops because the implementer changed nothing new ends on a
// round it never checked: that round is done, not failed, and the run's
// rounds are the ones checked.
func TestFactorySessionThatGetsStuckClosesItsUncheckedRound(t *testing.T) {
	run := scriptedRun(factory.ResultStuck)
	d, root := newFactoryDaemon(t, func(ctx context.Context, opts factory.RunOptions) (factory.Report, error) {
		rep, err := run(ctx, opts)
		if err != nil {
			return rep, err
		}
		opts.OnEvent(factory.Event{Kind: factory.EventImplementing, Round: 2, Prompt: opts.Prompt})
		opts.OnEvent(factory.Event{Kind: factory.EventImplemented, Round: 2, Turn: factory.Turn{Summary: "nothing new"}})
		opts.OnEvent(factory.Event{Kind: factory.EventCollected, Round: 2, Change: factory.Change{Stat: "1 file changed", Fingerprint: "f"}})
		return rep, nil
	})

	sess, err := d.startFactory(smeltd.FactoryRequest{Backend: smeltd.BackendCircleCI, ProjectRoot: root, Prompt: "add a flag"})
	assert.NilError(t, err)
	detail := waitForSessionEnd(t, d, sess.ID)

	assert.Equal(t, detail.State, smeltd.SessionDone)
	assert.Equal(t, detail.Stages[0].State, smeltd.StageFailed)
	assert.Equal(t, detail.Factory.Rounds, 1)
	assert.Equal(t, len(detail.Rounds), 2)
	assert.Equal(t, detail.Rounds[0].State, smeltd.RoundDone)
	last := detail.Rounds[1]
	assert.Equal(t, last.State, smeltd.RoundDone)
	assert.Equal(t, last.Note, "not checked: the implementer stopped changing the code after round 1, with checks still failing")
	assert.Equal(t, len(last.Reviews), 0)
}

func TestFactorySessionThatCannotStartSaysWhichStepFailed(t *testing.T) {
	d, root := newFactoryDaemon(t, func(context.Context, factory.RunOptions) (factory.Report, error) {
		return factory.Report{}, fmt.Errorf("create the run's sidecars: %w", errors.New("quota exceeded"))
	})

	sess, err := d.startFactory(smeltd.FactoryRequest{Backend: smeltd.BackendCircleCI, ProjectRoot: root, Prompt: "add a flag"})
	assert.NilError(t, err)
	detail := waitForSessionEnd(t, d, sess.ID)

	assert.Equal(t, detail.State, smeltd.SessionFailed)
	assert.Equal(t, detail.Error, "create the run's sidecars: quota exceeded")
	assert.Equal(t, detail.Stages[0].State, smeltd.StageFailed)
	assert.Equal(t, detail.Factory.Result, "")
}

func TestFactorySessionWhoseImplementerFailsRecordsTheTurnFailed(t *testing.T) {
	d, root := newFactoryDaemon(t, func(_ context.Context, opts factory.RunOptions) (factory.Report, error) {
		opts.OnEvent(factory.Event{Kind: factory.EventImplementing, Round: 1, Prompt: opts.Prompt})
		return factory.Report{Started: true}, errors.New("round 1: implement: implementer: claude exited 1")
	})

	sess, err := d.startFactory(smeltd.FactoryRequest{Backend: smeltd.BackendCircleCI, ProjectRoot: root, Prompt: "add a flag"})
	assert.NilError(t, err)
	detail := waitForSessionEnd(t, d, sess.ID)

	assert.Equal(t, detail.State, smeltd.SessionFailed)
	round := detail.Rounds[0]
	assert.Equal(t, round.State, smeltd.RoundFailed)
	assert.Equal(t, round.Implement.State, smeltd.ImplementFailed)
	assert.Assert(t, strings.Contains(round.Implement.Error, "claude exited 1"), round.Implement.Error)
}

func TestFactorySessionCanBeCancelled(t *testing.T) {
	d, root := newFactoryDaemon(t, func(ctx context.Context, _ factory.RunOptions) (factory.Report, error) {
		<-ctx.Done()
		return factory.Report{}, ctx.Err()
	})

	sess, err := d.startFactory(smeltd.FactoryRequest{Backend: smeltd.BackendCircleCI, ProjectRoot: root, Prompt: "add a flag"})
	assert.NilError(t, err)
	found, active := d.sessions.cancelSession(sess.ID)
	assert.Assert(t, found && active)
	ended := waitForSessionEnd(t, d, sess.ID)
	assert.Equal(t, ended.State, smeltd.SessionCancelled)
	assert.Assert(t, ended.StoppedAfterMS != nil && *ended.StoppedAfterMS >= 0, "stopped_after_ms is recorded")
}

// One session at a time per project.
func TestFactorySessionsTakeTurns(t *testing.T) {
	s := newSessionStore(context.Background())
	_, _, busy := s.add(smeltd.Session{ProjectRoot: "/p", Factory: &smeltd.FactoryRun{}}, 1)
	assert.NilError(t, busy)
	_, _, busy = s.add(smeltd.Session{ProjectRoot: "/p", Factory: &smeltd.FactoryRun{}}, 1)
	assert.Assert(t, busy != nil, "two factory runs ran at once")
}

// TestFactorySessionKeepsItsLog guards --log and --verbose on the daemon: the
// run is asked for them, and the record says where the log is.
func TestFactorySessionKeepsItsLog(t *testing.T) {
	var got factory.RunOptions
	run := scriptedRun(factory.ResultPassed)
	d, root := newFactoryDaemon(t, func(ctx context.Context, opts factory.RunOptions) (factory.Report, error) {
		got = opts
		rep, err := run(ctx, opts)
		rep.Log = "/logs/run-1.log"
		return rep, err
	})

	sess, err := d.startFactory(smeltd.FactoryRequest{Backend: smeltd.BackendCircleCI, ProjectRoot: root, Prompt: "add a flag", Log: "/logs/run-1.log", Verbose: true})
	assert.NilError(t, err)
	detail := waitForSessionEnd(t, d, sess.ID)

	assert.Equal(t, got.Log, "/logs/run-1.log")
	assert.Assert(t, got.Verbose)
	assert.Equal(t, detail.Factory.Log, "/logs/run-1.log")
}

func TestFactorySessionRecordsAnOutdatedKit(t *testing.T) {
	var got factory.RunOptions
	run := scriptedRun(factory.ResultPassed)
	d, root := newFactoryDaemon(t, func(ctx context.Context, opts factory.RunOptions) (factory.Report, error) {
		got = opts
		return run(ctx, opts)
	})

	sess, err := d.startFactory(smeltd.FactoryRequest{Backend: smeltd.BackendCircleCI, ProjectRoot: root, Prompt: "add a flag", KitWarning: "kit is old"})
	assert.NilError(t, err)
	detail := waitForSessionEnd(t, d, sess.ID)
	assert.DeepEqual(t, got.Warnings, []string{"kit is old"})
	assert.Assert(t, detail.Factory.KitOutdated)

	sess, err = d.startFactory(smeltd.FactoryRequest{Backend: smeltd.BackendCircleCI, ProjectRoot: root, Prompt: "add a flag"})
	assert.NilError(t, err)
	detail = waitForSessionEnd(t, d, sess.ID)
	assert.Equal(t, len(got.Warnings), 0)
	assert.Assert(t, !detail.Factory.KitOutdated)
}

func TestStartFactoryRefusesABadRequest(t *testing.T) {
	d, root := newFactoryDaemon(t, scriptedRun(factory.ResultPassed))

	for name, tc := range map[string]struct {
		req    smeltd.FactoryRequest
		status int
		msg    string
	}{
		"no prompt":       {smeltd.FactoryRequest{Backend: smeltd.BackendCircleCI, ProjectRoot: root, Prompt: "  "}, http.StatusBadRequest, "prompt required"},
		"unknown project": {smeltd.FactoryRequest{Backend: smeltd.BackendCircleCI, ProjectRoot: "/nowhere", Prompt: "x"}, http.StatusNotFound, "not tracking"},
		"escaping dir":    {smeltd.FactoryRequest{Backend: smeltd.BackendCircleCI, ProjectRoot: root, Prompt: "x", ReviewsDir: "../elsewhere"}, http.StatusBadRequest, "inside the project"},
		"missing dir":     {smeltd.FactoryRequest{Backend: smeltd.BackendCircleCI, ProjectRoot: root, Prompt: "x", ReviewsDir: "nope"}, http.StatusBadRequest, "nope"},
		"relative log":    {smeltd.FactoryRequest{Backend: smeltd.BackendCircleCI, ProjectRoot: root, Prompt: "x", Log: "run.log"}, http.StatusBadRequest, "absolute path"},
		"from with continue": {
			smeltd.FactoryRequest{Backend: smeltd.BackendCircleCI, ProjectRoot: root, Prompt: "x", From: "epic/x", Continue: "run-0"},
			http.StatusBadRequest, "from cannot be combined with continue",
		},
		"reviews alone": {
			smeltd.FactoryRequest{Backend: smeltd.BackendCircleCI, ProjectRoot: root, Prompt: "x", ReviewsDir: ".smelt/reviews", NoValidate: true},
			0, "",
		},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := d.startFactory(tc.req)
			if tc.status == 0 {
				assert.NilError(t, err, "reviews alone are enough")
				return
			}
			var ae *apiError
			assert.Assert(t, errors.As(err, &ae), "got %v", err)
			assert.Equal(t, ae.status, tc.status)
			assert.ErrorContains(t, ae, tc.msg)
		})
	}
	d.sessions.stopAll()

	// Neither reviews nor validation commands: the loop would pass anything.
	assert.NilError(t, os.RemoveAll(filepath.Join(root, ".smelt", "reviews")))
	_, err := d.startFactory(smeltd.FactoryRequest{Backend: smeltd.BackendCircleCI, ProjectRoot: root, Prompt: "x", NoValidate: true})
	var ae *apiError
	assert.Assert(t, errors.As(err, &ae), "got %v", err)
	assert.ErrorContains(t, ae, "nothing to check")
}

func TestFeedKeepsTheLatestLinesAndCountsThemAll(t *testing.T) {
	var f smeltd.Feed
	for i := range maxFeedLines + 5 {
		addFeedLine(&f, iostream.LevelInfo, fmt.Sprint(i))
	}
	assert.Equal(t, f.Total, maxFeedLines+5)
	assert.Equal(t, len(f.Lines), maxFeedLines)
	assert.Equal(t, f.Lines[0].Text, "5")

	// A follower that has seen all but two gets those two.
	got := f.Since(f.Total - 2)
	assert.Equal(t, len(got), 2)
	assert.Equal(t, got[1].Text, fmt.Sprint(maxFeedLines+4))
	// One that fell behind gets what is still kept, and one that is ahead
	// of the feed nothing.
	assert.Equal(t, len(f.Since(0)), maxFeedLines)
	assert.Equal(t, len(f.Since(f.Total+1)), 0)
}

// The session API spells out the factory's results and check statuses so a
// client need not import the factory to read them. They must stay the same
// strings, since the daemon passes the factory's values through as they are.
func TestAPIResultsMatchTheFactorys(t *testing.T) {
	for _, c := range [][2]string{
		{smeltd.ResultPassed, string(factory.ResultPassed)},
		{smeltd.ResultExhausted, string(factory.ResultExhausted)},
		{smeltd.ResultStuck, string(factory.ResultStuck)},
		{smeltd.ResultNoChange, string(factory.ResultNoChange)},
		{smeltd.ResultUnavailable, string(factory.ResultUnavailable)},
		{smeltd.StopHarnessUnavailable, factory.StopHarnessUnavailable},
		{smeltd.CheckPassed, string(factory.StatusPassed)},
		{smeltd.CheckFailed, string(factory.StatusFailed)},
		{smeltd.CheckErrored, string(factory.StatusErrored)},
	} {
		assert.Equal(t, c[0], c[1])
	}
}

// A client asking for work on a project the daemon has never seen need not
// register it first: the daemon adopts the top of a git repository itself, and
// remembers it as any registered project.
func TestStartFactoryAdoptsAnUnregisteredRepository(t *testing.T) {
	d, _ := newFactoryDaemon(t, scriptedRun(factory.ResultPassed))
	other := initRepo(t)
	cfg := `{"orgID":"org-1","commands":[{"name":"test","run":"go test ./..."}]}`
	assert.NilError(t, os.MkdirAll(filepath.Join(other, ".smelt"), 0o755))
	assert.NilError(t, os.WriteFile(filepath.Join(other, ".smelt", "config.json"), []byte(cfg), 0o644))
	root, err := filepath.EvalSymlinks(other)
	assert.NilError(t, err)

	s, err := d.startFactory(smeltd.FactoryRequest{Backend: smeltd.BackendCircleCI, ProjectRoot: root, Prompt: "add a flag"})
	assert.NilError(t, err)
	assert.Equal(t, s.ProjectRoot, root)

	known, err := sidecar.AllProjectRoots()
	assert.NilError(t, err)
	assert.Check(t, cmp.Contains(known, root), "the adopted project is registered")

	// A directory inside the repository is not the project, and is not adopted.
	sub := filepath.Join(root, "sub")
	assert.NilError(t, os.MkdirAll(sub, 0o755))
	_, err = d.startFactory(smeltd.FactoryRequest{Backend: smeltd.BackendCircleCI, ProjectRoot: sub, Prompt: "add a flag"})
	var ae *apiError
	assert.Assert(t, errors.As(err, &ae), "got %v", err)
	assert.Equal(t, ae.status, http.StatusNotFound)
}

func TestFactoryTargetDockerIgnoresSidecarImage(t *testing.T) {
	cfg := &config.ProjectConfig{
		Validation: &config.ValidationConfig{SidecarImage: "snapshot-id"},
	}
	req := smeltd.FactoryRequest{Backend: smeltd.BackendDocker}
	_, image, err := factoryTarget(t.TempDir(), cfg, req)
	assert.NilError(t, err)
	assert.Equal(t, image, "")

	cfg.Factory = &config.FactoryConfig{Image: "smelt-sandbox:local"}
	_, image, err = factoryTarget(t.TempDir(), cfg, req)
	assert.NilError(t, err)
	assert.Equal(t, image, "smelt-sandbox:local")

	req.Image = "other:tag"
	_, image, err = factoryTarget(t.TempDir(), cfg, req)
	assert.NilError(t, err)
	assert.Equal(t, image, "other:tag")
}

// A finished run's notes reach the session's record, where the CLI publishes
// them from.
func TestFactoryRecorderFinishKeepsTheNotes(t *testing.T) {
	s := newSessionStore(context.Background())
	entry, _, busy := s.add(smeltd.Session{ProjectRoot: "/p", Factory: &smeltd.FactoryRun{}}, 1)
	assert.NilError(t, busy)
	rec := &factoryRecorder{entry: entry, rounds: map[int]int{}}

	rec.finish(factory.Report{Committed: true, Notes: &factory.Notes{Title: "Add a flag", What: "- w", Why: "- y", How: "- h"}}, nil)

	got := entry.snapshot().Factory.Notes
	assert.Assert(t, got != nil)
	assert.DeepEqual(t, *got, smeltd.Notes{Title: "Add a flag", What: "- w", Why: "- y", How: "- h"})
}

// A run that continued an empty branch says so in its status.
func TestFactoryRecorderFinishRecordsContinuedFromEmpty(t *testing.T) {
	s := newSessionStore(context.Background())
	entry, _, busy := s.add(smeltd.Session{ProjectRoot: "/p", Factory: &smeltd.FactoryRun{}}, 1)
	assert.NilError(t, busy)
	rec := &factoryRecorder{entry: entry, rounds: map[int]int{}}

	rec.event(factory.Event{Kind: factory.EventNoPriorWork})
	rec.finish(factory.Report{Started: true, Outcome: factory.Outcome{Result: factory.ResultPassed, FromEmpty: true}}, nil)

	assert.Assert(t, entry.snapshot().Factory.ContinuedFromEmpty)
	assert.Equal(t, len(entry.snapshot().Rounds), 0, "the event began a round")
}

func TestFactorySessionRecordsBlockedAndIncompleteWork(t *testing.T) {
	run := scriptedRun(factory.ResultBlocked)
	d, root := newFactoryDaemon(t, func(ctx context.Context, opts factory.RunOptions) (factory.Report, error) {
		rep, err := run(ctx, opts)
		rep.Outcome.Blocked = "needs a credential"
		rep.Outcome.Unverified = []string{"run it in Docker"}
		rep.Outcome.Checks = []factory.Check{{
			Name: "adversarial", Kind: factory.KindReview, Status: factory.StatusFailed,
			Findings: []review.Finding{{File: "a.go", Severity: "medium", Body: "The CLI is missing. Nothing selects a harness."}},
		}}
		return rep, err
	})

	sess, err := d.startFactory(smeltd.FactoryRequest{Backend: smeltd.BackendCircleCI, ProjectRoot: root, Prompt: "add a flag"})
	assert.NilError(t, err)
	detail := waitForSessionEnd(t, d, sess.ID)

	assert.Equal(t, detail.Factory.Result, smeltd.ResultBlocked)
	assert.Equal(t, detail.Factory.Blocked, "needs a credential")
	assert.DeepEqual(t, detail.Factory.Unverified, []string{"run it in Docker"})
	assert.DeepEqual(t, detail.Factory.Incomplete, []string{"The CLI is missing"})
	assert.Equal(t, detail.Stages[0].Note, "the implementer is blocked: needs a credential")
}

func TestFactorySessionPlansAndRunsGatesFirstUnlessTurnedOff(t *testing.T) {
	tests := []struct {
		name       string
		cfg        string
		req        smeltd.FactoryRequest
		plan, gate bool
	}{
		{"defaults", `{"orgID":"org-1"}`, smeltd.FactoryRequest{}, true, true},
		{"--no-plan", `{"orgID":"org-1"}`, smeltd.FactoryRequest{NoPlan: true}, false, true},
		{"factory.plan false", `{"orgID":"org-1","factory":{"plan":false}}`, smeltd.FactoryRequest{}, false, true},
		{"factory.gatesFirst false", `{"orgID":"org-1","factory":{"gatesFirst":false}}`, smeltd.FactoryRequest{}, true, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var got factory.RunOptions
			run := scriptedRun(factory.ResultPassed)
			d, root := newFactoryDaemon(t, func(ctx context.Context, opts factory.RunOptions) (factory.Report, error) {
				got = opts
				return run(ctx, opts)
			})
			cfg := strings.Replace(tt.cfg, `{`, `{"commands":[{"name":"test","run":"go test ./..."}],`, 1)
			assert.NilError(t, os.WriteFile(filepath.Join(root, ".smelt", "config.json"), []byte(cfg), 0o644))
			req := tt.req
			req.Backend, req.ProjectRoot, req.Prompt = smeltd.BackendCircleCI, root, "add a flag"
			sess, err := d.startFactory(req)
			assert.NilError(t, err)
			waitForSessionEnd(t, d, sess.ID)
			assert.Equal(t, got.Plan, tt.plan)
			assert.Equal(t, got.GatesFirst, tt.gate)
		})
	}
}

func TestFactorySessionBuildsThePackUnlessTurnedOff(t *testing.T) {
	tests := []struct {
		name string
		cfg  string
		req  smeltd.FactoryRequest
		want factory.PackOptions
	}{
		{"defaults", `{"orgID":"org-1"}`, smeltd.FactoryRequest{}, factory.PackOptions{Enabled: true, MaxTokens: 25000}},
		{"--no-pack", `{"orgID":"org-1"}`, smeltd.FactoryRequest{NoPack: true}, factory.PackOptions{MaxTokens: 25000}},
		{"factory.pack.enabled false", `{"orgID":"org-1","factory":{"pack":{"enabled":false}}}`, smeltd.FactoryRequest{}, factory.PackOptions{MaxTokens: 25000}},
		{"factory.pack.maxTokens", `{"orgID":"org-1","factory":{"pack":{"maxTokens":900}}}`, smeltd.FactoryRequest{}, factory.PackOptions{Enabled: true, MaxTokens: 900}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var got factory.RunOptions
			run := scriptedRun(factory.ResultPassed)
			d, root := newFactoryDaemon(t, func(ctx context.Context, opts factory.RunOptions) (factory.Report, error) {
				got = opts
				return run(ctx, opts)
			})
			cfg := strings.Replace(tt.cfg, `{`, `{"commands":[{"name":"test","run":"go test ./..."}],`, 1)
			assert.NilError(t, os.WriteFile(filepath.Join(root, ".smelt", "config.json"), []byte(cfg), 0o644))
			req := tt.req
			req.Backend, req.ProjectRoot, req.Prompt = smeltd.BackendCircleCI, root, "add a flag"
			sess, err := d.startFactory(req)
			assert.NilError(t, err)
			waitForSessionEnd(t, d, sess.ID)
			assert.Equal(t, got.Pack, tt.want)
		})
	}
}

func TestFactoryRecorderCarriesThePackInTheStatus(t *testing.T) {
	s := newSessionStore(context.Background())
	entry, _, _ := s.add(smeltd.Session{ProjectRoot: "/p", Factory: &smeltd.FactoryRun{}}, 1)
	rec := &factoryRecorder{entry: entry, rounds: map[int]int{}}

	plan := factory.Plan{Summary: "do it",
		Pack: []factory.PackEntry{
			{Kind: factory.PackEdit, Path: "a.go", Start: 3, End: 9, Reason: "edit here", Priority: 1},
			{Kind: factory.PackReference, Path: "b.go", Reason: "reuse", Priority: 2, Excerpt: "code"},
		},
		PackDropped:   []factory.DroppedEntry{{Path: "c.go", Start: 900, End: 950, Reason: "beyond end of file (612 lines)"}},
		RejectedHints: []factory.RejectedHint{{Hint: "use x", Reason: "y is better"}},
		PackTokens:    42,
	}
	rec.event(factory.Event{Kind: factory.EventPlanReviewed, Plan: &plan})

	b, err := json.Marshal(entry.snapshot().Factory.Pack)
	assert.NilError(t, err)
	assert.Equal(t, string(b), `{"entries":[`+
		`{"kind":"edit","path":"a.go","start":3,"end":9,"reason":"edit here","priority":1,"inlined":false},`+
		`{"kind":"reference","path":"b.go","start":0,"end":0,"reason":"reuse","priority":2,"inlined":true}],`+
		`"dropped":[{"path":"c.go","start":900,"end":950,"reason":"beyond end of file (612 lines)"}],`+
		`"rejected_hints":[{"hint":"use x","reason":"y is better"}],"tokens":42}`)

	rec.setPack(factory.Pack{})
	assert.Assert(t, entry.snapshot().Factory.Pack == nil, "a continued run with no pack has none")
}

func TestFactoryRecorderRecordsThePlanSkippedReviewsAndSkippedTests(t *testing.T) {
	s := newSessionStore(context.Background())
	entry, _, busy := s.add(smeltd.Session{ProjectRoot: "/p", Factory: &smeltd.FactoryRun{}}, 1)
	assert.NilError(t, busy)
	rec := &factoryRecorder{entry: entry, prompts: []review.Prompt{{Name: "requirements"}}, rounds: map[int]int{}}

	plan := factory.Plan{Summary: "do it", Files: []factory.PlanFile{{Path: "a.go", Change: "add", Why: "w"}},
		Acceptance: []factory.PlanItem{{Item: "A", How: "h", VerifiableInSandbox: true}}, CannotVerifyHere: []string{"docker"}}
	rec.event(factory.Event{Kind: factory.EventPlanning})
	rec.event(factory.Event{Kind: factory.EventPlanReviewed, Plan: &plan})
	rec.event(factory.Event{Kind: factory.EventImplementing, Round: 1})
	rec.event(factory.Event{Kind: factory.EventImplemented, Round: 1, Turn: factory.Turn{Summary: "x", Unproven: []string{"lint"}}})
	rec.event(factory.Event{Kind: factory.EventChecked, Round: 1, Checks: []factory.Check{
		{Name: "requirements", Kind: factory.KindReview, Status: factory.StatusSkipped, Reason: "gates failed"},
		{Name: "test", Kind: factory.KindValidate, Status: factory.StatusFailed, Skipped: []string{"TestDocker"}},
	}})

	snap := entry.snapshot()
	f := snap.Factory
	assert.Equal(t, f.Plan.Summary, "do it")
	assert.DeepEqual(t, f.Plan.Files, []smeltd.PlanFile{{Path: "a.go", Change: "add", Why: "w"}})
	assert.DeepEqual(t, f.Plan.Acceptance, []smeltd.PlanItem{{Item: "A", How: "h", VerifiableInSandbox: true}})
	assert.DeepEqual(t, f.Plan.CannotVerifyHere, []string{"docker"})
	var lines []string
	for _, l := range f.Progress.Lines {
		lines = append(lines, l.Text)
	}
	assert.DeepEqual(t, lines, []string{"planning", "plan reviewed"})
	assert.DeepEqual(t, f.SkippedTests, []string{"TestDocker"})

	assert.Equal(t, len(snap.Rounds), 1, "the plan is not a round")
	round := snap.Rounds[0]
	assert.Equal(t, round.Reviews[0].State, smeltd.PromptSkipped)
	assert.Equal(t, round.Reviews[0].Status, smeltd.CheckSkipped)
	assert.DeepEqual(t, round.Implement.Unproven, []string{"lint"})
	assert.Assert(t, round.GatesOnlyFailed)
	assert.Assert(t, strings.Contains(round.Note, "1 review(s) skipped (gates failed)"), round.Note)
	assert.Assert(t, strings.Contains(round.Note, "lint"), round.Note)
}

func TestFactoryRecorderRecordsReviewModeAndDowngradedFindings(t *testing.T) {
	s := newSessionStore(context.Background())
	entry, _, busy := s.add(smeltd.Session{ProjectRoot: "/p", Factory: &smeltd.FactoryRun{}}, 1)
	assert.NilError(t, busy)
	rec := &factoryRecorder{entry: entry, prompts: []review.Prompt{{Name: "bugs"}, {Name: "style"}}, rounds: map[int]int{}}

	rec.event(factory.Event{Kind: factory.EventImplementing, Round: 1})
	rec.event(factory.Event{Kind: factory.EventImplemented, Round: 1})
	rec.event(factory.Event{Kind: factory.EventChecked, Round: 1, Checks: []factory.Check{
		{Name: "bugs", Kind: factory.KindReview, Status: factory.StatusPassed, Mode: factory.ModeRecheck, Findings: []review.Finding{
			{File: "a.go", Line: 3, Severity: review.SeverityLow, Body: "stray", Downgraded: "new in round 2 on unchanged code"},
			{File: "b.go", Line: 1, Severity: review.SeverityLow, Body: "plain"},
		}},
		{Name: "style", Kind: factory.KindReview, Status: factory.StatusPassed, Mode: factory.ModeCarried},
	}})

	d := entry.detail()
	modes := map[string]string{}
	for _, res := range d.Details[0].Results {
		modes[res.Prompt] = res.Mode
	}
	assert.DeepEqual(t, modes, map[string]string{"bugs": "recheck", "style": "carried"})
	for _, res := range d.Details[0].Results {
		if res.Prompt != "bugs" {
			continue
		}
		assert.Equal(t, len(res.Findings), 2)
		assert.Assert(t, res.Findings[0].Downgraded)
		assert.Equal(t, res.Findings[0].DowngradedReason, "new in round 2 on unchanged code")
		assert.Assert(t, !res.Findings[1].Downgraded)
	}
	b, err := json.Marshal(d.Details[0].Results)
	assert.NilError(t, err)
	assert.Assert(t, strings.Contains(string(b), `"mode":"recheck"`) && strings.Contains(string(b), `"downgraded":true`) && strings.Contains(string(b), `"downgraded_reason":`), string(b))
}

func TestFactoryRunUnverifiedJSON(t *testing.T) {
	b, err := json.Marshal(smeltd.FactoryRun{Unverified: []string{"run it in Docker"}})
	assert.NilError(t, err)
	assert.Assert(t, strings.Contains(string(b), `"unverified":["run it in Docker"]`), string(b))
	b, err = json.Marshal(smeltd.FactoryRun{})
	assert.NilError(t, err)
	assert.Assert(t, !strings.Contains(string(b), "unverified"), string(b))
}

func TestFactoryBackendCacheKey(t *testing.T) {
	off := false
	tests := []struct {
		name      string
		cfg       *config.ProjectConfig
		wantKey   bool
		wantGoMod bool
	}{
		{"default", &config.ProjectConfig{}, true, false},
		{"go project", &config.ProjectConfig{Environment: &envspec.Environment{Stack: envspec.StackGo}}, true, true},
		{"cache off", &config.ProjectConfig{Factory: &config.FactoryConfig{Cache: &off}, Environment: &envspec.Environment{Stack: envspec.StackGo}}, false, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			root := t.TempDir()
			b, err := factoryBackend(smeltd.BackendDocker, root, tt.cfg)
			assert.NilError(t, err)
			p := b.(*docker.Provider)
			assert.Equal(t, p.CacheKey != "", tt.wantKey)
			if tt.wantKey {
				assert.Equal(t, p.CacheKey, config.ProjectHash(root))
			}
			assert.Equal(t, p.GoStack, tt.wantGoMod)
		})
	}
}

// A run started with --from reports the ref and commit in its status.
func TestFactoryRecorderStartedRecordsFrom(t *testing.T) {
	s := newSessionStore(context.Background())
	entry, _, busy := s.add(smeltd.Session{ProjectRoot: "/p", Factory: &smeltd.FactoryRun{}}, 1)
	assert.NilError(t, busy)
	rec := &factoryRecorder{entry: entry, rounds: map[int]int{}}

	rec.started("run-1", factory.Worktree{Branch: "smelt/x/run-1", BaseBranch: "epic/x", From: &factory.FromRef{Ref: "epic/x", Commit: "abc123"}})

	f := entry.snapshot().Factory
	assert.Equal(t, f.BaseBranch, "epic/x")
	b, err := json.Marshal(f)
	assert.NilError(t, err)
	assert.Assert(t, strings.Contains(string(b), `"from":{"ref":"epic/x","commit":"abc123"}`), string(b))
}

// A run on a branch updated from its base reports where from in its status.
func TestFactoryRecorderStartedRecordsUpdatedFrom(t *testing.T) {
	s := newSessionStore(context.Background())
	entry, _, busy := s.add(smeltd.Session{ProjectRoot: "/p", Factory: &smeltd.FactoryRun{}}, 1)
	assert.NilError(t, busy)
	rec := &factoryRecorder{entry: entry, rounds: map[int]int{}}

	rec.started("run-1", factory.Worktree{Baseline: "def456", UpdatedFrom: &factory.UpdatedFrom{Branch: "main", Commit: "def456"}})

	b, err := json.Marshal(entry.snapshot().Factory)
	assert.NilError(t, err)
	assert.Assert(t, strings.Contains(string(b), `"baseline":"def456"`), string(b))
	assert.Assert(t, strings.Contains(string(b), `"updated_from":{"branch":"main","commit":"def456"}`), string(b))
}

func TestStartFactoryRefusesABadIntegrateRequest(t *testing.T) {
	d, root := newFactoryDaemon(t, scriptedRun(factory.ResultPassed))
	base := smeltd.FactoryRequest{Backend: smeltd.BackendCircleCI, ProjectRoot: root, Prompt: "spec"}
	for name, tc := range map[string]struct {
		mod func(*smeltd.FactoryRequest)
		msg string
	}{
		"no from":           {func(r *smeltd.FactoryRequest) { r.Integrate = true }, "integrate needs from"},
		"with continue":     {func(r *smeltd.FactoryRequest) { r.Integrate, r.From, r.Continue = true, "epic/x", "run-0" }, "from cannot be combined with continue"},
		"continue, no from": {func(r *smeltd.FactoryRequest) { r.Integrate, r.Continue = true, "run-0" }, "integrate cannot be combined with continue"},
		"no prompt":         {func(r *smeltd.FactoryRequest) { r.Integrate, r.From, r.Prompt = true, "epic/x", " " }, "prompt required"},
		"against alone":     {func(r *smeltd.FactoryRequest) { r.Against = "main" }, "against needs integrate"},
	} {
		t.Run(name, func(t *testing.T) {
			req := base
			tc.mod(&req)
			_, err := d.startFactory(req)
			var ae *apiError
			assert.Assert(t, errors.As(err, &ae), "got %v", err)
			assert.Equal(t, ae.status, http.StatusBadRequest)
			assert.ErrorContains(t, ae, tc.msg)
		})
	}
}

// An integrate run's attempts are fixed whatever the request says, and its
// status says which round was broad and which the re-check after the fix.
func TestFactorySessionRecordsAnIntegrateRun(t *testing.T) {
	var got factory.RunOptions
	d, root := newFactoryDaemon(t, func(_ context.Context, opts factory.RunOptions) (factory.Report, error) {
		got = opts
		wt := factory.Worktree{Path: "/data/factory/run-1", Branch: "smelt/run-1", Baseline: "base", Head: "tip", BaseBranch: "main", DefaultBranch: "main"}
		opts.OnStart("run-1", wt)
		bug := review.Finding{File: "a.go", Line: 3, Severity: "high", Body: "names differ"}
		failed := []factory.Check{{Name: "bugs", Kind: factory.KindReview, Status: factory.StatusFailed, Findings: []review.Finding{bug}}}
		passed := []factory.Check{{Name: "bugs", Kind: factory.KindReview, Status: factory.StatusPassed}}
		opts.OnEvent(factory.Event{Kind: factory.EventCollected, Round: 1, Change: factory.Change{Stat: "3 files changed", Fingerprint: "f"}})
		opts.OnEvent(factory.Event{Kind: factory.EventChecking, Round: 1})
		opts.OnEvent(factory.Event{Kind: factory.EventChecked, Round: 1, Checks: failed})
		opts.OnEvent(factory.Event{Kind: factory.EventImplementing, Round: 2, Prompt: "fix"})
		opts.OnEvent(factory.Event{Kind: factory.EventImplemented, Round: 2, Turn: factory.Turn{Summary: "renamed"}})
		opts.OnEvent(factory.Event{Kind: factory.EventCollected, Round: 2, Change: factory.Change{Stat: "4 files changed", Fingerprint: "g"}})
		opts.OnEvent(factory.Event{Kind: factory.EventChecking, Round: 2})
		opts.OnEvent(factory.Event{Kind: factory.EventChecked, Round: 2, Checks: passed})
		return factory.Report{RunID: "run-1", Worktree: wt, Started: true, Committed: true,
			Outcome: factory.Outcome{Result: factory.ResultPassed, Rounds: 2, Checks: passed}}, nil
	})

	sess, err := d.startFactory(smeltd.FactoryRequest{
		Backend: smeltd.BackendCircleCI, ProjectRoot: root, Prompt: "the spec",
		From: "epic/memory", Integrate: true, Against: "main", Attempts: 7,
	})
	assert.NilError(t, err)
	detail := waitForSessionEnd(t, d, sess.ID)

	assert.Assert(t, got.Integrate != nil)
	assert.Equal(t, got.Integrate.Against, "main")
	assert.Equal(t, got.From, "epic/memory")
	assert.Equal(t, got.Attempts, factory.IntegrateAttempts)
	assert.Equal(t, detail.Factory.Mode, "integrate")
	assert.Equal(t, detail.Factory.Against, "main")
	assert.Equal(t, detail.Factory.Attempts, factory.IntegrateAttempts)
	assert.Equal(t, len(detail.Rounds), 2)
	assert.Equal(t, detail.Rounds[0].Kind, smeltd.RoundBroad)
	assert.Assert(t, detail.Rounds[0].Implement == nil, "the broad round has no implementer turn")
	assert.Equal(t, detail.Rounds[1].Kind, smeltd.RoundRecheck)
	assert.Assert(t, detail.Rounds[1].Implement != nil && detail.Rounds[1].Implement.Fix, "the re-check follows the fix turn")

	b, err := json.Marshal(detail.Factory)
	assert.NilError(t, err)
	assert.Assert(t, strings.Contains(string(b), `"mode":"integrate"`) && strings.Contains(string(b), `"against":"main"`), string(b))
}

func TestFactorySessionThatIsNotIntegrateHasNoModeOrRoundKinds(t *testing.T) {
	d, root := newFactoryDaemon(t, scriptedRun(factory.ResultPassed))
	sess, err := d.startFactory(smeltd.FactoryRequest{Backend: smeltd.BackendCircleCI, ProjectRoot: root, Prompt: "x"})
	assert.NilError(t, err)
	detail := waitForSessionEnd(t, d, sess.ID)
	assert.Equal(t, detail.Factory.Mode, "")
	assert.Equal(t, detail.Rounds[0].Kind, smeltd.RoundKind(""))
	assert.Assert(t, !detail.Rounds[0].Implement.Fix)
}

// A round's quality check reaches the status JSON as the check's line and as
// the run's quality object, with the keys consumers read.
func TestFactoryRecorderRecordsTheQualityMeasurement(t *testing.T) {
	s := newSessionStore(context.Background())
	entry, _, busy := s.add(smeltd.Session{ProjectRoot: "/p", Factory: &smeltd.FactoryRun{}}, 1)
	assert.NilError(t, busy)
	rec := &factoryRecorder{entry: entry, rounds: map[int]int{}}

	sum := quality.Summary{
		Measured: true, Limit: 15, ErosionBefore: 27.3, ErosionAfter: 27.1,
		OverThresholdBefore: 14, OverThresholdAfter: 14, CloneLinesBefore: 120, CloneLinesAfter: 118,
		Worst: []quality.Function{{File: "a.go", Name: "T.run", Language: "go", Complexity: 22, Lines: 80}},
	}
	check := factory.FromQuality(sum, 0, nil)
	rec.event(factory.Event{Kind: factory.EventChecking, Round: 1})
	rec.checked(check)
	rec.event(factory.Event{Kind: factory.EventChecked, Round: 1, Checks: []factory.Check{check}})

	snap := entry.snapshot()
	assert.DeepEqual(t, snap.Rounds[0].Checks, []smeltd.RoundCheck{{
		Name: "quality", Status: "passed",
		Summary: "erosion 27.3% → 27.1% · functions over 15: 14 → 14 · clone lines 120 → 118",
	}})
	b, err := json.Marshal(snap.Factory.Quality)
	assert.NilError(t, err)
	assert.Equal(t, string(b), `{"measured":true,"line":"erosion 27.3% → 27.1% · functions over 15: 14 → 14 · clone lines 120 → 118",`+
		`"erosion_before":27.3,"erosion_after":27.1,"over_threshold_before":14,"over_threshold_after":14,`+
		`"clone_lines_before":120,"clone_lines_after":118,"worst":[{"func":"T.run","file":"a.go","language":"go","cc":22,"lines":80}]}`)

	rec.finish(factory.Report{Started: true, Outcome: factory.Outcome{Result: factory.ResultPassed, Rounds: 1, Checks: []factory.Check{check}}}, nil)
	assert.Equal(t, entry.snapshot().Factory.Quality.ErosionAfter, 27.1)
}

func TestFactoryRecorderReportsWhyQualityWasNotMeasured(t *testing.T) {
	s := newSessionStore(context.Background())
	entry, _, _ := s.add(smeltd.Session{ProjectRoot: "/p", Factory: &smeltd.FactoryRun{}}, 1)
	rec := &factoryRecorder{entry: entry, rounds: map[int]int{}}
	check := factory.FromQuality(quality.Summary{}, 0, errors.New("lizard is not installed in this image"))
	rec.event(factory.Event{Kind: factory.EventChecking, Round: 1})
	rec.checked(check)
	rec.event(factory.Event{Kind: factory.EventChecked, Round: 1, Checks: []factory.Check{check}})
	snap := entry.snapshot()
	assert.Equal(t, snap.Rounds[0].Checks[0].Status, smeltd.CheckNotMeasured)
	assert.Equal(t, snap.Rounds[0].Note, "1 of 1 checks passed")
	b, err := json.Marshal(snap.Factory.Quality)
	assert.NilError(t, err)
	assert.Equal(t, string(b), `{"measured":false,"reason":"lizard is not installed in this image","erosion_before":0,"erosion_after":0,"over_threshold_before":0,"over_threshold_after":0,"clone_lines_before":0,"clone_lines_after":0}`)
}

func TestFactoryRecorderOmitsQualityWhenTheCheckIsOff(t *testing.T) {
	s := newSessionStore(context.Background())
	entry, _, _ := s.add(smeltd.Session{ProjectRoot: "/p", Factory: &smeltd.FactoryRun{}}, 1)
	rec := &factoryRecorder{entry: entry, rounds: map[int]int{}}
	rec.finish(factory.Report{Started: true, Outcome: factory.Outcome{Result: factory.ResultPassed}}, nil)
	b, err := json.Marshal(entry.snapshot().Factory)
	assert.NilError(t, err)
	assert.Assert(t, !strings.Contains(string(b), "quality"), string(b))
}

func TestStatusCarriesContextPeaks(t *testing.T) {
	s := newSessionStore(context.Background())
	entry, _, _ := s.add(smeltd.Session{ProjectRoot: "/p", Factory: &smeltd.FactoryRun{}}, 1)
	rec := &factoryRecorder{entry: entry, prompts: []review.Prompt{{Name: "bugs"}}, rounds: map[int]int{}}
	rec.event(factory.Event{Kind: factory.EventImplementing, Round: 1})
	rec.event(factory.Event{Kind: factory.EventImplemented, Round: 1, Turn: factory.Turn{ContextPeak: 88412, ModelCalls: 24}})
	rec.event(factory.Event{Kind: factory.EventChecked, Round: 1, Checks: []factory.Check{
		{Name: "bugs", Kind: factory.KindReview, Status: factory.StatusPassed, Context: harness.Context{Peak: 41000, Calls: 1}},
	}})

	round := entry.snapshot().Rounds[0]
	assert.Equal(t, round.Implement.ContextPeak, 88412)
	assert.Equal(t, round.Implement.ModelCalls, 24)
	assert.Equal(t, round.Reviews[0].ContextPeak, 41000)
	b, err := json.Marshal(round)
	assert.NilError(t, err)
	assert.Assert(t, strings.Contains(string(b), `"context_peak":88412`), string(b))
	assert.Assert(t, strings.Contains(string(b), `"context_peak":41000`), string(b))

	tm := factory.Timing{ImplementContext: []factory.RoundContext{{Round: 1, Context: harness.Context{Peak: 88412, Calls: 24}}}}
	wire := wireTiming(tm)
	assert.Equal(t, wire.ContextPeakMax, 88412)
	b, err = json.Marshal(wire)
	assert.NilError(t, err)
	assert.Assert(t, strings.Contains(string(b), `"context_peak_max":88412`), string(b))
	assert.Assert(t, strings.Contains(wire.Line, "\nContext: implementer peak 88k"), wire.Line)
}

// A SARIF gate's counts reach the status JSON as {new, fixed, existing}.
func TestFactoryRecorderRecordsRatchetCounts(t *testing.T) {
	s := newSessionStore(context.Background())
	entry, _, _ := s.add(smeltd.Session{ProjectRoot: "/p", Factory: &smeltd.FactoryRun{}}, 1)
	rec := &factoryRecorder{entry: entry, rounds: map[int]int{}}
	rec.event(factory.Event{Kind: factory.EventChecking, Round: 1})
	rec.checked(factory.Check{Name: "lint", Kind: factory.KindValidate, Status: factory.StatusFailed,
		Ratchet: &sarif.Counts{New: 1, Fixed: 2, Existing: 3}})

	got := entry.snapshot().Rounds[0].Checks[0]
	assert.DeepEqual(t, got.Ratchet, &smeltd.RatchetCounts{New: 1, Fixed: 2, Existing: 3})
	b, err := json.Marshal(got)
	assert.NilError(t, err)
	assert.Assert(t, strings.Contains(string(b), `"ratchet":{"new":1,"fixed":2,"existing":3}`), string(b))
}

// The implementer's tool calls reach the status JSON under factory.activity,
// per round, with a result that arrives later set on its call.
func TestFactoryRecorderRecordsToolActivityInStatusJSON(t *testing.T) {
	s := newSessionStore(context.Background())
	entry, _, busy := s.add(smeltd.Session{ProjectRoot: "/p", Factory: &smeltd.FactoryRun{}}, 1)
	assert.NilError(t, busy)
	rec := &factoryRecorder{entry: entry, rounds: map[int]int{}}
	one := 1

	rec.event(factory.Event{Kind: factory.EventImplementing, Round: 1})
	rec.activity(harness.Activity{Tool: "Bash", Detail: "ls /x", ID: "t1"})
	rec.activity(harness.Activity{Detail: "thinking out loud"})
	rec.activity(harness.Activity{Tool: "Bash", Detail: "ls /x", ID: "t1", Update: true, Outcome: harness.CallFailed, ExitCode: &one, Error: "no such file"})
	rec.event(factory.Event{Kind: factory.EventImplementing, Round: 2})
	rec.activity(harness.Activity{Tool: "Bash", Detail: "echo hi", Outcome: harness.CallOK})

	b, err := json.Marshal(entry.detail())
	assert.NilError(t, err)
	var got struct {
		Factory struct {
			Activity json.RawMessage `json:"activity"`
		} `json:"factory"`
	}
	assert.NilError(t, json.Unmarshal(b, &got))
	assert.Equal(t, string(got.Factory.Activity), `[`+
		`{"round":1,"tool":"Bash","detail":"ls /x","outcome":"failed","exit_code":1,"error":"no such file"},`+
		`{"round":2,"tool":"Bash","detail":"echo hi","outcome":"ok"}]`)
}

func TestFactoryRecorderKeepsTheLatestToolActivity(t *testing.T) {
	s := newSessionStore(context.Background())
	entry, _, busy := s.add(smeltd.Session{ProjectRoot: "/p", Factory: &smeltd.FactoryRun{}}, 1)
	assert.NilError(t, busy)
	rec := &factoryRecorder{entry: entry, rounds: map[int]int{}}
	for i := range smeltd.MaxToolActivity + 5 {
		rec.activity(harness.Activity{Tool: "Bash", Detail: fmt.Sprint(i)})
	}
	got := entry.snapshot().Factory.Activity
	assert.Equal(t, len(got), smeltd.MaxToolActivity)
	assert.Equal(t, got[0].Detail, "5")
	assert.Equal(t, got[len(got)-1].Detail, fmt.Sprint(smeltd.MaxToolActivity+4))
}

// The daemon hands the recorder to the run as its activity hook, which is how
// a real run's tool calls reach the status.
func TestFactorySessionRecordsTheImplementersToolCalls(t *testing.T) {
	d, root := newFactoryDaemon(t, func(_ context.Context, opts factory.RunOptions) (factory.Report, error) {
		opts.OnEvent(factory.Event{Kind: factory.EventImplementing, Round: 1})
		opts.OnActivity(harness.Activity{Tool: "Bash", Detail: "ls /x", Outcome: harness.CallOK})
		return factory.Report{}, nil
	})
	sess, err := d.startFactory(smeltd.FactoryRequest{Backend: smeltd.BackendCircleCI, ProjectRoot: root, Prompt: "x"})
	assert.NilError(t, err)
	detail := waitForSessionEnd(t, d, sess.ID)
	assert.DeepEqual(t, detail.Factory.Activity, []smeltd.ToolActivity{{Round: 1, Tool: "Bash", Detail: "ls /x", Outcome: "ok"}})
}

func TestFactorySessionQueuesASyncUntilTheRunEnds(t *testing.T) {
	release := make(chan struct{})
	started := make(chan struct{})
	run := scriptedRun(factory.ResultPassed)
	d, root := newFactoryDaemon(t, func(ctx context.Context, opts factory.RunOptions) (factory.Report, error) {
		close(started)
		<-release
		rep, err := run(ctx, opts)
		// factory.Run closes the queue when its loop ends.
		opts.Syncs.Close()
		return rep, err
	})
	sess, err := d.startFactory(smeltd.FactoryRequest{Backend: smeltd.BackendCircleCI, ProjectRoot: root, Prompt: "add a flag"})
	assert.NilError(t, err)
	<-started
	entry := d.sessions.get(sess.ID)

	queued, err := entry.queueSync("main")
	assert.NilError(t, err)
	assert.Equal(t, queued.Base, "main")
	assert.Equal(t, queued.State, smeltd.SyncQueued)
	live := entry.detail().Factory.Syncs
	assert.Equal(t, len(live), 1)
	assert.Equal(t, live[0].State, smeltd.SyncQueued)

	close(release)
	ended := waitForSessionEnd(t, d, sess.ID)
	assert.Equal(t, len(ended.Factory.Syncs), 1)
	assert.Equal(t, ended.Factory.Syncs[0].State, smeltd.SyncDropped)
	assert.Assert(t, ended.Factory.Syncs[0].Reason != "")

	_, err = entry.queueSync("main")
	assert.ErrorIs(t, err, factory.ErrSyncsClosed)
}

// Completed syncs must survive the daemon's status snapshot and JSON encoding.
func TestFactoryStatusJSONIncludesCompletedSync(t *testing.T) {
	root := gitrepo.SetupGitRepo(t, "o", "r")
	git := func(args ...string) string {
		t.Helper()
		cmd := exec.CommandContext(t.Context(), "git", args...)
		cmd.Dir = root
		cmd.Env = gitrepo.GitEnv(root)
		out, err := cmd.CombinedOutput()
		assert.NilError(t, err, string(out))
		return strings.TrimSpace(string(out))
	}
	git("branch", "-M", "main")
	// A local origin keeps this fixture independent of network services.
	git("remote", "set-url", "origin", root)
	tip := git("rev-parse", "HEAD")
	git("branch", "-m", "smelt/run-1")
	git("branch", "main", tip)
	store := newSessionStore(t.Context())
	entry, _, err := store.add(smeltd.Session{ProjectRoot: root, Factory: &smeltd.FactoryRun{RunID: "run-1"}}, 1)
	assert.NilError(t, err)
	entry.syncs = &factory.SyncQueue{}
	_, err = entry.queueSync("main")
	assert.NilError(t, err)
	rec := factory.Record{RunID: "run-1", Worktree: root, Branch: "smelt/run-1", Baseline: tip, Head: tip}
	wt := factory.Worktree{Path: root, Branch: rec.Branch, Baseline: tip}
	steps := &factory.Sidecars{Base: &factory.BaseSync{Queue: entry.syncs, Root: root, Rec: &rec, Wt: &wt}}
	synced, err := steps.SyncBase(t.Context(), 2)
	assert.NilError(t, err)
	assert.Equal(t, synced.State, factory.SyncDone)
	b, err := json.Marshal(entry.detail())
	assert.NilError(t, err)
	var status smeltd.SessionDetail
	assert.NilError(t, json.Unmarshal(b, &status))
	assert.DeepEqual(t, status.Factory.Syncs, []smeltd.BaseSync{{Base: "main", State: smeltd.SyncDone, Round: 2, Commit: tip}})
}

func TestFactoryRecorderSyncMovesLiveAndFinishedBaseline(t *testing.T) {
	store := newSessionStore(t.Context())
	entry, _, err := store.add(smeltd.Session{ProjectRoot: "/p", Factory: &smeltd.FactoryRun{Baseline: "old"}}, 1)
	assert.NilError(t, err)
	r := &factoryRecorder{entry: entry, rounds: map[int]int{}}
	wt := factory.Worktree{Baseline: "new", UpdatedFrom: &factory.UpdatedFrom{Branch: "main", Commit: "tip"}}
	r.event(factory.Event{Kind: factory.EventSynced, Round: 1, Sync: &factory.Sync{State: factory.SyncDone}, Worktree: &wt})
	for _, finished := range []bool{false, true} {
		if finished {
			r.finish(factory.Report{Started: true, Worktree: wt}, nil)
		}
		f := entry.detail().Factory
		assert.Equal(t, f.Baseline, "new")
		assert.DeepEqual(t, f.UpdatedFrom, &smeltd.UpdatedFrom{Branch: "main", Commit: "tip"})
	}
}

func TestFactoryRunThatStaysAtCapacityEndsUnavailableWithAStop(t *testing.T) {
	d, root := newFactoryDaemon(t, func(ctx context.Context, opts factory.RunOptions) (factory.Report, error) {
		opts.OnStart("run-1", factory.Worktree{Path: "/w"})
		err := &factory.HarnessUnavailable{Harness: harness.Codex, Waited: 30 * time.Minute}
		return factory.Report{Started: true, Outcome: factory.Outcome{Result: factory.ResultUnavailable, Reason: err.Error(), Stop: factory.StopHarnessUnavailable}}, err
	})
	sess, err := d.startFactory(smeltd.FactoryRequest{Backend: smeltd.BackendCircleCI, ProjectRoot: root, Prompt: "ticket"})
	assert.NilError(t, err)
	detail := waitForSessionEnd(t, d, sess.ID)
	assert.Equal(t, detail.Factory.Result, smeltd.ResultUnavailable)
	assert.Equal(t, detail.Factory.Stop, smeltd.StopHarnessUnavailable)
	assert.Equal(t, detail.Factory.Reason, "codex at capacity for 30m")
	assert.Equal(t, detail.Factory.Rounds, 0)
	raw, err := json.Marshal(detail)
	assert.NilError(t, err)
	assert.Assert(t, strings.Contains(string(raw), `"stop":"harness_unavailable"`), string(raw))
}
