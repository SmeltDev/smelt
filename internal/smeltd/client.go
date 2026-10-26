package smeltd

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log"
	"net/http"
	neturl "net/url"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

// ErrDaemonUnavailable is returned by RunValidate when the daemon socket is
// unreachable, so callers can distinguish a transient connectivity failure from
// a real validation error and fall back to inline execution.
var ErrDaemonUnavailable = errors.New("daemon unavailable")

// daemonClient returns an http.Client for the running daemon. When
// SMELT_DAEMON_REMOTE_ADDR is set it connects over TCP; otherwise it uses the
// local Unix socket. Both variants ignore the URL hostname and always connect
// to the configured address, so callers keep using "http://smeltd/..." URLs.
func daemonClient() (*http.Client, error) {
	if addr := TCPRemoteAddr(); addr != "" {
		return tcpClient(addr), nil
	}
	sockPath, err := SocketPath()
	if err != nil {
		return nil, err
	}
	return unixClient(sockPath), nil
}

// longDaemonClient is like daemonClient but without a total-request timeout,
// suitable for long-running operations like /validate.
func longDaemonClient() (*http.Client, error) {
	if addr := TCPRemoteAddr(); addr != "" {
		return longTCPClient(addr), nil
	}
	sockPath, err := SocketPath()
	if err != nil {
		return nil, err
	}
	return longUnixClient(sockPath), nil
}

// Headers /ping adds to its BuildID body. A daemon predating them sends none,
// which reads as not draining and revision unknown.
const (
	HeaderDraining = "X-Smelt-Draining"
	HeaderRevision = "X-Smelt-Revision"
	HeaderHandover = "X-Smelt-Handover"
	headerTrue     = "true"
)

