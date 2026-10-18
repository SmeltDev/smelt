package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/mail"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"

	"github.com/smeltdev/smelt/internal/brand"
	"github.com/smeltdev/smelt/internal/envspec"
	"github.com/smeltdev/smelt/internal/quality"
)

// Command roles describe what a command does. Only RoleGate is acted on:
// sidecar setup marks gate commands for remote execution.
const (
	RoleGate    = "gate"    // pass/fail check
	RoleAutofix = "autofix" // rewrites files (formatters)
)

// CmdInstall is the conventional name for the dependency install command. It has
// no role of its own but sidecar setup treats it like a gate command.
const CmdInstall = "install"

// Backends a factory run's sidecars can execute on. The daemon's request type
// refers to these same names.
const (
	BackendCircleCI = "circleci"
	BackendDocker   = "docker"
)

// ValidBackend reports whether name is a backend. Empty is valid: it means the
// default, BackendDocker.
func ValidBackend(name string) bool {
	switch name {
	case "", BackendCircleCI, BackendDocker:
		return true
	}
	return false
}

// BackendOrDefault returns name, or BackendDocker when name is empty. CircleCI
// is opt-in: nothing selects it except naming it.
func BackendOrDefault(name string) string {
	if name == "" {
		return BackendDocker
	}
	return name
}

// UsesCircleCI reports whether the project opted into the CircleCI backend,
// which it does only by setting factory.backend to "circleci". A CircleCI token
// on the machine is not consent, so every gate that decides whether smelt may
// reach CircleCI on a project's behalf asks this rather than deriving it.
func (c *ProjectConfig) UsesCircleCI() bool {
	return c != nil && c.Factory != nil && c.Factory.Backend == BackendCircleCI
}

// UseCircleCI records the project's opt-in to the CircleCI backend.
func (c *ProjectConfig) UseCircleCI() {
	if c.Factory == nil {
		c.Factory = &FactoryConfig{}
	}
	c.Factory.Backend = BackendCircleCI
}

// Command timeouts, in seconds. DefaultCommandTimeout is what a command with no
// timeout of its own runs with, everywhere: smelt validate and the hook budgets
// computed from it. The role defaults are what command detection writes.
//
// Lint gets room for a cold run: a fresh sandbox builds the linter and fetches
// its dependencies first, which on its own can exceed a minute.
//
// DefaultInstallTimeout bounds the dependency install a factory sidecar or an
// image verification runs before anything else, so a hung install fails the
// run instead of blocking it.
const (
	DefaultInstallTimeout = 600
	DefaultCommandTimeout = 300
	DefaultTestTimeout    = 300
	DefaultLintTimeout    = 180
	DefaultFormatTimeout  = 30
)

// Async validation modes, the values ProjectConfig.AsyncValidate accepts.
//
// They exist because the judgement of which changes are safe to validate in the
// background is a judgement, and a repo whose checks it reads wrongly needs a
// way to say so without waiting for a new release.
const (
	// AsyncValidateAuto lets the daemon decide per change. The default.
	AsyncValidateAuto = "auto"
	// AsyncValidateAlways backgrounds every hook-driven run, however large.
	AsyncValidateAlways = "always"
	// AsyncValidateNever keeps every run blocking, as it was before background
	// validation existed.
	AsyncValidateNever = "never"
)

// Command is a single validation command.
type Command struct {
	Name         string `json:"name"`
	Run          string `json:"run"`
	Role         string `json:"role,omitempty"`
	Timeout      int    `json:"timeout,omitempty"`
	Remote       bool   `json:"remote,omitempty"`
	Local        bool   `json:"local,omitempty"`
	SidecarImage string `json:"sidecarImage,omitempty"`
	// Quick is free text telling a factory implementer how to check its work
	// cheaply before smelt runs this command in full, such as the tests of the
	// packages it changed. It is guidance, never run by smelt.
	Quick string `json:"quick,omitempty"`
	// Format, when FormatSARIF, says the command prints SARIF. A factory run
	// then fails the gate only on findings the change adds to the baseline's,
	// and ignores the command's exit code.
	Format string `json:"format,omitempty"`
	// SarifPath is where a FormatSARIF command writes its SARIF, relative to
	// the repo, when it writes a file rather than stdout.
	SarifPath string `json:"sarifPath,omitempty"`
}

