package smeltd

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	neturl "net/url"
	"sort"
	"strings"
	"time"
)

// SessionRefused is the daemon's refusal of a session request: it was reachable
// and understood the request, and said no. The message is the daemon's own and
// is meant to be shown as is; Status tells a caller which kind of no it was
// (409 a session is already active, 503 the daemon lacks a credential, ...).
type SessionRefused struct {
	Status  int
	Message string
	// Code and Active are set when the daemon's answer was JSON, as the
	// draining refusal is: Code names the refusal (CodeDaemonDraining) and
	// Active is the runs that were going.
	Code   string
	Active int
}

// ErrNoSessionAPI reports a daemon from a build without the session API, which
// is also one without drain mode.
var ErrNoSessionAPI = errors.New("daemon has no session API")

// refusalOf builds a SessionRefused from a failed response's body.
func refusalOf(status int, body string) *SessionRefused {
	r := &SessionRefused{Status: status, Message: body}
	var parsed struct {
		Code    string `json:"code"`
		Active  int    `json:"active"`
		Message string `json:"message"`
	}
	if strings.HasPrefix(body, "{") && json.Unmarshal([]byte(body), &parsed) == nil && parsed.Code != "" {
		r.Code, r.Active, r.Message = parsed.Code, parsed.Active, parsed.Message
	}
	return r
}

func (e *SessionRefused) Error() string { return e.Message }

// sessionCall performs one request against the session API and decodes a JSON
// answer into out (skipped when nil). A transport failure is wrapped in
// ErrDaemonUnavailable; a daemon that answered with an error is a
// SessionRefused.
func sessionCall(method, path string, in, out any) error {
	return sessionCallAt("", method, path, in, out)
}

func sessionCallAt(socket, method, path string, in, out any) error {
	return sessionCallTimeout(socket, 0, method, path, in, out)
}

// sessionCallTimeout is sessionCallAt with its own bound on the whole request;
// zero keeps the client's default.
func sessionCallTimeout(socket string, timeout time.Duration, method, path string, in, out any) error {
	client, err := callClient(socket, timeout)
	if err != nil {
		return err
	}
	defer client.CloseIdleConnections()
	var body io.Reader
	if in != nil {
		raw, err := json.Marshal(in)
		if err != nil {
			return fmt.Errorf("marshal request: %w", err)
		}
		body = bytes.NewReader(raw)
	}
	req, err := http.NewRequest(method, "http://smeltd"+path, body)
	if err != nil {
		return fmt.Errorf("build request: %w", err)
	}
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("%w: %w", ErrDaemonUnavailable, requestError(err))
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode == http.StatusUnauthorized {
		return ErrUnauthorized
	}
	if resp.StatusCode >= 300 {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		msg := strings.TrimSpace(string(raw))
		// The router's own generic 404 means a daemon from a build without the
		// session API; the handlers' 404s always say something more specific.
		if resp.StatusCode == http.StatusNotFound && strings.HasPrefix(msg, "404 page not found") {
			return fmt.Errorf("%w: %w", ErrDaemonUnavailable, ErrNoSessionAPI)
		}
		return refusalOf(resp.StatusCode, msg)
	}
	if out == nil {
		_, _ = io.Copy(io.Discard, resp.Body)
		return nil
	}
	if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
		return fmt.Errorf("decode response: %w", err)
	}
	return nil
}

// callClient picks the client for a session call: the socket's when one is
// named and no remote daemon is set, else the default.
func callClient(socket string, timeout time.Duration) (*http.Client, error) {
	var client *http.Client
	if socket != "" && TCPRemoteAddr() == "" {
		client = unixClient(socket)
	} else {
		var err error
		if client, err = daemonClient(); err != nil {
			return nil, err
		}
	}
	if timeout > 0 {
		client.Timeout = timeout
	}
	return client, nil
}

// StartFactory asks the daemon to start a factory session and returns its ID
// without waiting for it. It is followed and cancelled like any other session.
func StartFactory(req FactoryRequest) (string, error) {
	return StartFactoryWith(req, StartPolicy{})
}

// StartFactoryWith is StartFactory with an explicit policy. The request carries
// a key the daemon remembers, so a POST that timed out is repeated safely: the
// daemon answers with the first run instead of starting another. If every POST
// times out, the daemon's sessions are searched for the key before giving up.
func StartFactoryWith(req FactoryRequest, policy StartPolicy) (string, error) {
	policy = policy.orDefault()
	if req.RequestID == "" {
		id, err := newRequestID()
		if err != nil {
			return "", err
		}
		req.RequestID = id
	}
	socket, err := startSocket(req)
	if err != nil {
		return "", err
	}
	id, err := postStart(socket, req, policy)
	var refused *SessionRefused
	if errors.As(err, &refused) && refused.Code == CodeDaemonHandover {
		if err = EnsureRunning(); err == nil {
			id, err = postStart("", req, policy)
		}
	}
	if isTimeout(err) {
		return lookupStart(req, err)
	}
	return id, err
}

// startSocket is the daemon a start goes to: the one that owns the run being
// continued, or the default one.
func startSocket(req FactoryRequest) (string, error) {
	if req.Continue == "" {
		return "", nil
	}
	sessions, err := ListSessions(req.ProjectRoot)
	if err != nil && !errors.Is(err, ErrDaemonUnavailable) {
		return "", err
	}
	for _, s := range sessions {
		if s.Factory != nil && (s.Factory.RunID == strings.TrimSpace(req.Continue) || s.Factory.Branch == strings.TrimSpace(req.Continue)) {
			return s.Socket, nil
		}
	}
	return "", nil
}

