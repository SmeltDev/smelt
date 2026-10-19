package factory

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/smeltdev/smelt/internal/harness"
	"github.com/smeltdev/smelt/internal/review"
)

const (
	// planTimeout bounds one attempt at writing a plan.
	planTimeout = 5 * time.Minute
	// maxPlanOutputBytes caps the plan agent's result.
	maxPlanOutputBytes = 128 * 1024
	// planFile is the path a plan review gives a finding about the plan as a
	// whole; findings need a repository path, and the plan has none.
	planFile = "plan"
)

// readTools let a read-only agent read the repository, and gitLogTool its history.
var readTools = []string{"Read", "Grep", "Glob"}

const gitLogTool = "Bash(git log:*)"

// planTools limits the plan writer to reading the repository.
var planTools = slices.Concat(readTools, []string{gitLogTool, "Bash(git diff:*)"})

// PlanSchema is the JSON Schema of the plan the implementer writes before it
// writes code. It is passed with --json-schema (--output-schema for Codex), so
// the agent's CLI validates the answer itself.
const PlanSchema = `{
  "type": "object",
  "properties": {
    "summary": {"type": "string", "description": "2-4 sentences: the approach."},
    "files": {
      "type": "array",
      "description": "Every file the change touches.",
      "items": {
        "type": "object",
        "properties": {
          "path": {"type": "string"},
          "change": {"type": "string", "description": "add, modify, delete or new package."},
          "why": {"type": "string"}
        },
        "required": ["path", "change", "why"],
        "additionalProperties": false
      }
    },
    "acceptance": {
      "type": "array",
      "description": "One entry per requirement and acceptance item of the request.",
      "items": {
        "type": "object",
        "properties": {
          "item": {"type": "string", "description": "The acceptance line, quoted."},
          "how": {"type": "string", "description": "How the change will meet it."},
          "verifiable_in_sandbox": {"type": "boolean"}
        },
        "required": ["item", "how", "verifiable_in_sandbox"],
        "additionalProperties": false
      }
    },
    "risks": {"type": "array", "items": {"type": "string"}},
    "questions": {"type": "array", "items": {"type": "string"}, "description": "Decisions the request leaves open. Answer each with the most reasonable choice and say so."},
    "cannot_verify_here": {"type": "array", "items": {"type": "string"}, "description": "What cannot be verified in the sandbox, such as needing Docker on the host."},
    "pack": {
      "type": "array",
      "description": "The context pack: the exact code the implementer and reviewers need. Empty when not asked for.",
      "items": {
        "type": "object",
        "properties": {
          "kind": {"type": "string", "enum": ["edit", "reference", "test_pattern", "rule", "new_file"]},
          "path": {"type": "string", "description": "Repository-relative path; for a rule, the document it comes from."},
          "start": {"type": "integer", "description": "First line, 1-based; 0 for a whole file, a rule or a new file."},
          "end": {"type": "integer", "description": "Last line; 0 for a whole file, a rule or a new file."},
          "reason": {"type": "string", "description": "Why it is needed; for a rule, the quote; for a new file, the existing file it follows."},
          "priority": {"type": "integer", "description": "1 (must have) to 3 (nice to have)."}
        },
        "required": ["kind", "path", "start", "end", "reason", "priority"],
        "additionalProperties": false
      }
    },
    "rejected_hints": {
      "type": "array",
      "description": "Hints from the request's Context section that the pack does not follow.",
      "items": {
        "type": "object",
        "properties": {
          "hint": {"type": "string"},
          "reason": {"type": "string"}
        },
        "required": ["hint", "reason"],
        "additionalProperties": false
      }
    }
  },
  "required": ["summary", "files", "acceptance", "risks", "questions", "cannot_verify_here", "pack", "rejected_hints"],
  "additionalProperties": false
}`

// Plan is what the implementer says it will do before it does it.
type Plan struct {
	Summary          string     `json:"summary"`
	Files            []PlanFile `json:"files"`
	Acceptance       []PlanItem `json:"acceptance"`
	Risks            []string   `json:"risks"`
	Questions        []string   `json:"questions"`
	CannotVerifyHere []string   `json:"cannot_verify_here"`
	// Pack and RejectedHints are the scout's answer. Once Sidecars.Plan has
	// validated them, Pack holds only entries that hold against the baseline
	// (with their excerpts in memory), and PackDropped the ones that did not.
	Pack          []PackEntry    `json:"pack,omitempty"`
	RejectedHints []RejectedHint `json:"rejected_hints,omitempty"`
	PackDropped   []DroppedEntry `json:"pack_dropped,omitempty"`
	PackTokens    int            `json:"pack_tokens,omitempty"`
	// Context is how large the prompt of the turn that wrote the plan got. It
	// is measured, never read from the agent's answer.
	Context harness.Context `json:"-"`
}

