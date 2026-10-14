package claude

import (
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
