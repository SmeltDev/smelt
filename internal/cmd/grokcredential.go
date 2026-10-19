package cmd

import (
	"context"
	"fmt"
	"time"

	"github.com/smeltdev/smelt/internal/grokauth"
	"github.com/smeltdev/smelt/internal/harness"
	"github.com/smeltdev/smelt/internal/iostream"
	"github.com/smeltdev/smelt/internal/ui"
)

// grokHost is the grok CLI and login on this machine. A var so tests can point
// it at a fake runner and auth file.
var grokHost = grokauth.Host{}

const hintGrokLogin = "Run 'grok login' on this machine, or set XAI_API_KEY."

// grokDaemonAuth is the daemon's Grok setup: a login is looked for once, at
// startup, but checked and renewed at the start of each run, since a daemon
// can outlive a login's access token.
func grokDaemonAuth(host grokauth.Host) (refresh func(context.Context) (harness.Credential, error), authError string) {
	if _, err := host.Find(); err != nil {
		return nil, fmt.Sprintf("no Grok credential — factory runs with grok unavailable (%s)", hintGrokLogin)
	}
	return func(ctx context.Context) (harness.Credential, error) {
		return grokRunCredential(ctx, host)
	}, ""
}

// grokRunCredential is the Grok credential for one run: the host's login,
// renewed when its access token is about to expire.
func grokRunCredential(ctx context.Context, host grokauth.Host) (harness.Credential, error) {
	cred, err := host.ForRun(ctx, time.Now())
	if err != nil {
		return harness.Credential{}, fmt.Errorf("grok: %w", err)
	}
	return cred, nil
}

// grokAuthStep is the init step for Grok. Grok's login is `grok login` on this
// machine, which smelt reads but does not run, so it only reports. Never fatal.
func grokAuthStep(streams iostream.Streams) {
	if _, err := grokHost.Find(); err == nil {
		streams.ErrPrintln("Grok credential: found")
		return
	}
	streams.ErrPrintln(ui.Dim("No Grok credential. " + hintGrokLogin))
}
