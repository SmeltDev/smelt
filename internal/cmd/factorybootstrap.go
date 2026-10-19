package cmd

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/smeltdev/smelt/internal/circleci"
	"github.com/smeltdev/smelt/internal/config"
	"github.com/smeltdev/smelt/internal/factory"
	"github.com/smeltdev/smelt/internal/harness"
	"github.com/smeltdev/smelt/internal/harness/all"
	"github.com/smeltdev/smelt/internal/harness/claude"
	"github.com/smeltdev/smelt/internal/harness/codex"
	"github.com/smeltdev/smelt/internal/harness/grok"
	"github.com/smeltdev/smelt/internal/iostream"
	"github.com/smeltdev/smelt/internal/review"
	"github.com/smeltdev/smelt/internal/sidecar"
	"github.com/smeltdev/smelt/internal/ui"
)

// bootstrapPoolName names the pool of the one sidecar a bootstrap uses.
const bootstrapPoolName = "factory-bootstrap"

// maxToolLine caps how much of a tool call's target is shown.
const maxToolLine = 120

func newFactoryBootstrapCmd() *cobra.Command {
	var harnessFlag, model, output string
	var timeout time.Duration
	var jsonOut bool

	cmd := &cobra.Command{
		Use:   "bootstrap",
		Short: "Have an agent read the project and write its first review prompts",
		Long: fmt.Sprintf(`Start one sandbox, have a coding agent read the project there (read-only), and
write a starting set of review prompts to %s. Edit them afterwards: they
are a first draft built from the project's own docs, configuration, history and,
when 'smelt build-prompt' wrote it, %s.

It refuses at once when the directory already holds review prompts, and never
overwrites a file. Pass --output to write to another directory and compare.

Each prompt is given a "Patterns to look for" section from smelt's catalogue
('smelt review patterns') for its area, with examples in the project's
languages, found from its files.

The agent runs in the project's factory backend and image (see 'smelt factory
build'; build the image first with 'smelt factory image'), with the agent chosen
by --harness or factory.harness. Each tool call it makes is shown as it happens.
The validation commands from the config are left out of the reviews, since they
already enforce them.`, review.DefaultDir, factory.StandardsFile),
		Args:         cobra.NoArgs,
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx := cmd.Context()
			streams := iostream.FromCmd(cmd)
			root, err := sessionProjectRoot(ctx)
			if err != nil {
				return err
			}
			cfg, err := config.LoadProjectConfig(root)
			if err != nil {
				return &userError{msg: msgValidateNotConfigured, suggestion: suggestionRunInit, err: err, blocked: true}
			}
			return runBootstrap(ctx, streams, bootstrapRequest{
				Root: root, Cfg: cfg, Harness: harnessFlag, Model: model, Timeout: timeout, Output: output,
				JSON: jsonOut, CanPrompt: canPrompt(cmd.InOrStdin(), jsonOut),
			}, bootstrapEnv{
				harness:    all.ByName,
				credential: bootstrapCredential(cmd),
				target:     bootstrapTarget(cmd, streams, root, cfg),
			})
		},
	}
	cmd.Flags().StringVar(&harnessFlag, "harness", "", "coding agent: claude, codex or grok (default: factory.harness from config, else ask)")
	cmd.Flags().StringVar(&model, "model", "", "model for the chosen agent (default: factory.models.<harness> from config, else the agent's own default)")
	cmd.Flags().DurationVar(&timeout, "timeout", factory.DefaultBootstrapTimeout, "max time for the agent to read the project and write the prompts")
	cmd.Flags().StringVar(&output, "output", "", fmt.Sprintf("directory to write the prompts to (default: %s)", review.DefaultDir))
	cmd.Flags().BoolVar(&jsonOut, "json", false, "Output as JSON")
	return cmd
}

// bootstrapRequest is what one bootstrap is asked to do.
type bootstrapRequest struct {
	Root      string
	Cfg       *config.ProjectConfig
	Harness   string
	Model     string
	Timeout   time.Duration
	Output    string
	JSON      bool
	CanPrompt bool
}

