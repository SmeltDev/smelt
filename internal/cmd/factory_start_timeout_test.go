package cmd

import (
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"gotest.tools/v3/assert"

	"github.com/smeltdev/smelt/internal/smeltd"
)

// slowStartDaemon is a fake daemon whose start POST answers after postDelay and
// whose session list is listHandler: the daemon of a loaded machine.
type slowStartDaemon struct {
	postDelay   time.Duration
	listHandler http.HandlerFunc

	mu       sync.Mutex
	posts    int
	sessions map[string]string // request key → session ID
}

func (f *slowStartDaemon) handle(w http.ResponseWriter, r *http.Request) {
	switch {
	case r.URL.Path == "/ping":
		_, _ = io.WriteString(w, smeltd.BuildID())
	case r.Method == http.MethodPost && r.URL.Path == "/factory":
		f.start(w, r)
	case r.Method == http.MethodGet && r.URL.Path == "/factory":
		f.listHandler(w, r)
	case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/factory/"):
		writeFakeJSON(w, smeltd.SessionDetail{Session: smeltd.Session{
			ID: strings.TrimPrefix(r.URL.Path, "/factory/"), State: smeltd.SessionRunning,
			Factory: &smeltd.FactoryRun{RunID: "run-1", Branch: "smelt/run-1"},
		}})
	default:
		http.NotFound(w, r)
	}
}

func (f *slowStartDaemon) start(w http.ResponseWriter, r *http.Request) {
	var req smeltd.FactoryRequest
	_ = json.NewDecoder(r.Body).Decode(&req)
	f.mu.Lock()
	f.posts++
	id, ok := f.sessions[req.RequestID]
	if !ok {
		id = "session-1"
		f.sessions[req.RequestID] = id
	}
	f.mu.Unlock()
	select {
	case <-time.After(f.postDelay):
	case <-r.Context().Done():
		return
	}
	w.WriteHeader(http.StatusAccepted)
	writeFakeJSON(w, smeltd.SessionStartResponse{ID: id})
}

func (f *slowStartDaemon) counts() (posts, sessions int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.posts, len(f.sessions)
}

func writeFakeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

func emptyList(w http.ResponseWriter, _ *http.Request) {
	writeFakeJSON(w, smeltd.SessionList{})
}

func failingList(w http.ResponseWriter, _ *http.Request) {
	http.Error(w, "overloaded", http.StatusInternalServerError)
}

// serveSlowStart stands the fake up on SMELT_DAEMON_DIR, as serveConflicts does.
func serveSlowStart(t *testing.T, f *slowStartDaemon) {
	t.Helper()
	dir, err := os.MkdirTemp("", "wd")
	assert.NilError(t, err)
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	t.Setenv("SMELT_DAEMON_DIR", dir)
	f.sessions = map[string]string{}
	pidPath, err := smeltd.PIDPath()
	assert.NilError(t, err)
	assert.NilError(t, smeltd.WritePID(pidPath, os.Getpid()))
	sock, err := smeltd.SocketPath()
	assert.NilError(t, err)
	ln, err := net.Listen("unix", sock)
	assert.NilError(t, err)
	srv := &http.Server{Handler: http.HandlerFunc(f.handle), ReadHeaderTimeout: 5 * time.Second}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Close() })
}

// withStartPolicy sets the cmd package's test seam for one test.
func withStartPolicy(t *testing.T, p smeltd.StartPolicy) {
	t.Helper()
	startPolicy = p
	t.Cleanup(func() { startPolicy = smeltd.StartPolicy{} })
}

func TestFactoryBuildWaitsOutASlowDaemonAndStartsOneRun(t *testing.T) {
	if testing.Short() {
		t.Skip("sleeps 8 s like a loaded daemon")
	}
	factoryProject(t)
	f := &slowStartDaemon{postDelay: 8 * time.Second, listHandler: emptyList}
	serveSlowStart(t, f)

	stdout, stderr, err := runFactoryBuildCmd(t, "add a flag", "--detach", "--json")
	assert.NilError(t, err, stderr)
	assert.Assert(t, strings.Contains(stdout, `"session_id": "session-1"`), stdout)
	posts, sessions := f.counts()
	assert.Equal(t, posts, 1)
	assert.Equal(t, sessions, 1)
}

func TestFactoryBuildJSONReportsAnUnconfirmedStartWithItsKey(t *testing.T) {
	factoryProject(t)
	withStartPolicy(t, smeltd.StartPolicy{Timeout: 100 * time.Millisecond, Retries: 1})
	f := &slowStartDaemon{postDelay: time.Second, listHandler: failingList}
	serveSlowStart(t, f)

	_, _, err := runFactoryBuildCmd(t, "add a flag", "--detach", "--json")
	assert.Assert(t, err != nil)

	// What main.go reads off the error to print the --json body.
	coded, ok := err.(interface{ ErrorCode() string })
	assert.Assert(t, ok, "%T", err)
	assert.Equal(t, coded.ErrorCode(), "factory.start_unconfirmed")
	keyed, ok := err.(interface{ RequestID() string })
	assert.Assert(t, ok)
	f.mu.Lock()
	defer f.mu.Unlock()
	assert.Equal(t, len(f.sessions), 1)
	for key := range f.sessions {
		assert.Equal(t, keyed.RequestID(), key)
	}
}

func TestFactoryBuildReportsADaemonTimeoutWhenNoRunExists(t *testing.T) {
	factoryProject(t)
	withStartPolicy(t, smeltd.StartPolicy{Timeout: 100 * time.Millisecond, Retries: 1})
	f := &slowStartDaemon{postDelay: time.Second, listHandler: emptyList}
	serveSlowStart(t, f)

	_, _, err := runFactoryBuildCmd(t, "add a flag", "--detach", "--json")
	var ue *userError
	assert.Assert(t, errors.As(err, &ue), "%v", err)
	assert.Equal(t, ue.ErrorCode(), "factory.daemon_timeout")
	assert.Equal(t, ue.UserMessage(), "the daemon didn't confirm the start; no run was found with this request")
	assert.Equal(t, ue.Detail(), "")
	assert.Assert(t, ue.HideDetail())
	assert.Assert(t, !strings.Contains(ue.Suggestion(), "smelt watch"))
}
