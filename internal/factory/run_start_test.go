package factory

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"gotest.tools/v3/assert"

	"github.com/smeltdev/smelt/internal/circleci"
	"github.com/smeltdev/smelt/internal/config"
	"github.com/smeltdev/smelt/internal/iostream"
	"github.com/smeltdev/smelt/internal/review"
	"github.com/smeltdev/smelt/internal/sidecar"
)

// gatedBackend is a localBackend whose pool is made of clones of a seed, as a
// real one is, and holds back the clone named for blocked until gate is
// closed. blockedDone is closed once that clone exists.
type gatedBackend struct {
	*localBackend
	blocked     string
	gate        chan struct{}
	blockedDone chan struct{}

	mu   sync.Mutex
	seed string
}

func newGatedBackend(dir, blocked string) *gatedBackend {
	return &gatedBackend{
		localBackend: &localBackend{dir: dir},
		blocked:      blocked,
		gate:         make(chan struct{}),
		blockedDone:  make(chan struct{}),
	}
}

func (b *gatedBackend) Snapshot(context.Context, string, string) (string, error) {
	return "snapshot", nil
}

func (b *gatedBackend) Create(ctx context.Context, name, image, repoPath string) (sidecar.Instance, error) {
	if image != "snapshot" {
		inst, err := b.localBackend.Create(ctx, name, image, repoPath)
		b.mu.Lock()
		b.seed = inst.ID
		b.mu.Unlock()
		return inst, err
	}
	isBlocked := strings.HasSuffix(name, b.blocked)
	if isBlocked {
		select {
		case <-b.gate:
		case <-ctx.Done():
			return sidecar.Instance{}, ctx.Err()
		}
		defer close(b.blockedDone)
	}
	inst, err := b.localBackend.Create(ctx, name+"-clone", "", repoPath)
	if err != nil {
		return inst, err
	}
	b.mu.Lock()
	seed := b.seed
	b.mu.Unlock()
	out, err := exec.CommandContext(ctx, "cp", "-a", b.workspace(seed)+"/.", b.workspace(inst.ID)).CombinedOutput()
	if err != nil {
		return inst, &os.PathError{Op: "cp", Path: string(out), Err: err}
	}
	return inst, nil
}

func (b *gatedBackend) release() {
	select {
	case <-b.gate:
	default:
		close(b.gate)
	}
}

func skipWithoutTools(t *testing.T) {
	t.Helper()
	for _, bin := range []string{"rsync", "git", "cp"} {
		if _, err := exec.LookPath(bin); err != nil {
			t.Skipf("%s is not installed", bin)
		}
	}
}

// A run starts its implementer while a reviewer sidecar is still being made,
// and says so. A continued run has its work back in the worktree by then,
// without having waited for that sidecar.
func TestRunStartsTheImplementerBeforeTheReviewersAreReady(t *testing.T) {
	skipWithoutTools(t)
	t.Setenv(config.EnvHome, t.TempDir())
	root, rec := finishedRun(t)

	home := t.TempDir()
	installFakeClaude(t, home, streamOK)
	backend := newGatedBackend(t.TempDir(), "-1")
	t.Cleanup(backend.release)

	var (
		mu        sync.Mutex
		statuses  []string
		firstExec sync.Once
		atStart   struct {
			stillBlocked bool
			work         string
			statuses     []string
		}
	)
	run := backend.inWorkspace(withoutPkill(localExec(home)))
	execFn := func(ctx context.Context, e *sidecar.PoolEntry, script string, env map[string]string, out circleci.OutputFn, sub func(string)) (int, error) {
		firstExec.Do(func() {
			select {
			case <-backend.blockedDone:
			default:
				atStart.stillBlocked = true
			}
			b, _ := os.ReadFile(filepath.Join(rec.Worktree, "flag.go"))
			atStart.work = string(b)
			mu.Lock()
			atStart.statuses = append([]string(nil), statuses...)
			mu.Unlock()
			backend.release()
		})
		return run(ctx, e, script, env, out, sub)
	}

	// A pool that waited for every member before restoring the work or
	// starting would never get here: the gate opens only once it has.
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	_, err := Run(ctx, RunOptions{
		Root:       root,
		Continue:   &Continuation{From: rec},
		Attempts:   1,
		Reviewers:  1,
		Backend:    backend,
		Exec:       execFn,
		Credential: review.Credential{EnvVar: "ANTHROPIC_API_KEY", Value: "k"},
		Status: func(_ iostream.Level, msg string) {
			mu.Lock()
			defer mu.Unlock()
			statuses = append(statuses, msg)
		},
	})
	assert.NilError(t, err)

	assert.Assert(t, atStart.stillBlocked, "the implementer's first call came after the reviewer sidecar was ready")
	assert.Equal(t, atStart.work, "package main\n", "the work was not back in the worktree when the implementer started")
	assert.Assert(t, contains(strings.Join(atStart.statuses, "\n"), "implementer started (reviewers still starting)"), "%v", atStart.statuses)
}

// reviewersOf waits for the pool before it lists the members, which only exist
// once they are made.
func TestReviewersOfWaitsForThePool(t *testing.T) {
	skipWithoutTools(t)
	t.Setenv(config.EnvHome, t.TempDir())
	workDir := setupRepo(t)
	backend := newGatedBackend(t.TempDir(), "-1")
	t.Cleanup(backend.release)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	pool, err := sidecar.NewPool(ctx, nil, sidecar.PoolOptions{
		Size: 2, Name: "factory-run", Backend: backend, WorkDir: workDir, StateDir: t.TempDir(),
	}, func(iostream.Level, string) {})
	assert.NilError(t, err)
	t.Cleanup(func() { pool.Close(context.Background()) })
	impl, err := pool.Acquire(ctx)
	assert.NilError(t, err)
	assert.Assert(t, !pool.Synced())

	type result struct {
		members []*sidecar.PoolEntry
		err     error
	}
	done := make(chan result, 1)
	go func() {
		m, err := reviewersOf(pool, impl)(ctx)
		done <- result{m, err}
	}()
	select {
	case r := <-done:
		t.Fatalf("reviewersOf returned before the pool was synced: %+v", r)
	case <-time.After(200 * time.Millisecond):
	}

	backend.release()
	r := <-done
	assert.NilError(t, r.err)
	assert.Assert(t, pool.Synced())
	assert.Equal(t, len(r.members), 1)
	assert.Assert(t, r.members[0].ID != impl.ID)
	assert.Equal(t, r.members[0].RepoPath, impl.RepoPath)
}
