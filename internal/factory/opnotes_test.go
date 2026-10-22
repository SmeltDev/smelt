package factory

import (
	"strings"
	"sync"
	"testing"
	"time"

	"gotest.tools/v3/assert"

	"github.com/smeltdev/smelt/internal/review"
)

func TestNoteLogTakesOldestFirstAndOnce(t *testing.T) {
	l := NewNoteLog(t.TempDir(), "run-1")
	now := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	a, err := l.Add("first", now)
	assert.NilError(t, err)
	b, err := l.Add("second", now.Add(time.Second))
	assert.NilError(t, err)
	assert.Equal(t, a.ID, "n1")
	assert.Equal(t, b.ID, "n2")

	got, err := l.Take(1)
	assert.NilError(t, err)
	assert.Equal(t, len(got), 2)
	assert.Equal(t, got[0].Text, "first")
	assert.Equal(t, notesSection(got), "## Notes from the operator\n\nfirst\n\nsecond")

	again, err := l.Take(2)
	assert.NilError(t, err)
	assert.Equal(t, len(again), 0)

	all, err := l.List()
	assert.NilError(t, err)
	assert.Equal(t, *all[1].DeliveredRound, 1)
}

func TestNoteLogRefusesEmptyAndOversized(t *testing.T) {
	l := NewNoteLog(t.TempDir(), "run-1")
	_, err := l.Add("  \n", time.Now())
	assert.ErrorIs(t, err, ErrNoteEmpty)
	_, err = l.Add(strings.Repeat("x", MaxOperatorNoteBytes+1), time.Now())
	assert.ErrorIs(t, err, ErrNoteTooLarge)
	_, err = l.Add(strings.Repeat("x", MaxOperatorNoteBytes), time.Now())
	assert.NilError(t, err)
}

func TestNoteLogConcurrentAddsGetDistinctIDs(t *testing.T) {
	l := NewNoteLog(t.TempDir(), "run-1")
	var wg sync.WaitGroup
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := l.Add("n", time.Now())
			assert.Check(t, err)
		}()
	}
	wg.Wait()
	all, err := l.List()
	assert.NilError(t, err)
	assert.Equal(t, len(all), 8)
}

func TestRoundNotesReachOneRoundOfPrompts(t *testing.T) {
	l := NewNoteLog(t.TempDir(), "run-1")
	_, err := l.Add("stay out of a.go", time.Now())
	assert.NilError(t, err)
	_, err = l.Add("and b.go", time.Now())
	assert.NilError(t, err)
	s := &Sidecars{OperatorNotes: l, Request: "do it", Prompts: []review.Prompt{{Name: "r", Body: "check"}}}

	first := s.reviewSet(1).Prompts[0].Body
	assert.Assert(t, strings.HasSuffix(first, "## Notes from the operator\n\nstay out of a.go\n\nand b.go"), first)
	assert.Equal(t, s.reviewSet(1).Prompts[0].Body, first)
	assert.Assert(t, !strings.Contains(s.reviewSet(2).Prompts[0].Body, "Notes from the operator"))
	assert.Equal(t, s.roundNotes(2), "")
	assert.Equal(t, withNotes("", "p"), "p")
}

func TestImplementPromptCarriesNotesForOneRound(t *testing.T) {
	s, prompts, _ := scriptedImplementer(t, claudeTurn("one"), claudeTurn("two"))
	l := NewNoteLog(t.TempDir(), "run-1")
	_, err := l.Add("stay out of a.go", time.Now())
	assert.NilError(t, err)
	_, err = l.Add("and b.go", time.Now())
	assert.NilError(t, err)
	s.OperatorNotes = l

	s.round = 1
	_, err = s.Implement(t.Context(), "do it")
	assert.NilError(t, err)
	s.round = 2
	_, err = s.Implement(t.Context(), "fix it")
	assert.NilError(t, err)

	assert.Equal(t, len(*prompts), 2)
	first := (*prompts)[0]
	assert.Assert(t, strings.Contains(first, "## Notes from the operator\n\nstay out of a.go\n\nand b.go"), first)
	assert.Assert(t, strings.Contains(first, "do it"), first)
	assert.Assert(t, !strings.Contains((*prompts)[1], "Notes from the operator"), (*prompts)[1])
	assert.Assert(t, strings.Contains((*prompts)[1], "fix it"))
}