// doPingInfo sends a /ping request with the given client and returns what the
// daemon says about itself.
func doPingInfo(client *http.Client) PingInfo {
	defer client.CloseIdleConnections()
	resp, err := client.Get("http://smeltd/ping")
	if err != nil {
		return PingInfo{}
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode == http.StatusUnauthorized {
		log.Printf("smeltd: daemon rejected request — check SMELT_DAEMON_TCP_TOKEN")
		return PingInfo{}
	}
	if resp.StatusCode != http.StatusOK {
		return PingInfo{}
	}
	info := PingInfo{
		Reachable: true,
		Revision:  resp.Header.Get(HeaderRevision),
		Draining:  resp.Header.Get(HeaderDraining) == headerTrue,
		Handover:  resp.Header.Get(HeaderHandover) == headerTrue,
	}
	if body, err := io.ReadAll(io.LimitReader(resp.Body, 512)); err == nil {
		info.BuildID = strings.TrimSpace(string(body))
	}
	return info
}

// doPing sends a /ping request with the given client and returns reachability
// and the daemon's build identity.
func doPing(client *http.Client) (bool, string) {
	info := doPingInfo(client)
	return info.Reachable, info.BuildID
}

// pingDaemon pings via the configured transport (TCP when
// SMELT_DAEMON_REMOTE_ADDR is set, Unix socket otherwise).
func pingDaemon() (bool, string) {
	client, err := daemonClient()
	if err != nil {
		return false, ""
	}
	return doPing(client)
}

// ErrUnauthorized reports that a remote daemon rejected the bearer token. It is
// its own sentinel because the fix (set SMELT_DAEMON_TCP_TOKEN to the daemon's
// value) has nothing in common with the fix for an unreachable daemon.
var ErrUnauthorized = errors.New("the remote smelt daemon rejected the token (check SMELT_DAEMON_TCP_TOKEN)")

// Connection describes which daemon this process talks to.
type Connection struct {
	// Remote is the TCP address of a remote daemon, empty for the local socket.
	Remote string
}

// CurrentConnection reports the daemon this process is configured to use.
func CurrentConnection() Connection {
	return Connection{Remote: TCPRemoteAddr()}
}

// Label is the short human-readable form: "local", or "remote host:port".
func (c Connection) Label() string {
	if c.Remote == "" {
		return "local"
	}
	return "remote " + c.Remote
}

// requestError wraps a failed request with where it was going. The bare
// transport error names the placeholder host "smeltd", which tells a reader
// nothing about which daemon did not answer.
func requestError(err error) error {
	if addr := TCPRemoteAddr(); addr != "" {
		return fmt.Errorf("remote smelt daemon at %s unreachable: %w", addr, err)
	}
	return fmt.Errorf("connect to smelt daemon: %w", err)
}

// statusError turns a non-200 response into an error, mapping 401 onto
// ErrUnauthorized.
func statusError(resp *http.Response) error {
	if resp.StatusCode == http.StatusUnauthorized {
		return ErrUnauthorized
	}
	return fmt.Errorf("smelt daemon returned %s", resp.Status)
}

// FetchSnapshot connects to the running smelt daemon and returns the current
// snapshot for the given project roots. If roots is empty all known projects
// are returned.
func FetchSnapshot(roots []string) (Snapshot, error) {
	client, err := daemonClient()
	if err != nil {
		return Snapshot{}, err
	}
	body, err := json.Marshal(roots)
	if err != nil {
		return Snapshot{}, fmt.Errorf("marshal roots: %w", err)
	}
	resp, err := client.Post("http://smeltd/snapshot", "application/json", bytes.NewReader(body))
	if err != nil {
		return Snapshot{}, requestError(err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return Snapshot{}, statusError(resp)
	}
	var snap Snapshot
	if err := json.NewDecoder(resp.Body).Decode(&snap); err != nil {
		return Snapshot{}, fmt.Errorf("decode snapshot: %w", err)
	}
	return snap, nil
}

// FetchSnapshotRelaunching is FetchSnapshot for a caller that keeps polling.
// If the local daemon has gone away it starts one and asks again, once.
//
// Starting one at startup is not enough on its own: the daemon can exit while
// a dashboard is open — it crashes, someone kills it, or a smelt from another
// build replaces it (see BuildID) — and the dashboard would then hold whatever
// it last saw, which reads as a quiet dashboard rather than a broken one.
//
// It relaunches with EnsureLaunched rather than EnsureRunning on purpose: a
// failed poll should start a daemon if none is answering, not replace one that
// is. A remote daemon is managed elsewhere, so it is never relaunched.
func FetchSnapshotRelaunching(roots []string) (Snapshot, error) {
	fetch := func() (Snapshot, error) { return FetchSnapshot(roots) }
	if TCPRemoteAddr() != "" {
		return fetch()
	}
	return fetchRelaunching(fetch, EnsureLaunched)
}

// fetchRelaunching fetches, relaunching and retrying once on failure. fetch and
// relaunch are parameters so this can be tested without a daemon.
func fetchRelaunching(fetch func() (Snapshot, error), relaunch func() error) (Snapshot, error) {
	snap, err := fetch()
	if err == nil {
		return snap, nil
	}
	if relaunchErr := relaunch(); relaunchErr != nil {
		// Report the fetch failure rather than the relaunch failure: the first is
		// what the reader is looking at, and the second is usually a restatement.
		return Snapshot{}, err
	}
	return fetch()
}

// registerTimeout bounds a command registration. It is deliberately short: this
// call sits on the hook path, in front of a command the developer is waiting for,
// and a logs pane is never worth delaying that.
const registerTimeout = 2 * time.Second

// RegisterCommand tells the running smelt daemon to stream and buffer a command's
// output.
//
// It is best-effort by design and reports no error. If the daemon is not running,
// the command still runs and still streams to the caller's own stdout; the only
// thing lost is the buffered copy. Notably this does not start the daemon:
// spawning a background process as a side effect of a hook firing is intrusive,
// and a hook that hangs waiting for a daemon launch is a far worse failure than a
// missing logs pane.
func RegisterCommand(reg CommandReg) {
	client, err := daemonClient()
	if err != nil {
		return
	}
	body, err := json.Marshal(reg)
	if err != nil {
		return
	}
	client.Timeout = registerTimeout
	resp, err := client.Post("http://smeltd/command", "application/json", bytes.NewReader(body))
	if err != nil {
		return
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()
}

// conflictTimeout bounds a conflict query. Short for the same reason
// registerTimeout is: this call sits on the hook path in front of work the
// developer is waiting for, and an advisory notice is never worth delaying it.
const conflictTimeout = 2 * time.Second

// ErrDaemonUnreachable reports that no smelt daemon answered.
//
// Callers on the hook path are expected to treat this as "nothing to say" and
// carry on. The daemon is optional — it runs when the developer has `smelt
// watch` open — and a hook that complains about its absence would fire on every
// session end for everyone who does not.
var ErrDaemonUnreachable = errors.New("no smelt daemon is running")

// ErrDaemonTimeout reports that a daemon was there but did not answer within
// conflictTimeout.
//
// Kept apart from ErrDaemonUnreachable because the two call for different
// advice: one means start the daemon, the other means it is already running and
// busy, so asking again is what helps. Collapsing them told people with a
// working daemon to go start one.
var ErrDaemonTimeout = errors.New("the smelt daemon did not answer in time")

// ErrDaemonPermission reports that the socket is there but this user cannot
// open it — a daemon running as somebody else, or a directory whose mode has
// been changed.
//
// Kept apart from ErrDaemonUnreachable for the same reason ErrDaemonTimeout is:
// the advice differs. Starting a second daemon leaves the same socket just as
// unreadable, so "run smelt watch" is the one thing that cannot help here.
var ErrDaemonPermission = errors.New("the smelt daemon socket is not accessible to this user")

// FetchConflicts asks the running daemon whether root's branch still merges
// cleanly into its merge target.
//
// Unlike RegisterCommand this reports its errors, because the caller decides
// how loudly to fail: a hook stays quiet, a person running the command by hand
// gets told why there is no answer.
func FetchConflicts(root string) (ConflictReport, error) {
	sockPath, err := SocketPath()
	if err != nil {
		return ConflictReport{}, err
	}
	reqURL := "http://smeltd/conflicts?root=" + neturl.QueryEscape(root)
	client := unixClient(sockPath)
	client.Timeout = conflictTimeout
	resp, err := client.Get(reqURL)
	if err != nil {
		return ConflictReport{}, classifyDialError(err, sockPath)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return ConflictReport{}, fmt.Errorf("smelt daemon returned %s", resp.Status)
	}
	var report ConflictReport
	if err := json.NewDecoder(resp.Body).Decode(&report); err != nil {
		return ConflictReport{}, fmt.Errorf("decode conflicts: %w", err)
	}
	return report, nil
}

// classifyDialError turns a failed request to the socket into the sentinel that
// carries the right advice.
//
// Only the errors that actually mean "nothing is listening" become
// ErrDaemonUnreachable. Anything else keeps its own text rather than being
// reported as an absent daemon: the advice attached to that sentinel is to go
// start one, which is wrong — and unfalsifiable to the reader — for every cause
// but the one it names.
func classifyDialError(err error, sockPath string) error {
	switch {
	case os.IsTimeout(err):
		// A refused connection and an expired deadline are different answers.
		// os.IsTimeout sees through the *url.Error the client wraps around it,
		// which is why the check is not an errors.Is against a sentinel.
		return ErrDaemonTimeout
	case errors.Is(err, fs.ErrPermission):
		return fmt.Errorf("%w: %s", ErrDaemonPermission, sockPath)
	case errors.Is(err, fs.ErrNotExist), errors.Is(err, syscall.ECONNREFUSED):
		// No socket file, or one left behind by a daemon that died. Both mean
		// there is nothing to talk to, which is what starting one fixes.
		return ErrDaemonUnreachable
	default:
		return fmt.Errorf("reach the smelt daemon at %s: %w", sockPath, err)
	}
}

// FetchOutput reads buffered output for a command starting at offset.
func FetchOutput(commandID string, offset int64) (OutputChunk, error) {
	client, err := daemonClient()
	if err != nil {
		return OutputChunk{}, err
	}
	reqURL := fmt.Sprintf("http://smeltd/output?command_id=%s&offset=%d",
		neturl.QueryEscape(commandID), offset)
	resp, err := client.Get(reqURL)
	if err != nil {
		return OutputChunk{}, requestError(err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return OutputChunk{}, statusError(resp)
	}
	var piece OutputChunk
	if err := json.NewDecoder(resp.Body).Decode(&piece); err != nil {
		return OutputChunk{}, fmt.Errorf("decode output: %w", err)
	}
	return piece, nil
}

// ping reports whether the daemon at sockPath is reachable, along with the
// build identity it names. A daemon older than that identity reports "".
func ping(sockPath string) (bool, string) {
	return doPing(unixClient(sockPath))
}

// pingInfo is ping with everything the daemon reports about itself.
func pingInfo(sockPath string) PingInfo {
	return doPingInfo(unixClient(sockPath))
}

// Status pings the local daemon and returns what it reports.
func Status() (PingInfo, error) {
	sockPath, err := SocketPath()
	if err != nil {
		return PingInfo{}, err
	}
	return pingInfo(sockPath), nil
}

// ErrDrainUnsupported reports a daemon from a build before drain mode.
var ErrDrainUnsupported = errors.New("this smelt daemon predates drain mode")

// Drain asks the daemon to stop admitting factory runs and exit once the active
// ones end. Repeating it is harmless.
func Drain() (DrainResponse, error) { return drainCall(http.MethodPost) }

// CancelDrain makes a draining daemon admit factory runs again.
func CancelDrain() (DrainResponse, error) { return drainCall(http.MethodDelete) }

func drainCall(method string) (DrainResponse, error) {
	var out DrainResponse
	err := sessionCall(method, "/drain", nil, &out)
	if errors.Is(err, ErrNoSessionAPI) {
		return DrainResponse{}, ErrDrainUnsupported
	}
	return out, err
}

// Stop terminates the daemon with the given pid and waits until it stops
// answering. It cancels the daemon's active runs.
func Stop(pid int) error {
	sockPath, err := SocketPath()
	if err != nil {
		return err
	}
	return stopDaemon(pid, sockPath)
}

// stopDaemon asks the daemon to exit and waits until it stops answering, so the
// replacement does not race it for the socket.
func stopDaemon(pid int, sockPath string) error {
	if err := terminate(pid); err != nil {
		return fmt.Errorf("signal pid %d: %w", pid, err)
	}
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if reachable, _ := ping(sockPath); !reachable {
			return nil
		}
		time.Sleep(50 * time.Millisecond)
	}
	return fmt.Errorf("smelt daemon pid %d did not exit within 3s", pid)
}

// StopForCredentialChange stops a running smelt daemon so that the next launch
// picks up newly stored credentials.
//
// The daemon resolves its CircleCI client once, at startup, so one that started
// before a login holds a nil client for the rest of its life and streams no
// output however many times the developer retries. Stopping it here is what
// makes `smelt auth login` take effect: a `smelt watch` already on screen
// relaunches it through EnsureLaunched on its next poll, and otherwise the next
// `smelt watch` starts a daemon that can authenticate.
//
// Best-effort and silent, like RegisterCommand. Failing to stop the daemon must
// not fail a login that has otherwise succeeded, and the cost of not stopping it
// is the buffered output of a daemon that was not streaming anything anyway.
func StopForCredentialChange() bool {
	// A remote daemon is managed externally; we have no way to restart it, and
	// the local pid file / socket are unrelated to it.
	if TCPRemoteAddr() != "" {
		return false
	}
	pidPath, err := PIDPath()
	if err != nil {
		return false
	}
	sockPath, err := SocketPath()
	if err != nil {
		return false
	}
	running, pid, err := IsRunning(pidPath)
	if err != nil || !running {
		return false
	}
	// Only stop something that is actually answering: a stale pid file is the
	// launcher's problem to clean up, not ours.
	if reachable, _ := ping(sockPath); !reachable {
		return false
	}
	return stopDaemon(pid, sockPath) == nil
}

// IsDaemonRunning reports whether the smelt daemon is reachable. Use
// IsDaemonCompatible when the caller needs to confirm the build identity too.
func IsDaemonRunning() bool {
	ok, _ := pingDaemon()
	return ok
}

// IsDaemonCompatible reports whether the smelt daemon is reachable and suitable
// for delegation. For a local daemon that means matching the current build (a
// build mismatch means it may not support all API endpoints). For a remote
// daemon the build ID can never match — the binary lives on a different host
// with a different path and mtime — so reachability is the meaningful check.
func IsDaemonCompatible() bool {
	ok, build := pingDaemon()
	if !ok {
		return false
	}
	if TCPRemoteAddr() != "" {
		return true
	}
	return build == BuildID()
}

// RunValidate delegates a validate run to the daemon. req.ProjectRoot is the
// repo to validate, already resolved by the caller.
//
// The caller is responsible for deciding which fields to populate: req.Env and
// req.CircleCIToken should only be set when using the local Unix socket — over
// TCP the remote daemon uses its own credentials and environment. See
// runValidateViaDaemon in cmd/ for the canonical call site.
//
// Set req.AllowAsync to offer the daemon the option of releasing this caller
// and reporting later; a response carrying a TaskID is that offer taken, and
// means nothing has run yet.
//
// It takes the request type rather than a list of arguments because what the
// daemon needs to know about a run keeps growing, and every addition would
// otherwise be another positional parameter at two call sites.
func RunValidate(req ValidateRequest) (ValidateResponse, error) {
	client, err := longDaemonClient()
	if err != nil {
		return ValidateResponse{}, err
	}
	body, err := json.Marshal(req)
	if err != nil {
		return ValidateResponse{}, fmt.Errorf("marshal validate request: %w", err)
	}
	resp, err := client.Post("http://smeltd/validate", "application/json", bytes.NewReader(body))
	if err != nil {
		return ValidateResponse{}, fmt.Errorf("%w: %w", ErrDaemonUnavailable, err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		err := fmt.Errorf("smelt daemon returned %s: %s", resp.Status, bytes.TrimSpace(msg))
		// 404 means the daemon is running but does not have the /validate
		// endpoint — it is from an older build. Treat it as unavailable so
		// callers fall back to inline execution instead of surfacing the error.
		if resp.StatusCode == http.StatusNotFound {
			return ValidateResponse{}, fmt.Errorf("%w: %w", ErrDaemonUnavailable, err)
		}
		return ValidateResponse{}, err
	}
	var result ValidateResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return ValidateResponse{}, fmt.Errorf("decode validate response: %w", err)
	}
	return result, nil
}

// ErrAsyncRefused is returned by StartAsyncValidate when the daemon will not
// take this run asynchronously: the tree could not be fingerprinted, so a
// stale result would be undetectable, or the project already has its cap of
// runs in flight. Callers fall back to running inline, where the answer
// reaches whoever asked for it while it is still true. The daemon's reason is
// wrapped alongside the sentinel.
var ErrAsyncRefused = errors.New("async validation refused")

// StartAsyncValidate asks the daemon to validate projectRoot in the background
// and returns the new task's ID without waiting for the run.
func StartAsyncValidate(projectRoot string, args []string, circleCIToken string) (string, error) {
	sockPath, err := SocketPath()
	if err != nil {
		return "", err
	}
	body, err := json.Marshal(ValidateRequest{
		Args:          args,
		CircleCIToken: circleCIToken,
		Env:           os.Environ(),
		ProjectRoot:   projectRoot,
		WorkDir:       projectRoot,
	})
	if err != nil {
		return "", fmt.Errorf("marshal async validate request: %w", err)
	}
	// The short client is right here: this call returns as soon as the run is
	// accepted, so it must not inherit the no-timeout client /validate needs.
	resp, err := unixClient(sockPath).Post("http://smeltd/validate/async", "application/json", bytes.NewReader(body))
	if err != nil {
		return "", fmt.Errorf("%w: %w", ErrDaemonUnavailable, err)
	}
	defer func() { _ = resp.Body.Close() }()

	switch resp.StatusCode {
	case http.StatusAccepted:
		var result AsyncValidateResponse
		if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
			return "", fmt.Errorf("decode async validate response: %w", err)
		}
		return result.TaskID, nil
	case http.StatusConflict:
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return "", fmt.Errorf("%w: %s", ErrAsyncRefused, bytes.TrimSpace(msg))
	case http.StatusNotFound:
		// A daemon from a build without this endpoint. Unavailable, so the caller
		// runs inline rather than surfacing an error.
		return "", fmt.Errorf("%w: daemon has no /validate/async endpoint", ErrDaemonUnavailable)
	default:
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return "", fmt.Errorf("smelt daemon returned %s: %s", resp.Status, bytes.TrimSpace(msg))
	}
}

// CollectValidateResults returns the finished async results for projectRoot and
// clears them from the daemon, so a result is reported once and not repeated.
//
// Best-effort: with no daemon running there is nothing to collect and nothing to
// report, which is not an error worth surfacing on a hook path.
func CollectValidateResults(projectRoot string) ([]TaskState, error) {
	sockPath, err := SocketPath()
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrDaemonUnavailable, err)
	}
	url := "http://smeltd/validate/collect?root=" + neturl.QueryEscape(projectRoot)
	resp, err := unixClient(sockPath).Get(url)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrDaemonUnavailable, err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode == http.StatusNotFound {
		return nil, fmt.Errorf("%w: daemon has no /validate/collect endpoint", ErrDaemonUnavailable)
	}
	if resp.StatusCode != http.StatusOK {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return nil, fmt.Errorf("smelt daemon returned %s: %s", resp.Status, bytes.TrimSpace(msg))
	}
	var result CollectResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, fmt.Errorf("decode collect response: %w", err)
	}
	return result.Tasks, nil
}

