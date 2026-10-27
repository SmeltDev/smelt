package server

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"gotest.tools/v3/assert"

	"github.com/smeltdev/smelt/internal/smeltd"
)

// finish settles an entry the way a session run would, so the store treats it
// as ended.
func finish(e *sessionEntry, state smeltd.SessionState) {
	e.mu.Lock()
	e.s.State = state
	e.mu.Unlock()
	close(e.done)
}

func TestSessionStoreAllowsOneActiveSessionPerProject(t *testing.T) {
	store := newSessionStore(t.Context())

	first, _, busy := store.add(smeltd.Session{ProjectRoot: "/p"}, 1)
	assert.NilError(t, busy)
	assert.Equal(t, len(first.snapshot().Stages), len(newStages()))

	_, _, busy = store.add(smeltd.Session{ProjectRoot: "/p"}, 1)
	assert.Assert(t, busy != nil, "a second session on the same project must be refused")

	_, _, busy = store.add(smeltd.Session{ProjectRoot: "/other"}, 1)
	assert.NilError(t, busy, "another project is independent")

	finish(first, smeltd.SessionDone)
	_, _, busy = store.add(smeltd.Session{ProjectRoot: "/p"}, 1)
	assert.NilError(t, busy, "an ended session no longer blocks the project")
}

func TestSessionStoreKeepsTheNewestFinishedSessionsNewestFirst(t *testing.T) {
	store := newSessionStore(t.Context())

	var ids []string
	for range MaxSessionsPerProject + 3 {
		e, _, busy := store.add(smeltd.Session{ProjectRoot: "/p"}, 1)
		assert.NilError(t, busy)
		ids = append(ids, e.snapshot().ID)
		finish(e, smeltd.SessionDone)
	}

	got := store.forProject("/p")
	assert.Equal(t, len(got), MaxSessionsPerProject)
	assert.Equal(t, got[0].ID, ids[len(ids)-1], "newest first")
	assert.Assert(t, store.get(ids[0]) == nil, "the oldest finished session is dropped")
}

func TestSessionAPIListsGetsAndCancels(t *testing.T) {
	d := newTestDaemon()
	mux := http.NewServeMux()
	registerSessionRoutes(mux, d)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	entry, ctx, busy := d.sessions.add(smeltd.Session{ProjectRoot: "/p"}, 1)
	assert.NilError(t, busy)
	id := entry.snapshot().ID
	// Cancelling cancels the entry's context; settle it as a run would.
	go func() {
		<-ctx.Done()
		finish(entry, smeltd.SessionCancelled)
	}()

	resp, err := http.Get(srv.URL + "/factory/" + id)
	assert.NilError(t, err)
	defer func() { _ = resp.Body.Close() }()
	assert.Equal(t, resp.StatusCode, http.StatusOK)
	var detail smeltd.SessionDetail
	assert.NilError(t, json.NewDecoder(resp.Body).Decode(&detail))
	assert.Equal(t, detail.ID, id)

	missing, err := http.Get(srv.URL + "/factory/nope")
	assert.NilError(t, err)
	defer func() { _ = missing.Body.Close() }()
	assert.Equal(t, missing.StatusCode, http.StatusNotFound)

	for range 2 { // cancelling twice is not an error
		cancel, err := http.Post(srv.URL+"/factory/"+id+"/cancel", "", nil)
		assert.NilError(t, err)
		_ = cancel.Body.Close()
		assert.Equal(t, cancel.StatusCode, http.StatusAccepted)
	}
	<-entry.done
	assert.Equal(t, entry.snapshot().State, smeltd.SessionCancelled)
}

func TestSessionStoreLimitsActiveSessionsPerProject(t *testing.T) {
	store := newSessionStore(t.Context())

	first, _, busy := store.add(smeltd.Session{ProjectRoot: "/p"}, 2)
	assert.NilError(t, busy)
	_, _, busy = store.add(smeltd.Session{ProjectRoot: "/p"}, 2)
	assert.NilError(t, busy)
	_, _, busy = store.add(smeltd.Session{ProjectRoot: "/p"}, 2)
	assert.Error(t, busy, "project already has 2 active run(s) (limit 2; raise factory.maxConcurrent)")

	finish(first, smeltd.SessionDone)
	_, _, busy = store.add(smeltd.Session{ProjectRoot: "/p"}, 2)
	assert.NilError(t, busy, "a finished run frees its slot")
}

