package factory

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/smeltdev/smelt/internal/brand"
	"github.com/smeltdev/smelt/internal/config"
	"github.com/smeltdev/smelt/internal/harness"
	"github.com/smeltdev/smelt/internal/iostream"
	"github.com/smeltdev/smelt/internal/review"
	"github.com/smeltdev/smelt/internal/sidecar"
)

const (
	// DefaultBootstrapTimeout bounds the agent's reading of the project, which
	// takes longer on a large repository than a review does.
	DefaultBootstrapTimeout = 30 * time.Minute

	// maxBootstrapPrompts is how many of the agent's prompts are kept.
	maxBootstrapPrompts = 6
	// maxBootstrapPromptBytes caps one prompt's body.
	maxBootstrapPromptBytes = 32 << 10
	// maxStandardsBytes caps the team standards put in the request.
	maxStandardsBytes = 64 << 10
	// maxBootstrapOutputBytes caps the agent's output, which with an activity
	// stream holds everything it read.
	maxBootstrapOutputBytes = 64 << 20
)

// StandardsFile is where `smelt build-prompt` writes the team's review
// standards, relative to the project root.
const StandardsFile = brand.ProjectDir + "/context/review-prompt.md"

// ErrPromptsExist means the directory a bootstrap would write to already holds
// review prompts, which a bootstrap never overwrites.
var ErrPromptsExist = errors.New("review prompts already exist")

// bootstrapTools limits the agent to reading the repository and its history.
var bootstrapTools = slices.Concat(readTools, []string{gitLogTool, "Bash(git show:*)", "Bash(git ls-files:*)"})

// promptNameRe is the shape of a prompt's name: lowercase words joined by
// hyphens. It is also what keeps the agent's text out of file paths.
var promptNameRe = regexp.MustCompile(`^[a-z0-9]+(-[a-z0-9]+)*$`)

// BootstrapSchema is the JSON Schema of the agent's answer. Every property is
// required and no counts are set, as Codex's strict mode needs; the counts are
// enforced in code.
const BootstrapSchema = `{
  "type": "object",
  "properties": {
    "summary": {"type": "string", "description": "A few sentences on what you learned about the repository and why you chose these review areas."},
    "reviews": {
      "type": "array",
      "items": {
        "type": "object",
        "properties": {
          "name": {"type": "string", "description": "File name without extension, lowercase words joined by hyphens, such as \"testing\"."},
          "body": {"type": "string", "description": "The review prompt, as markdown."}
        },
        "required": ["name", "body"],
        "additionalProperties": false
      }
    }
  },
  "required": ["summary", "reviews"],
  "additionalProperties": false
}`

// Bootstrap is the checked answer of a bootstrap: the prompts to write.
type Bootstrap struct {
	Summary string
	Prompts []review.Prompt
	// Dropped is how many of the agent's prompts failed the checks.
	Dropped int
	// CostUSD is what the agent run cost, 0 when the harness does not say.
	CostUSD float64
}

// bootstrapAnswer is the agent's answer, as the schema shapes it.
type bootstrapAnswer struct {
	Summary string          `json:"summary"`
	Reviews []review.Prompt `json:"reviews"`
}

// BootstrapOptions is what RunBootstrap needs.
type BootstrapOptions struct {
	// Entry is the sidecar the agent runs on, synced with the project, and
	// Exec runs scripts on it.
	Entry *sidecar.PoolEntry
	Exec  review.Execer
	// Root is the project root on this machine.
	Root       string
	Harness    harness.Harness
	Credential harness.Credential
	BaseURL    string
	Model      string
	// Commands are the project's validation commands, which reviews leave alone.
	Commands []config.Command
	// Timeout bounds the agent's run; DefaultBootstrapTimeout when zero.
	Timeout time.Duration
	// OnTool is told of each tool call the agent makes, as it makes it.
	OnTool func(harness.Activity)
}