// FormatSARIF is the Command.Format of a command that prints SARIF.
const FormatSARIF = "sarif"

// validate reports a command whose placement or format settings do not make sense.
func (c Command) validate() error {
	if c.Local && c.Remote {
		return fmt.Errorf("command %q cannot be both local and remote", c.Name)
	}
	if c.Format != "" && c.Format != FormatSARIF {
		return fmt.Errorf("command %q: format %q must be empty or %q", c.Name, c.Format, FormatSARIF)
	}
	if c.SarifPath != "" && c.Format != FormatSARIF {
		return fmt.Errorf("command %q: sarifPath needs format %q", c.Name, FormatSARIF)
	}
	if err := validateRepoRelative("sarifPath", c.SarifPath); err != nil {
		return fmt.Errorf("command %q: %w", c.Name, err)
	}
	return nil
}

// FastGateSeconds is the configured timeout, in seconds, up to which a gate is
// cheap enough for the implementer to run in full after its last edit.
const FastGateSeconds = 60

// IsFastGate reports whether the command is a gate that configures a timeout of
// FastGateSeconds or less. A gate with no timeout runs with the default, which
// says nothing about how fast it is, so it does not qualify.
func (c Command) IsFastGate() bool {
	return c.Role == RoleGate && c.Timeout > 0 && c.Timeout <= FastGateSeconds
}

// EffectiveTimeout is the timeout, in seconds, the command runs with: its own,
// or DefaultCommandTimeout when it sets none.
func (c Command) EffectiveTimeout() int {
	if c.Timeout > 0 {
		return c.Timeout
	}
	return DefaultCommandTimeout
}

// KeepLongerTimeouts returns detected with each command's timeout raised to that
// of the previous command of the same name, when the previous one was longer.
// Re-running detection must not undo a timeout someone raised by hand, while a
// stale default shorter than today's still gives way to it. Timeouts are
// ceilings, so the longer one is always the safe choice. Neither input is
// modified.
func KeepLongerTimeouts(detected, previous []Command) []Command {
	longest := make(map[string]int, len(previous))
	for _, c := range previous {
		longest[c.Name] = max(longest[c.Name], c.Timeout)
	}
	out := make([]Command, len(detected))
	for i, c := range detected {
		c.Timeout = max(c.Timeout, longest[c.Name])
		out[i] = c
	}
	return out
}

// RunsLocally reports whether the command explicitly opts out of remote execution.
func (c Command) RunsLocally() bool { return c.Local }

// RunsRemotely reports whether the command uses the default remote placement.
func (c Command) RunsRemotely() bool { return !c.Local }

// VCSConfig holds VCS configuration for the project.
type VCSConfig struct {
	Org  string `json:"org,omitempty"`
	Repo string `json:"repo,omitempty"`
}

// ValidationConfig holds project-level defaults for validation behaviour.
type ValidationConfig struct {
	SidecarImage string `json:"sidecarImage,omitempty"`
}

// Coding agents a factory run can use. The harness package names them with
// these, so the config and the registry agree.
const (
	HarnessClaude = "claude"
	HarnessCodex  = "codex"
)

// ValidHarness reports whether name is a coding agent. Empty is valid: it
// means none is chosen.
func ValidHarness(name string) bool {
	switch name {
	case "", HarnessClaude, HarnessCodex:
		return true
	}
	return false
}

