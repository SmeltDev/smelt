package claude

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"gotest.tools/v3/assert"

	"github.com/smeltdev/smelt/internal/harness"
)

func TestBashTimeouts(t *testing.T) {
	tests := []struct {
		name        string
		longestGate time.Duration
		turn        time.Duration
		wantDefault time.Duration
		wantCeiling time.Duration
	}{
		{"short gates get the ten minute floor", 30 * time.Second, 30 * time.Minute, 10 * time.Minute, 30 * time.Minute},
		{"a long gate gets two minutes of slack", 15 * time.Minute, 30 * time.Minute, 17 * time.Minute, 30 * time.Minute},
		{"no gates get the floor", 0, 30 * time.Minute, 10 * time.Minute, 30 * time.Minute},
		{"the default never exceeds the turn", 25 * time.Minute, 20 * time.Minute, 20 * time.Minute, 20 * time.Minute},
		{"an unknown turn sets nothing", 5 * time.Minute, 0, 0, 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			def, ceiling := BashTimeouts(tt.longestGate, tt.turn)
			assert.Equal(t, def, tt.wantDefault)
			assert.Equal(t, ceiling, tt.wantCeiling)
		})
	}
}

func TestImplementScriptExportsTheBashTimeouts(t *testing.T) {
	script := Harness{}.ImplementScript(harness.ImplementInput{
		RepoPath: "/repo", Prompt: "go", LongestGate: 5 * time.Minute, Timeout: 30 * time.Minute,
	})
	assert.Assert(t, strings.Contains(script, "export BASH_DEFAULT_TIMEOUT_MS=600000\n"), script)
	assert.Assert(t, strings.Contains(script, "export BASH_MAX_TIMEOUT_MS=1800000\n"), script)

	script = Harness{}.ImplementScript(harness.ImplementInput{RepoPath: "/repo", Prompt: "go"})
	assert.Assert(t, !strings.Contains(script, "BASH_"), script)
}

func TestImplementScriptAutoCompaction(t *testing.T) {
	for _, tc := range []struct {
		window int
		want   string
	}{{150000, "--autocompact 150000"}, {250000, "--autocompact 250000"}, {0, ""}} {
		script := Harness{}.ImplementScript(harness.ImplementInput{RepoPath: "/repo", Prompt: "go", ContextWindow: tc.window})
		if tc.want == "" {
			assert.Assert(t, !strings.Contains(script, "--autocompact"), script)
		} else {
			assert.Assert(t, strings.Contains(script, strings.ReplaceAll(tc.want, " ", "' '")), script)
		}
	}
}

func TestRunErrorDeadline(t *testing.T) {
	ctx, cancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer cancel()
	tests := []struct {
		name    string
		timeout time.Duration
		want    string
	}{
		{"with a run timeout", 30 * time.Second, "timed out after 30s"},
		{"without one", 0, "timed out: context deadline exceeded"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Error(t, Harness{}.RunError(ctx, tt.timeout, -1, nil, "", ""), tt.want)
		})
	}
}

func TestReviewScriptStream(t *testing.T) {
	in := harness.ReviewInput{RepoPath: "/r", Prompt: "look", Schema: `{}`, Tools: []string{"Read"}}
	plain := Harness{}.ReviewScript(in)
	assert.Assert(t, strings.Contains(plain, "'json'"), plain)
	assert.Assert(t, !strings.Contains(plain, "--verbose"), plain)

	in.Stream = true
	streamed := Harness{}.ReviewScript(in)
	assert.Assert(t, strings.Contains(streamed, "'stream-json'"), streamed)
	assert.Assert(t, strings.Contains(streamed, "'--verbose'"), "stream-json needs --verbose: %s", streamed)
	assert.Assert(t, strings.Contains(streamed, "'--json-schema'"), "the schema still applies: %s", streamed)
	assert.Assert(t, strings.Contains(streamed, "'--allowedTools' 'Read'"), streamed)
}

func TestReviewContext(t *testing.T) {
	b, err := os.ReadFile("../testdata/real/claude-stream.jsonl")
	assert.NilError(t, err)
	lines := strings.Split(strings.TrimSpace(string(b)), "\n")
	result := lines[len(lines)-1]
	assert.Assert(t, strings.Contains(result, `"num_turns":1`))

	// One turn is one model call, so the turn's usage is that call's size.
	assert.Equal(t, Harness{}.ReviewContext(result), harness.Context{Peak: 2796 + 2074 + 15333, Calls: 1})
	// Several turns only have a total, which says nothing of the peak.
	several := strings.Replace(result, `"num_turns":1`, `"num_turns":4`, 1)
	assert.Equal(t, Harness{}.ReviewContext(several), harness.Context{})
	assert.Equal(t, Harness{}.ReviewContext("not json"), harness.Context{})
}

func TestFailureClassification(t *testing.T) {
	for _, tc := range []struct {
		name           string
		code           int
		stdout, stderr string
		kind           harness.FailureKind
	}{
		{"missing", harness.MissingExitCode, "", "", harness.Unavailable},
		{"auth", 1, "", "Failed to authenticate: API Error: 401 Unauthorized", harness.Auth},
		{"crash", 2, "", "process crashed", harness.Unavailable},
		{"code outcome", 0, "BLOCKED: need a decision", "", ""},
		{"rate limit with reset", 1, `{"type":"rate_limit_event","rate_limit_info":{"status":"rejected","resetsAt":1792365600}}`, "", harness.RateLimited},
		{"rate limit without reset", 1, `{"type":"rate_limit_event","rate_limit_info":{"status":"rejected"}}`, "", harness.RateLimited},
		{"allowed overage rejection", 0, `{"type":"rate_limit_event","rate_limit_info":{"status":"allowed","overageStatus":"rejected"}}`, "", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := (Harness{}).RunError(context.Background(), time.Minute, tc.code, nil, tc.stdout, tc.stderr)
			var failure *harness.Failure
			if tc.kind == "" {
				assert.Assert(t, !errors.As(err, &failure))
				return
			}
			assert.Assert(t, errors.As(err, &failure))
			assert.Equal(t, failure.Kind, tc.kind)
			if tc.name == "rate limit with reset" {
				assert.Assert(t, failure.ResetAt != nil)
			}
			if tc.name == "missing" {
				assert.ErrorIs(t, err, ErrMissing)
			}
			if tc.name == "auth" {
				assert.ErrorIs(t, err, ErrCredentialRejected)
			}
		})
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	assert.ErrorIs(t, (Harness{}).RunError(ctx, time.Minute, 1, ctx.Err(), "", ""), context.Canceled)
	deadline, done := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer done()
	var failure *harness.Failure
	assert.Assert(t, errors.As((Harness{}).RunError(deadline, time.Minute, 1, nil, "", ""), &failure))
	assert.Equal(t, failure.Kind, harness.Timeout)
}
