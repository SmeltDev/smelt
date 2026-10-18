// Package grok drives xAI's Grok CLI (grok -p) as a harness.
package grok

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/smeltdev/smelt/internal/brand"
	"github.com/smeltdev/smelt/internal/harness"
	"github.com/smeltdev/smelt/internal/sidecar"
)

// ErrMissing is returned when a sidecar has no grok binary.
var ErrMissing = errors.New("grok is not installed in this image: run `" + brand.Command + " factory image --update-kit`")

// ErrCredentialRejected is returned when xAI rejects the credential, which for
// a subscription login mostly means its access token expired during the run.
var ErrCredentialRejected = errors.New("grok's login expired or was rejected: run `grok login` on this machine")

// Environment variables and paths on the sidecar.
const (
	// EnvAuthJSON carries the base64 of auth.json to Prepare, and only to it.
	EnvAuthJSON = "SMELT_GROK_AUTH_JSON"
	// EnvAPIKey is the variable grok reads an API key from.
	EnvAPIKey = "XAI_API_KEY" //nolint:gosec // the name of a variable, not a credential

	guardDir = "/tmp/smelt-guard"
	// authDir is where grok keeps its login, relative to $HOME.
	authDir = ".grok"
)

const (
	binary     = "grok"
	stderrTail = 2000
	// maxTurns bounds one implementer turn's model calls. It is generous: the
	// turn's own timeout is what normally ends a runaway.
	maxTurns = 200
	// reviewTools is what a reviewer may use: reading only.
	reviewTools = "read_file,grep,list_dir"
)

var credentialRejectedRe = regexp.MustCompile(`(?i)(401 unauthorized|not logged in|login (has )?expired|session expired|invalid api key|authentication (required|failed)|run .?grok login)`)

// Harness is xAI's Grok CLI.
type Harness struct{}

// Name implements harness.Harness.
func (Harness) Name() harness.Name { return harness.Grok }

// Binary implements harness.Harness.
func (Harness) Binary() string { return binary }

// ImplementScript implements harness.Harness. The container is the sandbox, as
// with claude's --dangerously-skip-permissions. Grok has no system-prompt flag,
// so the rules go ahead of the prompt, and the git guard stands in for
// claude's --disallowedTools. The prompt goes in through a file, which
// --prompt-file reads, so its size and quoting never reach the command line.
func (Harness) ImplementScript(in harness.ImplementInput) string {
	args := []string{binary, "--output-format", "streaming-messages-json", "--always-approve", "--max-turns", strconv.Itoa(maxTurns)}
	if in.Model != "" {
		args = append(args, "-m", in.Model)
	}
	switch {
	case in.Resume:
		args = append(args, "--resume", in.SessionID)
	case in.SessionID != "":
		args = append(args, "--session-id", in.SessionID)
	}
	prompt := in.Prompt
	if in.SystemPrompt != "" {
		prompt = "## Rules\n\n" + in.SystemPrompt + "\n\n## Task\n\n" + in.Prompt
	}
	return harness.ScriptHead(binary, guardDir+":") + promptFileLine(prompt) + fmt.Sprintf(`cd %s && %s --prompt-file "$pf"
code=$?
rm -f "$pf"
exit $code
`, sidecar.ShellEscape(in.RepoPath), sidecar.ShellJoin(args))
}

// promptFileLine is the script lines that write prompt, base64-encoded so no
// quoting in it can reach the shell, to a temporary file named by $pf.
func promptFileLine(prompt string) string {
	return fmt.Sprintf(`pf=$(mktemp) || exit 1
echo %s | base64 -d > "$pf"
`, base64.StdEncoding.EncodeToString([]byte(prompt)))
}

// reviewChangedTree is what the review script prints when the reviewer changed
// files, which a review must not do.
const reviewChangedTree = "smelt: the review changed files in the working tree (git status differs from before); a review is read-only, so its answer was dropped"

// ReviewScript implements harness.Harness. The reviewer gets only read tools,
// and the working tree is compared before and after as a second check, as for
// codex: a review that changed it fails and its answer is dropped. The answer
// is one JSON object, held back until the check passes, so Stream changes
// nothing.
func (Harness) ReviewScript(in harness.ReviewInput) string {
	args := []string{binary}
	if in.Schema != "" {
		args = append(args, "--json-schema", in.Schema)
	}
	args = append(args, "--tools", reviewTools)
	if in.Model != "" {
		args = append(args, "-m", in.Model)
	}
	repo := "git -C " + sidecar.ShellEscape(in.RepoPath)
	status := "{ " + repo + " status --porcelain; " + repo + " diff HEAD; } 2>&1 | cksum"
	return harness.ScriptHead(binary, guardDir+":") + "before=$(" + status + ")\n" + promptFileLine(in.Prompt) +
		fmt.Sprintf(`out=$(mktemp) || exit 1
cd %s && %s --prompt-file "$pf" >"$out"
code=$?
after=$(%s)
if [ "$before" != "$after" ]; then
  echo "%s" >&2
  code=1
fi
[ $code -eq 0 ] && cat "$out"
rm -f "$pf" "$out"
exit $code
`, sidecar.ShellEscape(in.RepoPath), sidecar.ShellJoin(args), status, reviewChangedTree)
}

