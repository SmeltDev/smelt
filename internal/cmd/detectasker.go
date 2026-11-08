package cmd

import (
	"context"
	"fmt"
	"os/exec"
	"strings"

	"github.com/smeltdev/smelt/internal/anthropic"
	"github.com/smeltdev/smelt/internal/config"
	"github.com/smeltdev/smelt/internal/harness"
	"github.com/smeltdev/smelt/internal/harness/claude"
	"github.com/smeltdev/smelt/internal/validate"
)

// detectionAsker is what command detection asks Claude with, built from the
// credential in effect, or nil when there is none. An API key goes to the
// Messages API. A subscription token is not accepted there, so it goes
// through the claude CLI, which is how the factory uses it too.
func detectionAsker(rc config.ResolvedConfig, workDir string) validate.Asker {
	cred, _, err := reviewCredential(rc)
	if err != nil {
		return nil
	}
	if cred.EnvVar == config.EnvAnthropicAPIKey {
		client, err := anthropic.New(anthropic.Config{APIKey: cred.Value, BaseURL: rc.AnthropicBaseURL})
		if err != nil {
			return nil
		}
		return client
	}
	return cliAsker{Dir: workDir, Cred: cred}
}

// cliAsker answers prompts by running the claude CLI on this machine, without
// tools, using the credential it is given.
type cliAsker struct {
	Dir  string
	Cred harness.Credential
}

// Ask implements validate.Asker. The CLI has no token limit flag and no
// separate system prompt, so maxTokens is not applied and system leads the prompt.
func (a cliAsker) Ask(ctx context.Context, model string, _ int, prompt string, system ...string) (string, error) {
	h := claude.Harness{}
	if len(system) > 0 {
		prompt = strings.Join(system, "\n") + "\n\n" + prompt
	}
	script := h.ReviewScript(harness.ReviewInput{RepoPath: a.Dir, Prompt: prompt, Model: model})
	cmd := exec.CommandContext(ctx, "sh", "-c", script)
	cmd.Env = cmd.Environ()
	for k, v := range h.Env(a.Cred, "") {
		cmd.Env = append(cmd.Env, k+"="+v)
	}
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("claude: %w", err)
	}
	return string(out), nil
}