// DaemonSubcommand is the hidden command the daemon runs as. Starting the
// daemon is re-executing this binary with it, so the command that registers it
// and the code that launches it share this one name.
const DaemonSubcommand = "_daemon"

// launchArgs is the command line the daemon is started with.
func launchArgs() []string { return []string{DaemonSubcommand} }

// launch starts the daemon process. A variable so tests can see whether
// EnsureRunning and EnsureLaunched decided to start one, without starting one.
var launch = launchDaemon
var replaceDaemon = stopDaemon

// EnsureRunning checks whether the smelt daemon is running and serving, and
// launches it if not.
func EnsureRunning() error {
	return (&DaemonClient{}).EnsureRunning()
}

// DaemonClient ensures the daemon is running and tells an interactive user when
// the one it found is from an older build. The zero value is silent. It owns the
// once-only state of that warning, so one client warns once however often it is
// asked.
type DaemonClient struct {
	// Stderr receives the stale-daemon warning when Interactive is set.
	Stderr      io.Writer
	Interactive bool

	warnOnce sync.Once
}

// EnsureRunningWithWarning is EnsureRunning for a command that reports a stale
// daemon on stderr when interactive.
func EnsureRunningWithWarning(stderr io.Writer, interactive bool) error {
	return (&DaemonClient{Stderr: stderr, Interactive: interactive}).EnsureRunning()
}