// reviewResult is the part of grok's json output that a review needs. The text
// field is not read: it can hold several objects, a premature one first.
type reviewResult struct {
	StructuredOutput *struct {
		Review   string          `json:"review"`
		Findings json.RawMessage `json:"findings"`
	} `json:"structuredOutput"`
	StopReason string `json:"stopReason"`
}

// ParseReview implements harness.Harness. A result with no structuredOutput is
// an error, not a guess from the text.
func (Harness) ParseReview(stdout string) (harness.ReviewPayload, error) {
	var res reviewResult
	if err := json.Unmarshal([]byte(strings.TrimSpace(stdout)), &res); err != nil {
		return harness.ReviewPayload{}, fmt.Errorf("read grok's result: %w", err)
	}
	if res.StructuredOutput == nil {
		return harness.ReviewPayload{}, fmt.Errorf("grok gave no structured findings (stop reason %q)", res.StopReason)
	}
	return harness.ReviewPayload{Review: res.StructuredOutput.Review, Findings: res.StructuredOutput.Findings}, nil
}

// ReviewContext implements harness.Harness. The json output has only the turn's
// total usage, which is one call's size only when the run made one call.
func (Harness) ReviewContext(stdout string) harness.Context {
	var res struct {
		Usage struct {
			InputTokens         int `json:"input_tokens"`
			CacheCreationTokens int `json:"cache_creation_input_tokens"`
			CacheReadTokens     int `json:"cache_read_input_tokens"`
		} `json:"usage"`
		ModelUsage map[string]struct {
			ModelCalls int `json:"modelCalls"`
		} `json:"modelUsage"`
	}
	if json.Unmarshal([]byte(stdout), &res) != nil {
		return harness.Context{}
	}
	calls := 0
	for _, m := range res.ModelUsage {
		calls += m.ModelCalls
	}
	if calls != 1 {
		return harness.Context{}
	}
	u := res.Usage
	return harness.Context{Peak: u.InputTokens + u.CacheCreationTokens + u.CacheReadTokens, Calls: 1}
}

// Env implements harness.Harness. A turn only ever gets an API key; the
// subscription login is a file that Prepare wrote.
func (Harness) Env(cred harness.Credential, _ string) map[string]string {
	env := map[string]string{}
	if cred.EnvVar == EnvAPIKey {
		env[EnvAPIKey] = cred.Value
	}
	return env
}

// Prepare implements harness.Harness: it installs the git guard and, for a
// subscription login, writes auth.json from the environment, in a directory
// only the user can read.
func (Harness) Prepare(cred harness.Credential) (string, map[string]string) {
	guard := base64.StdEncoding.EncodeToString([]byte(harness.GitGuardScript(guardDir)))
	script := fmt.Sprintf(`umask 077
mkdir -p %[1]s && chmod 700 %[1]s || exit 1
echo %[2]s | base64 -d > %[1]s/git && chmod 755 %[1]s/git || exit 1
`, guardDir, guard)
	env := map[string]string{}
	if cred.EnvVar == EnvAuthJSON {
		script += fmt.Sprintf(`mkdir -p "$HOME/%[1]s" && chmod 700 "$HOME/%[1]s" || exit 1
printf %%s "$%[2]s" | base64 -d > "$HOME/%[1]s/auth.json" && chmod 600 "$HOME/%[1]s/auth.json" || exit 1
`, authDir, EnvAuthJSON)
		env[EnvAuthJSON] = cred.Value
	}
	return script, env
}

// RunError implements harness.Harness.
func (Harness) RunError(ctx context.Context, timeout time.Duration, code int, execErr error, stdout, stderr string) error {
	switch {
	case ctx.Err() == context.DeadlineExceeded:
		return harness.TimeoutError(timeout, ctx.Err())
	case execErr != nil:
		return fmt.Errorf("exec: %w", execErr)
	case code == harness.MissingExitCode:
		return ErrMissing
	case code != 0 && (credentialRejectedRe.MatchString(stdout) || credentialRejectedRe.MatchString(stderr)):
		return ErrCredentialRejected
	case code != 0:
		stderr = strings.TrimSpace(stderr)
		if stderr == "" {
			return fmt.Errorf("grok exited %d", code)
		}
		return fmt.Errorf("grok exited %d: %s", code, harness.Tail(stderr, stderrTail))
	}
	return nil
}
