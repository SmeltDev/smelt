// Package claude drives Claude Code (claude -p) as a harness.
package claude

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/smeltdev/smelt/internal/harness"
)

// ErrMissing is returned when a sidecar has no claude binary.
var ErrMissing = errors.New("claude is not installed on the sidecar")

// ErrCredentialRejected is returned when Anthropic rejects the credential.
var ErrCredentialRejected = errors.New("anthropic rejected the credential")

// credentialRejectedRe matches claude's own authentication failure, which reads
// the same for a revoked API key and a stale subscription token.
var credentialRejectedRe = regexp.MustCompile(`(?i)failed to authenticate.*\b401\b`)

// defaultBaseURL is where claude sends requests when no base URL is set.
const defaultBaseURL = "https://api.anthropic.com"

// stderrTail is how much of stderr a failed command reports.
const stderrTail = 2000

// disallowedGit are the git commands that would move the implementer's work
// out of the working tree the loop collects it from.
var disallowedGit = []string{
	"Bash(git commit:*)", "Bash(git stash:*)", "Bash(git reset:*)",
	"Bash(git checkout:*)", "Bash(git switch:*)", "Bash(git rebase:*)",
}

// binary is the executable name.
const binary = "claude"

// Harness is Claude Code.
type Harness struct{}

// Name implements harness.Harness.
func (Harness) Name() harness.Name { return harness.Claude }

// Binary implements harness.Harness.
func (Harness) Binary() string { return binary }

// ImplementScript implements harness.Harness. The sidecar is a sandbox, which
// is what lets claude skip permission prompts there even when the image runs it
// as root.
func (Harness) ImplementScript(in harness.ImplementInput) string {
	args := []string{
		binary, "-p", "--output-format", "stream-json", "--verbose",
		"--dangerously-skip-permissions",
		"--disallowedTools", strings.Join(disallowedGit, ","),
		"--append-system-prompt", in.SystemPrompt,
	}
	switch {
	case in.Resume:
		args = append(args, "--resume", in.SessionID)
	case in.SessionID != "":
		args = append(args, "--session-id", in.SessionID)
	}
	if in.Model != "" {
		args = append(args, "--model", in.Model)
	}
	if in.ContextWindow != 0 {
		args = append(args, "--autocompact", fmt.Sprint(in.ContextWindow))
	}
	return "export IS_SANDBOX=1\n" + bashTimeoutExports(in) + Script(in.RepoPath, in.Prompt, args)
}

// Claude Code's Bash tool gives up on a command after two minutes unless told
// otherwise, which makes an agent background a long test run and poll it.
const (
	bashTimeoutSlack = 2 * time.Minute
	bashTimeoutFloor = 10 * time.Minute
)

// BashTimeouts are the values of BASH_DEFAULT_TIMEOUT_MS and
// BASH_MAX_TIMEOUT_MS for a turn: a command may by default run as long as the
// longest gate plus slack (at least ten minutes), and no longer than the turn
// itself. Both are zero when the turn's timeout is unknown.
func BashTimeouts(longestGate, turn time.Duration) (def, ceiling time.Duration) {
	if turn <= 0 {
		return 0, 0
	}
	def = min(max(longestGate+bashTimeoutSlack, bashTimeoutFloor), turn)
	return def, turn
}

// bashTimeoutExports is the script lines that set Claude Code's Bash timeouts
// for the turn, or "" when they are not known.
func bashTimeoutExports(in harness.ImplementInput) string {
	def, ceiling := BashTimeouts(in.LongestGate, in.Timeout)
	if ceiling == 0 {
		return ""
	}
	return fmt.Sprintf("export BASH_DEFAULT_TIMEOUT_MS=%d\nexport BASH_MAX_TIMEOUT_MS=%d\n", def.Milliseconds(), ceiling.Milliseconds())
}

// ReviewScript implements harness.Harness. With Stream the stdout is claude's
// stream-json, whose last line is the result a plain run prints alone.
func (Harness) ReviewScript(in harness.ReviewInput) string {
	format := "text"
	if in.Schema != "" {
		format = "json"
	}
	if in.Stream {
		format = "stream-json"
	}
	args := []string{binary, "-p", "--output-format", format, "--allowedTools", strings.Join(in.Tools, ",")}
	if in.Stream {
		// stream-json refuses to run without --verbose.
		args = append(args, "--verbose")
	}
	if in.Schema != "" {
		args = append(args, "--json-schema", in.Schema)
	}
	if in.Model != "" {
		args = append(args, "--model", in.Model)
	}
	return Script(in.RepoPath, in.Prompt, args)
}

// Script builds the shell script that runs claude in repoPath on a sidecar with
// prompt on stdin. args is the whole command line, starting with "claude".
func Script(repoPath, prompt string, args []string) string {
	return harness.PipeScript(binary, "", repoPath, prompt, args)
}

// result is the part of claude's --output-format json result that a review
// needs. structured_output is set only when the answer satisfied the schema.
type result struct {
	IsError          bool   `json:"is_error"`
	Subtype          string `json:"subtype"`
	Result           string `json:"result"`
	StructuredOutput *struct {
		Review   string          `json:"review"`
		Findings json.RawMessage `json:"findings"`
	} `json:"structured_output"`
}