// EnsureRunning checks whether the daemon is running and serving, and launches
// it if not. A daemon from another build that still has runs is kept and
// drained rather than replaced.
func (c *DaemonClient) EnsureRunning() error {
	// Remote daemons are managed externally; local pid/socket operations are
	// irrelevant and would start a stray local daemon.
	if TCPRemoteAddr() != "" {
		return nil
	}
	pidPath, err := PIDPath()
	if err != nil {
		return err
	}
	sockPath, err := SocketPath()
	if err != nil {
		return err
	}
	running, pid, err := IsRunning(pidPath)
	if err != nil {
		return fmt.Errorf("check running: %w", err)
	}
	if !running {
		return launch()
	}
	info := pingInfo(sockPath)
	if !info.Reachable {
		return launch()
	}
	// A draining daemon is on its way out, finishing runs that replacing it
	// would cancel. Leave it: once it exits the next call launches the build
	// on disk.
	if info.BuildID == BuildID() || info.Draining && !info.Handover {
		return nil
	}
	return c.replaceStale(pid, sockPath)
}

// replaceStale deals with a reachable daemon from another build: it hands the
// main socket over, drains it, or replaces it when it has no runs to protect.
func (c *DaemonClient) replaceStale(pid int, sockPath string) error {
	if _, err := Handover(); err == nil {
		return launch()
	} else if !errors.Is(err, ErrHandoverUnsupported) {
		return err
	}
	// A reachable old daemon may own runs that must finish on its build. An
	// unreadable list is treated conservatively for the same reason.
	sessions, listErr := MainSessions("")
	active, listErr := activeRuns(sessions, listErr)
	if listErr != nil || active > 0 {
		// A daemon too old to drain still serves its runs; it is replaced by
		// the next command after it ends.
		_, _ = Drain()
		c.warnStale(active, listErr)
		return nil
	}
	// A daemon from another build is replaced rather than reused: see BuildID
	// for why reusing it degrades silently.
	if stopErr := replaceDaemon(pid, sockPath); stopErr != nil {
		return fmt.Errorf("replace stale smelt daemon: %w", stopErr)
	}
	return launch()
}

