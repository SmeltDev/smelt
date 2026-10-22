package cmd

import (
	"encoding/json"
	"strings"
	"testing"

	"gotest.tools/v3/assert"

	"github.com/smeltdev/smelt/internal/config"
	"github.com/smeltdev/smelt/internal/factory"
)

func TestFactoryNoteQueuesAndStatusShowsDelivery(t *testing.T) {
	project := factoryProject(t)
	startSessionDaemon(t, fakeSessionConfig())
	writeRecord(t, project, factory.Record{RunID: "run-9", Prompt: "p", Branch: "smelt/p/run-9"})

	out, _, err := runCobra(t, newFactoryNoteCmd(), "run-9", "avoid a.go", "--json")
	assert.NilError(t, err)
	assert.Equal(t, out, "{\n  \"run_id\": \"run-9\",\n  \"note_id\": \"n1\",\n  \"queued\": true\n}\n")

	notesOf := func() []map[string]any {
		out, _, err := runCobra(t, newFactoryStatusCmd(), "run-9", "--json")
		assert.NilError(t, err)
		var got struct {
			Notes []map[string]any `json:"operator_notes"`
		}
		assert.NilError(t, json.Unmarshal([]byte(out), &got), out)
		return got.Notes
	}
	notes := notesOf()
	assert.Equal(t, len(notes), 1)
	assert.Equal(t, notes[0]["text"], "avoid a.go")
	assert.Assert(t, notes[0]["delivered_round"] == nil)
	assert.Assert(t, notes[0]["received_at"] != nil)

	dir, err := config.ProjectDataDir(project)
	assert.NilError(t, err)
	_, err = factory.NewNoteLog(dir, "run-9").Take(1)
	assert.NilError(t, err)
	assert.Equal(t, notesOf()[0]["delivered_round"], float64(1))
}

func TestFactoryNoteRefusals(t *testing.T) {
	project := factoryProject(t)
	startSessionDaemon(t, fakeSessionConfig())
	writeRecord(t, project, factory.Record{RunID: "run-done", Result: factory.ResultPassed})
	writeRecord(t, project, factory.Record{RunID: "run-live"})

	_, _, err := runCobra(t, newFactoryNoteCmd(), "run-done", "hi")
	assert.ErrorContains(t, err, "Run run-done has ended; notes reach live runs only.")

	_, _, err = runCobra(t, newFactoryNoteCmd(), "run-nope", "hi")
	assert.Assert(t, err != nil)

	_, _, err = runCobra(t, newFactoryNoteCmd(), "run-live", strings.Repeat("x", factory.MaxOperatorNoteBytes+1))
	assert.ErrorContains(t, err, "the most a note can be")
	_, _, err = runCobra(t, newFactoryNoteCmd(), "run-live", "  ")
	assert.ErrorContains(t, err, "empty")
	_, _, err = runCobra(t, newFactoryNoteCmd(), "run-live", strings.Repeat("x", factory.MaxOperatorNoteBytes))
	assert.NilError(t, err)
}
