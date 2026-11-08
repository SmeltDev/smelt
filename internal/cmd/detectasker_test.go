package cmd

import (
	"os"
	"path/filepath"
	"testing"

	"gotest.tools/v3/assert"

	"github.com/smeltdev/smelt/internal/config"
	"github.com/smeltdev/smelt/internal/harness"
)

func TestDetectionAskerFollowsTheCredential(t *testing.T) {
	assert.Assert(t, detectionAsker(config.ResolvedConfig{}, t.TempDir()) == nil)

	_, isCLI := detectionAsker(config.ResolvedConfig{ClaudeOAuthToken: "tok"}, t.TempDir()).(cliAsker)
	assert.Assert(t, isCLI, "a subscription token goes through the claude CLI")

	key := detectionAsker(config.ResolvedConfig{AnthropicAPIKey: "k"}, t.TempDir())
	assert.Assert(t, key != nil)
	_, isCLI = key.(cliAsker)
	assert.Assert(t, !isCLI, "an API key uses the Messages API")
}

func TestCLIAskerRunsClaudeWithTheToken(t *testing.T) {
	bin := t.TempDir()
	fake := "#!/bin/sh\ncat >/dev/null\necho \"make test $CLAUDE_CODE_OAUTH_TOKEN\"\n"
	assert.NilError(t, os.WriteFile(filepath.Join(bin, "claude"), []byte(fake), 0o755))
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))

	a := cliAsker{Dir: t.TempDir(), Cred: harness.Credential{EnvVar: config.EnvClaudeOAuthToken, Value: "tok"}}
	got, err := a.Ask(t.Context(), "m", 64, "prompt", "system")
	assert.NilError(t, err)
	assert.Equal(t, got, "make test tok\n")
}