func TestSessionStoreRefusalWithLimitOneNamesTheLimit(t *testing.T) {
	store := newSessionStore(t.Context())
	_, _, busy := store.add(smeltd.Session{ProjectRoot: "/p"}, 0)
	assert.NilError(t, busy)
	_, _, busy = store.add(smeltd.Session{ProjectRoot: "/p"}, 0)
	assert.Error(t, busy, "project already has 1 active run(s) (limit 1; raise factory.maxConcurrent)")
}

func TestSessionStoreEvictionNeverDropsActiveSessions(t *testing.T) {
	store := newSessionStore(t.Context())
	const limit = 12
	var active []string
	for range limit {
		e, _, busy := store.add(smeltd.Session{ProjectRoot: "/p"}, limit)
		assert.NilError(t, busy)
		active = append(active, e.snapshot().ID)
	}
	for _, id := range active {
		assert.Assert(t, store.get(id) != nil)
	}
	for _, id := range active {
		finish(store.get(id), smeltd.SessionDone)
	}
	e, _, busy := store.add(smeltd.Session{ProjectRoot: "/p"}, limit)
	assert.NilError(t, busy)
	assert.Equal(t, len(store.forProject("/p")), limit+1, "finished runs are kept while the cap allows")
	assert.Assert(t, store.get(e.snapshot().ID) != nil)
}

func busyCode(t *testing.T, err error) *busyError {
	t.Helper()
	var busy *busyError
	assert.Assert(t, errors.As(err, &busy), "got %v", err)
	return busy
}

func TestSessionStoreMachineLimitCountsEveryProject(t *testing.T) {
	store := newSessionStore(t.Context())
	store.limits = MachineLimits{MaxRuns: 3}

	var first *sessionEntry
	for _, root := range []string{"/work/smelt", "/work/smelt", "/work/smelt-pilot"} {
		e, _, err := store.add(smeltd.Session{ProjectRoot: root}, 5)
		assert.NilError(t, err)
		if first == nil {
			first = e
		}
	}

	for _, root := range []string{"/work/smelt", "/work/smelt-pilot", "/work/other"} {
		_, _, err := store.add(smeltd.Session{ProjectRoot: root}, 5)
		busy := busyCode(t, err)
		assert.Equal(t, busy.code, smeltd.CodeDaemonBusy)
		assert.Equal(t, busy.Error(), "smelt is running 3 of 3 runs (smelt 2, smelt-pilot 1); try again when one finishes")
	}

	finish(first, smeltd.SessionDone)
	_, _, err := store.add(smeltd.Session{ProjectRoot: "/work/other"}, 5)
	assert.NilError(t, err, "a finished run no longer counts")
}

func TestSessionStoreMachineLimitComesBeforeProjectLimit(t *testing.T) {
	store := newSessionStore(t.Context())
	store.limits = MachineLimits{MaxRuns: 1}
	_, _, err := store.add(smeltd.Session{ProjectRoot: "/p"}, 1)
	assert.NilError(t, err)
	_, _, err = store.add(smeltd.Session{ProjectRoot: "/p"}, 1)
	assert.Equal(t, busyCode(t, err).code, smeltd.CodeDaemonBusy)
}

func TestSessionStoreProjectLimitRefusalCarriesItsCode(t *testing.T) {
	store := newSessionStore(t.Context())
	_, _, err := store.add(smeltd.Session{ProjectRoot: "/p"}, 1)
	assert.NilError(t, err)
	_, _, err = store.add(smeltd.Session{ProjectRoot: "/p"}, 1)
	busy := busyCode(t, err)
	assert.Equal(t, busy.code, smeltd.CodeProjectBusy)
	ae := admissionRefusal(err)
	assert.Equal(t, ae.status, http.StatusConflict)
	assert.Equal(t, ae.code, smeltd.CodeProjectBusy)
}