// activeRuns counts running sessions. A daemon without the session API has no
// runs to protect, so that error is not reported.
func activeRuns(sessions []Session, listErr error) (int, error) {
	active := 0
	for _, session := range sessions {
		if session.State == SessionRunning {
			active++
		}
	}
	if errors.Is(listErr, ErrNoSessionAPI) {
		listErr = nil
	}
	return active, listErr
}

// warnStale tells an interactive user, once, that the daemon is an older build.
func (c *DaemonClient) warnStale(active int, listErr error) {
	if !c.Interactive || c.Stderr == nil {
		return
	}
	c.warnOnce.Do(func() {
		if listErr != nil {
			_, _ = fmt.Fprintln(c.Stderr, "smelt daemon is from an older build and its active run count is unavailable; it will be replaced when they finish")
			return
		}
		_, _ = fmt.Fprintf(c.Stderr, "smelt daemon is from an older build and has %d active run(s); it will be replaced when they finish\n", active)
	})
}

// EnsureLaunched starts the daemon when nothing is answering and otherwise
// leaves whatever is there alone.
//
// Unlike EnsureRunning it never replaces a daemon from another build. It is
// called when a poll fails mid-session, and a dashboard that has been open for a
// while has no business restarting a daemon another one is using: the build
// check is a startup decision, made once, where the cost of being wrong is one
// restart rather than a restart per poll for as long as two dashboards are open.
func EnsureLaunched() error {
	// Remote daemons are managed externally; starting a local one would be wrong.
	if TCPRemoteAddr() != "" {
		return nil
	}
	pidPath, err := PIDPath()
	if err != nil {
		return err
	}
	sockPath, err := SocketPath()
	if err != nil {
		return err
	}
	running, _, err := IsRunning(pidPath)
	if err != nil {
		return fmt.Errorf("check running: %w", err)
	}
	if running {
		if reachable, _ := ping(sockPath); reachable {
			return nil
		}
	}
	return launch()
}

