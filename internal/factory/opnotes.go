package factory

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

// MaxOperatorNoteBytes is the longest note an operator can leave a run.
const MaxOperatorNoteBytes = 4096

// Errors Add returns for text it will not store.
var (
	ErrNoteEmpty    = errors.New("the note is empty")
	ErrNoteTooLarge = fmt.Errorf("the note is longer than %d bytes", MaxOperatorNoteBytes)
)

// notesHeading heads the section of operator notes in a round's prompts.
const notesHeading = "## Notes from the operator"

// OperatorNote is something an operator told a live run. It reaches the run's
// prompts in one round only.
type OperatorNote struct {
	ID         string    `json:"id"`
	Text       string    `json:"text"`
	ReceivedAt time.Time `json:"received_at"`
	// DeliveredRound is the round whose prompts carried the note, nil until one
	// has.
	DeliveredRound *int `json:"delivered_round"`
}

// NoteLog is a run's operator notes, one file each in a directory beside the
// run's record. Add only creates files and Take, called by the run alone, is
// the only writer of existing ones, so the CLI and the run need no lock.
type NoteLog struct {
	dir string
}

// NewNoteLog is the notes of run runID in the project data directory dataDir.
func NewNoteLog(dataDir, runID string) *NoteLog {
	return &NoteLog{dir: filepath.Join(dataDir, "factory", runID+".notes")}
}

// Add stores text as a new note received at now. Concurrent calls get
// different ids.
func (l *NoteLog) Add(text string, now time.Time) (OperatorNote, error) {
	if strings.TrimSpace(text) == "" {
		return OperatorNote{}, ErrNoteEmpty
	}
	if len(text) > MaxOperatorNoteBytes {
		return OperatorNote{}, ErrNoteTooLarge
	}
	if err := os.MkdirAll(l.dir, 0o700); err != nil {
		return OperatorNote{}, fmt.Errorf("store the note: %w", err)
	}
	existing, err := l.List()
	if err != nil {
		return OperatorNote{}, err
	}
	seq := 1
	if n := len(existing); n > 0 {
		seq = noteSeq(existing[n-1].ID) + 1
	}
	for {
		note := OperatorNote{ID: "n" + strconv.Itoa(seq), Text: strings.TrimSpace(text), ReceivedAt: now.UTC()}
		err := l.create(note, seq)
		if err == nil {
			return note, nil
		}
		if !errors.Is(err, os.ErrExist) {
			return OperatorNote{}, fmt.Errorf("store the note: %w", err)
		}
		seq++
	}
}

// create links note into place under its sequence number, which fails when
// another note has it.
func (l *NoteLog) create(note OperatorNote, seq int) error {
	tmp, err := l.writeTemp(note)
	if err != nil {
		return err
	}
	defer func() { _ = os.Remove(tmp) }()
	return os.Link(tmp, l.path(seq))
}

// writeTemp writes note to a new temporary file, which List ignores.
func (l *NoteLog) writeTemp(note OperatorNote) (string, error) {
	b, err := json.Marshal(note)
	if err != nil {
		return "", err
	}
	f, err := os.CreateTemp(l.dir, "note-*.tmp")
	if err != nil {
		return "", err
	}
	_, werr := f.Write(b)
	if cerr := f.Close(); werr == nil {
		werr = cerr
	}
	if werr != nil {
		_ = os.Remove(f.Name())
		return "", werr
	}
	return f.Name(), nil
}

func (l *NoteLog) path(seq int) string {
	return filepath.Join(l.dir, fmt.Sprintf("n%04d.json", seq))
}

// noteSeq is the sequence number in a note id such as "n3".
func noteSeq(id string) int {
	n, _ := strconv.Atoi(strings.TrimPrefix(id, "n"))
	return n
}

// List is every note of the run, oldest first. A run with none has none.
func (l *NoteLog) List() ([]OperatorNote, error) {
	entries, err := os.ReadDir(l.dir)
	if errors.Is(err, os.ErrNotExist) {
		return []OperatorNote{}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read the run's notes: %w", err)
	}
	notes := []OperatorNote{}
	for _, e := range entries {
		name := e.Name()
		if !strings.HasPrefix(name, "n") || !strings.HasSuffix(name, ".json") {
			continue
		}
		b, err := os.ReadFile(filepath.Join(l.dir, name))
		if err != nil {
			return nil, fmt.Errorf("read the run's notes: %w", err)
		}
		var n OperatorNote
		if err := json.Unmarshal(b, &n); err != nil {
			return nil, fmt.Errorf("read note %s: %w", name, err)
		}
		notes = append(notes, n)
	}
	sort.Slice(notes, func(i, j int) bool { return noteSeq(notes[i].ID) < noteSeq(notes[j].ID) })
	return notes, nil
}

// Take returns the notes no round has read yet, oldest first, and marks them
// delivered in round. Only the run calls it.
func (l *NoteLog) Take(round int) ([]OperatorNote, error) {
	all, err := l.List()
	if err != nil {
		return nil, err
	}
	var taken []OperatorNote
	for _, n := range all {
		if n.DeliveredRound != nil {
			continue
		}
		n.DeliveredRound = &round
		tmp, err := l.writeTemp(n)
		if err != nil {
			return taken, fmt.Errorf("mark note %s delivered: %w", n.ID, err)
		}
		if err := os.Rename(tmp, l.path(noteSeq(n.ID))); err != nil {
			_ = os.Remove(tmp)
			return taken, fmt.Errorf("mark note %s delivered: %w", n.ID, err)
		}
		taken = append(taken, n)
	}
	return taken, nil
}

// notesSection is the prompt section for notes, empty when there are none.
func notesSection(notes []OperatorNote) string {
	if len(notes) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString(notesHeading)
	for _, n := range notes {
		b.WriteString("\n\n" + n.Text)
	}
	return b.String()
}

// withNotes is prompt led by the notes section, or prompt itself when there
// is none.
func withNotes(section, prompt string) string {
	if section == "" {
		return prompt
	}
	return section + "\n\n" + prompt
}