// The request to the agent, in the parts the validation commands and the team
// standards go between.
const (
	bootstrapIntro = `You are setting up code review for this repository. An automated loop makes changes here: an implementer agent writes each change, then independent reviewers check it, one per review prompt, and their findings are sent back to the implementer until the change passes. Your job is to write those review prompts.

## How the prompts are used

- A reviewer is given one prompt, together with the requested change, which is the specification, and an instruction to read the uncommitted change with ` + "`git diff HEAD`" + `. Write only the review instructions; do not repeat those parts.
- A reviewer can read the repository and its history but cannot edit files or run commands such as tests.
- A reviewer reports findings as structured data, each with a severity: high, medium, low or info. Do not describe an output format.
- Every high or medium finding sends the change back for another round of work. A prompt that produces high or medium findings for things not worth fixing makes the loop slower and its results worse. So every prompt must say precisely what clears the high and medium bar in its area, and send everything else to low.
`
	bootstrapValidation = `- These validation commands also run on every change, and their failures are sent back too, so reviews must not check what they enforce, such as formatting, lint rules, build errors or failing tests:
`
	bootstrapRead = `
## What to read first

Read what the repository says about itself before you write anything: AGENTS.md, CLAUDE.md, CONTRIBUTING and README files, docs, CI configuration, linter and formatter configuration, and a sample of the code and its tests. Use git log to see how recent changes are made. Work out the languages and frameworks, the architectural rules, and the error handling and testing conventions the project actually follows.
`
	bootstrapStandards = `The team's review standards, mined from comments on its past pull requests, are in ` + "`" + StandardsFile + "`" + ` and below. Treat them as evidence of what this team pushes back on, and tell reviewers to read that file.

<standards>
%s
</standards>
`
	//nolint:gosec // prose for an agent, not a credential
	bootstrapWrite = `
## What to write

- Write 2 to 4 prompts. Each covers a distinct area, with no overlap: any finding should belong to exactly one prompt. Choose the areas this repository needs, such as correctness of the core logic, tests, architecture and conventions, or security, and leave out any area you have nothing specific to say about.
- Each prompt has:
  - A title, and a short paragraph naming the project and what this reviewer looks for.
  - The repository's own rules for the area, made concrete from what you read, with file paths and the patterns the code uses. Leave out generic advice a competent reviewer already knows.
  - A severity section that defines high, medium and low for the area. A high or medium finding must come with a concrete failure the reviewer can describe: the input or state, and what goes wrong.
  - These instructions: review only the change, not code it does not touch, unless the change makes a problem newly reachable; treat the requested change as the scope, so behavior it does not ask for is not required; report all the findings at once rather than holding related ones back for a later round; and report no findings when nothing clears the bar.
- Keep each prompt under about 80 lines of markdown.
- Give each prompt a name: its file name without an extension, in lowercase words joined by hyphens, such as "testing".
- In the summary, say in a few sentences what you learned about the repository and why you chose these areas.
`
)

// bootstrapPrompt is the request to the agent. standards is the team's review
// standards, "" when there are none.
func bootstrapPrompt(commands []config.Command, standards string) string {
	var b strings.Builder
	b.WriteString(bootstrapIntro)
	if len(commands) > 0 {
		b.WriteString(bootstrapValidation)
		for _, c := range commands {
			fmt.Fprintf(&b, "  - %s: `%s`\n", c.Name, c.Run)
		}
	}
	b.WriteString(bootstrapRead)
	if s := strings.TrimSpace(standards); s != "" {
		b.WriteString("\n")
		fmt.Fprintf(&b, bootstrapStandards, capUTF8(s, maxStandardsBytes))
	}
	b.WriteString(bootstrapWrite)
	return b.String()
}

// capUTF8 cuts s to at most n bytes without splitting a character.
func capUTF8(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n]
}

// checkBootstrap checks the agent's answer, which becomes files: prompts that
// fail the checks are dropped and counted, and an answer with none left is an
// error.
func checkBootstrap(a bootstrapAnswer) (Bootstrap, error) {
	out := Bootstrap{Summary: strings.TrimSpace(a.Summary)}
	seen := map[string]bool{}
	for _, r := range a.Reviews {
		body := strings.TrimSpace(r.Body)
		switch {
		case len(out.Prompts) >= maxBootstrapPrompts,
			!promptNameRe.MatchString(r.Name),
			seen[r.Name],
			body == "",
			len(body) > maxBootstrapPromptBytes:
			out.Dropped++
			continue
		}
		seen[r.Name] = true
		out.Prompts = append(out.Prompts, review.Prompt{Name: r.Name, Body: body})
	}
	if len(out.Prompts) == 0 {
		return Bootstrap{}, errors.New("the agent gave no usable review prompts")
	}
	return out, nil
}

// CheckPromptsDir returns ErrPromptsExist when dir already holds review
// prompts. A directory that is missing or has none is fine.
func CheckPromptsDir(dir string) error {
	entries, err := os.ReadDir(dir)
	switch {
	case errors.Is(err, os.ErrNotExist):
		return nil
	case err != nil:
		return fmt.Errorf("read %s: %w", dir, err)
	}
	for _, e := range entries {
		if e.Type().IsRegular() && review.IsPromptFile(e.Name()) {
			return fmt.Errorf("%w in %s", ErrPromptsExist, dir)
		}
	}
	return nil
}