func launchDaemon() error {
	daemonDir, err := EnsureDir()
	if err != nil {
		return fmt.Errorf("ensure daemon dir: %w", err)
	}
	logPath, err := LogPath()
	if err != nil {
		return err
	}
	logFile, err := os.OpenFile(logPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return fmt.Errorf("open log file: %w", err)
	}
	defer func() { _ = logFile.Close() }()

	executable, err := os.Executable()
	if err != nil {
		return fmt.Errorf("get executable: %w", err)
	}

	child := exec.Command(executable, launchArgs()...)
	child.Stdout = logFile
	child.Stderr = logFile
	child.Stdin = nil
	// The daemon outlives the command and often the checkout that launched it.
	// Give it a stable cwd so a removed worktree cannot make later rsync and
	// shell commands fail at getcwd before they reach their explicit workdir.
	child.Dir = daemonDir
	detachProcess(child)
	if err := child.Start(); err != nil {
		return fmt.Errorf("start smelt daemon: %w", err)
	}

	pidPath, err := PIDPath()
	if err != nil {
		return err
	}
	sockPath, err := SocketPath()
	if err != nil {
		return err
	}
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		ok, _, _ := IsRunning(pidPath)
		if reachable, _ := ping(sockPath); ok && reachable {
			return nil
		}
		time.Sleep(100 * time.Millisecond)
	}
	return fmt.Errorf("smelt daemon did not start within 5s; check %s", logPath)
}