// ContextPack is the plan's pack as validated.
func (p Plan) ContextPack() Pack {
	return Pack{Entries: p.Pack, Dropped: p.PackDropped, RejectedHints: p.RejectedHints, Tokens: p.PackTokens}
}

// withPack is the plan carrying pk as its pack.
func (p Plan) withPack(pk Pack) Plan {
	p.Pack, p.PackDropped, p.RejectedHints, p.PackTokens = pk.Entries, pk.Dropped, pk.RejectedHints, pk.Tokens
	return p
}

// PlanFile is one file a plan touches.
type PlanFile struct {
	Path   string `json:"path"`
	Change string `json:"change"`
	Why    string `json:"why"`
}

// PlanItem is how a plan meets one acceptance item of the request.
type PlanItem struct {
	Item                string `json:"item"`
	How                 string `json:"how"`
	VerifiableInSandbox bool   `json:"verifiable_in_sandbox"`
}

// PlanInput is what a plan is asked for: the request, and when the plan is a
// revision, the plan under revision and what its review found wrong with it.
type PlanInput struct {
	Request  string
	Previous *Plan
	Findings []review.Finding
	// Scout asks the plan turn for a context pack too.
	Scout bool
}

// Planner writes the plan a run implements and has it reviewed. Sidecars
// implements it; tests implement it in memory.
type Planner interface {
	// Plan writes a plan for the request, or revises one.
	Plan(ctx context.Context, in PlanInput) (Plan, error)
	// ReviewPlan returns what the plan's reviewers found wrong with it.
	ReviewPlan(ctx context.Context, request string, plan Plan) ([]review.Finding, error)
}

// Markdown renders the plan for the agents that read it.
func (p Plan) Markdown() string {
	var b strings.Builder
	b.WriteString(strings.TrimSpace(p.Summary) + "\n")
	if len(p.Files) > 0 {
		b.WriteString("\nFiles:\n")
		for _, f := range p.Files {
			fmt.Fprintf(&b, "- %s (%s): %s\n", f.Path, f.Change, f.Why)
		}
	}
	if len(p.Acceptance) > 0 {
		b.WriteString("\nAcceptance:\n")
		for _, a := range p.Acceptance {
			verify := ""
			if !a.VerifiableInSandbox {
				verify = " [not verifiable in the sandbox]"
			}
			fmt.Fprintf(&b, "- %s: %s%s\n", a.Item, a.How, verify)
		}
	}
	writeList(&b, "Risks", p.Risks)
	writeList(&b, "Open decisions, as the plan settles them", p.Questions)
	writeList(&b, "Cannot be verified here", p.CannotVerifyHere)
	return strings.TrimRight(b.String(), "\n")
}

func writeList(b *strings.Builder, heading string, items []string) {
	if len(items) == 0 {
		return
	}
	fmt.Fprintf(b, "\n%s:\n", heading)
	for _, it := range items {
		fmt.Fprintf(b, "- %s\n", it)
	}
}

// planPrompt asks for a plan, or a revision of one.
func planPrompt(in PlanInput) string {
	var b strings.Builder
	b.WriteString("You are about to implement the change requested below in this repository. Before writing any code, write a short plan. ")
	b.WriteString("Read the repository as much as you need, but do not modify anything.\n\n")
	b.WriteString("The plan names the files you will touch, the approach, how each requirement and acceptance item of the request will be met ")
	b.WriteString("(one acceptance entry per item, quoting it), the risks, and what you cannot verify in a sandbox. ")
	b.WriteString("Where the request leaves a decision open, answer it with the most reasonable choice and say so in questions. ")
	b.WriteString("Base the plan on what the code does, not on what you expect it to do.\n\n")
	if in.Scout {
		b.WriteString(scoutInstructions)
	}
	b.WriteString("## Requested change\n\n")
	b.WriteString(strings.TrimSpace(in.Request))
	b.WriteString("\n")
	if in.Previous != nil {
		b.WriteString("\n## Your previous plan\n\n")
		b.WriteString(in.Previous.Markdown())
		b.WriteString("\n\n## What its review found\n\n")
		b.WriteString(findingList(in.Findings))
		b.WriteString("\nWrite a revised plan that resolves these. If you disagree with a finding, keep the plan and say why in questions.\n")
	}
	return b.String()
}

