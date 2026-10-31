package server

import (
	"context"
	"encoding/json"
	"net/http"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"gotest.tools/v3/assert"

	"github.com/smeltdev/smelt/internal/factory"
	"github.com/smeltdev/smelt/internal/smeltd"
)

// newDedupeFixture is a drain fixture whose runs count how often they start.
func newDedupeFixture(t *testing.T) (*drainFixture, *atomic.Int32) {
	t.Helper()
	f := newDrainFixture(t)
	runs := &atomic.Int32{}
	f.d.rcfg.RunFactory = func(ctx context.Context, _ factory.RunOptions) (factory.Report, error) {
		runs.Add(1)
		<-ctx.Done()
		return factory.Report{}, ctx.Err()
	}
	return f, runs
}

func (f *drainFixture) startKeyed(key string) (int, string) {
	body, _ := json.Marshal(smeltd.FactoryRequest{Backend: smeltd.BackendCircleCI, ProjectRoot: f.root, Prompt: "add a flag", RequestID: key})
	rec := f.do(http.MethodPost, "/factory", string(body))
	var resp smeltd.SessionStartResponse
	_ = json.Unmarshal(rec.Body.Bytes(), &resp)
	return rec.Code, resp.ID
}

func TestRepeatedStartWithOneKeyStartsOneSession(t *testing.T) {
	f, runs := newDedupeFixture(t)

	code, first := f.startKeyed("key-1")
	assert.Equal(t, code, http.StatusAccepted)
	code, second := f.startKeyed("key-1")
	assert.Equal(t, code, http.StatusAccepted)

	assert.Assert(t, first != "")
	assert.Equal(t, first, second)
	assert.Equal(t, len(f.d.sessions.sessions), 1)
	assert.Equal(t, f.d.sessions.sessions[first].snapshot().RequestID, "key-1")
	time.Sleep(50 * time.Millisecond)
	assert.Equal(t, runs.Load(), int32(1))
}

func TestConcurrentStartsWithOneKeyStartOneSession(t *testing.T) {
	f, runs := newDedupeFixture(t)

	const n = 8
	ids := make([]string, n)
	var wg sync.WaitGroup
	for i := range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, ids[i] = f.startKeyed("key-race")
		}()
	}
	wg.Wait()

	for _, id := range ids {
		assert.Equal(t, id, ids[0])
	}
	assert.Assert(t, ids[0] != "")
	time.Sleep(50 * time.Millisecond)
	assert.Equal(t, runs.Load(), int32(1))
}

func TestStartKeyIsForgottenAfterItsTTL(t *testing.T) {
	f, _ := newDedupeFixture(t)
	now := time.Now()
	f.d.sessions.now = func() time.Time { return now }
	// A second concurrent run in the project must be allowed for the new start.
	cfg := `{"orgID":"org-1","commands":[{"name":"test","run":"true"}],"factory":{"maxConcurrent":2}}`
	writeProjectConfig(t, f.root, cfg)

	_, first := f.startKeyed("key-ttl")
	now = now.Add(requestKeyTTL + time.Second)
	_, second := f.startKeyed("key-ttl")

	assert.Assert(t, first != second, "an expired key starts a new run")
	assert.Equal(t, len(f.d.sessions.requests), 1, "the expired key was swept")
}

func TestRefusedStartDoesNotRecordItsKey(t *testing.T) {
	s := newSessionStore(t.Context())
	first, _, err := s.add(smeltd.Session{ProjectRoot: "/p", RequestID: "a"}, 1)
	assert.NilError(t, err)

	_, _, err = s.add(smeltd.Session{ProjectRoot: "/p", RequestID: "b"}, 1)
	assert.Assert(t, err != nil, "the project is at its limit")
	assert.Assert(t, s.replayed("b") == nil)

	// Once there is room the same key starts normally.
	close(first.done)
	first.mu.Lock()
	first.s.State = smeltd.SessionDone
	first.mu.Unlock()
	entry, ctx, err := s.add(smeltd.Session{ProjectRoot: "/p", RequestID: "b"}, 1)
	assert.NilError(t, err)
	assert.Assert(t, ctx != nil)
	assert.Assert(t, s.replayed("b") == entry)
}
