// Package grokauth holds smelt's side of a Grok login: the ~/.grok/auth.json
// that `grok login` writes, the copy of it that may enter a container, its
// expiry, and renewing it on this machine before a run.
package grokauth

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/smeltdev/smelt/internal/harness"
	"github.com/smeltdev/smelt/internal/harness/grok"
)

// Binary is the grok executable on the host.
const Binary = "grok"

// RenewWithin is how long before the access token expires that a login is
// renewed rather than used: a run must not outlive its credential, and the
// container copy has no refresh token.
const RenewWithin = 2 * time.Hour

// renewTimeout bounds the host command that makes grok renew its login.
const renewTimeout = 2 * time.Minute

// renewArgs is a cheap authenticated grok command; grok renews an access token
// that is close to expiry when it makes a request.
var renewArgs = []string{"models"}

// ErrNone is returned when there is no Grok credential at all.
var ErrNone = errors.New("no Grok credential found: run `grok login`, or set " + grok.EnvAPIKey)

// ErrExpired is returned when the login is, or is about to be, too old to
// last a run and could not be renewed on this machine.
var ErrExpired = errors.New("your Grok login has expired or expires soon: run `grok login`")

// Runner runs a command on this machine. It is the seam that lets a test
// stand in for the grok CLI.
type Runner func(ctx context.Context, name string, args ...string) error

// ExecRunner runs the command for real.
func ExecRunner(ctx context.Context, name string, args ...string) error {
	if err := exec.CommandContext(ctx, name, args...).Run(); err != nil {
		return fmt.Errorf("%s %s: %w", name, strings.Join(args, " "), err)
	}
	return nil
}

// Host is the grok CLI and its login on this machine. The zero value uses the
// real CLI and ~/.grok/auth.json.
type Host struct {
	// AuthPath is the auth.json to read; ~/.grok/auth.json when empty.
	AuthPath string
	// Binary is the grok executable; "grok" when empty.
	Binary string
	// Run runs the renewal command; ExecRunner when nil.
	Run Runner
	// Env looks up an environment variable; os.Getenv when nil.
	Env func(string) string
}

func (h Host) authPath() (string, error) {
	if h.AuthPath != "" {
		return h.AuthPath, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("find the home directory: %w", err)
	}
	return filepath.Join(home, ".grok", "auth.json"), nil
}

func (h Host) binary() string {
	if h.Binary == "" {
		return Binary
	}
	return h.Binary
}

func (h Host) getenv(k string) string {
	if h.Env == nil {
		return os.Getenv(k)
	}
	return h.Env(k)
}

// entry is one login in auth.json. Fields smelt does not read stay in the raw
// map, so a copy keeps them.
type entry = map[string]json.RawMessage

// parse reads auth.json: a map with one entry per login.
func parse(raw []byte) (map[string]entry, error) {
	var doc map[string]entry
	if err := json.Unmarshal(raw, &doc); err != nil || len(doc) == 0 {
		return nil, errors.New("auth.json has no logins: it is not from `grok login`")
	}
	return doc, nil
}

// ContainerCopy is auth.json as a container may have it: every refresh token
// is blanked, so a container can never rotate the host's login and a leaked
// sandbox holds a credential that dies with the access token. The fields stay,
// the token as an empty string; every other field is kept as it was.
func ContainerCopy(raw []byte) ([]byte, error) {
	doc, err := parse(raw)
	if err != nil {
		return nil, err
	}
	blank, _ := json.Marshal("")
	for _, e := range doc {
		e["refresh_token"] = blank
	}
	return json.Marshal(doc)
}

// Expiry is when the login in auth.json stops working: the latest expires_at
// of its entries. The token itself is never returned or printed.
func Expiry(raw []byte) (time.Time, error) {
	doc, err := parse(raw)
	if err != nil {
		return time.Time{}, err
	}
	var latest time.Time
	for _, e := range doc {
		t, err := parseExpiry(e["expires_at"])
		if err != nil {
			return time.Time{}, err
		}
		if t.After(latest) {
			latest = t
		}
	}
	return latest, nil
}