// FactoryConfig holds project defaults for `smelt factory build`.
type FactoryConfig struct {
	// Harness is the coding agent runs use: "claude" or "codex". Empty means
	// ask, or fail when there is no terminal.
	Harness string `json:"harness,omitempty"`
	// Models maps a harness to the model it uses when --model is not given, so
	// switching harness does not carry a name the other CLI rejects.
	Models  map[string]string `json:"models,omitempty"`
	Backend string            `json:"backend,omitempty"` // "docker" (the default) or "circleci"
	Image   string            `json:"image,omitempty"`   // Docker image for the docker backend; user-managed, never auto-built
	// Dockerfile is the repo-relative Dockerfile smelt builds the docker
	// backend's image from. Unlike Image it is managed: the tag is computed
	// from its content and the image is built when missing.
	Dockerfile  string `json:"dockerfile,omitempty"`
	MaxAttempts int    `json:"maxAttempts,omitempty"` // 0 means the built-in default
	Log         bool   `json:"log,omitempty"`         // keep a run log by default
	// MaxConcurrent is how many factory runs may be active in the project at
	// once. 0 means 1.
	MaxConcurrent int `json:"maxConcurrent,omitempty"`
	// Author is who the commits on a run's branch are attributed to, as
	// "Name <email>". Empty means the developer's own git identity.
	Author string `json:"author,omitempty"`
	// Plan asks the implementer for a reviewed plan before it writes code.
	// Unset means true.
	Plan *bool `json:"plan,omitempty"`
	// GatesFirst runs the validation commands before the reviews and skips
	// the reviews of a round whose gates failed. Unset means true.
	GatesFirst *bool `json:"gatesFirst,omitempty"`
	// Cache keeps the docker backend's build and module caches in volumes that
	// outlive sidecars. Unset means true.
	Cache *bool `json:"cache,omitempty"`
	// Quality tunes the built-in quality check of a run. Unset means the check
	// is on with its default limits.
	Quality *QualityConfig `json:"quality,omitempty"`
}

// QualityConfig is factory.quality: the limits of the built-in quality check.
// A zero limit means the default, which internal/quality owns.
type QualityConfig struct {
	// Enabled turns the check off when false. Unset means true.
	Enabled *bool `json:"enabled,omitempty"`
	// MaxFunctionComplexity is the cyclomatic complexity above which a new or
	// changed function is a finding.
	MaxFunctionComplexity int `json:"maxFunctionComplexity,omitempty"`
	// MaxErosionRise is how many percentage points a change may raise erosion.
	MaxErosionRise float64 `json:"maxErosionRise,omitempty"`
	// MinCloneLines is the shortest duplicated block, in lines.
	MinCloneLines int `json:"minCloneLines,omitempty"`
	// Command replaces the default provider, lizard: it runs in the sandbox from
	// the repo root and prints the metrics contract as JSON on stdout (see
	// internal/quality).
	Command string `json:"command,omitempty"`
	// Exclude are globs (** crosses directories) of files to leave out, on top
	// of the defaults: vendored and test code and generated files.
	Exclude []string `json:"exclude,omitempty"`
	// Include are globs of files to measure although a default leaves them out.
	Include []string `json:"include,omitempty"`
	// DefaultExcludes turns the built-in exclusions off when false. Unset means
	// true.
	DefaultExcludes *bool `json:"defaultExcludes,omitempty"`
}

// validateLimits checks the numeric settings, which must not be negative.
func (f *FactoryConfig) validateLimits() error {
	if f.MaxAttempts < 0 {
		return fmt.Errorf("factory.maxAttempts must not be negative, got %d", f.MaxAttempts)
	}
	if f.MaxConcurrent < 0 {
		return fmt.Errorf("factory.maxConcurrent must not be negative, got %d", f.MaxConcurrent)
	}
	return f.Quality.validate()
}

func (q *QualityConfig) validate() error {
	switch {
	case q == nil:
		return nil
	case q.MaxFunctionComplexity < 0:
		return fmt.Errorf("factory.quality.maxFunctionComplexity must not be negative, got %d", q.MaxFunctionComplexity)
	case q.MaxErosionRise < 0:
		return fmt.Errorf("factory.quality.maxErosionRise must not be negative, got %g", q.MaxErosionRise)
	case q.MinCloneLines < 0:
		return fmt.Errorf("factory.quality.minCloneLines must not be negative, got %d", q.MinCloneLines)
	}
	for _, g := range q.Exclude {
		if err := quality.ValidateGlob(g); err != nil {
			return fmt.Errorf("factory.quality.exclude: %w", err)
		}
	}
	for _, g := range q.Include {
		if err := quality.ValidateGlob(g); err != nil {
			return fmt.Errorf("factory.quality.include: %w", err)
		}
	}
	return nil
}

