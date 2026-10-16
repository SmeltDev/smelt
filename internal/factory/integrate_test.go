package factory

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"gotest.tools/v3/assert"

	"github.com/smeltdev/smelt/internal/review"
	"github.com/smeltdev/smelt/internal/testing/gitrepo"
)

const wantBroadPreamble = "This branch combines several finished tickets that were each reviewed against their own ticket. Nothing has reviewed it as a whole, and that is your job.\n\n" +
	"Does the combined change meet every requirement and acceptance item of the spec below? Do the parts fit together: names, types, data flow, duplicated helpers, a seam one part assumes and another does not provide? " +
	"The requested change is the whole spec; judge the combined change, not one part. Do not implement changes yourself.\n\n" +
	"## Requested change\n\nBuild memory.\n\n" + changeUnderReview

func TestBroadPreambleGolden(t *testing.T) {
	assert.Equal(t, broadPreamble("  Build memory.\n"), wantBroadPreamble)
}

func TestRecheckPreambleGolden(t *testing.T) {
	prior := priorRound{
		Round:    1,
		Findings: []review.Finding{{File: "a.go", Line: 7, Severity: "high", Body: "names differ"}},
		Reply:    "Renamed it.",
	}
	want := "You reviewed this branch as a whole before, and the implementer has made one round of fixes. This is a focused re-check, not a second broad review. " +
		"Check only whether the findings below are fixed, and whether the fix broke something nearby. Check the reply against the code rather than taking it on trust. " +
		"Raise nothing else. Do not implement changes yourself.\n\n" +
		"## Your earlier findings\n\n- [high] a.go:7: names differ\n\nImplementer's reply:\n\nRenamed it.\n\n" +
		"## Requested change\n\nBuild memory.\n\n" + changeUnderReview
	assert.Equal(t, recheckPreamble("Build memory.", prior), want)
}

func TestIntegratePromptsSelectByRound(t *testing.T) {
	prompts := []review.Prompt{{Name: "bugs", Body: "find bugs"}, {Name: "style", Body: "check style"}, {Name: "docs", Body: "check docs"}}
	worth := []review.Finding{{File: "a.go", Line: 1, Severity: "medium", Body: "x"}}
	history := map[string][]priorRound{
		"bugs":  {{Round: 1, Findings: worth, Reply: "fixed"}},
		"style": {{Round: 1}},
		// docs errored in round 1, so it has no history.
	}

	first, carried := integratePrompts("spec", prompts, nil, 1)
	assert.Equal(t, len(first), 3)
	assert.Equal(t, len(carried), 0)
	for i, p := range prompts {
		assert.Equal(t, first[i].Body, wantBroadSpec+"\n\n"+severityScope+"\n\n## Review instructions\n\n"+p.Body)
	}

	second, carried := integratePrompts("spec", prompts, history, 2)
	assert.Equal(t, len(second), 2)
	assert.Equal(t, second[0].Name, "bugs")
	assert.Equal(t, second[0].Body, recheckPreamble("spec", history["bugs"][0])+"\n\n"+severityScope+"\n\n## Review instructions\n\nfind bugs")
	assert.Equal(t, second[1].Name, "docs")
	assert.Assert(t, strings.HasPrefix(second[1].Body, broadPreamble("spec")), "a review with no broad pass yet gets one")
	assert.DeepEqual(t, carried, []Check{{Name: "style", Kind: KindReview, Status: StatusPassed}})
	// The project's own prompts are not rewritten.
	assert.Equal(t, prompts[0].Body, "find bugs")
}

var wantBroadSpec = broadPreamble("spec")

func TestIntegrateLoopPassingBroadCheckSpendsNoTurn(t *testing.T) {
	steps := &fakeSteps{current: change(3), checks: [][]Check{{pass}}}
	out, err := Loop{Attempts: IntegrateAttempts, CheckFirst: true}.Run(context.Background(), steps, integrateFixLead)
	assert.NilError(t, err)
	assert.Equal(t, out.Result, ResultPassed)
	assert.Equal(t, out.Rounds, 1)
	assert.Equal(t, len(steps.prompts), 0)
}

func TestIntegrateLoopFailingBroadCheckGetsOneFixTurnThenARecheck(t *testing.T) {
	steps := &fakeSteps{current: change(3), changes: []Change{change(4)}, checks: [][]Check{{pass, fail}, {pass, pass}}}
	out, err := Loop{Attempts: IntegrateAttempts, CheckFirst: true}.Run(context.Background(), steps, integrateFixLead)
	assert.NilError(t, err)
	assert.Equal(t, out.Result, ResultPassed)
	assert.Equal(t, out.Rounds, 2)
	assert.Equal(t, len(steps.prompts), 1)
	assert.Assert(t, strings.HasPrefix(steps.prompts[0], integrateFixLead+"\n\n"), steps.prompts[0])
	assert.Assert(t, strings.Contains(steps.prompts[0], "a.go:3: nil deref"), steps.prompts[0])
}

func TestIntegrateLoopFailingRecheckIsExhaustedWithoutASecondBroadRound(t *testing.T) {
	steps := &fakeSteps{current: change(3), changes: []Change{change(4), change(5)}, checks: [][]Check{{fail}, {fail}, {pass}}}
	out, err := Loop{Attempts: IntegrateAttempts, CheckFirst: true}.Run(context.Background(), steps, integrateFixLead)
	assert.NilError(t, err)
	assert.Equal(t, out.Result, ResultExhausted)
	assert.Equal(t, out.Rounds, 2)
	assert.Equal(t, steps.checked, 2)
	assert.Equal(t, len(steps.prompts), 1)
}

