package factory

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"gotest.tools/v3/assert"

	"github.com/smeltdev/smelt/internal/config"
	"github.com/smeltdev/smelt/internal/iostream"
	"github.com/smeltdev/smelt/internal/sidecar"
)

var testGates = []config.Command{
	{Name: "lint", Run: "golangci-lint run", Role: config.RoleGate, Timeout: 30},
	{Name: "test", Run: "go test ./...", Role: config.RoleGate, Timeout: 30},
}

func TestUnprovenGates(t *testing.T) {
	edit := Activity{Tool: "Edit", Detail: "main.go"}
	tests := []struct {
		name string
		acts []Activity
		want []string
	}{
		{"nothing ran", []Activity{edit}, []string{"lint", "test"}},
		{"gates by their command after the edit", []Activity{edit, {Tool: "Bash", Detail: "golangci-lint run ./..."}, {Tool: "Bash", Detail: "cd x && go test ./... -race"}}, nil},
		{"gates by the task target that wraps them", []Activity{edit, {Tool: "Bash", Detail: "task test"}, {Tool: "Bash", Detail: "task lint"}}, nil},
		{"a task target with a path", []Activity{edit, {Tool: "Bash", Detail: "task test:unit && task lint"}}, nil},
		{"a run before the last edit proves nothing", []Activity{{Tool: "Bash", Detail: "task test"}, edit, {Tool: "Bash", Detail: "task lint"}}, []string{"test"}},
		{"a later edit voids earlier runs", []Activity{edit, {Tool: "Bash", Detail: "task test"}, {Tool: "Bash", Detail: "task lint"}, {Tool: "Write", Detail: "b.go"}}, []string{"lint", "test"}},
		{"no edits at all", []Activity{{Tool: "Bash", Detail: "task test"}, {Tool: "Bash", Detail: "task lint"}}, nil},
		{"a similarly named target is not the gate", []Activity{edit, {Tool: "Bash", Detail: "task testdata"}, {Tool: "Bash", Detail: "echo task lint"}}, []string{"test"}},
		{"reading the command is not running it", []Activity{edit, {Tool: "Read", Detail: "go test ./..."}}, []string{"lint", "test"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.DeepEqual(t, unproven(tt.acts, testGates), tt.want)
		})
	}
}

func TestUnprovenConsidersOnlyFastGatesAndQuickChecks(t *testing.T) {
	edit := Activity{Tool: "Edit", Detail: "main.go"}
	slow := config.Command{Name: "test", Run: "task ci:test", Role: config.RoleGate, Timeout: 600}
	quick := config.Command{Name: "test", Run: "task ci:test", Role: config.RoleGate, Timeout: 600, Quick: "go test ./pkg/..."}
	tests := []struct {
		name string
		gate config.Command
		acts []Activity
		want []string
	}{
		{"a slow gate without a quick check is not flagged", slow, []Activity{edit, {Tool: "Bash", Detail: "go test ./..."}}, nil},
		{"a quick check not run is flagged", quick, []Activity{edit, {Tool: "Bash", Detail: "go build ./..."}}, []string{"test"}},
		{"a command containing the quick text proves it", quick, []Activity{edit, {Tool: "Bash", Detail: "go test ./pkg/... -count=1"}}, nil},
		{"the full gate proves it too", quick, []Activity{edit, {Tool: "Bash", Detail: "task ci:test"}}, nil},
		{"the quick text before the edit proves nothing", quick, []Activity{{Tool: "Bash", Detail: "go test ./pkg/..."}, edit}, []string{"test"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.DeepEqual(t, unproven(tt.acts, []config.Command{tt.gate}), tt.want)
		})
	}
}

func TestClaimsGatesPass(t *testing.T) {
	for summary, want := range map[string]bool{
		"All tests pass.":                           true,
		"go test passes and lint is clean":          true,
		"Gates: green":                              true,
		"Added the flag.\n\n## Gates\n```\nok\n```": true,
		"Added a pass-through wrapper.":             false,
		"Added the flag. Tests not run: no docker.": false,
		"": false,
	} {
		assert.Equal(t, claimsGatesPass(summary), want, summary)
	}
}

func TestGateCommandsAreOnlyTheGates(t *testing.T) {
	install := config.Command{Name: config.CmdInstall, Run: "task mod-download"}
	format := config.Command{Name: "format", Run: "task fmt", Role: config.RoleAutofix}
	got := gateCommands(append(append([]config.Command{install, format}, testGates...), MarkersGate))
	assert.DeepEqual(t, got, testGates)
}

