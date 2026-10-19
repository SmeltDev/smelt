package factory

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"

	"gotest.tools/v3/assert"

	"github.com/smeltdev/smelt/internal/iostream"
	"github.com/smeltdev/smelt/internal/review"
	"github.com/smeltdev/smelt/internal/sidecar"
)

// streamingSidecars answers every turn like claude with --output-format
// stream-json on a real repo: about a megabyte of events, written in small
// chunks as they arrive, and then the result event on one line.
func streamingSidecars(t *testing.T, result string) *Sidecars {
	t.Helper()
	var line bytes.Buffer
	assert.NilError(t, json.Compact(&line, []byte(result)))
	event := `{"type":"user","message":{"content":[{"type":"tool_result","content":"` + strings.Repeat("x", 4000) + `"}]}}` + "\n"
	exec := func(_ context.Context, _ *sidecar.PoolEntry, _ string, _ map[string]string, out iostream.OutputFn, _ func(string)) (int, error) {
		for range 260 {
			out(iostream.StreamStdout, []byte(event))
		}
		out(iostream.StreamStdout, []byte(line.String()+"\n"))
		return 0, nil
	}
	entry := &sidecar.PoolEntry{ID: "impl", RepoPath: "/repo"}
	return &Sidecars{Exec: exec, Implementer: &Implementer{Entry: entry, Credential: review.Credential{EnvVar: "ANTHROPIC_API_KEY", Value: "k"}}}
}

// Regression: on 2026-10-19 every plan on a real repo failed with "read
// claude's result: invalid character '{' after top-level value", because only
// the first 128 KiB of the stream were kept and the result comes last.
func TestPlanParsesTheResultAfterAMegabyteOfStream(t *testing.T) {
	plan, err := streamingSidecars(t, planJSON).Plan(context.Background(), PlanInput{Request: "r"})
	assert.NilError(t, err)
	assert.Equal(t, plan.Summary, "Add the flag in cmd.")
}

func TestNotesParseTheResultAfterAMegabyteOfStream(t *testing.T) {
	notes := `{"type":"result","is_error":false,"structured_output":{"title":"Add a flag","what":"- a flag","why":"- users asked","how":"- cmd/flag.go"}}`
	n, err := streamingSidecars(t, notes).WriteNotes(context.Background(), NotesInput{Request: "r"})
	assert.NilError(t, err)
	assert.Equal(t, n.Title, "Add a flag")
}