// QualityCommand is the project's own metrics provider, "" for the default.
func (f *FactoryConfig) QualityCommand() string {
	if f == nil || f.Quality == nil {
		return ""
	}
	return strings.TrimSpace(f.Quality.Command)
}

// QualityEnabled reports whether runs measure quality; the default is yes.
func (f *FactoryConfig) QualityEnabled() bool {
	return f == nil || f.Quality == nil || f.Quality.Enabled == nil || *f.Quality.Enabled
}

// CacheEnabled reports whether docker sidecars share per-project cache volumes;
// the default is yes.
func (f *FactoryConfig) CacheEnabled() bool {
	return f == nil || f.Cache == nil || *f.Cache
}

// PlanEnabled reports whether runs plan before implementing; the default is yes.
func (f *FactoryConfig) PlanEnabled() bool {
	return f == nil || f.Plan == nil || *f.Plan
}

// GatesFirstEnabled reports whether a round's validation commands run before its
// reviews; the default is yes.
func (f *FactoryConfig) GatesFirstEnabled() bool {
	return f == nil || f.GatesFirst == nil || *f.GatesFirst
}

// CommitAuthor is a git identity that commits are attributed to.
type CommitAuthor struct {
	Name  string
	Email string
}

// Complete reports whether both the name and the email are set.
func (a CommitAuthor) Complete() bool { return a.Name != "" && a.Email != "" }

// String writes the author as "Name <email>", the form ParseCommitAuthor reads.
func (a CommitAuthor) String() string { return a.Name + " <" + a.Email + ">" }

// ParseCommitAuthor reads an author written as "Name <email>".
func ParseCommitAuthor(s string) (CommitAuthor, error) {
	addr, err := mail.ParseAddress(s)
	if err != nil {
		return CommitAuthor{}, fmt.Errorf("parse author %q: %w", s, err)
	}
	if addr.Name == "" {
		return CommitAuthor{}, fmt.Errorf("author %q has no name", s)
	}
	return CommitAuthor{Name: addr.Name, Email: addr.Address}, nil
}

// ConcurrentRuns is how many runs may be active at once: MaxConcurrent, at
// least 1.
func (f *FactoryConfig) ConcurrentRuns() int {
	if f == nil {
		return 1
	}
	return max(f.MaxConcurrent, 1)
}

// CommitAuthor is the configured author, the zero value when none is set.
func (f *FactoryConfig) CommitAuthor() (CommitAuthor, error) {
	if f == nil || f.Author == "" {
		return CommitAuthor{}, nil
	}
	return ParseCommitAuthor(f.Author)
}

// ProjectConfig is the per-repo configuration stored in .smelt/config.json.
type ProjectConfig struct {
	Commands            []Command            `json:"commands,omitempty"`
	VCS                 *VCSConfig           `json:"vcs,omitempty"`
	Validation          *ValidationConfig    `json:"validation,omitempty"`
	Factory             *FactoryConfig       `json:"factory,omitempty"`
	OrgID               string               `json:"orgID,omitempty"`
	StopHookMaxAttempts int                  `json:"stopHookMaxAttempts,omitempty"`
	Environment         *envspec.Environment `json:"environment,omitempty"`
	// AsyncValidate is one of the AsyncValidate* modes. Empty means auto.
	AsyncValidate string `json:"asyncValidate,omitempty"`
	// AsyncValidateMaxLines overrides how large a change may be and still be
	// validated in the background. Zero means the built-in default.
	AsyncValidateMaxLines int `json:"asyncValidateMaxLines,omitempty"`
	// AsyncValidateWorktree runs background checks in a checked-out snapshot
	// instead of the live working tree, so editing while they run cannot make
	// the answer describe code that has moved. Off by default: a snapshot holds
	// nothing git was told to ignore, so a project whose checks need installed
	// dependencies or a build cache would see environment failures reported as
	// code failures.
	AsyncValidateWorktree bool `json:"asyncValidateWorktree,omitempty"`
	// AsyncValidateInert adds to the paths this project counts as prose: an
	// extension (".sql") or an exact file name ("NOTICE"). A change confined to
	// them is validated in the background however large it is.
	//
	// It adds rather than replaces, because the built-in list is an allowlist
	// and an allowlist fails towards making somebody wait. A project that wants
	// one of the defaults checked says so with AsyncValidateBlocking.
	AsyncValidateInert []string `json:"asyncValidateInert,omitempty"`
	// AsyncValidateBlocking names paths whose change always blocks, in the same
	// two forms. It is the narrower instruction and so it wins over everything
	// else — an inert default, a small diff, and the "always" mode included,
	// since a project naming a path here has said it wants to wait for it.
	AsyncValidateBlocking []string `json:"asyncValidateBlocking,omitempty"`
}