// scoutInstructions asks the plan turn for the context pack.
const scoutInstructions = `Also act as a scout and fill pack with the exact code the implementer and the reviewers need, so they do not have to search for it. ` +
	`Trace each acceptance item to the code it touches, find the helpers and types to reuse, the nearest existing test to copy, the rules of AGENTS.md or the docs that apply, and the traps. ` +
	`Entry kinds: edit (code the change modifies; it is shown as a pointer, so give path and line range only), reference (code to reuse or follow; smelt puts its lines in the prompt), ` +
	`test_pattern (the nearest test to copy; a reference), rule (path is the document, reason is a short quote), ` +
	`new_file (a file the change creates; reason names the existing file it follows, which you also list as a reference). ` +
	`start and end are 1-based lines, 0 for a whole file, a rule or a new file. priority is 1 (must have) to 3 (nice to have). ` +
	`Keep the pack small: only what the work needs. ` +
	`If the request has a "## Context" section, every hint in it must appear in the pack or in rejected_hints with the reason you did not follow it.` + "\n\n"

// findingList renders findings as a markdown list.
func findingList(findings []review.Finding) string {
	var b strings.Builder
	for _, f := range findings {
		fmt.Fprintf(&b, "- [%s] %s: %s\n", f.Severity, f.Location(), f.Body)
	}
	return b.String()
}

// planReviewBody is the whole of a plan review's instructions. A plan review is
// not the diff review its prompt is written for, so the prompt's own text is
// left out: only the name says which reviewer asks.
const planReviewBody = `Review the plan below against the requested change. No code has been written, so there is no diff: do not look for one, and do not implement anything.

## Requested change

%s

## Plan

%s

%s## Review instructions

Answer only these questions:

1. Does the plan cover every requirement and acceptance item the request states?
2. Does any file choice contradict the request?
3. Is anything in the plan out of scope?
%s
Report a high or medium finding only for a requirement or acceptance item the plan leaves out, or a choice that contradicts the request. Use ` + "`" + planFile + "`" + ` as the file for a finding about the plan as a whole, or the file the finding is about. Anything else is low.
Do not judge how the code will be written; judge only what the plan commits to.`

// planReviews are the prompts that review a plan: the reviews that judge a
// change against the whole request, when the project has them.
func planReviews(request string, plan Plan, prompts []review.Prompt) []review.Prompt {
	var out []review.Prompt
	for _, p := range prompts {
		if slices.Contains(completenessReviews, p.Name) {
			out = append(out, review.Prompt{Name: p.Name, Body: planReviewPrompt(request, plan)})
		}
	}
	return out
}

// packReviewQuestions are the questions a plan review adds when the plan has a
// context pack.
const packReviewQuestions = `4. Does every acceptance item have the code it needs in the context pack?
5. Is any pack entry out of scope?
A pack that leaves an acceptance item without its code, or an entry that is out of scope, is a medium finding.
`

// planReviewPrompt is the instructions of one plan review.
func planReviewPrompt(request string, plan Plan) string {
	pack, questions := "", ""
	if section := plan.ContextPack().ReviewMarkdown(); section != "" {
		pack, questions = section+"\n\n", packReviewQuestions
	}
	return fmt.Sprintf(planReviewBody, strings.TrimSpace(request), plan.Markdown(), pack, questions)
}

// Plan asks the implementer's agent, which has the repository on its sidecar,
// for a plan. The agent may read the repository but not change it.
func (s *Sidecars) Plan(ctx context.Context, in PlanInput) (Plan, error) {
	im := s.Implementer
	in.Scout = s.scouting()
	prompt := planPrompt(in)
	out, h, err := s.agentAnswer(ctx, "plan", harness.ReviewInput{
		RepoPath: im.Entry.RepoPath, Prompt: prompt, Model: im.Model, Schema: PlanSchema, Tools: planTools,
	}, planTimeout, maxPlanOutputBytes)
	if err != nil {
		return Plan{}, err
	}
	p, err := parseAnswer[Plan](h, out, "plan")
	if err != nil {
		return Plan{}, err
	}
	p.Context = h.ReviewContext(out)
	p, err = validPlan(p)
	if err != nil {
		return Plan{}, err
	}
	p = s.scoutPack(ctx, p)
	s.plan = &p
	return p, nil
}

