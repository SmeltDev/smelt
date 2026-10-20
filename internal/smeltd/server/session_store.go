package server

import (
	"context"
	"fmt"
	"log"
	"slices"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/smeltdev/smelt/internal/smeltd"
)

// MaxSessionsPerProject caps retained sessions per project when few runs may
// be active at once. Only finished ones are evicted, so a live session is never
// lost.
const MaxSessionsPerProject = 10

// finishedSessionsKept is how many finished sessions a project keeps on top of
// its active ones, so a busy project does not evict runs as they finish.
const finishedSessionsKept = 8

// retainedSessions is the per-project cap for a project that may run limit
// sessions at once.
func retainedSessions(limit int) int {
	return max(MaxSessionsPerProject, limit+finishedSessionsKept)
}

// sessionEntry is one session and the machinery that owns it.
type sessionEntry struct {
	mu      sync.Mutex
	s       smeltd.Session
	details []smeltd.RoundDetail // parallel to s.Rounds
	cancel  context.CancelFunc
	// cancelled records that cancellation was asked for, so a session stopped by
	// it is reported as cancelled rather than as whatever error the stop made.
	cancelled bool
	// stopRequestedAt is when cancellation was first asked for.
	stopRequestedAt time.Time
	done            chan struct{}
	// loopNote is why the loop ended, and loopFailed that it ran to its end
	// without its work passing. Both are internal: the record shows their
	// consequences.
	loopNote   string
	loopFailed bool
}

func cloneSession(s smeltd.Session) smeltd.Session {
	out := s
	out.Stages = slices.Clone(s.Stages)
	out.Rounds = make([]smeltd.Round, len(s.Rounds))
	for i, r := range s.Rounds {
		out.Rounds[i] = r
		out.Rounds[i].Reviews = slices.Clone(r.Reviews)
		if r.EndedAt != nil {
			ended := *r.EndedAt
			out.Rounds[i].EndedAt = &ended
		}
		out.Rounds[i].Checks = slices.Clone(r.Checks)
		if r.Implement != nil {
			impl := *r.Implement
			out.Rounds[i].Implement = &impl
		}
	}
	if s.Factory != nil {
		f := *s.Factory
		f.HarnessSwaps = slices.Clone(s.Factory.HarnessSwaps)
		f.Turns = slices.Clone(s.Factory.Turns)
		f.SidecarIDs = slices.Clone(s.Factory.SidecarIDs)
		f.KeptSidecars = slices.Clone(s.Factory.KeptSidecars)
		f.Activity = slices.Clone(s.Factory.Activity)
		f.Progress = cloneFeed(s.Factory.Progress)
		out.Factory = &f
	}
	if s.EndedAt != nil {
		ended := *s.EndedAt
		out.EndedAt = &ended
	}
	return out
}

func (e *sessionEntry) snapshot() smeltd.Session {
	e.mu.Lock()
	defer e.mu.Unlock()
	return cloneSession(e.s)
}

func (e *sessionEntry) detail() smeltd.SessionDetail {
	e.mu.Lock()
	defer e.mu.Unlock()
	d := smeltd.SessionDetail{Session: cloneSession(e.s), Details: make([]smeltd.RoundDetail, len(e.details))}
	for i, rd := range e.details {
		d.Details[i] = smeltd.RoundDetail{Number: rd.Number, Results: slices.Clone(rd.Results)}
	}
	return d
}

// reviewIndexLocked finds a review of a round by name, or -1.
func (e *sessionEntry) reviewIndexLocked(round int, name string) int {
	for i := range e.s.Rounds[round].Reviews {
		if e.s.Rounds[round].Reviews[i].Name == name {
			return i
		}
	}
	return -1
}

// stageLocked returns the stage with the given ID.
func (e *sessionEntry) stageLocked(id smeltd.StageID) *smeltd.Stage {
	for i := range e.s.Stages {
		if e.s.Stages[i].ID == id {
			return &e.s.Stages[i]
		}
	}
	return nil
}

// sessionStore holds every session. Like the task store it lives in memory only:
// a daemon restart forgets sessions. A factory run's work is not forgotten with
// it: it is committed on the run's branch.
type sessionStore struct {
	parent context.Context

	mu        sync.Mutex
	sessions  map[string]*sessionEntry
	byProject map[string][]string // project root → session IDs, oldest first

	// draining refuses new sessions; it is read and set under mu, the lock add
	// admits a session under, so a drain cannot race a start.
	draining bool
	// onIdle runs when the last active session has ended while draining. Set
	// before the store is shared and never reassigned.
	onIdle func(uint64)
	// drainGeneration invalidates delayed callbacks when a drain is cancelled.
	drainGeneration uint64
	drainCommitted  bool
	drainRemaining  int
}

// drainingError is add's refusal while the daemon is draining.
type drainingError struct{ active int }

func (e *drainingError) Error() string {
	return fmt.Sprintf("smelt is restarting to finish an upgrade after %d active run(s); try again shortly", e.active)
}

func newSessionStore(parent context.Context) *sessionStore {
	if parent == nil {
		parent = context.Background()
	}
	return &sessionStore{
		parent:    parent,
		sessions:  make(map[string]*sessionEntry),
		byProject: make(map[string][]string),
	}
}

// activeLocked counts sessions whose run has not wound down: done has not
// closed. That is later than Finished, which is set before the run commits its
// work, and a restart must wait for the commit.
func (s *sessionStore) activeLocked() int {
	n := 0
	for _, e := range s.sessions {
		select {
		case <-e.done:
		default:
			n++
		}
	}
	return n
}

// activeCount is the number of sessions still winding down or running.
func (s *sessionStore) activeCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.activeLocked()
}

