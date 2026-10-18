package cmd

import (
	"context"
	"path/filepath"
	"testing"

	"gotest.tools/v3/assert"

	"github.com/smeltdev/smelt/internal/config"
	"github.com/smeltdev/smelt/internal/grokauth"
	"github.com/smeltdev/smelt/internal/harness"
	"github.com/smeltdev/smelt/internal/harness/grok"
)

// useGrokKey points the grok host at an API key and no login file.
func useGrokKey(t *testing.T, key string) {
	t.Helper()
	old := grokHost
	grokHost = grokauth.Host{
		AuthPath: filepath.Join(t.TempDir(), "missing.json"),
		Env: func(k string) string {
			if k == grok.EnvAPIKey {
				return key
			}
			return ""
		},
	}
	t.Cleanup(func() { grokHost = old })
}

func TestGrokHarnessCredentialAndReadiness(t *testing.T) {
	useGrokKey(t, "xai-key")
	assert.Assert(t, harnessReady(harness.Grok))
	ac, err := harnessCredential(context.Background(), harness.Grok, config.ResolvedConfig{})
	assert.NilError(t, err)
	assert.DeepEqual(t, ac.Credential, harness.Credential{EnvVar: grok.EnvAPIKey, Value: "xai-key"})

	useGrokKey(t, "")
	assert.Assert(t, !harnessReady(harness.Grok))
	_, err = harnessCredential(context.Background(), harness.Grok, config.ResolvedConfig{})
	assert.ErrorContains(t, err, "Grok")
}

func TestBootstrapCredentialForGrokNeedsNoClaudeConfig(t *testing.T) {
	useGrokKey(t, "xai-key")
	ac, err := bootstrapCredential(testCmd())(context.Background(), harness.Grok)
	assert.NilError(t, err)
	assert.Equal(t, ac.Credential.Value, "xai-key")
}

func TestDaemonReviewConfigCarriesGrok(t *testing.T) {
	useGrokKey(t, "xai-key")
	cfg := daemonReviewConfig(config.ResolvedConfig{}, nil)
	assert.Assert(t, cfg.Grok.Refresh != nil)
	cred, err := cfg.Grok.Refresh(context.Background())
	assert.NilError(t, err)
	assert.Equal(t, cred.Value, "xai-key")

	useGrokKey(t, "")
	cfg = daemonReviewConfig(config.ResolvedConfig{}, nil)
	assert.Assert(t, cfg.Grok.Refresh == nil)
	assert.Assert(t, cfg.Grok.AuthError != "")
}
