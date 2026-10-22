package factory

import (
	"errors"
	"fmt"
	"slices"
	"sync"
)

// SyncState is where a requested base sync stands.
type SyncState string

// The states of a Sync.
const (
	// SyncQueued means the sync waits for the end of the current round.
	SyncQueued SyncState = "queued"
	// SyncDone means the base was merged into the run's branch.
	SyncDone SyncState = "done"
	// SyncDropped means the run ended before a round boundary came, so the
	// sync was never applied.
	SyncDropped SyncState = "dropped"
)

// ErrSyncsClosed means the run has stopped taking base syncs: it is ending.
var ErrSyncsClosed = errors.New("the run is ending, so it takes no more base syncs")

// Sync is a request to merge a base branch into a live run's branch at the end
// of the round, and how it came out. Its JSON form is what `factory status
// --json` shows.
type Sync struct {
	// Base is the branch whose remote-tracking ref is merged.
	Base  string    `json:"base"`
	State SyncState `json:"state"`
	// Round is the round whose end applied the sync; 0 until it is done.
	Round int `json:"round,omitempty"`
	// Commit is the base's tip that was merged, once done.
	Commit string `json:"commit,omitempty"`
	// Files is how many files the merge changed on the branch.
	Files int `json:"files,omitempty"`
	// Conflicts are the files committed with conflict markers in them.
	Conflicts []string `json:"conflicts,omitempty"`
	// Reason says why a sync was dropped.
	Reason string `json:"reason,omitempty"`
}

// SyncQueue holds the base syncs requested for a live run. The zero value is
// ready; it is safe for the goroutines of the requester and the loop. A nil
// queue is a run that takes no syncs.
type SyncQueue struct {
	mu     sync.Mutex
	syncs  []Sync
	closed bool
}

// Enqueue asks for base to be merged at the end of the current round. Asking
// again for a base that is still waiting changes nothing. A different base
// must wait until the pending sync has been applied.
func (q *SyncQueue) Enqueue(base string) (Sync, error) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.closed {
		return Sync{}, ErrSyncsClosed
	}
	for _, s := range q.syncs {
		if s.State == SyncQueued {
			if s.Base == base {
				return s, nil
			}
			return Sync{}, fmt.Errorf("a sync of %s is already queued; wait for it before syncing %s", s.Base, base)
		}
	}
	s := Sync{Base: base, State: SyncQueued}
	q.syncs = append(q.syncs, s)
	return s, nil
}

// Snapshot is every sync asked for, in order.
func (q *SyncQueue) Snapshot() []Sync {
	if q == nil {
		return nil
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	return cloneSyncs(q.syncs)
}

// next is the oldest sync still waiting, and its position.
func (q *SyncQueue) next() (int, Sync, bool) {
	if q == nil {
		return 0, Sync{}, false
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	for i, s := range q.syncs {
		if s.State == SyncQueued {
			return i, s, true
		}
	}
	return 0, Sync{}, false
}

// settle replaces the sync at position i with its outcome.
func (q *SyncQueue) settle(i int, s Sync) {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.syncs[i] = s
}

// Close stops the queue taking syncs and drops the ones still waiting: the run
// is over, so no round boundary is left to apply them at. It returns every
// sync, and is safe to call again.
func (q *SyncQueue) Close() []Sync {
	if q == nil {
		return nil
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	q.closed = true
	for i, s := range q.syncs {
		if s.State == SyncQueued {
			q.syncs[i].State = SyncDropped
			q.syncs[i].Reason = "the run ended before the next round"
		}
	}
	return cloneSyncs(q.syncs)
}

func cloneSyncs(syncs []Sync) []Sync {
	out := slices.Clone(syncs)
	for i := range out {
		out[i].Conflicts = slices.Clone(out[i].Conflicts)
	}
	return out
}