// bootstrapEnv is what a bootstrap reaches outside the project for. The command
// fills it from the machine; tests fill it with fakes.
type bootstrapEnv struct {
	harness    func(harness.Name) (harness.Harness, error)
	credential func(context.Context, harness.Name) (agentCredential, error)
	target     func(context.Context, factorySettings) (sandboxTarget, error)
}

// sandboxTarget is where the bootstrap's sidecar is created.
type sandboxTarget struct {
	Backend sidecar.Backend
	// Client and OrgID are set for the CircleCI backend only.
	Client      *circleci.Client
	OrgID       string
	Image       string
	TokenSource string
}

// bootstrapReport is the --json output.
type bootstrapReport struct {
	Dir     string          `json:"dir"`
	Prompts []bootstrapFile `json:"prompts"`
	Summary string          `json:"summary"`
	Dropped int             `json:"dropped"`
	CostUSD float64         `json:"cost_usd,omitempty"`
	Harness string          `json:"harness"`
}

// bootstrapFile is one prompt written.
type bootstrapFile struct {
	Name string `json:"name"`
	Path string `json:"path"`
}

// runBootstrap checks the target directory, readies the agent and its sandbox,
// and writes what the agent proposes. Everything that can be refused is refused
// before the first sidecar is created.
func runBootstrap(ctx context.Context, streams iostream.Streams, req bootstrapRequest, env bootstrapEnv) error {
	dir, err := bootstrapDir(req)
	if err != nil {
		return err
	}
	if err := factory.CheckPromptsDir(dir); err != nil {
		return promptsDirError(dir, err)
	}
	settings, err := resolveBuildSettings(ctx, streams, req.Root, factorySettings{}, factoryExplicit{}, req.Cfg)
	if err != nil {
		return err
	}
	name, err := resolveHarness(ctx, streams, harnessChoice{
		Root: req.Root, Cfg: req.Cfg, Flag: req.Harness, Image: settings.dockerImage(), CanPrompt: req.CanPrompt,
	})
	if err != nil {
		return err
	}
	agent, err := env.harness(harness.Name(name))
	if err != nil {
		return unknownHarnessError(name)
	}
	cred, err := env.credential(ctx, agent.Name())
	if err != nil {
		return err
	}
	target, err := env.target(ctx, settings)
	if err != nil {
		return err
	}

	status := newStatusFunc(streams)
	result, err := bootstrapOnSidecar(ctx, bootstrapRun{
		req: req, target: target, agent: agent, cred: cred, status: status,
		model: modelFor(req, name),
	})
	if err != nil {
		return err
	}
	paths, writeErr := factory.WritePrompts(dir, result.Prompts)
	report := newBootstrapReport(dir, paths, result, name)
	if req.JSON {
		if err := iostream.PrintJSON(streams.Out, report); err != nil {
			return fmt.Errorf("write report: %w", err)
		}
	} else {
		printBootstrap(streams, status, report)
	}
	if writeErr != nil {
		return &userError{msg: "Could not write every review prompt.", err: writeErr}
	}
	return nil
}

// modelFor is the model the agent runs with: --model, else factory.models.<harness>.
func modelFor(req bootstrapRequest, name string) string {
	if req.Model != "" || req.Cfg == nil || req.Cfg.Factory == nil {
		return req.Model
	}
	return req.Cfg.Factory.Models[name]
}

// bootstrapDir is the absolute directory the prompts go to.
func bootstrapDir(req bootstrapRequest) (string, error) {
	if req.Output == "" {
		return filepath.Join(req.Root, review.DefaultDir), nil
	}
	abs, err := filepath.Abs(req.Output)
	if err != nil {
		return "", fmt.Errorf("resolve %s: %w", req.Output, err)
	}
	return abs, nil
}

// promptsDirError explains a target directory a bootstrap cannot use.
func promptsDirError(dir string, err error) error {
	if !errors.Is(err, factory.ErrPromptsExist) {
		return &userError{msg: fmt.Sprintf("Could not read %s.", dir), err: err}
	}
	return newUserError(fmt.Sprintf("%s already has review prompts.", dir)).
		withCode("factory.prompts_exist").
		withSuggestion("Pass --output to write elsewhere and compare.").
		withExitCode(ExitBadArgs).
		wrap(err).
		withoutDetail()
}