// scouting reports whether the plan turn builds a context pack.
func (s *Sidecars) scouting() bool {
	return s.Scout.Enabled && s.Scout.Files != nil
}

// scoutPack replaces the pack the plan came with by the validated one, and
// gives it to the reviewers. A plan without a usable pack is still a plan.
func (s *Sidecars) scoutPack(ctx context.Context, p Plan) Plan {
	if !s.scouting() {
		return p.withPack(Pack{})
	}
	pk := ValidatePack(ctx, p.Pack, s.Scout.Files, s.Scout.MaxTokens)
	pk.RejectedHints = p.RejectedHints
	s.Pack = &pk
	return p.withPack(pk)
}

// ReviewPlan runs the plan reviews once, each on a reviewer sidecar. A review
// that could not run says nothing against the plan, so it adds no findings.
func (s *Sidecars) ReviewPlan(ctx context.Context, request string, plan Plan) ([]review.Finding, error) {
	prompts := planReviews(request, plan, s.Prompts)
	if len(prompts) == 0 {
		return nil, nil
	}
	if err := s.prepareReviewers(ctx); err != nil {
		return nil, err
	}
	// Progress is left out: it is recorded against a round, and none has begun.
	s.plan = &plan
	if err := s.captureReviewHandoff(ctx); err != nil {
		return nil, err
	}
	opts := s.reviewOptions(0, "plan_review", s.Review)
	opts.ProgressFn = nil
	results, err := review.RunPass(ctx, s.Acquire, s.Release, s.Exec, prompts, opts)
	if err != nil {
		return nil, fmt.Errorf("plan reviews: %w", err)
	}
	var out []review.Finding
	for _, r := range results {
		for _, f := range r.Parsed.Findings {
			f.Prompt = r.Prompt
			out = append(out, f)
		}
	}
	return out, nil
}

// validPlan checks that a plan says what it will do.
func validPlan(p Plan) (Plan, error) {
	p.Summary = strings.TrimSpace(p.Summary)
	if p.Summary == "" {
		return Plan{}, errors.New("the plan has no summary")
	}
	return p, nil
}

// parseAnswer reads the schema-checked object out of the stdout of a review
// script, whichever harness ran it; what names it in errors.
func parseAnswer[T any](h harness.Harness, out, what string) (T, error) {
	var v T
	raw, err := h.ParseAnswer(out)
	if err != nil {
		return v, err
	}
	if err := json.Unmarshal(raw, &v); err != nil {
		return v, fmt.Errorf("read the %s: %w", what, err)
	}
	return v, nil
}

// planPreamble is what the implementer is told before the request: the plan it
// wrote, what its reviewers still disagreed with when it went ahead, and what
// it expected not to be able to verify.
func planPreamble(plan Plan, unresolved []review.Finding) string {
	var b strings.Builder
	b.WriteString("Your plan, reviewed and approved:\n\n")
	b.WriteString(plan.Markdown())
	if pack := plan.ContextPack().Markdown(); pack != "" {
		b.WriteString("\n\n" + pack)
	}
	b.WriteString("\n\nImplement it. If you must deviate, say where and why in your summary.\n")
	if len(unresolved) > 0 {
		b.WriteString("\nThe plan's reviewers still disagreed with it after one revision. Weigh these as you build:\n\n")
		b.WriteString(findingList(unresolved))
	}
	if len(plan.CannotVerifyHere) > 0 {
		b.WriteString("\nYou expected not to be able to verify these in the sandbox:\n")
		for _, c := range plan.CannotVerifyHere {
			fmt.Fprintf(&b, "- %s\n", c)
		}
		b.WriteString("Unless you find a way to verify one, finish the work anyway and end your summary with it on its own " + unverifiedMarker + " line, saying how to check it.\n")
	}
	b.WriteString("\n---\n\n")
	return b.String()
}
