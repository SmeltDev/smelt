package factory

import (
	"context"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"gotest.tools/v3/assert"

	"github.com/smeltdev/smelt/internal/config"
	"github.com/smeltdev/smelt/internal/iostream"
	"github.com/smeltdev/smelt/internal/review"
	"github.com/smeltdev/smelt/internal/sidecar"
)

// waitFixture is a Sidecars whose reviewers arrive through WaitReviewers, the
// way a run's do. The fixture's own Prepare set up fixed reviewers; they are
// handed back by the wait so the baseline commits can be counted from zero.
type waitFixture struct {
	*Sidecars
	impl      *sidecar.PoolEntry
	waits     atomic.Int32
	baselines atomic.Int32
	installs  atomic.Int32
	waitErr   atomic.Pointer[error]
	// gate, when set, blocks WaitReviewers until it is closed.
	gate chan struct{}
}

func newWaitFixture(t *testing.T) *waitFixture {
	t.Helper()
	f := &waitFixture{}
	s, impl, reviewers, _ := newSidecarsFixture(t, func(run review.Execer) review.Execer {
		return func(ctx context.Context, e *sidecar.PoolEntry, script string, env map[string]string, out iostream.OutputFn, sub func(string)) (int, error) {
			if e.ID != "impl" {
				if strings.Contains(script, "smelt factory baseline") {
					f.baselines.Add(1)
				}
				if strings.Contains(script, "make deps") {
					f.installs.Add(1)
				}
			}
			return run(ctx, e, script, env, out, sub)
		}
	})
	f.Sidecars, f.impl = s, impl
	s.Install = "true # make deps"
	s.Prompts = append(s.Prompts, review.Prompt{Name: "requirements", Body: "check"})
	s.Reviewers, s.reviewersReady = nil, false
	f.baselines.Store(0)
	s.WaitReviewers = func(ctx context.Context) ([]*sidecar.PoolEntry, error) {
		f.waits.Add(1)
		if f.gate != nil {
			select {
			case <-f.gate:
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		}
		if err := f.waitErr.Load(); err != nil {
			return nil, *err
		}
		return reviewers, nil
	}
	return f
}

func TestPrepareDoesNotWaitForTheReviewers(t *testing.T) {
	f := newWaitFixture(t)
	f.gate = make(chan struct{})
	defer close(f.gate)
	assert.NilError(t, f.Prepare(context.Background()))
	assert.Equal(t, f.waits.Load(), int32(0))
	assert.Equal(t, f.baselines.Load(), int32(0))
}

func TestReviewPlanAndCheckWaitForTheReviewersOnce(t *testing.T) {
	ctx := context.Background()
	f := newWaitFixture(t)
	assert.NilError(t, f.Prepare(ctx))

	_, err := f.ReviewPlan(ctx, "request", Plan{Summary: "s"})
	assert.NilError(t, err)
	assert.Equal(t, f.waits.Load(), int32(1))
	assert.Equal(t, f.baselines.Load(), int32(2), "one baseline per reviewer")
	assert.Equal(t, f.installs.Load(), int32(2), "reviewers install their dependencies")

	writeFile(t, f.impl.RepoPath, "main.go", "package main\n\n// v1\n")
	_, err = f.Collect(ctx)
	assert.NilError(t, err)
	for round := 1; round <= 3; round++ {
		_, err = f.Check(ctx, round)
		assert.NilError(t, err)
	}
	assert.Equal(t, f.waits.Load(), int32(1))
	assert.Equal(t, f.baselines.Load(), int32(2))
	assert.Equal(t, f.installs.Load(), int32(2))
}

func TestFirstCheckWaitsForTheReviewers(t *testing.T) {
	ctx := context.Background()
	f := newWaitFixture(t)
	assert.NilError(t, f.Prepare(ctx))
	_, err := f.Check(ctx, 1)
	assert.NilError(t, err)
	assert.Equal(t, f.waits.Load(), int32(1))
	assert.Equal(t, f.baselines.Load(), int32(2))
}

func TestReviewPlanBlocksUntilTheReviewersAreReady(t *testing.T) {
	ctx := context.Background()
	f := newWaitFixture(t)
	f.gate = make(chan struct{})
	assert.NilError(t, f.Prepare(ctx))

	done := make(chan error, 1)
	go func() {
		_, err := f.ReviewPlan(ctx, "request", Plan{Summary: "s"})
		done <- err
	}()
	select {
	case err := <-done:
		t.Fatalf("the plan review ran before the reviewers were ready: %v", err)
	default:
	}
	close(f.gate)
	assert.NilError(t, <-done)
}

func TestWaitReviewersErrorFailsTheFirstStepThatNeedsThem(t *testing.T) {
	boom := errors.New("image will not start")
	tests := []struct {
		name string
		step func(*waitFixture) error
	}{
		{"plan review", func(f *waitFixture) error {
			_, err := f.ReviewPlan(context.Background(), "r", Plan{Summary: "s"})
			return err
		}},
		{"first check", func(f *waitFixture) error {
			_, err := f.Check(context.Background(), 1)
			return err
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newWaitFixture(t)
			assert.NilError(t, f.Prepare(context.Background()))
			f.waitErr.Store(&boom)
			err := tt.step(f)
			assert.ErrorContains(t, err, "get the reviewer sidecars ready")
			assert.ErrorIs(t, err, boom)
			assert.Assert(t, !f.reviewersReady)

			// A failed wait is retried rather than remembered.
			f.waitErr.Store(nil)
			assert.NilError(t, tt.step(f))
			assert.Equal(t, f.waits.Load(), int32(2))
			assert.Assert(t, f.reviewersReady)
		})
	}
}

func TestGatesFirstWaitsForTheReviewersOnlyWhenGatesPass(t *testing.T) {
	tests := []struct {
		gate      string
		wantWaits int32
	}{
		{"exit 1", 0},
		{"true", 1},
	}
	for _, tt := range tests {
		t.Run(tt.gate, func(t *testing.T) {
			ctx := context.Background()
			f := newWaitFixture(t)
			f.GatesFirst = true
			f.Commands = []config.Command{{Name: "lint", Run: tt.gate}}
			assert.NilError(t, f.Prepare(ctx))
			_, err := f.Check(ctx, 1)
			assert.NilError(t, err)
			assert.Equal(t, f.waits.Load(), tt.wantWaits)
		})
	}
}

func TestReviewPlanWithoutPlanReviewsDoesNotWait(t *testing.T) {
	f := newWaitFixture(t)
	f.Prompts = []review.Prompt{{Name: "style", Body: "b"}}
	assert.NilError(t, f.Prepare(context.Background()))
	found, err := f.ReviewPlan(context.Background(), "r", Plan{Summary: "s"})
	assert.NilError(t, err)
	assert.Equal(t, len(found), 0)
	assert.Equal(t, f.waits.Load(), int32(0))
}

func TestFixedReviewersAreSetUpByPrepare(t *testing.T) {
	var mu sync.Mutex
	baselines := map[string]int{}
	s, _, _, _ := newSidecarsFixture(t, func(run review.Execer) review.Execer {
		return func(ctx context.Context, e *sidecar.PoolEntry, script string, env map[string]string, out iostream.OutputFn, sub func(string)) (int, error) {
			if strings.Contains(script, "smelt factory baseline") {
				mu.Lock()
				baselines[e.ID]++
				mu.Unlock()
			}
			return run(ctx, e, script, env, out, sub)
		}
	})
	assert.Assert(t, s.reviewersReady)
	assert.DeepEqual(t, baselines, map[string]int{"impl": 1, "rev-1": 1, "rev-2": 1})
}