// bootstrapRun is one agent run on a sidecar.
type bootstrapRun struct {
	req    bootstrapRequest
	target sandboxTarget
	agent  harness.Harness
	cred   agentCredential
	status iostream.StatusFunc
	model  string
}

// bootstrapOnSidecar creates the one sidecar, has the agent read the project
// on it and destroys it, whatever the outcome.
func bootstrapOnSidecar(ctx context.Context, r bootstrapRun) (factory.Bootstrap, error) {
	if err := factory.PrepareBackend(ctx, r.target.Backend, r.target.Image, r.status); err != nil {
		return factory.Bootstrap{}, &userError{msg: "The sandbox backend is not ready.", err: err}
	}
	r.status(iostream.LevelStep, "Starting a sandbox...")
	pool, err := newPool(ctx, r.target.Client, sidecar.PoolOptions{
		Size:    1,
		Name:    bootstrapPoolName,
		OrgID:   r.target.OrgID,
		Image:   r.target.Image,
		Backend: r.target.Backend,
		WorkDir: r.req.Root,
	}, "bootstrap sidecar", r.target.TokenSource, r.status)
	if err != nil {
		return factory.Bootstrap{}, err
	}
	defer closePool(ctx, pool, true, r.status)
	if err := waitPoolReady(ctx, pool, "bootstrap sidecar"); err != nil {
		return factory.Bootstrap{}, err
	}
	entry, err := pool.Acquire(ctx)
	if err != nil {
		return factory.Bootstrap{}, &userError{msg: "Could not get the bootstrap sidecar.", err: err}
	}
	defer pool.Release(entry)

	// Without languages the prompts' patterns have definitions alone, which is
	// a thinner prompt but not a wrong one.
	languages, err := factory.DetectLanguages(ctx, r.req.Root)
	if err != nil {
		r.status(iostream.LevelWarn, "Could not detect the project's languages; the patterns will have no examples: "+err.Error())
	}

	r.status(iostream.LevelStep, fmt.Sprintf("Reading the project with %s...", r.agent.Name()))
	result, err := factory.RunBootstrap(ctx, factory.BootstrapOptions{
		Entry: entry, Exec: review.ClientExec, Root: r.req.Root, Harness: r.agent,
		Credential: r.cred.Credential, BaseURL: r.cred.BaseURL, Model: r.model,
		Languages: languages, Commands: r.req.Cfg.Commands, Timeout: r.req.Timeout,
		OnTool: func(a harness.Activity) { r.status(iostream.LevelInfo, toolLine(a)) },
	})
	if err != nil {
		return factory.Bootstrap{}, bootstrapAgentError(err)
	}
	return result, nil
}

// bootstrapAgentError turns the agent's failure into one the user can act on.
func bootstrapAgentError(err error) error {
	switch {
	case errors.Is(err, claude.ErrMissing), errors.Is(err, codex.ErrMissing), errors.Is(err, grok.ErrMissing):
		return &userError{
			msg:        "The coding agent is not installed in the sandbox image.",
			suggestion: "Update the Dockerfile's agent kit: smelt factory image --update-kit",
			hideDetail: true,
			err:        err,
		}
	case errors.Is(err, claude.ErrCredentialRejected), errors.Is(err, codex.ErrCredentialRejected), errors.Is(err, grok.ErrCredentialRejected):
		return &userError{
			msg:        "The coding agent's credential was rejected.",
			suggestion: "Check it with 'smelt auth status', or log in again with 'smelt auth login'.",
			exitCode:   ExitAuthError,
			hideDetail: true,
			err:        err,
		}
	}
	return &userError{msg: "The agent could not write review prompts.", err: err}
}