// TestIntegrateCheckRechecksOnlyReviewsWithFindings runs Sidecars.Check on
// real fakes: round 1 runs both reviews, round 2 only the one that found
// something, and the other is carried forward as passed.
func TestIntegrateCheckRechecksOnlyReviewsWithFindings(t *testing.T) {
	ctx := context.Background()
	s, impl, _, home := newSidecarsFixture(t, nil)
	s.Integrate = true
	writeFile(t, impl.RepoPath, "main.go", "package main\n\n// v1\n")
	_, err := s.Collect(ctx)
	assert.NilError(t, err)

	first, err := s.Check(ctx, 1)
	assert.NilError(t, err)
	assert.Equal(t, seenReviews(t, home), 2)
	for _, c := range first {
		assert.Assert(t, c.SidecarID != "", "%s ran in the broad round", c.Name)
	}

	// The fake reviewers report nothing, so say bugs had a finding.
	s.history = map[string][]priorRound{
		"bugs":  {{Round: 1, Findings: []review.Finding{{File: "main.go", Line: 1, Severity: "high", Body: "x"}}}},
		"style": {{Round: 1}},
	}
	second, err := s.Check(ctx, 2)
	assert.NilError(t, err)
	assert.Equal(t, seenReviews(t, home), 3, "only bugs was reviewed again")
	ran := map[string]bool{}
	for _, c := range second {
		ran[c.Name] = c.SidecarID != ""
		assert.Equal(t, c.Status, StatusPassed)
	}
	assert.DeepEqual(t, ran, map[string]bool{"bugs": true, "style": false})
}

func TestRunRejectsIntegrateWithContinueOrWithoutFrom(t *testing.T) {
	for name, opts := range map[string]RunOptions{
		"continue":   {Integrate: &IntegrateOptions{Against: "main"}, Continue: &Continuation{}},
		"no from":    {Integrate: &IntegrateOptions{Against: "main"}},
		"no against": {Integrate: &IntegrateOptions{}, From: "epic"},
	} {
		_, err := Run(context.Background(), opts)
		assert.Assert(t, err != nil, name)
	}
}

// epicProject is a repository with main and an epic branch of three commits
// off it, each adding a file; main then moves on.
func epicProject(t *testing.T) (root, base, tip string) {
	t.Helper()
	root = localRepo(t)
	writeFile(t, root, "README.md", "# repo\n")
	gitrepo.AddFile(t, root, "README.md")
	gitOutput(t, root, "commit", "-m", "base")
	gitOutput(t, root, "branch", "-M", "main")
	base = gitOutput(t, root, "rev-parse", "HEAD")
	gitOutput(t, root, "checkout", "-b", "epic/memory")
	for _, f := range []string{"a.go", "b.go", "c.go"} {
		writeFile(t, root, f, "package x\n")
		gitrepo.AddFile(t, root, f)
		gitOutput(t, root, "commit", "-m", f)
	}
	tip = gitOutput(t, root, "rev-parse", "HEAD")
	gitOutput(t, root, "checkout", "main")
	writeFile(t, root, "other.go", "package x\n")
	gitrepo.AddFile(t, root, "other.go")
	gitOutput(t, root, "commit", "-m", "main moves on")
	return root, base, tip
}

func TestResolveIntegrateFindsTheMergeBase(t *testing.T) {
	root, base, tip := epicProject(t)
	in, err := ResolveIntegrate(context.Background(), root, "epic/memory", "main")
	assert.NilError(t, err)
	assert.Equal(t, in.Tip, tip)
	assert.Equal(t, in.Base, base, "main's later commit is not part of the epic")
	assert.Equal(t, in.AgainstBranch, "main")

	_, err = ResolveIntegrate(context.Background(), root, "epic/memory", "nope")
	assert.Assert(t, errors.Is(err, ErrUnknownRef), "%v", err)

	_, err = ResolveIntegrate(context.Background(), root, base, "main")
	assert.Assert(t, errors.Is(err, ErrNothingToIntegrate), "%v", err)
}

func TestIntegrateWorktreeChangeIsMergeBaseToTip(t *testing.T) {
	ctx := context.Background()
	root, base, tip := epicProject(t)
	wt, err := CreateWorktree(ctx, WorktreeOptions{
		Root: root, Path: filepath.Join(t.TempDir(), "wt"), RunID: "run-1", Prompt: "build memory",
		From: "epic/memory", Against: "main",
	})
	assert.NilError(t, err)

	assert.Equal(t, wt.Baseline, base)
	assert.Equal(t, wt.Head, tip)
	assert.Equal(t, gitOutput(t, wt.Path, "rev-parse", "HEAD"), tip)
	assert.Equal(t, wt.BaseBranch, "main", "the PR goes into what the epic was compared with")
	assert.Equal(t, wt.From.Commit, tip)

	stat, err := wt.DiffStat(ctx)
	assert.NilError(t, err)
	for _, f := range []string{"a.go", "b.go", "c.go"} {
		assert.Assert(t, strings.Contains(stat, f), "%s missing from the diff:\n%s", f, stat)
	}
	assert.Assert(t, !strings.Contains(stat, "other.go"), stat)
}

func TestIntegrateNotesSayTheRequestIsTheSpec(t *testing.T) {
	assert.Assert(t, strings.Contains(notesPrompt(NotesInput{Request: "spec", Integrate: true}), "combines several finished tickets"))
	assert.Assert(t, !strings.Contains(notesPrompt(NotesInput{Request: "spec"}), "finished tickets"))
}