func TestSystemPromptListsTheGates(t *testing.T) {
	p := implementerSystemPrompt("be brief", testGates)
	for _, want := range []string{"smelt runs these after your turn", "- lint: golangci-lint run", "- test: go test ./...", "Run-specific instructions:\nbe brief"} {
		assert.Assert(t, contains(p, want), "missing %q in:\n%s", want, p)
	}
	assert.Assert(t, !contains(implementerSystemPrompt("", nil), "smelt runs"))
}

// claudeTurn is the stream of one claude turn that used tools in order, each
// "Tool:detail", and ended with result.
func claudeTurn(result string, tools ...string) string {
	var b strings.Builder
	b.WriteString(`{"type":"system","subtype":"init","session_id":"s"}` + "\n")
	for _, tool := range tools {
		name, detail, _ := strings.Cut(tool, ":")
		key := "command"
		if name != "Bash" {
			key = "file_path"
		}
		in, _ := json.Marshal(map[string]string{key: detail})
		fmt.Fprintf(&b, `{"type":"assistant","message":{"content":[{"type":"tool_use","name":%q,"input":%s}]}}`+"\n", name, in)
	}
	res, _ := json.Marshal(result)
	fmt.Fprintf(&b, `{"type":"result","subtype":"success","is_error":false,"result":%s,"total_cost_usd":0.5}`+"\n", res)
	return b.String()
}

// scriptedImplementer is a Sidecars whose implementer plays turns in order.
func scriptedImplementer(t *testing.T, turns ...string) (*Sidecars, *[]string, *[]string) {
	t.Helper()
	var prompts, notices []string
	exec := func(_ context.Context, _ *sidecar.PoolEntry, script string, _ map[string]string, out iostream.OutputFn, _ func(string)) (int, error) {
		prompts = append(prompts, promptOf(t, script))
		out(iostream.StreamStdout, []byte(turns[len(prompts)-1]))
		return 0, nil
	}
	return &Sidecars{
		Exec:        exec,
		Commands:    append(append([]config.Command{}, testGates...), MarkersGate),
		Implementer: &Implementer{Exec: exec, Entry: &sidecar.PoolEntry{ID: "impl", RepoPath: "/repo"}},
		Status:      func(_ iostream.Level, msg string) { notices = append(notices, msg) },
	}, &prompts, &notices
}

func TestImplementNotesAClaimWithoutARunAndAsksForNothing(t *testing.T) {
	s, prompts, notices := scriptedImplementer(t,
		claudeTurn("Done. All tests pass.", "Edit:main.go"),
		claudeTurn("never asked"))
	turn, err := s.Implement(context.Background(), "do it")
	assert.NilError(t, err)

	assert.Equal(t, len(*prompts), 1, "no extra implementer turn")
	assert.DeepEqual(t, turn.Unproven, []string{"lint", "test"})
	assert.Equal(t, turn.CostUSD, 0.5)
	assert.Equal(t, len(*notices), 1)
	assert.Assert(t, contains((*notices)[0], "were not run after the last edit"), (*notices)[0])
}

func TestImplementDoesNotNoteWhenTheGatesRan(t *testing.T) {
	s, prompts, notices := scriptedImplementer(t,
		claudeTurn("Done. All tests pass.", "Edit:main.go", "Bash:task test", "Bash:golangci-lint run ./..."))
	turn, err := s.Implement(context.Background(), "do it")
	assert.NilError(t, err)
	assert.Equal(t, len(*prompts), 1)
	assert.Equal(t, len(turn.Unproven), 0)
	assert.Equal(t, len(*notices), 0)
}

func TestImplementDoesNotNoteWithoutAClaim(t *testing.T) {
	s, prompts, notices := scriptedImplementer(t, claudeTurn("Added the flag. I did not run anything.", "Edit:main.go"))
	turn, err := s.Implement(context.Background(), "do it")
	assert.NilError(t, err)
	assert.Equal(t, len(*prompts), 1)
	assert.Equal(t, len(turn.Unproven), 0)
	assert.Equal(t, len(*notices), 0)
}

func TestImplementNeverCountsInstallAsAGate(t *testing.T) {
	s, _, notices := scriptedImplementer(t, claudeTurn("All tests pass.", "Edit:main.go", "Bash:task test", "Bash:task lint"))
	s.Commands = append([]config.Command{{Name: config.CmdInstall, Run: "task mod-download"}}, s.Commands...)
	turn, err := s.Implement(context.Background(), "do it")
	assert.NilError(t, err)
	assert.Equal(t, len(turn.Unproven), 0)
	assert.Equal(t, len(*notices), 0)
}

func TestImplementWithoutGatesIsNotChecked(t *testing.T) {
	s, prompts, _ := scriptedImplementer(t, claudeTurn("All tests pass.", "Edit:main.go"))
	s.Commands = []config.Command{MarkersGate}
	_, err := s.Implement(context.Background(), "do it")
	assert.NilError(t, err)
	assert.Equal(t, len(*prompts), 1)
}
