package server

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"gotest.tools/v3/assert"

	"github.com/smeltdev/smelt/internal/factory"
	"github.com/smeltdev/smelt/internal/smeltd"
)

// drainFixture is a daemon whose factory runs block until released, with its
// idle hook counted.
type drainFixture struct {
	d       *daemon
	root    string
	handler http.Handler
	release chan struct{}
	idle    atomic.Int32
}

func newDrainFixture(t *testing.T) *drainFixture {
	t.Helper()
	f := &drainFixture{release: make(chan struct{})}
	f.d, f.root = newFactoryDaemon(t, func(ctx context.Context, _ factory.RunOptions) (factory.Report, error) {
		select {
		case <-f.release:
			return factory.Report{}, nil
		case <-ctx.Done():
			return factory.Report{}, ctx.Err()
		}
	})
	f.d.sessions.onIdle = func(uint64) { f.idle.Add(1) }
	f.handler = newServer(f.d).Handler
	return f
}

func (f *drainFixture) do(method, path, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	rec := httptest.NewRecorder()
	f.handler.ServeHTTP(rec, req)
	return rec
}

func (f *drainFixture) startRun() *httptest.ResponseRecorder {
	body, _ := json.Marshal(smeltd.FactoryRequest{Backend: smeltd.BackendCircleCI, ProjectRoot: f.root, Prompt: "add a flag"})
	return f.do(http.MethodPost, "/factory", string(body))
}

func drainResponse(t *testing.T, rec *httptest.ResponseRecorder) smeltd.DrainResponse {
	t.Helper()
	assert.Equal(t, rec.Code, http.StatusOK)
	var resp smeltd.DrainResponse
	assert.NilError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	return resp
}

func TestDrainRefusesNewRunsAndExitsWhenTheLastEnds(t *testing.T) {
	f := newDrainFixture(t)
	assert.Equal(t, f.startRun().Code, http.StatusAccepted)

	resp := drainResponse(t, f.do(http.MethodPost, "/drain", ""))
	assert.DeepEqual(t, resp, smeltd.DrainResponse{Draining: true, Active: 1})
	assert.Equal(t, f.idle.Load(), int32(0), "the daemon must not exit with a run active")

	refused := f.startRun()
	assert.Equal(t, refused.Code, http.StatusServiceUnavailable)
	var body struct {
		Code   string `json:"code"`
		Active int    `json:"active"`
	}
	assert.NilError(t, json.Unmarshal(refused.Body.Bytes(), &body))
	assert.Equal(t, body.Code, smeltd.CodeDaemonDraining)
	assert.Equal(t, body.Active, 1)

	close(f.release)
	waitUntil(t, "the daemon to go idle", func() bool { return f.idle.Load() == 1 })
}

func TestDrainWithNoRunsGoesIdleAtOnce(t *testing.T) {
	f := newDrainFixture(t)
	resp := drainResponse(t, f.do(http.MethodPost, "/drain", ""))
	assert.DeepEqual(t, resp, smeltd.DrainResponse{Draining: true})
	assert.Equal(t, f.idle.Load(), int32(1))
}

func TestDrainTwiceGivesTheSameResult(t *testing.T) {
	f := newDrainFixture(t)
	assert.Equal(t, f.startRun().Code, http.StatusAccepted)
	first := drainResponse(t, f.do(http.MethodPost, "/drain", ""))
	second := drainResponse(t, f.do(http.MethodPost, "/drain", ""))
	assert.DeepEqual(t, first, second)
	close(f.release)
	waitUntil(t, "the daemon to go idle", func() bool { return f.idle.Load() >= 1 })
}

func TestDeleteDrainAdmitsRunsAgain(t *testing.T) {
	f := newDrainFixture(t)
	assert.Equal(t, f.startRun().Code, http.StatusAccepted)
	drainResponse(t, f.do(http.MethodPost, "/drain", ""))
	assert.Equal(t, f.startRun().Code, http.StatusServiceUnavailable)

	resp := drainResponse(t, f.do(http.MethodDelete, "/drain", ""))
	assert.Equal(t, resp.Draining, false)
	assert.Equal(t, resp.Active, 1)

	// A project runs one at a time, so a different outcome than draining's 503
	// shows the flag is clear.
	assert.Equal(t, f.startRun().Code, http.StatusConflict)

	close(f.release)
	waitUntil(t, "the run to end", func() bool { return f.d.sessions.activeCount() == 0 })
	assert.Equal(t, f.idle.Load(), int32(0), "a drain that was cancelled must not shut the daemon down")
}

