package factory

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/smeltdev/smelt/internal/harness"
	"github.com/smeltdev/smelt/internal/harness/claude"
	"github.com/smeltdev/smelt/internal/harness/codex"
	"github.com/smeltdev/smelt/internal/iostream"
	"github.com/smeltdev/smelt/internal/review"
	"github.com/smeltdev/smelt/internal/sidecar"
	"gotest.tools/v3/assert"
)

func TestFactoryIndividualReviewFailsOverBeforePassCancellation(t *testing.T) {
	entry := &sidecar.PoolEntry{ID: "reviewer", RepoPath: "/repo"}
	s := &Sidecars{Request: "ticket", Implementer: &Implementer{Harness: claude.Harness{}}, Failover: NewFailover([]Candidate{{Harness: claude.Harness{}, Credential: harness.Credential{Value: "first"}}, {Harness: isolatedHarness{codex.Harness{}}, Credential: harness.Credential{Value: "second"}, Model: "own"}}, 0, nil)}
	calls := 0
	s.Exec = func(ctx context.Context, e *sidecar.PoolEntry, script string, env map[string]string, output iostream.OutputFn, submitted func(string)) (int, error) {
		calls++
		if calls == 1 {
			output(iostream.StreamStderr, []byte("Failed to authenticate: 401"))
			return 1, nil
		}
		assert.Assert(t, strings.Contains(script, "codex"))
		assert.Assert(t, strings.Contains(script, "own"))
		output(iostream.StreamStdout, []byte(`{"review":"fine","findings":[]}`))
		return 0, nil
	}
	acquire := func(context.Context) (*sidecar.PoolEntry, error) { return entry, nil }
	opts := review.Options{Harness: claude.Harness{}, StructuredFindings: true}
	results, err := review.RunPass(context.Background(), acquire, func(*sidecar.PoolEntry) {}, s.Exec, []review.Prompt{{Name: "bugs", Body: "only review bugs"}}, s.reviewOptions(2, "review", opts))
	assert.NilError(t, err)
	assert.Equal(t, calls, 2)
	assert.Equal(t, results[0].Harness, harness.Codex)
	assert.Equal(t, FromReview(results[0]).Harness, harness.Codex)
	assert.Equal(t, s.Failover.Swaps()[0].Turn, "review:bugs")
	assert.Equal(t, s.Failover.Swaps()[0].Round, 2)
	assert.Equal(t, s.attempts()[0].Kind, harness.Auth)
	assert.Equal(t, s.attempts()[1].Model, "own")
}

func TestPlanningAndNotesPreserveClassifiedFailures(t *testing.T) {
	for _, phase := range []string{"plan", "notes"} {
		t.Run(phase, func(t *testing.T) {
			s := &Sidecars{Request: "ticket", Implementer: &Implementer{Harness: claude.Harness{}, Entry: &sidecar.PoolEntry{ID: "impl", RepoPath: "/repo"}}, Failover: NewFailover([]Candidate{{Harness: claude.Harness{}}}, 0, nil)}
			s.Exec = func(ctx context.Context, entry *sidecar.PoolEntry, script string, env map[string]string, output iostream.OutputFn, submitted func(string)) (int, error) {
				output(iostream.StreamStdout, []byte(`{"type":"rate_limit_event","rate_limit_info":{"status":"rejected","resetsAt":1792365600}}`+"\n"))
				return 1, nil
			}
			var err error
			if phase == "plan" {
				_, err = s.Plan(context.Background(), PlanInput{Request: "ticket"})
			} else {
				_, err = s.WriteNotes(context.Background(), NotesInput{Request: "ticket"})
			}
			var exhausted *Exhaustion
			assert.Assert(t, errors.As(err, &exhausted))
			assert.Assert(t, exhausted.ResetAt != nil)
			assert.Equal(t, s.attempts()[0].Kind, harness.RateLimited)
			assert.Equal(t, s.attempts()[0].Round, 0)
		})
	}
}