// parseExpiry reads expires_at as RFC 3339 text or as a Unix time in seconds
// or milliseconds.
func parseExpiry(v json.RawMessage) (time.Time, error) {
	errBad := errors.New("auth.json has no readable expires_at")
	if len(v) == 0 {
		return time.Time{}, errBad
	}
	var s string
	if json.Unmarshal(v, &s) == nil {
		if t, err := time.Parse(time.RFC3339, s); err == nil {
			return t, nil
		}
		if n, err := strconv.ParseFloat(s, 64); err == nil {
			return unixTime(n), nil
		}
		return time.Time{}, errBad
	}
	var n float64
	if json.Unmarshal(v, &n) == nil && n > 0 {
		return unixTime(n), nil
	}
	return time.Time{}, errBad
}

func unixTime(n float64) time.Time {
	if n > 1e11 { // milliseconds: seconds will not reach 1e11 for millennia
		n /= 1000
	}
	return time.Unix(int64(n), 0)
}

// Login is a Grok credential found on this machine.
type Login struct {
	// APIKey is a console.x.ai key; it wins over a subscription login.
	APIKey string
	// Auth is the full auth.json of a `grok login`, refresh token and all. It
	// stays on this machine.
	Auth []byte
}

// Find looks for a credential: XAI_API_KEY first, then the grok login.
func (h Host) Find() (Login, error) {
	if k := h.getenv(grok.EnvAPIKey); k != "" {
		return Login{APIKey: k}, nil
	}
	path, err := h.authPath()
	if err != nil {
		return Login{}, err
	}
	raw, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return Login{}, ErrNone
	}
	if err != nil {
		return Login{}, fmt.Errorf("read the Grok login: %w", err)
	}
	return Login{Auth: raw}, nil
}

// Credential is what a run's sidecars get: for an API key the key, for a login
// the base64 of the container copy of auth.json.
func (l Login) Credential() (harness.Credential, error) {
	if l.APIKey != "" {
		return harness.Credential{EnvVar: grok.EnvAPIKey, Value: l.APIKey}, nil
	}
	cp, err := ContainerCopy(l.Auth)
	if err != nil {
		return harness.Credential{}, err
	}
	return harness.Credential{EnvVar: grok.EnvAuthJSON, Value: base64.StdEncoding.EncodeToString(cp)}, nil
}

// ForRun finds the credential for a run and makes sure a login lasts it: one
// with less than RenewWithin left is renewed by running grok on this machine
// and read again. A login that is still too short, or unreadable, is an error
// rather than a run that dies partway.
func (h Host) ForRun(ctx context.Context, now time.Time) (harness.Credential, error) {
	l, err := h.Find()
	if err != nil {
		return harness.Credential{}, err
	}
	if l.APIKey == "" {
		if l.Auth, err = h.ensure(ctx, l.Auth, now); err != nil {
			return harness.Credential{}, err
		}
	}
	return l.Credential()
}

func (h Host) ensure(ctx context.Context, raw []byte, now time.Time) ([]byte, error) {
	exp, err := Expiry(raw)
	if err != nil {
		return nil, err
	}
	if exp.Sub(now) >= RenewWithin {
		return raw, nil
	}
	run := h.Run
	if run == nil {
		run = ExecRunner
	}
	ctx, cancel := context.WithTimeout(ctx, renewTimeout)
	defer cancel()
	// A failed renewal is not fatal by itself: the login may simply be too old
	// to renew, which the re-read below reports.
	renewErr := run(ctx, h.binary(), renewArgs...)
	path, err := h.authPath()
	if err != nil {
		return nil, err
	}
	fresh, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read the renewed Grok login: %w", err)
	}
	if exp, err = Expiry(fresh); err != nil {
		return nil, err
	}
	if exp.Sub(now) < RenewWithin {
		if renewErr != nil {
			return nil, fmt.Errorf("%w (renewing it failed: %w)", ErrExpired, renewErr)
		}
		return nil, ErrExpired
	}
	return fresh, nil
}
