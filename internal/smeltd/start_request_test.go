package smeltd

import (
	"encoding/json"
	"errors"
	"net/http"
	"sync"
	"testing"
	"time"

	"gotest.tools/v3/assert"
)

var shortStart = StartPolicy{Timeout: 100 * time.Millisecond, Retries: 2}

// startRecorder is a fake daemon's view of /factory: it admits a start the way
// the real one does, once per request key, and can stall its answers.
type startRecorder struct {
	mu      sync.Mutex
	byKey   map[string]string
	posts   int
	started int
	// stall holds the answer to the nth POST (1-based) past the client timeout;
	// stallAll holds every answer.
	stall    int
	stallAll bool
	list     http.HandlerFunc
}

func (f *startRecorder) handle(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodGet {
		f.list(w, r)
		return
	}
	var req FactoryRequest
	_ = json.NewDecoder(r.Body).Decode(&req)
	f.mu.Lock()
	f.posts++
	n := f.posts
	id, ok := f.byKey[req.RequestID]
	if !ok {
		f.started++
		id = "session-" + req.RequestID[:4]
		f.byKey[req.RequestID] = id
	}
	f.mu.Unlock()
	if f.stallAll || n == f.stall {
		select {
		case <-time.After(400 * time.Millisecond):
		case <-r.Context().Done():
		}
	}
	w.WriteHeader(http.StatusAccepted)
	_ = json.NewEncoder(w).Encode(SessionStartResponse{ID: id})
}

func newStartRecorder(t *testing.T) *startRecorder {
	t.Helper()
	f := &startRecorder{byKey: map[string]string{}}
	startTestDaemon(t, "build", f.handle)
	return f
}

func TestStartFactoryRetriesASlowPostWithTheSameKey(t *testing.T) {
	f := newStartRecorder(t)
	f.stall = 1

	id, err := StartFactoryWith(FactoryRequest{ProjectRoot: "/repo", Prompt: "go"}, shortStart)
	assert.NilError(t, err)
	assert.Assert(t, id != "")
	f.mu.Lock()
	defer f.mu.Unlock()
	assert.Equal(t, f.started, 1)
	assert.Equal(t, f.posts, 2)
	assert.Equal(t, len(f.byKey), 1)
}

func TestStartFactoryFindsTheRunByKeyWhenEveryPostTimesOut(t *testing.T) {
	f := newStartRecorder(t)
	f.stallAll = true
	f.list = func(w http.ResponseWriter, _ *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		var sessions []Session
		for key, id := range f.byKey {
			sessions = append(sessions, Session{ID: "other"}, Session{ID: id, RequestID: key})
		}
		_ = json.NewEncoder(w).Encode(SessionList{Sessions: sessions})
	}

	id, err := StartFactoryWith(FactoryRequest{ProjectRoot: "/repo", Prompt: "go"}, shortStart)
	assert.NilError(t, err)
	f.mu.Lock()
	defer f.mu.Unlock()
	assert.Equal(t, id, f.byKey[f.keyOf(t)])
	assert.Equal(t, f.posts, 3)
	assert.Equal(t, f.started, 1)
}

func (f *startRecorder) keyOf(t *testing.T) string {
	t.Helper()
	assert.Equal(t, len(f.byKey), 1)
	for k := range f.byKey {
		return k
	}
	return ""
}

func TestStartFactoryWithoutAMatchIsADaemonTimeout(t *testing.T) {
	f := newStartRecorder(t)
	f.stallAll = true
	f.list = func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(SessionList{Sessions: []Session{{ID: "unrelated", RequestID: "someone-else"}}})
	}

	_, err := StartFactoryWith(FactoryRequest{ProjectRoot: "/repo", Prompt: "go"}, shortStart)
	var timedOut *DaemonTimeoutError
	assert.Assert(t, errors.As(err, &timedOut), "got %v", err)
	assert.Equal(t, err.Error(), "the daemon didn't confirm the start; no run was found with this request")
}

func TestStartFactoryWhenTheListFailsIsUnconfirmedAndCarriesTheKey(t *testing.T) {
	f := newStartRecorder(t)
	f.stallAll = true
	f.list = func(w http.ResponseWriter, _ *http.Request) { http.Error(w, "boom", http.StatusInternalServerError) }

	_, err := StartFactoryWith(FactoryRequest{ProjectRoot: "/repo", Prompt: "go"}, shortStart)
	var unconfirmed *StartUnconfirmedError
	assert.Assert(t, errors.As(err, &unconfirmed), "got %v", err)
	f.mu.Lock()
	defer f.mu.Unlock()
	assert.Equal(t, unconfirmed.RequestID, f.keyOf(t))
}

func TestStartFactoryKeepsAKeyTheCallerGave(t *testing.T) {
	f := newStartRecorder(t)
	_, err := StartFactoryWith(FactoryRequest{ProjectRoot: "/repo", Prompt: "go", RequestID: "abcd1234"}, shortStart)
	assert.NilError(t, err)
	f.mu.Lock()
	defer f.mu.Unlock()
	assert.Equal(t, f.keyOf(t), "abcd1234")
}

func TestStartFactoryRefusedConnectionIsNotATimeout(t *testing.T) {
	t.Setenv("SMELT_DAEMON_DIR", socketDir(t))

	_, err := StartFactoryWith(FactoryRequest{ProjectRoot: "/repo", Prompt: "go"}, shortStart)
	assert.Assert(t, errors.Is(err, ErrDaemonUnavailable), "got %v", err)
	var timedOut *DaemonTimeoutError
	assert.Assert(t, !errors.As(err, &timedOut))
}

func TestNewRequestIDIs128BitsOfHex(t *testing.T) {
	a, err := newRequestID()
	assert.NilError(t, err)
	b, _ := newRequestID()
	assert.Equal(t, len(a), 32)
	assert.Assert(t, a != b)
}