// toolLine is one tool call as a status line: what the agent writes is
// untrusted, so it is flattened to one line without control characters.
func toolLine(a harness.Activity) string {
	detail := strings.Map(func(r rune) rune {
		if r < ' ' && r != '\t' && r != '\n' || r == 0x7f {
			return -1
		}
		return r
	}, a.Detail)
	line := strings.TrimSpace(a.Tool + " " + strings.Join(strings.Fields(detail), " "))
	if r := []rune(line); len(r) > maxToolLine {
		return string(r[:maxToolLine]) + "…"
	}
	return line
}

func newBootstrapReport(dir string, paths []string, b factory.Bootstrap, harnessName string) bootstrapReport {
	files := make([]bootstrapFile, 0, len(paths))
	for _, p := range paths {
		files = append(files, bootstrapFile{Name: strings.TrimSuffix(filepath.Base(p), ".md"), Path: p})
	}
	return bootstrapReport{
		Dir: dir, Prompts: files, Summary: b.Summary, Dropped: b.Dropped, CostUSD: b.CostUSD, Harness: harnessName,
	}
}

// printBootstrap says what the agent learned and which files it wrote.
func printBootstrap(streams iostream.Streams, status iostream.StatusFunc, r bootstrapReport) {
	if r.Summary != "" {
		streams.Println(r.Summary)
		streams.Println("")
	}
	for _, f := range r.Prompts {
		status(iostream.LevelDone, "Wrote "+f.Path)
	}
	if r.Dropped > 0 {
		status(iostream.LevelWarn, fmt.Sprintf("Dropped %d prompt(s) that were not usable.", r.Dropped))
	}
	if r.CostUSD > 0 {
		status(iostream.LevelInfo, fmt.Sprintf("Cost: $%.2f", r.CostUSD))
	}
	status(iostream.LevelInfo, "Read them and edit them to fit the project before running 'smelt factory build'.")
}

// bootstrapCredential finds the credential of the chosen agent on this machine.
func bootstrapCredential(cmd *cobra.Command) func(context.Context, harness.Name) (agentCredential, error) {
	return func(ctx context.Context, name harness.Name) (agentCredential, error) {
		rc, err := config.Resolve("", "", insecureStorageFlag(cmd))
		if err != nil && name != harness.Codex && name != harness.Grok {
			return agentCredential{}, &userError{msg: "Could not read the configuration.", err: err}
		}
		return harnessCredential(ctx, name, rc)
	}
}

// bootstrapTarget resolves where the sidecar is created from the settings: the
// project's factory backend. Docker is the default; CircleCI only when the
// project chose it, never because a token happens to be on the machine.
func bootstrapTarget(cmd *cobra.Command, streams iostream.Streams, root string, cfg *config.ProjectConfig) func(context.Context, factorySettings) (sandboxTarget, error) {
	return func(ctx context.Context, s factorySettings) (sandboxTarget, error) {
		if s.Backend != config.BackendCircleCI {
			b, err := factory.DockerBackend(root, cfg)
			if err != nil {
				return sandboxTarget{}, &userError{msg: "Docker is not available.", suggestion: "Start Docker, or choose another backend with factory.backend in .smelt/config.json.", err: err}
			}
			return sandboxTarget{Backend: b, Image: s.Image}, nil
		}
		if !cfg.UsesCircleCI() {
			return sandboxTarget{}, newUserError("The CircleCI backend is not enabled for this project.").
				withSuggestion("Set factory.backend to circleci in .smelt/config.json to opt in.").
				withExitCode(ExitBadArgs).
				withoutDetail()
		}
		rc, _ := config.Resolve("", "", insecureStorageFlag(cmd))
		client, err := ensureCircleCIClient(ctx, cmd, rc, streams, ui.PromptHidden)
		if err != nil {
			return sandboxTarget{}, err
		}
		orgID, err := resolveOrgID(cfg.OrgID, root, orgPicker(ctx, client, rc.CircleCITokenSource, streams))
		if err != nil {
			return sandboxTarget{}, err
		}
		return sandboxTarget{
			Backend: sidecar.NewCircleCIBackend(client, orgID), Client: client, OrgID: orgID,
			Image: s.Image, TokenSource: rc.CircleCITokenSource,
		}, nil
	}
}