// drain stops admitting sessions and returns how many are active. With none,
// onIdle runs at once. Calling it again changes nothing.
func (s *sessionStore) drain() int {
	s.mu.Lock()
	if !s.draining {
		s.drainGeneration++
	}
	s.draining = true
	active := s.activeLocked()
	s.drainRemaining = active
	// Serialize the request log with completion notifications, so a fast run
	// cannot log its completion before the request that started the drain.
	log.Printf("smeltd: drain requested (%d active runs)", active)
	generation := s.drainGeneration
	s.mu.Unlock()
	if active == 0 {
		s.fireIdle(generation)
	}
	return active
}

// undrain admits sessions again unless shutdown has already committed.
func (s *sessionStore) undrain() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.drainCommitted {
		return
	}
	s.draining = false
	s.drainGeneration++
}

func (s *sessionStore) isDraining() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.draining
}

func (s *sessionStore) fireIdle(generation uint64) {
	if s.onIdle != nil {
		s.onIdle(generation)
	}
}

// commitDrain revalidates a delayed idle decision and cancels the root under
// the admission lock. No undrain or new session can slip between the check
// and cancellation. Once committed, admission stays closed until exit.
func (s *sessionStore) commitDrain(generation uint64, shutdown context.CancelFunc) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.draining || s.drainCommitted || s.drainGeneration != generation || s.activeLocked() != 0 {
		return
	}
	s.drainCommitted = true
	shutdown()
}

// ended is called after an entry's done has closed. Progress and idle decisions
// share the admission lock with drain requests and shutdown commitment.
func (s *sessionStore) ended() {
	s.mu.Lock()
	active := s.activeLocked()
	if s.draining && active != s.drainRemaining {
		s.drainRemaining = active
		log.Printf("smeltd: draining: %d runs left", active)
	}
	idle := s.draining && active == 0
	generation := s.drainGeneration
	s.mu.Unlock()
	if idle {
		s.fireIdle(generation)
	}
}

// add creates a running entry for s. If the daemon is draining, or the project
// already has limit sessions that have not ended, it creates nothing and says
// why; a draining refusal is a *drainingError.
func (s *sessionStore) add(sess smeltd.Session, limit int) (*sessionEntry, context.Context, error) {
	limit = max(limit, 1)
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.draining {
		return nil, nil, &drainingError{active: s.activeLocked()}
	}
	active := 0
	for _, id := range s.byProject[sess.ProjectRoot] {
		if other := s.sessions[id]; other != nil && !other.snapshot().State.Finished() {
			active++
		}
	}
	if active >= limit {
		return nil, nil, fmt.Errorf("project already has %d active run(s) (limit %d; raise factory.maxConcurrent)", active, limit)
	}
	sess.ID = uuid.NewString()
	sess.State = smeltd.SessionRunning
	sess.Stages = newStages()
	sess.StartedAt = time.Now()
	ctx, cancel := context.WithCancel(s.parent)
	entry := &sessionEntry{
		s:      sess,
		cancel: cancel,
		done:   make(chan struct{}),
	}
	s.sessions[sess.ID] = entry
	s.byProject[sess.ProjectRoot] = append(s.byProject[sess.ProjectRoot], sess.ID)
	s.evictLocked(sess.ProjectRoot, retainedSessions(limit))
	return entry, ctx, nil
}

// evictLocked drops the oldest finished sessions until the project is within
// the cap. A terminal state alone does not mean teardown has completed.
func (s *sessionStore) evictLocked(root string, keep int) {
	ids := s.byProject[root]
	for len(ids) > keep {
		dropped := false
		for i, id := range ids {
			if e := s.sessions[id]; e != nil {
				select {
				case <-e.done:
				default:
					continue
				}
			}
			delete(s.sessions, id)
			ids = slices.Delete(ids, i, i+1)
			dropped = true
			break
		}
		if !dropped {
			break
		}
	}
	s.byProject[root] = ids
}

func (s *sessionStore) get(id string) *sessionEntry {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.sessions[id]
}

// forProject lists a project's sessions, newest first. Safe on a nil store.
func (s *sessionStore) forProject(root string) []smeltd.Session {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	ids := slices.Clone(s.byProject[root])
	entries := make([]*sessionEntry, 0, len(ids))
	for _, id := range ids {
		if e := s.sessions[id]; e != nil {
			entries = append(entries, e)
		}
	}
	s.mu.Unlock()

	out := make([]smeltd.Session, 0, len(entries))
	for i := len(entries) - 1; i >= 0; i-- {
		out = append(out, entries[i].snapshot())
	}
	return out
}

// cancelSession asks a session to stop. It reports whether the session exists and was
// still going; cancelling an ended session is not an error worth a status code.
func (s *sessionStore) cancelSession(id string) (found, wasActive bool) {
	e := s.get(id)
	if e == nil {
		return false, false
	}
	e.mu.Lock()
	active := !e.s.State.Finished()
	if active {
		if !e.cancelled {
			e.stopRequestedAt = time.Now()
		}
		e.cancelled = true
	}
	e.mu.Unlock()
	if active {
		e.cancel()
	}
	return true, active
}

// stopAll cancels every session in flight and waits for them to settle.
func (s *sessionStore) stopAll() {
	if s == nil {
		return
	}
	s.mu.Lock()
	entries := make([]*sessionEntry, 0, len(s.sessions))
	for _, e := range s.sessions {
		entries = append(entries, e)
	}
	s.mu.Unlock()
	for _, e := range entries {
		e.cancel()
	}
	for _, e := range entries {
		<-e.done
	}
}

// newStages returns the stages of a session that is starting: its loop,
// running.
func newStages() []smeltd.Stage {
	return []smeltd.Stage{{ID: smeltd.StageFactoryLoop, State: smeltd.StageRunning}}
}