func TestPingReportsDrainingAndRevision(t *testing.T) {
	f := newDrainFixture(t)
	ping := func() *httptest.ResponseRecorder { return f.do(http.MethodGet, "/ping", "") }

	rec := ping()
	assert.Equal(t, rec.Header().Get(smeltd.HeaderDraining), "false")
	assert.Equal(t, strings.TrimSpace(rec.Body.String()), smeltd.BuildID(), "the body stays the build ID")

	f.do(http.MethodPost, "/drain", "")
	rec = ping()
	assert.Equal(t, rec.Header().Get(smeltd.HeaderDraining), "true")
	assert.Equal(t, strings.TrimSpace(rec.Body.String()), smeltd.BuildID())
	_, present := rec.Header()[smeltd.HeaderRevision]
	assert.Assert(t, present, "the revision header is always sent")
}

// The idle hook must wait for done, not for the Finished state, which is set
// before a run has committed its work.
func TestDrainWaitsForTheRunToWindDown(t *testing.T) {
	s := newSessionStore(context.Background())
	var idle atomic.Int32
	s.onIdle = func(uint64) { idle.Add(1) }
	entry, _, err := s.add(smeltd.Session{ProjectRoot: "/p", Factory: &smeltd.FactoryRun{}}, 1)
	assert.NilError(t, err)
	assert.Equal(t, s.drain(), 1)

	entry.mu.Lock()
	entry.s.State = smeltd.SessionDone
	entry.mu.Unlock()
	s.ended()
	assert.Equal(t, idle.Load(), int32(0), "finished is not wound down")

	close(entry.done)
	s.ended()
	assert.Equal(t, idle.Load(), int32(1))
}

func TestDrainRetainsFinishedRunsUntilDone(t *testing.T) {
	s := newSessionStore(t.Context())
	first, _, err := s.add(smeltd.Session{ProjectRoot: "/p"}, 1)
	assert.NilError(t, err)
	first.s.State = smeltd.SessionDone
	for range MaxSessionsPerProject {
		e, _, err := s.add(smeltd.Session{ProjectRoot: "/p"}, 1)
		assert.NilError(t, err)
		e.s.State = smeltd.SessionDone
		close(e.done)
	}
	assert.Equal(t, s.activeCount(), 1, "retention must not hide unfinished teardown")
	assert.Assert(t, s.get(first.s.ID) == first)
	assert.Equal(t, s.drain(), 1)
	close(first.done)
}

// Capture the idle token as a timer would, then exercise every boundary without
// relying on scheduler timing. A cancelled drain cannot cancel renewed work or
// shorten the flush delay of a later drain.
func TestDrainDelayedCommitRevalidatesAdmission(t *testing.T) {
	s := newSessionStore(t.Context())
	var token uint64
	s.onIdle = func(generation uint64) { token = generation }
	assert.Equal(t, s.drain(), 0)
	old := token
	s.undrain()
	entry, ctx, err := s.add(smeltd.Session{ProjectRoot: "/p"}, 1)
	assert.NilError(t, err)
	var shutdowns int
	shutdown := func() { shutdowns++ }
	s.commitDrain(old, shutdown)
	assert.Equal(t, shutdowns, 0)
	assert.NilError(t, ctx.Err())
	assert.Equal(t, s.drain(), 1)
	s.commitDrain(old, shutdown)
	assert.Equal(t, shutdowns, 0)
	close(entry.done)
	s.ended()
	s.commitDrain(old, shutdown)
	assert.Equal(t, shutdowns, 0, "old timer cannot commit a later drain")
	s.commitDrain(token, shutdown)
	assert.Equal(t, shutdowns, 1)
	s.undrain()
	_, _, err = s.add(smeltd.Session{ProjectRoot: "/other"}, 1)
	var draining *drainingError
	assert.Assert(t, errors.As(err, &draining), "admission must stay closed after shutdown commits: %v", err)
	s.commitDrain(token, shutdown)
	assert.Equal(t, shutdowns, 1, "shutdown commits only once")
}

func TestDrainCommitRechecksOpenDoneChannels(t *testing.T) {
	s := newSessionStore(t.Context())
	entry, ctx, err := s.add(smeltd.Session{ProjectRoot: "/p"}, 1)
	assert.NilError(t, err)
	s.drain()
	called := false
	shutdown := func() { called = true }
	s.commitDrain(s.drainGeneration, shutdown)
	assert.Assert(t, !called)
	assert.NilError(t, ctx.Err())
	finish(entry, smeltd.SessionDone)
	s.commitDrain(s.drainGeneration, shutdown)
	assert.Assert(t, called)
}