// ErrHandoverUnsupported identifies daemons requiring legacy drain waiting.
var ErrHandoverUnsupported = errors.New("daemon does not support listener handover")

// Handover releases main ownership while preserving existing runs.
func Handover() (DrainResponse, error) {
	var out DrainResponse
	err := sessionCall(http.MethodPost, "/handover", nil, &out)
	if errors.Is(err, ErrNoSessionAPI) {
		return out, ErrHandoverUnsupported
	}
	return out, err
}

// DiscoverDaemons returns main first, then private sockets in deterministic order.
// Only a confirmed missing listener is removed; slow or inaccessible sockets survive.
func DiscoverDaemons() ([]DaemonStatus, error) {
	paths, err := localSockets()
	if err != nil {
		return nil, err
	}
	result := make([]DaemonStatus, 0)
	for i, path := range paths {
		status, ok, err := probeDaemon(path, i == 0)
		if err != nil {
			return nil, err
		}
		if ok {
			result = append(result, status)
		}
	}
	return result, nil
}

// probeDaemon pings one socket. ok is false when nothing usable listens there;
// a confirmed-stale private socket is removed on the way.
func probeDaemon(path string, main bool) (DaemonStatus, bool, error) {
	before, _ := os.Lstat(path)
	client := unixClient(path)
	defer client.CloseIdleConnections()
	resp, err := client.Get("http://smeltd/ping")
	if err != nil {
		classified := classifyDialError(err, path)
		if !errors.Is(classified, ErrDaemonUnreachable) {
			return DaemonStatus{}, false, classified
		}
		if !main {
			removeIfStale(path, before)
		}
		return DaemonStatus{}, false, nil
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return DaemonStatus{}, false, nil
	}
	status := statusFromHeaders(resp.Header, path)
	if main {
		fillMainStatus(&status, resp.Header.Get("X-Smelt-Active") == "")
		return status, true, nil
	}
	expected, err := PIDSocketPath(status.PID)
	return status, err == nil && expected == path, nil
}