// WritePrompts writes each prompt to dir as <name>.md and returns the paths
// written. It never overwrites: a file that appeared since CheckPromptsDir
// fails that prompt, and the rest are still written.
func WritePrompts(dir string, prompts []review.Prompt) ([]string, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("create %s: %w", dir, err)
	}
	var paths []string
	var errs []error
	for _, p := range prompts {
		path := filepath.Join(dir, p.Name+".md")
		if err := writeNew(path, p.Body+"\n"); err != nil {
			errs = append(errs, err)
			continue
		}
		paths = append(paths, path)
	}
	return paths, errors.Join(errs...)
}

// writeNew creates path with content, failing when path exists.
func writeNew(path, content string) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644) //nolint:gosec // a review prompt is meant to be read and committed
	if err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}
	if _, err := f.WriteString(content); err != nil {
		_ = f.Close()
		return fmt.Errorf("write %s: %w", path, err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}
	return nil
}

// RunBootstrap has the agent read the project on the sidecar, read-only, and
// returns the review prompts it wrote, checked. Nothing is written to disk.
func RunBootstrap(ctx context.Context, o BootstrapOptions) (Bootstrap, error) {
	timeout := o.Timeout
	if timeout <= 0 {
		timeout = DefaultBootstrapTimeout
	}
	// An unreadable standards file means no standards: it only sharpens the prompts.
	standards, _ := os.ReadFile(filepath.Join(o.Root, StandardsFile))
	if script, env := o.Harness.Prepare(o.Credential); script != "" {
		if _, err := review.RunScriptEnv(ctx, o.Exec, o.Entry, script, env, 64<<10); err != nil {
			return Bootstrap{}, fmt.Errorf("set up %s: %w", o.Harness.Name(), err)
		}
	}
	script := o.Harness.ReviewScript(harness.ReviewInput{
		RepoPath: o.Entry.RepoPath,
		Prompt:   bootstrapPrompt(ValidationCommands(o.Commands), string(standards)),
		Model:    o.Model,
		Schema:   BootstrapSchema,
		Tools:    bootstrapTools,
		Stream:   true,
	})
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	out, cost, err := runStreamed(ctx, o, script, timeout)
	if err != nil {
		return Bootstrap{}, err
	}
	answer, err := parseAnswer[bootstrapAnswer](o.Harness, out, "review prompts")
	if err != nil {
		return Bootstrap{}, err
	}
	b, err := checkBootstrap(answer)
	b.CostUSD = cost
	return b, err
}

// runStreamed runs the agent's script, shows its tool calls as they happen and
// returns its stdout and cost. Both streams go to a parser of their own, since
// Claude's events are on stdout and Codex's on stderr; a stream with no events
// on it is ignored by its parser.
func runStreamed(ctx context.Context, o BootstrapOptions, script string, timeout time.Duration) (string, float64, error) {
	var (
		mu           sync.Mutex
		out, errOut  strings.Builder
		tooBig       bool
		onOut, onErr = o.Harness.NewImplementParser(o.showTool), o.Harness.NewImplementParser(o.showTool)
	)
	code, execErr := o.Exec(ctx, o.Entry, script, o.Harness.Env(o.Credential, o.BaseURL), func(stream string, data []byte) {
		mu.Lock()
		defer mu.Unlock()
		if stream == iostream.StreamStderr {
			if errOut.Len() < maxBootstrapOutputBytes {
				errOut.Write(data)
			}
			onErr.Write(data)
			return
		}
		if out.Len()+len(data) > maxBootstrapOutputBytes {
			tooBig = true
			return
		}
		out.Write(data)
		onOut.Write(data)
	}, nil)
	onOut.Flush()
	onErr.Flush()
	if err := o.Harness.RunError(ctx, timeout, code, execErr, out.String(), errOut.String()); err != nil {
		return "", 0, err
	}
	if tooBig {
		return "", 0, fmt.Errorf("the agent's output is larger than %d bytes", maxBootstrapOutputBytes)
	}
	return out.String(), onOut.Outcome().Turn.CostUSD, nil
}

// showTool reports an activity that is a tool call; the agent's prose and its
// reasoning are left out.
func (o BootstrapOptions) showTool(a harness.Activity) {
	if o.OnTool != nil && a.Tool != "" && a.Tool != harness.ReasoningTool {
		o.OnTool(a)
	}
}