// LoadProjectConfig reads .smelt/config.json from workDir.
func LoadProjectConfig(workDir string) (*ProjectConfig, error) {
	path := filepath.Join(workDir, brand.ProjectDir, "config.json")
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("could not read config.json: %w", err)
	}
	var cfg ProjectConfig
	if err := json.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("parse config.json: %w", err)
	}
	if err := cfg.validate(); err != nil {
		return nil, fmt.Errorf("validate config.json: %w", err)
	}
	// Configs written by earlier versions may carry a "test" step in the saved
	// environment. Drop it on load so it is neither run as a setup step nor
	// written back out on the next save.
	cfg.Environment = cfg.Environment.ForConfig()
	return &cfg, nil
}

// HasCommands reports whether any commands are configured.
func (c *ProjectConfig) HasCommands() bool {
	return len(c.Commands) > 0
}

// HasSidecarImage reports whether a project-level sidecar snapshot image is configured.
func (c *ProjectConfig) HasSidecarImage() bool {
	return c != nil && c.Validation != nil && c.Validation.SidecarImage != ""
}

func commandEligibleForSidecarRemote(cmd Command) bool {
	if cmd.Remote || cmd.Local {
		return false
	}
	if cmd.Name == CmdInstall {
		return true
	}
	return cmd.Role == RoleGate
}

// MarkRemoteCommandsForSidecarSetup marks install and gate commands for remote
// execution after a successful sidecar setup. Returns true when any command
// was updated.
func (c *ProjectConfig) MarkRemoteCommandsForSidecarSetup() bool {
	if c == nil {
		return false
	}
	changed := false
	for i := range c.Commands {
		if commandEligibleForSidecarRemote(c.Commands[i]) {
			c.Commands[i].Remote = true
			changed = true
		}
	}
	return changed
}

// ErrNoSuchCommand reports a command name that is not in the project config.
var ErrNoSuchCommand = errors.New("no such command")

// MarkCommandRemote marks commands for remote execution. An empty name applies
// to every configured command except autofix ones: a formatter rewrites files,
// and on a sidecar those edits never reach the local working tree, so sweeping
// them in would break the thing silently. Naming one explicitly still marks it,
// for the caller who means it. Returns the names it changed — commands already
// marked remote are left out, so an empty slice means there was nothing to do.
func (c *ProjectConfig) MarkCommandRemote(name string) ([]string, error) {
	if c == nil {
		return nil, fmt.Errorf("%w: %q", ErrNoSuchCommand, name)
	}
	if name != "" && c.FindCommand(name) == nil {
		return nil, fmt.Errorf("%w: %q", ErrNoSuchCommand, name)
	}
	var changed []string
	for i := range c.Commands {
		if name == "" && c.Commands[i].Role == RoleAutofix {
			continue
		}
		if name != "" && c.Commands[i].Name != name {
			continue
		}
		if c.Commands[i].Remote {
			continue
		}
		c.Commands[i].Remote = true
		c.Commands[i].Local = false
		changed = append(changed, c.Commands[i].Name)
	}
	return changed, nil
}