// statusFromHeaders reads the daemon metadata a ping response carries.
func statusFromHeaders(h http.Header, path string) DaemonStatus {
	pid, _ := strconv.Atoi(h.Get("X-Smelt-Pid"))
	active, _ := strconv.Atoi(h.Get("X-Smelt-Active"))
	return DaemonStatus{PID: pid, Revision: h.Get(HeaderRevision), Draining: h.Get(HeaderDraining) == headerTrue, ActiveRuns: active, Socket: path}
}

// fillMainStatus covers a main daemon too old to report its pid or active runs
// in the ping headers.
func fillMainStatus(status *DaemonStatus, countRuns bool) {
	if countRuns {
		if sessions, err := MainSessions(""); err == nil {
			for _, s := range sessions {
				if s.Factory != nil && !s.State.Finished() {
					status.ActiveRuns++
				}
			}
		}
	}
	if status.PID == 0 {
		if p, err := PIDPath(); err == nil {
			status.PID, _ = ReadPID(p)
		}
	}
}

// removeIfStale removes path when it is still the socket file seen before the
// failed dial, so a socket a new daemon just bound is never deleted.
func removeIfStale(path string, before os.FileInfo) {
	info, err := os.Lstat(path)
	if err == nil && before != nil && os.SameFile(before, info) && info.Mode()&os.ModeSocket != 0 {
		_ = os.Remove(path)
	}
}
