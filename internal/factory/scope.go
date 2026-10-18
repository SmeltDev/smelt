package factory

import (
	"fmt"
	"strings"

	"github.com/smeltdev/smelt/internal/review"
)

// severityScope is the rubric added to every review. It says what may block a
// change, so reviewers do not send the implementer back for style remarks or for
// tests that could be more thorough.
const severityScope = `## What blocks a change

Only high and medium findings are sent back to the implementer, and each one costs another round of work. Block a change only for a problem you can demonstrate: give the concrete input or state and the wrong result. The problem must be one of these:

- it breaks something the requested change asks for, including accessibility behavior it asks for: a live region that is hidden or never announces counts;
- it breaks existing behavior, or the codebase's ability to build and run;
- it noticeably degrades run time, or loses data, or is a security problem.

A change that meets the request and has no demonstrated problem should come back with no high or medium findings. That is the expected result for sound work, not a failure of the review. Do not look for something to report.

- **high**: a demonstrated problem of the kinds above that breaks the main path, loses data or is a security problem.
- **medium**: a demonstrated problem of the kinds above with narrower impact, or behavior the requested change asks for that no test exercises at all.
- **low**: everything else, including improvements to design, naming and style, a test that could be more thorough, an assertion for behavior that is already tested, and anything you cannot back with a concrete scenario. Suggestions for improving the code are welcome as low; they never block.

Report a given problem once.`

// priorRound is what one reviewer found in one round, and how the implementer
// answered. Each round's reviewer starts fresh, so without this a finding the
// implementer already answered comes back, or the advice changes between rounds.
type priorRound struct {
	Round    int
	Findings []review.Finding
	// Reply is the implementer's summary of the turn that followed. It answers
	// every reviewer's findings together, since they are fed back as one prompt.
	Reply string
	// Tree is the tree the reviewer saw, "" when it could not be recorded. A
	// later round diffs against it to tell which lines the fix touched.
	Tree string
}

// scopeInput is what differs between kinds of run in round scoping.
type scopeInput struct {
	// Broad is the preamble of a full review: the request, or an integrate
	// run's spec, and where the change is.
	Broad string
	// Spec is the requested change a re-check is shown.
	Spec string
}

// roundScope is a round's reviews: the prompts to run, the checks of the
// reviews that are not run again, and how each prompt was scoped.
type roundScope struct {
	Prompts []review.Prompt
	Carried []Check
	Modes   map[string]Mode
}

// scopePrompts is the reviews to run in round, as each reviewer is sent them.
// The first round runs every review broadly. Later rounds re-check only the
// reviews that had findings worth changing; one that passed is carried forward
// as passed, and one that never produced findings (it errored or was skipped)
// has had no broad pass yet, so it gets one.
func scopePrompts(in scopeInput, prompts []review.Prompt, history map[string][]priorRound, round int) roundScope {
	rs := roundScope{Modes: map[string]Mode{}}
	for _, p := range prompts {
		body, mode := in.Broad, ModeBroad
		if rounds := history[p.Name]; round > 1 && len(rounds) > 0 {
			last := rounds[len(rounds)-1]
			if len(last.Findings) == 0 {
				rs.Carried = append(rs.Carried, Check{Name: p.Name, Kind: KindReview, Status: StatusPassed, Mode: ModeCarried})
				continue
			}
			body, mode = recheckPreamble(in.Spec, last), ModeRecheck
		}
		rs.Modes[p.Name] = mode
		rs.Prompts = append(rs.Prompts, review.Prompt{Name: p.Name, Body: body + "\n\n" + severityScope + "\n\n" + fmt.Sprintf(reviewTail, p.Body)})
	}
	return rs
}

// legacyHistory renders a reviewer's earlier rounds for its next review, or ""
// for a first review. It is the prompt of a run with factory.focusedRechecks
// off: every round is a full review that is asked to hold back.
func legacyHistory(rounds []priorRound) string {
	if len(rounds) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("## Earlier rounds\n\nYou reviewed this change before, and the implementer has revised it since. What you reported, and how the implementer replied, is below. " +
		"Check the replies against the code rather than taking them on trust. Do not raise a finding again if the reply or the new code answers it, and do not contradict your earlier guidance unless it was wrong, in which case say so. " +
		"Look first at whether each earlier finding is fixed, then at the code that changed since. Your job is to help the change meet the original request, not to find new things to report: " +
		"raise a problem in code that did not change only if it is high severity and you missed it before.\n")
	for _, r := range rounds {
		fmt.Fprintf(&b, "\n### Round %d\n\n", r.Round)
		if len(r.Findings) == 0 {
			b.WriteString("You reported nothing that needed fixing.\n")
		}
		for _, f := range r.Findings {
			fmt.Fprintf(&b, "- [%s] %s: %s\n", f.Severity, f.Location(), f.Body)
		}
		if r.Reply != "" {
			fmt.Fprintf(&b, "\nImplementer's reply:\n\n%s\n", r.Reply)
		}
	}
	return b.String()
}