// FindCommand returns the command with the given name, or nil if not found.
func (c *ProjectConfig) FindCommand(name string) *Command {
	for i := range c.Commands {
		if c.Commands[i].Name == name {
			return &c.Commands[i]
		}
	}
	return nil
}

// SaveProjectConfig writes the config back to .smelt/config.json.
func SaveProjectConfig(workDir string, cfg *ProjectConfig) error {
	if err := cfg.validate(); err != nil {
		return fmt.Errorf("validate config.json: %w", err)
	}
	dir := filepath.Join(workDir, brand.ProjectDir)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	data, err := marshalIndent(cfg)
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, "config.json"), append(data, '\n'), 0o644)
}

// Validate reports whether the config is usable, for callers assembling one
// before it is written.
func (c *ProjectConfig) Validate() error { return c.validate() }

func (c *ProjectConfig) validate() error {
	if c == nil {
		return nil
	}
	for _, command := range c.Commands {
		if err := command.validate(); err != nil {
			return err
		}
	}
	if err := c.Factory.validate(); err != nil {
		return err
	}
	// Trimmed before the checks below rather than after, so the entry validated
	// and the entry stored are the one somebody meant.
	trimPathRules(c.AsyncValidateInert)
	trimPathRules(c.AsyncValidateBlocking)
	// Reported rather than ignored. A misspelt entry here fails silently in the
	// direction nobody checks: the project believes it is waiting for its .sql
	// changes, and nothing ever tells it otherwise.
	if err := validatePathRules("asyncValidateInert", c.AsyncValidateInert); err != nil {
		return err
	}
	if err := validatePathRules("asyncValidateBlocking", c.AsyncValidateBlocking); err != nil {
		return err
	}
	for _, entry := range c.AsyncValidateBlocking {
		if slices.Contains(c.AsyncValidateInert, entry) {
			return fmt.Errorf("%q is in both asyncValidateInert and asyncValidateBlocking", entry)
		}
	}
	return nil
}

// validate reports a factory config that is not usable. A nil config is.
func (f *FactoryConfig) validate() error {
	if f == nil {
		return nil
	}
	if !ValidBackend(f.Backend) {
		return fmt.Errorf("factory.backend %q must be %q or %q", f.Backend, BackendCircleCI, BackendDocker)
	}
	if !ValidHarness(f.Harness) {
		return fmt.Errorf("factory.harness %q must be %q or %q", f.Harness, HarnessClaude, HarnessCodex)
	}
	for name := range f.Models {
		if name == "" || !ValidHarness(name) {
			return fmt.Errorf("factory.models.%s: unknown harness (want %q or %q)", name, HarnessClaude, HarnessCodex)
		}
	}
	if err := validateRepoRelative("factory.dockerfile", f.Dockerfile); err != nil {
		return err
	}
	if err := f.validateLimits(); err != nil {
		return err
	}
	if _, err := f.CommitAuthor(); err != nil {
		return fmt.Errorf("factory.author must look like \"Name <email>\": %w", err)
	}
	return nil
}

// trimPathRules strips surrounding whitespace from an inert or blocking list,
// in place.
//
// Whitespace in a config file is invisible to the person who wrote it, so
// "sql " has to mean what "sql" means or every check below is one a typo walks
// past: the bare-extension check misses it, the entry is stored as a file name
// nothing is called, and the project waits for migrations that never block.
// A file name whose whitespace is load-bearing loses here, which is the right
// way round — such a name is close to unheard of, and the cost of trimming it
// is an entry that does not match, the cost of keeping it an entry that
// silently matches nothing.
//
// Entries left empty are not dropped. validatePathRules reports them, because
// a list that quietly shrinks is the silence this whole check exists to break.
func trimPathRules(entries []string) {
	for i, entry := range entries {
		entries[i] = strings.TrimSpace(entry)
	}
}