// postStart sends the start request, repeating it with the same key while it
// times out.
func postStart(socket string, req FactoryRequest, policy StartPolicy) (string, error) {
	var err error
	for attempt := 0; attempt <= policy.Retries; attempt++ {
		var resp SessionStartResponse
		err = sessionCallTimeout(socket, policy.Timeout, http.MethodPost, "/factory", req, &resp)
		if !isTimeout(err) {
			return resp.ID, err
		}
	}
	return "", err
}

// lookupStart finds the run a timed-out start made, by its key.
func lookupStart(req FactoryRequest, timedOut error) (string, error) {
	sessions, err := ListSessions(req.ProjectRoot)
	if err != nil {
		return "", &StartUnconfirmedError{RequestID: req.RequestID, Err: err}
	}
	if s, ok := findByRequestID(sessions, req.RequestID); ok {
		return s.ID, nil
	}
	return "", &DaemonTimeoutError{Err: timedOut}
}

// FetchSession returns one session with the text of its reviews.
func FetchSession(id string) (SessionDetail, error) {
	var detail SessionDetail
	if err := routedSessionCall(id, http.MethodGet, "/factory/"+neturl.PathEscape(id), nil, &detail); err != nil {
		return SessionDetail{}, err
	}
	return detail, nil
}

// ListSessions returns the daemon's sessions, newest first per project. An empty
// root lists every project's.
func listSessionsAt(socket, root string) ([]Session, error) {
	path := "/factory"
	if root != "" {
		path += "?root=" + neturl.QueryEscape(root)
	}
	var list SessionList
	if err := sessionCallAt(socket, http.MethodGet, path, nil, &list); err != nil {
		return nil, err
	}
	return list.Sessions, nil
}

// CancelSession stops a session. It is the only thing that does: a viewer that
// quits merely detaches.
func CancelSession(id string) error {
	return routedSessionCall(id, http.MethodPost, "/factory/"+neturl.PathEscape(id)+"/cancel", nil, nil)
}

// QueueSync asks the live session id to merge the remote-tracking base into
// its branch at the end of the current round, and returns the queued sync.
func QueueSync(id, base string) (BaseSync, error) {
	var queued BaseSync
	err := routedSessionCall(id, http.MethodPost, "/factory/"+neturl.PathEscape(id)+"/sync", SyncRequest{Base: base}, &queued)
	return queued, err
}

// MainSessions lists only the main daemon, for lifecycle decisions.
func MainSessions(root string) ([]Session, error) { return listSessionsAt("", root) }

// ListSessions merges live local daemon sessions, newest first.
func ListSessions(root string) ([]Session, error) {
	if TCPRemoteAddr() != "" {
		return listSessionsAt("", root)
	}
	daemons, err := DiscoverDaemons()
	if err != nil {
		return nil, err
	}
	if len(daemons) == 0 {
		return listSessionsAt("", root)
	}
	var sessions []Session
	visited, seen := make(map[string]bool), make(map[string]bool)
	for pass := 0; pass < 2; pass++ {
		for _, d := range daemons {
			if visited[d.Socket] {
				continue
			}
			visited[d.Socket] = true
			list, err := sessionsFromDaemon(d.Socket, root)
			if err != nil {
				return nil, err
			}
			for _, session := range list {
				if seen[session.ID] {
					continue
				}
				seen[session.ID] = true
				sessions = append(sessions, session)
			}
		}
		// A main listener may have moved while its list request was in flight.
		// Include newly created private sockets without duplicating its sessions.
		paths, err := localSockets()
		if err != nil {
			return nil, err
		}
		added := false
		for _, path := range paths[1:] {
			if !visited[path] {
				added = true
				break
			}
		}
		if !added {
			break
		}
		daemons, err = DiscoverDaemons()
		if err != nil {
			return nil, err
		}
	}
	sort.SliceStable(sessions, func(i, j int) bool { return sessions[i].StartedAt.After(sessions[j].StartedAt) })
	return sessions, nil
}

func sessionsFromDaemon(socket, root string) ([]Session, error) {
	list, err := listSessionsAt(socket, root)
	var refused *SessionRefused
	if errors.As(err, &refused) && refused.Status == http.StatusNotFound {
		return nil, nil
	}
	if errors.Is(err, ErrDaemonUnavailable) && errors.Is(classifyDialError(err, socket), ErrDaemonUnreachable) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	for i := range list {
		if list[i].Socket == "" {
			list[i].Socket = socket
		}
	}
	return list, nil
}

func routedSessionCall(id, method, path string, in, out any) error {
	if TCPRemoteAddr() != "" {
		return sessionCall(method, path, in, out)
	}
	socket, err := sessionOwner(id)
	if err != nil {
		return err
	}
	err = sessionCallAt(socket, method, path, in, out)
	// Reads can safely rediscover an owner if transfer raced discovery. Never
	// replay a mutation after a transport failure: its response may have been lost.
	var refused *SessionRefused
	if method == http.MethodGet && (errors.Is(err, ErrDaemonUnavailable) || errors.As(err, &refused) && refused.Status == http.StatusNotFound) {
		owner, lookupErr := sessionOwner(id)
		if lookupErr == nil && owner != socket {
			return sessionCallAt(owner, method, path, in, out)
		}
	}
	return err
}

func sessionOwner(id string) (string, error) {
	sessions, err := ListSessions("")
	if err != nil {
		return "", err
	}
	for _, s := range sessions {
		if s.ID == id {
			return s.Socket, nil
		}
	}
	return "", nil
}
