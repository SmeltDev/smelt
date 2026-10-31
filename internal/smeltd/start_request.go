package smeltd

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"time"
)

// Codes of the errors a factory start ends in when the daemon never answers.
const (
	// CodeDaemonTimeout means the start timed out and the daemon holds no run
	// made by the request, so nothing is going and starting again is safe.
	CodeDaemonTimeout = "factory.daemon_timeout"
	// CodeStartUnconfirmed means the start timed out and the daemon's sessions
	// could not be read either: a run may exist, under the request key.
	CodeStartUnconfirmed = "factory.start_unconfirmed"
)

// StartPolicy is how long a factory start waits for the daemon and how often it
// repeats the request. The zero value waits 30 s per POST and repeats twice.
type StartPolicy struct {
	// Timeout bounds each POST, response body included. Zero is 30 s.
	Timeout time.Duration
	// Retries is how many times a timed-out POST is repeated with the same
	// request key. Zero is two; negative is none.
	Retries int
}

const (
	defaultStartTimeout = 30 * time.Second
	defaultStartRetries = 2
)

func (p StartPolicy) orDefault() StartPolicy {
	if p.Timeout <= 0 {
		p.Timeout = defaultStartTimeout
	}
	switch {
	case p.Retries == 0:
		p.Retries = defaultStartRetries
	case p.Retries < 0:
		p.Retries = 0
	}
	return p
}

// DaemonTimeoutError is a start the daemon never confirmed and that left no run.
type DaemonTimeoutError struct{ Err error }

func (e *DaemonTimeoutError) Error() string {
	return "the daemon didn't confirm the start; no run was found with this request"
}

func (e *DaemonTimeoutError) Unwrap() error { return e.Err }

// StartUnconfirmedError is a start the daemon never confirmed, with no way to
// tell whether it made a run. RequestID finds the run if there is one.
type StartUnconfirmedError struct {
	RequestID string
	Err       error
}

func (e *StartUnconfirmedError) Error() string {
	return fmt.Sprintf("the daemon didn't confirm the start and its runs could not be listed (request %s): %v", e.RequestID, e.Err)
}

func (e *StartUnconfirmedError) Unwrap() error { return e.Err }

// newRequestID returns a random 128-bit key as hex.
func newRequestID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("generate request id: %w", err)
	}
	return hex.EncodeToString(b[:]), nil
}

// isTimeout reports whether err is a request that ran out of time, as opposed
// to one the daemon refused or could not be reached for. requestError wraps the
// transport error twice, so it is found with errors.As, not os.IsTimeout.
func isTimeout(err error) bool {
	var t interface{ Timeout() bool }
	return errors.As(err, &t) && t.Timeout()
}

// findByRequestID returns the session a start with this key created.
func findByRequestID(sessions []Session, id string) (Session, bool) {
	for _, s := range sessions {
		if s.RequestID == id {
			return s, true
		}
	}
	return Session{}, false
}
