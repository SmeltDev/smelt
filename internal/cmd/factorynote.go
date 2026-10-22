package cmd

import (
	"context"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/smeltdev/smelt/internal/config"
	"github.com/smeltdev/smelt/internal/factory"
	"github.com/smeltdev/smelt/internal/iostream"
	"github.com/smeltdev/smelt/internal/smeltd"
)

// noteResult is what `smelt factory note --json` prints.
type noteResult struct {
	RunID  string `json:"run_id"`
	NoteID string `json:"note_id"`
	Queued bool   `json:"queued"`
}

func newFactoryNoteCmd() *cobra.Command {
	var jsonOut bool
	cmd := &cobra.Command{
		Use:   "note <run-id> [text|-] [--json]",
		Short: "Tell a running factory run something its next round reads",
		Long: `Leave a note for a live factory run. At the start of its next round the
implementer's and the reviewers' prompts get the notes the run has not read yet,
under "## Notes from the operator", oldest first. A note reaches one round only.

The text is the second argument, or stdin when it is "-" or omitted. It is at
most 4096 bytes. A run that has ended refuses a note. A note left while the last
round is being checked is never read; 'smelt factory status --json' lists the
notes, and delivered_round is null until a round has read one. A run that
crashed leaves a record that looks live, so a note to it is accepted and never
read.`,
		Args:         cobra.RangeArgs(1, 2),
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			text := "-"
			if len(args) == 2 {
				text = args[1]
			}
			return runFactoryNote(cmd.Context(), iostream.FromCmd(cmd), cmd.InOrStdin(), args[0], text, jsonOut)
		},
	}
	cmd.Flags().BoolVar(&jsonOut, "json", false, "print the result as JSON")
	return cmd
}

func runFactoryNote(ctx context.Context, streams iostream.Streams, in io.Reader, arg, text string, jsonOut bool) error {
	if text == "-" {
		b, err := io.ReadAll(in)
		if err != nil {
			return &userError{msg: "Could not read the note from stdin.", err: err}
		}
		text = string(b)
	}
	if err := checkNoteText(text); err != nil {
		return err
	}
	root, err := sessionProjectRoot(ctx)
	if err != nil {
		return err
	}
	runID := factory.ParseRunID(arg)
	s := runSession(root, runID)
	var rec *factory.Record
	if s == nil {
		r, err := loadRunRecord(root, runID)
		if err != nil {
			return err
		}
		rec = &r
	}
	if err := noteLiveness(s, rec, runID); err != nil {
		return err
	}
	dataDir, err := config.ProjectDataDir(root)
	if err != nil {
		return &userError{msg: "Could not find the project's data directory.", err: err}
	}
	note, err := factory.NewNoteLog(dataDir, runID).Add(text, time.Now())
	if err != nil {
		return &userError{msg: "Could not store the note.", err: err}
	}
	if jsonOut {
		return iostream.PrintJSON(streams.Out, noteResult{RunID: runID, NoteID: note.ID, Queued: true})
	}
	streams.Printf("Note %s queued for run %s; its next round reads it.\n", note.ID, runID)
	return nil
}

// checkNoteText refuses text a run will not take, before anything is read.
func checkNoteText(text string) error {
	switch {
	case strings.TrimSpace(text) == "":
		return noteArgError("The note is empty.")
	case len(text) > factory.MaxOperatorNoteBytes:
		return noteArgError(fmt.Sprintf("The note is %d bytes; the most a note can be is %d.", len(text), factory.MaxOperatorNoteBytes))
	}
	return nil
}

func noteArgError(msg string) error {
	return newUserError(msg).withCode("command.invalid_args").withExitCode(ExitBadArgs).withoutDetail()
}

// noteLiveness is nil when the run can still read a note. s is the run's
// daemon session, if the daemon has one; otherwise rec is its record.
func noteLiveness(s *smeltd.Session, rec *factory.Record, runID string) error {
	ended := s != nil && s.State.Finished() || s == nil && rec != nil && rec.Result != ""
	if !ended {
		return nil
	}
	return newUserError(fmt.Sprintf("Run %s has ended; notes reach live runs only.", runID)).withoutDetail()
}

// runNotes is the notes of run runID, never nil.
func runNotes(root, runID string) ([]factory.OperatorNote, error) {
	dataDir, err := config.ProjectDataDir(root)
	if err != nil {
		return nil, &userError{msg: "Could not find the project's data directory.", err: err}
	}
	notes, err := factory.NewNoteLog(dataDir, runID).List()
	if err != nil {
		return nil, &userError{msg: fmt.Sprintf("Could not read the notes of run %s.", runID), err: err}
	}
	return notes, nil
}

// sessionStatus is a daemon session's status with the run's notes.
type sessionStatus struct {
	smeltd.SessionDetail
	Notes []factory.OperatorNote `json:"operator_notes"`
}