// validatePathRules checks the entries of an inert or blocking list.
//
// An entry is either an extension with its leading dot (".sql") or an exact
// file name ("NOTICE"). Anything else is a mistake worth naming: a path, a
// glob, and a bare extension are all things somebody would reasonably write
// and none of them would ever match.
func validatePathRules(key string, entries []string) error {
	for _, entry := range entries {
		switch {
		case strings.TrimSpace(entry) == "":
			return fmt.Errorf("%s has an empty entry", key)
		case entry == ".":
			return fmt.Errorf("%s: %q is not an extension or a file name", key, entry)
		case strings.ContainsAny(entry, `/\`):
			return fmt.Errorf("%s: %q looks like a path; use an extension (\".md\") or a file name (\"NOTICE\")", key, entry)
		case strings.Contains(entry, "*"):
			return fmt.Errorf("%s: %q looks like a glob; use an extension (\".md\") or a file name (\"NOTICE\")", key, entry)
		case knownExtensions[strings.ToLower(entry)]:
			return fmt.Errorf("%s: %q looks like an extension missing its dot; write %q", key, entry, "."+entry)
		}
	}
	return nil
}

// knownExtensions are extension names, without their leading dot, that are
// rejected when written bare. An entry with no dot is stored as an exact file
// name, so "sql" waits for a file called "sql" and never sees a migration.
//
// It is a list of names rather than a test of shape because the two shapes are
// the same: "sql" and "mvnw" are both lowercase letters with no dot, and a shape
// test would reject the second with no way for a project to insist. A list only
// ever misses one, and the ones it holds are the ones somebody writes.
var knownExtensions = map[string]bool{
	"adoc": true, "bash": true, "bat": true, "c": true, "cc": true, "clj": true,
	"cljs": true, "cpp": true, "cs": true, "css": true, "csv": true, "dart": true,
	"edn": true, "erl": true, "ex": true, "exs": true, "gif": true, "go": true,
	"gql": true, "gradle": true, "graphql": true, "h": true, "hpp": true,
	"hs": true, "htm": true, "html": true, "ini": true, "java": true, "jpeg": true,
	"jpg": true, "js": true, "json": true, "jsx": true, "kt": true, "kts": true,
	"less": true, "lua": true, "markdown": true, "md": true, "php": true,
	"pl": true, "png": true, "proto": true, "ps1": true, "py": true, "rb": true,
	"rs": true, "rst": true, "scala": true, "scss": true, "sh": true, "sql": true,
	"svg": true, "swift": true, "tf": true, "tfvars": true, "toml": true,
	"ts": true, "tsv": true, "tsx": true, "txt": true, "xml": true, "yaml": true,
	"yml": true, "zsh": true,
}

// SaveCommand upserts a command in .smelt/config.json.
func SaveCommand(workDir, name, command string) error {
	cfg, err := LoadProjectConfig(workDir)
	if err != nil {
		cfg = &ProjectConfig{}
	}

	found := false
	for i := range cfg.Commands {
		if cfg.Commands[i].Name == name {
			cfg.Commands[i].Run = command
			found = true
			break
		}
	}
	if !found {
		cfg.Commands = append(cfg.Commands, Command{Name: name, Run: command})
	}

	return SaveProjectConfig(workDir, cfg)
}

// validateRepoRelative reports a configured path that is not relative or that
// leaves the repo. An empty path is unset and valid.
func validateRepoRelative(key, p string) error {
	if p == "" {
		return nil
	}
	clean := path.Clean(filepath.ToSlash(p))
	if path.IsAbs(clean) || filepath.IsAbs(p) || clean == ".." || strings.HasPrefix(clean, "../") {
		return fmt.Errorf("%s %q must be a relative path inside the repo", key, p)
	}
	return nil
}

// InstallCommand is the command that installs the project's dependencies: the
// detected install setup step, else the command named "install". Empty when
// there is none.
func (c *ProjectConfig) InstallCommand() string {
	if c == nil {
		return ""
	}
	if c.Environment != nil {
		if cmd := c.Environment.SetupStep(CmdInstall); cmd != "" {
			return cmd
		}
	}
	for _, cmd := range c.Commands {
		if cmd.Name == CmdInstall {
			return cmd.Run
		}
	}
	return ""
}

// ModelFor is the model configured for harness, or "" when none is.
func (f *FactoryConfig) ModelFor(harness string) string {
	if f == nil {
		return ""
	}
	return f.Models[harness]
}
