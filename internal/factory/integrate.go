package factory

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/smeltdev/smelt/internal/gitexec"
	"github.com/smeltdev/smelt/internal/gitutil"
	"github.com/smeltdev/smelt/internal/review"
)

// ModeIntegrate is the mode of a run that checks a finished branch against a
// whole spec instead of building anything.
const ModeIntegrate = "integrate"

// IntegrateAttempts is the rounds an integrate run checks: the broad check and
// one focused re-check after the single fix turn.
const IntegrateAttempts = 2

// integrateFixLead starts the implementer's one fix turn, ahead of the
// findings.
const integrateFixLead = "This branch combines several finished tickets. Fix exactly these findings; do not refactor beyond them."

// IntegrateOptions makes a run an integrate run: the work is already on the
// --from branch and is checked against the spec as a whole.
type IntegrateOptions struct {
	// Against is the ref the branch is compared with; the change under review
	// is everything since its merge base with the branch.
	Against string
}

// ErrNothingToIntegrate is returned when the branch adds nothing to what it is
// compared with.
var ErrNothingToIntegrate = errors.New("nothing to integrate")

// Integration is what ResolveIntegrate settles.
type Integration struct {
	// Tip is the commit --from resolved to and Base the merge base of it and
	// --against: the change under review is Base..Tip.
	Tip, Base string
	// AgainstBranch is the branch --against names, "" for a commit or a tag.
	AgainstBranch string
	// Warning is a fetch problem that did not stop the resolution.
	Warning string
}

// ResolveIntegrate resolves from and against as ResolveFrom does and finds
// their merge base. A branch with no merge base with against, or one that
// adds nothing to it, cannot be integrated.
func ResolveIntegrate(ctx context.Context, root, from, against string) (Integration, error) {
	tip, err := ResolveFrom(ctx, root, from)
	if err != nil {
		return Integration{}, err
	}
	ag, err := ResolveFrom(ctx, root, against)
	if err != nil {
		return Integration{}, fmt.Errorf("--against: %w", err)
	}
	out, err := (gitexec.Runner{Dir: root}).Output(ctx, "merge-base", tip.Commit, ag.Commit)
	base := strings.TrimSpace(string(out))
	if err != nil || base == "" {
		return Integration{}, fmt.Errorf("%s and %s have no common ancestor", from, against)
	}
	if base == tip.Commit {
		return Integration{}, fmt.Errorf("%w: %s adds nothing to %s", ErrNothingToIntegrate, from, against)
	}
	return Integration{Tip: tip.Commit, Base: base, AgainstBranch: ag.Branch, Warning: strings.Join(nonEmpty(tip.Warning, ag.Warning), "; ")}, nil
}

func nonEmpty(ss ...string) []string {
	var out []string
	for _, s := range ss {
		if s != "" {
			out = append(out, s)
		}
	}
	return out
}

// broadPreamble is what a review is told ahead of its own instructions in the
// broad round: the spec is the request and the combined change is judged as one.
func broadPreamble(spec string) string {
	return fmt.Sprintf(`This branch combines several finished tickets that were each reviewed against their own ticket. Nothing has reviewed it as a whole, and that is your job.

Does the combined change meet every requirement and acceptance item of the spec below? Do the parts fit together: names, types, data flow, duplicated helpers, a seam one part assumes and another does not provide? The requested change is the whole spec; judge the combined change, not one part. Do not implement changes yourself.

## Requested change

%s

`, strings.TrimSpace(spec)) + changeUnderReview
}

// recheckPreamble is what a review is told in the round after the fix: only
// its own earlier findings are in question.
func recheckPreamble(spec string, prior priorRound) string {
	var b strings.Builder
	b.WriteString("You reviewed this branch as a whole before, and the implementer has made one round of fixes. This is a focused re-check, not a second broad review. " +
		"Check only whether the findings below are fixed, and whether the fix broke something nearby. Check the reply against the code rather than taking it on trust. " +
		"Raise nothing else. Do not implement changes yourself.\n\n")
	b.WriteString("## Your earlier findings\n\n")
	for _, f := range prior.Findings {
		fmt.Fprintf(&b, "- [%s] %s: %s\n", f.Severity, f.Location(), f.Body)
	}
	if prior.Reply != "" {
		fmt.Fprintf(&b, "\nImplementer's reply:\n\n%s\n", prior.Reply)
	}
	fmt.Fprintf(&b, "\n## Requested change\n\n%s\n\n", strings.TrimSpace(spec))
	b.WriteString(changeUnderReview)
	return b.String()
}

// integratePrompts is the reviews to run in round, as each reviewer is sent
// them, and the checks of the reviews that are not run again. The first round
// runs every review broadly. Later rounds re-check only the reviews that had
// findings worth changing; one that passed is carried forward as passed, and
// one that never produced findings (it errored or was skipped) has had no broad
// pass yet, so it gets one.
func integratePrompts(spec string, prompts []review.Prompt, history map[string][]priorRound, round int) (scoped []review.Prompt, carried []Check) {
	for _, p := range prompts {
		body := broadPreamble(spec)
		if round > 1 {
			rounds := history[p.Name]
			if len(rounds) > 0 {
				last := rounds[len(rounds)-1]
				if len(last.Findings) == 0 {
					carried = append(carried, Check{Name: p.Name, Kind: KindReview, Status: StatusPassed})
					continue
				}
				body = recheckPreamble(spec, last)
			}
		}
		scoped = append(scoped, review.Prompt{Name: p.Name, Body: body + "\n\n" + severityScope + "\n\n" + fmt.Sprintf(reviewTail, p.Body)})
	}
	return scoped, carried
}

// ErrNoDefaultBranch is returned when --against is not given and the
// repository records no default branch.
var ErrNoDefaultBranch = errors.New("the repository records no default branch")

// ResolveAgainst is the ref an integrate run compares its branch with: against
// as given, else the repository's default branch.
func ResolveAgainst(root, against string) (string, error) {
	if against != "" {
		return against, nil
	}
	branch, err := gitutil.DefaultBranchIn(root)
	if err != nil || branch == "" {
		return "", fmt.Errorf("%w; name what to compare with: --against <ref>", ErrNoDefaultBranch)
	}
	return branch, nil
}