// ReviewContext implements harness.Harness. The json result has no per-call
// usage, only the turn's total, which is one call's size only when the run
// made one call.
func (Harness) ReviewContext(stdout string) harness.Context {
	var res struct {
		NumTurns int `json:"num_turns"`
		Usage    struct {
			InputTokens         int `json:"input_tokens"`
			CacheCreationTokens int `json:"cache_creation_input_tokens"`
			CacheReadTokens     int `json:"cache_read_input_tokens"`
		} `json:"usage"`
	}
	if json.Unmarshal([]byte(harness.ResultLine(stdout)), &res) != nil || res.NumTurns != 1 {
		return harness.Context{}
	}
	u := res.Usage
	return harness.Context{Peak: u.InputTokens + u.CacheCreationTokens + u.CacheReadTokens, Calls: 1}
}

// maxResultInError caps how much of claude's own result text an error quotes.
const maxResultInError = 500

// ParseReview implements harness.Harness. A result with no structured output is
// an error, not a prose-only review.
func (Harness) ParseReview(stdout string) (harness.ReviewPayload, error) {
	var res result
	if err := json.Unmarshal([]byte(harness.ResultLine(stdout)), &res); err != nil {
		return harness.ReviewPayload{}, fmt.Errorf("read claude's result: %w", err)
	}
	if res.IsError || res.StructuredOutput == nil {
		text := []rune(strings.TrimSpace(res.Result))
		if len(text) > maxResultInError {
			text = append(text[:maxResultInError], '…')
		}
		return harness.ReviewPayload{}, fmt.Errorf("claude gave no structured findings (%s): %s", res.Subtype, string(text))
	}
	return harness.ReviewPayload{Review: res.StructuredOutput.Review, Findings: res.StructuredOutput.Findings}, nil
}

// ParseAnswer implements harness.Harness. stdout is the whole json result, or
// a stream whose last "result" event is it.
func (Harness) ParseAnswer(stdout string) (json.RawMessage, error) {
	var res struct {
		IsError          bool            `json:"is_error"`
		Subtype          string          `json:"subtype"`
		Result           string          `json:"result"`
		StructuredOutput json.RawMessage `json:"structured_output"`
	}
	if err := json.Unmarshal([]byte(resultLine(stdout)), &res); err != nil {
		return nil, fmt.Errorf("read claude's result: %w", err)
	}
	if res.IsError || len(res.StructuredOutput) == 0 || string(res.StructuredOutput) == "null" {
		text := []rune(strings.TrimSpace(res.Result))
		if len(text) > maxResultInError {
			text = append(text[:maxResultInError], '…')
		}
		return nil, fmt.Errorf("claude gave no structured answer (%s): %s", res.Subtype, string(text))
	}
	return res.StructuredOutput, nil
}

// resultLine is the line of claude's output that is its result: the whole
// output for --output-format json, the last "result" event for stream-json.
func resultLine(output string) string {
	lines := strings.Split(strings.TrimSpace(output), "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		var e struct {
			Type string `json:"type"`
		}
		if json.Unmarshal([]byte(lines[i]), &e) == nil && e.Type == "result" {
			return lines[i]
		}
	}
	return output
}

// Env implements harness.Harness: only the credential, and the base URL when it
// points somewhere other than Anthropic.
func (Harness) Env(cred harness.Credential, baseURL string) map[string]string {
	env := map[string]string{cred.EnvVar: cred.Value}
	if baseURL != "" && strings.TrimRight(baseURL, "/") != defaultBaseURL {
		env["ANTHROPIC_BASE_URL"] = baseURL
	}
	return env
}

// Prepare implements harness.Harness: claude needs no setup.
func (Harness) Prepare(harness.Credential) (string, map[string]string) { return "", nil }

// RunError implements harness.Harness. Every caller running claude classifies
// its end here, so a missing binary or a rejected credential reads the same
// whatever ran.
func (Harness) RunError(ctx context.Context, timeout time.Duration, code int, execErr error, stdout, stderr string) error {
	switch {
	case ctx.Err() == context.DeadlineExceeded:
		return harness.TimeoutError(timeout, ctx.Err())
	case ctx.Err() == context.Canceled:
		return ctx.Err()
	case execErr != nil:
		return harness.Classified(harness.Unavailable, fmt.Errorf("exec: %w", execErr), nil)
	case code == harness.MissingExitCode:
		return harness.Classified(harness.Unavailable, ErrMissing, nil)
	case code != 0 && CredentialRejected(stdout, stderr):
		return harness.Classified(harness.Auth, ErrCredentialRejected, nil)
	case code != 0:
		p := Harness{}.NewImplementParser(nil)
		p.Write([]byte(stdout + "\n" + stderr))
		p.Flush()
		rl := p.Outcome().Turn.RateLimit
		if rl != nil && rl.Status != "allowed" && rl.Status != "allowed_warning" {
			return harness.Classified(harness.RateLimited, exitError(code, stderr), rl.ResetsAt)
		}
		if !p.Outcome().Done {
			return harness.Classified(harness.Unavailable, exitError(code, stderr), nil)
		}
		return exitError(code, stderr)
	}
	return nil
}

// CredentialRejected reports whether claude failed to authenticate. Both
// streams are checked: the 401 lands on stdout, while stderr can carry
// unrelated warnings.
func CredentialRejected(stdout, stderr string) bool {
	return credentialRejectedRe.MatchString(stdout) || credentialRejectedRe.MatchString(stderr)
}

func exitError(code int, stderr string) error {
	stderr = strings.TrimSpace(stderr)
	if stderr == "" {
		return fmt.Errorf("claude exited %d", code)
	}
	return fmt.Errorf("claude exited %d: %s", code, harness.Tail(stderr, stderrTail))
}
