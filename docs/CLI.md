# CLI Command Tree

Complete command reference for the `smelt` CLI.

## Command Tree

```
smelt
├── auth
│   ├── login [codex]               # Log in to CircleCI via browser (recommended), or to Codex with your ChatGPT account
│   │   --no-browser                # Print the login URL instead of opening a browser
│   │   --device-auth               # With codex: sign in with a device code instead of a browser
│   ├── signup                      # Sign up for a new CircleCI account via browser
│   │   --no-browser                # Print the signup URL instead of opening a browser
│   ├── set <provider>               # Store credential (circleci | anthropic | anthropic-oauth | github | codex)
│   ├── refresh [provider]          # Re-mint the Claude subscription token (claude | anthropic-oauth; default claude) and restart a running daemon, or log in to Codex again (codex)
│   ├── status                      # Check authentication status (CircleCI, Anthropic, Codex, GitHub)
│   └── remove <provider>           # Remove stored credential (circleci | anthropic | anthropic-oauth | github | codex)
│
├── refresh [provider]              # Same as `auth refresh`
│
├── org                             # Manage CircleCI organizations
│   └── create <name>               # Create a new standalone CircleCI organization
│
├── review
│   └── patterns                    # Print the catalogue of review patterns (see "Review patterns")
│       --language <lang>           # Limit examples to a language, repeatable (python, go, ...)
│       --area <area>               # requirements | correctness | maintainability | testing
│       --json                      # Output the versioned JSON document
│
├── build-prompt                    # Mine PR comments → analyze → generate prompt
│   --org <org>                     # GitHub org (auto-detected from git remote)
│   --repos <items>                 # Comma-separated repo names
│   --top <n>                       # Top reviewers to include (default: 5)
│   --since <YYYY-MM-DD>            # Start date (default: 3 months ago)
│   --output <path>                 # Output path (default: .smelt/context/review-prompt.md)
│   --max-comments <n>              # Max comments per reviewer (0 = no limit)
│   --analyze-model <model>         # Model for analysis step
│   --prompt-model <model>          # Model for prompt generation step
│   --include-attribution           # Include reviewer attribution
│   --debug                  # Write intermediate files (details JSON, analysis, PR rankings CSV) for debugging
│
├── config
│   ├── show                        # Display resolved configuration
│   │   --json                      # Output as JSON
│   └── set <key> <value>           # Set a config value (see Config keys below)
│
├── init                            # Initialize project configuration
│   --force                         # Overwrite existing config
│   --skip-hooks                    # Skip hook file generation
│   --skip-validate                 # Skip validate command detection
│   --skip-completions              # Skip shell completion installation
│   --skip-skills                   # Skip agent skill installation
│   --skip-auth                     # Skip the coding agent login check
│   --harness <name>                 # Default coding agent for factory runs: claude, codex or grok; skips the "Coding agent" prompt
│   --skip-factory                  # Skip the factory backend step
│   --factory-backend <name>        # docker (default) or circleci; skips the prompt. Asked first: the circleci path alone detects the org ID, installs the smelt-sidecar skills and offers test-suites.yml
│   --factory-image <image>         # Docker image for factory runs (user-managed, never auto-built); skips the prompt
│   --skip-image-build              # Write Dockerfile.smelt for docker runs but don't offer to build the image
│   --factory-author "Name <email>" # Who factory commits are attributed to (default: your git identity)
│   --skip-test-suites              # Skip .circleci/test-suites.yml scaffolding (circleci backend only; default: true; pass =false to generate)
│   --project-dir <path>            # Project directory (defaults to cwd)
│
├── task
│   ├── run                         # Trigger a task run
│   │   --definition <name|uuid>    # Definition name or UUID (required)
│   │   --prompt <text>             # Prompt text (required)
│   │   --branch <branch>           # Branch override
│   │   --new-branch                # Create a new branch
│   │   --no-pipeline-as-tool       # Disable pipeline-as-tool mode
│   │   --json                      # Output as JSON
│   └── config                      # Set up .smelt/run.json for this repository
│       --force                     # Overwrite existing configuration without confirmation
│
├── harness
│   ├── status                      # Per agent: host and sandbox versions, installation, login, plan and usage
│   │   --json                      # Output as JSON
│   └── refresh                     # Pull and select sandbox agents without upgrading smelt
│       --kit-ref <image:tag>        # Published kit to select (default: this CLI's kit tag)
│
├── skill
│   ├── install                     # Install all skills
│   └── list                        # List skills and install status
│
├── quality                         # Measure erosion, complex functions and duplicated code (any language; in a container)
│   --base <rev>                    # Also compare with this revision; exit non-zero on medium findings
│   --json                          # Print the measurement as JSON
│   --project <dir>                 # Override project directory
│
├── mutate [path]                   # Find test coverage gaps via mutation testing
│   --parallel <n>                  # Sidecar pool capacity (0: enumerate only)
│   --max <n>                       # Limit the number of mutations (0: no limit)
│   --output <table|json>           # Enumeration output format
│   --test-cmd <command>            # Test command override
│   --test-timeout <duration>       # Per-mutation test timeout
│   --org-id <id>                   # CircleCI organization ID
│   --destroy-pool                  # Delete pool sidecars after the run
│
├── validate                        # Run validation commands
│   [name]                          # Optional: run a specific named command
│   --dry-run                       # Print commands without executing
│   --list                          # List all configured commands
│   --json                          # Output as JSON (only applies with --list)
│   --cmd <command>                 # Run an inline command
│   --save                          # Save --cmd to config
│   --remote                        # Run using the active sidecar pool
│   --parallel <n>                  # Max sidecars for remote commands (0: auto, up to 3)
│   --mark-remote                   # Mark [name] (or all commands) remote in config, then exit
│   --sidecar-id <id>               # Remote execution in specific sidecar
│   --org-id <id>                   # Organization ID (used when creating a new sidecar)
│   --workdir <path>                # Working directory on sidecar
│   --project <path>                # Override project directory
│   -e / --env KEY=VALUE            # Set env var in remote sidecar session (repeatable)
│   --env-file <path>               # Env file to load (default: .env.local; pass a path to override)
│   --async                         # Run in the background via the smelt daemon; results are read later
│   │
│   ├── results                     # Print results of finished background runs, then forget them
│   │   --project <path>            # Override project directory
│   │
│   └── variants <variants-file>    # Run code variants on a temporary sidecar pool
│       --name <command>            # Validate command to run (default: all remote commands)
│       --parallel <n>              # Maximum pool capacity (default 5)
│       --timeout <seconds>         # Per-command timeout when the command sets none (0 for no limit)
│       --org-id <id>               # Organization ID
│       --image <id>                # Snapshot ID, or Docker image with --backend docker (default: validation.sidecarImage, or factory.image for docker)
│       --workdir <path>            # Remote working directory
│
├── sidecar
│   ├── list                        # List sidecars
│   │   --org-id <id>               # Organization ID
│   │   --all                       # List all sidecars in the org (requires org admin)
│   │   --json                      # Output as JSON
│   ├── create                      # Create a sidecar
│   │   --org-id <id>               # Organization ID (see Org ID resolution)
│   │   --name <name>               # Sidecar name (auto-generated if omitted)
│   │   --image <image>             # E2B template ID or container image
│   ├── use <id>                    # Set the active sidecar for this project
│   ├── current                     # Show the active sidecar
│   │   --json                      # Output as JSON
│   ├── forget                      # Clear the active sidecar
│   ├── exec                        # Execute command in sidecar
│   │   --sidecar-id <id>           # Sidecar ID (defaults to active sidecar)
│   │   --command <cmd>             # Command to run (required)
│   │   --args <args>               # Command arguments
│   ├── logs <command-id>           # Print output of a command that ran on a sidecar
│   │   -f / --follow               # Keep printing until the command exits
│   ├── add-ssh-key                 # Add SSH key to sidecar
│   │   --sidecar-id <id>           # Sidecar ID (defaults to active sidecar)
│   │   --public-key <key>          # SSH public key string
│   │   --public-key-file <path>    # Path to public key file
│   ├── ssh                         # SSH into sidecar (stdin forwarded when piped)
│   │   --sidecar-id <id>           # Sidecar ID (defaults to active sidecar)
│   │   -e / --env KEY=VALUE        # Set env var in remote session (repeatable)
│   │   --env-file <path>           # Env file to load (default: .env.local; pass a path to override)
│   ├── sync                        # Sync files to sidecar
│   │   --sidecar-id <id>           # Sidecar ID (defaults to active sidecar)
│   │   --workdir <path>            # Destination path on sidecar (auto-detected when omitted)
│   │   --checkout                  # Sync via git checkout/patch instead of bundle (requires branch pushed to GitHub)
│   ├── env                         # Detect tech stack and print environment spec as JSON
│   │   --dir <path>                # Directory to analyse (default: .)
│   │   --no-save                   # Print only, do not save to .smelt/config.json
│   ├── build                       # Generate Dockerfile and build test image from env spec
│   │   --dir <path>                # Directory to write Dockerfile.test and build from
│   │   --tag <tag>                 # Image tag (e.g. myapp:latest)
│   ├── setup                       # Detect env, sync files, and run install steps
│   │   --dir <path>                # Directory to detect environment in (default: .)
│   │   --sidecar-id <id>           # Sidecar ID (defaults to active sidecar)
│   │   --org-id <id>               # Organization ID (used when creating a new sidecar)
│   │   --name <name>               # Sidecar name (used when creating a new sidecar)
│   │   --skip-sync                 # Skip syncing files to the sidecar
│   │   --force                     # Re-detect environment even if cached
│   │   -e / --env KEY=VALUE        # Set env var in remote sidecar session (repeatable)
│   │   --env-file <path>           # Env file to load (default: .env.local; pass a path to override)
│   └── snapshot
│       ├── create                  # Snapshot a sidecar, then delete the source sidecar
│       │   --sidecar-id <id>       # Sidecar ID (defaults to active sidecar)
│       │   --name <name>           # Snapshot name (required)
│       ├── get <snapshot-id>       # Get a snapshot by ID
│       │   --json                  # Output as JSON
│       └── list                    # List snapshots
│           --org-id <id>           # Organization ID
│           --json                  # Output as JSON
│
├── factory                         # Build and manage factory runs
│   ├── build [prompt|-]            # Implement a prompt on a sidecar, then review and validate it until it passes
│       --plan <file>               # Read the prompt from a file (usually a markdown spec)
│       --backend <name>            # docker or circleci (default: factory.backend, else docker)
│       --max-attempts <n>          # Maximum rounds of review and validation (default: factory.maxAttempts, else 10)
│       --from <ref>                # Start from a branch or commit instead of your files (not a config key; not with --continue)
│       --integrate                 # With --from: check the finished branch against the whole spec (the prompt) and fix only what fails
│       --against <ref>             # With --integrate: what the branch is compared with (default: the default branch)
│       --continue <run id|branch>  # Pick up an earlier run; its ID is the branch's final segment
│       --reviewers <n>             # Reviewer sidecars (0: one per review prompt)
│       --reviews <dir>             # Directory of review prompts (default: .smelt/reviews)
│       --no-validate               # Skip the project's validation commands
│       --no-plan                   # Skip the reviewed plan the implementer writes before it implements (default: factory.plan, else on)
│       --no-pack                   # Skip the context pack built by the plan turn (default: factory.pack.enabled, else on)
│       --keep-sidecars             # Leave the sidecars running when the run ends
│       --push                     # Push the run's branch when it ends, without asking
│       --pr                       # Push and open a PR (or update the branch's PR), without asking
│       --no-publish               # Never offer to push or open a PR
│       --base <branch>            # Branch the PR targets (default: branch you started from, else the default)
│       --pr-title <t>             # PR title (default: the prompt's first line)
│       --pr-body <b>              # PR description (default: What / Why / How notes)
│       --draft                    # Open the PR as a draft
│       --org-id <id>               # Organization ID
│       --image <id>                # Snapshot ID, or Docker image with --backend docker (default: validation.sidecarImage, or factory.image for docker)
│       --sandbox-dockerfile <path> # Build the toolchain from a Dockerfile anywhere on this machine; the repo is still the build context (docker backend; not with --image)
│       --harness <name>            # Coding agent for the implementer and reviewers: claude, codex or grok (default: factory.harness, else ask in a terminal; fails without one)
│       --model <name>              # Model for the chosen agent (default: factory.models.<harness>, else the agent's own default)
│       --implement-timeout <duration> # Max time for each implementer turn
│       --review-timeout <duration> # Max time for each review
│       --log                       # Keep a log of the run's full context in ~/.smelt/factory/run-<start time>.log
│       --log-file <path>           # Keep the log in this file instead (implies --log)
│       --verbose                   # Log review prompts, passing output and reviewer checks, and show the log here (implies --log)
│       --detach                    # Start the run and return at once; it continues on the daemon (not with --pr/--push)
│       --json                      # Output as JSON (with --detach: {"run_id","session_id","branch","worktree"})
│   ├── list                        # List the project's factory runs and their worktrees
│   │   --json                      # Output as JSON (each entry has run_id, the newest, and state)
│   ├── status [run-id]             # A run's state, rounds, reviews, result and cost (default: the newest run)
│   │   --json                      # Output as JSON: the daemon's session, or the record with "state":"done"
│   ├── stop <run-id>               # Stop a running run; the work so far is committed. Waits for the run to end (grace + 5 s at most). A finished run is reported, not an error
│   ├── publish <run-id>            # Push a finished run's branch, or open or update its PR
│   │   --push | --pr               # Mode; without either, asks in a terminal
│   │   --base <branch>             # Branch the PR targets
│   │   --pr-title <title>          # PR title
│   │   --pr-body <text>            # PR description
│   │   --draft                     # Open the PR as a draft
│   ├── update <run-id>             # Merge the run's base branch into its branch (conflicts are committed with their markers)
│   │   --base <branch>             # Branch to merge (default: the run's base, else the default branch)
│   │   --push                      # Push the branch after a clean update
│   │   --json                      # {"run_id","branch","base","result","merged_commits","conflicts"}
│   ├── clean [run...]              # Remove runs' worktrees, branches and records (default: every run not running)
│   │   --yes                       # Remove without asking
│   │   --force                     # Also remove worktrees with uncommitted changes, discarding them
│   │   --dry-run                   # List what would be removed and stop
│   │   --cache                     # Remove the project's docker cache volumes (smelt-cache-<hash>-<uid>, smelt-gomod-<hash>-<uid>, every uid, and older ones without a uid) instead of runs
│   ├── author ["Name <email>"]     # Show or set who factory commits are attributed to (factory.author)
│   │   --unset                     # Remove factory.author, so each person commits as themselves
│   └── image                       # Build the Docker image factory runs execute in (factory.dockerfile)
│       --sandbox-dockerfile <path> # Build the toolchain from a Dockerfile anywhere on this machine; the repo is still the build context (never repaired or regenerated)
│       --rebuild                   # Build even when the image exists, without the layer cache
│       --regenerate                # Render Dockerfile.smelt again, show the diff, confirm before writing
│       --kit-from-source           # Force rebuilding the agent kit from embedded sources
│       --update-kit                # Retired: prints a notice and does nothing (smelt adds the kit itself)
│       --yes                       # Retired with --update-kit; does nothing
│
├── watch [dir...]                  # Live TUI dashboard for active pools and recent activity
│   --focus                         # Watch only the current directory instead of all known projects
│
├── daemon                          # Inspect and restart the smelt daemon (local daemon only)
│   ├── status                      # All local daemons: PID, revision, active runs
│   │   --json                      # Print [{pid, revision, draining, active_runs, socket}, ...]
│   ├── restart --when-idle         # Hand over the listener while existing runs finish
│   │   --timeout <duration>        # Legacy fallback: cancel draining if runs remain after this long (default: no limit)
│   └── stop                        # Stop the daemon unless a factory run is active (or that can't be told)
│       --force                     # Stop it anyway, cancelling active runs
│
├── conflicts                       # Report whether this branch still merges cleanly into its merge target
│   --project <path>                # Override project directory
│   --hook                          # Emit Claude Code hook JSON so the notice reaches the agent
│   --json                          # Print the raw conflict report as JSON
│
├── hook                            # Manage smelt hook execution
│   --project <path>                # Override project directory
│   ├── disable                     # Disable smelt validate hooks
│   ├── enable                      # Re-enable smelt validate hooks
│   └── status                      # Show whether hooks are enabled or disabled
│
├── completion
│   ├── install                     # Install zsh completion
│   └── uninstall                   # Remove zsh completion
│
└── upgrade                         # Update a release install to the latest release (see Behavior Decisions)
```

## Behavior Decisions

- `auth login` and `auth signup` both use OAuth and store the resulting token in the system keychain (or `~/.config/smelt/config.json` with `--insecure-storage`). They differ only in which page the browser opens: login for existing accounts, signup for new ones. Use `--no-browser` to print the URL instead of opening it automatically.
- `auth signup` fails with a user-friendly error if a CircleCI token is already stored; run `smelt auth remove circleci` first to clear it. Existing accounts should use `smelt auth login`.
- After a successful `auth signup`, smelt checks whether the new account belongs to any organization. If it belongs to none, smelt prompts for a name and creates a standalone org, so the first sidecar command does not fail on having none. Without a terminal it prints a warning pointing at `smelt org create <name>` instead; the signup itself still succeeds.
- Commands that resolve an org (sidecar, validate, init, mutate, prune) offer the same org creation prompt when the account has no organizations. Without a terminal, or with `CI` set, sidecar, validate, mutate and prune fail with a suggestion to run `smelt org create <name>` or pass `--org-id`; `init` prints a warning and finishes without writing an org ID.
- `init` ends with a Claude credential step: it reports the credential in use, or (interactively) offers to run `claude setup-token` and store the token in the keychain. Without a terminal, with `CI` set, or with `--insecure-storage` it only prints a hint. It also runs when `.smelt/config.json` already exists. A failed or declined login is never fatal. The token is never written to `.smelt/config.json`.
- `auth refresh` / `refresh` always replace the stored token without a confirmation, warn when `CLAUDE_CODE_OAUTH_TOKEN` or an API key overrides it, and restart a running smelt daemon. `auth set anthropic-oauth` and `auth remove anthropic-oauth` restart it too.
- `org create` is hidden from the default help output. It requires CircleCI authentication and creates a standalone org (not tied to a VCS provider), printing the org name, ID, and slug on success.
- `build-prompt` auto-detects org and repos from the git remote when flags
  are omitted. If `--org` is provided explicitly, `--repos` is required.
- `build-prompt --output` creates parent directories automatically.
- `build-prompt --since` defaults to 3 months before the current date.
- `build-prompt` does not write intermediate files by default. Pass `--debug` to write the raw details JSON, analysis markdown, and PR rankings CSV alongside the prompt — useful when diagnosing unexpected prompt output.
- `task run` defaults to pipeline-as-tool mode; use `--no-pipeline-as-tool`
  to disable.
- `config set` user keys: `model`, `telemetry`, `notifications`. Project keys (`.smelt/config.json`):
  `orgID`, `validation.sidecarImage`. Credentials use `smelt auth set`, not `config set`.
- **CircleCI is opt-in, and a CircleCI token is not consent.** smelt contacts
  CircleCI only for a project with `"factory": {"backend": "circleci"}` in
  `.smelt/config.json` (or a request that names `--backend circleci`), and for
  the explicitly CircleCI commands: `auth`, `sidecar`, `task`, `org`, `prune`,
  `build-prompt`'s CircleCI parts, `review`, `mutate`. An empty backend means
  `docker`. Having `CIRCLE_TOKEN`, a keychain entry or an `orgID` in the config
  never causes traffic by itself.
- Without the circleci backend, every validation command runs in the local
  working tree, `remote: true` is ignored, and `smelt validate`, the git
  pre-commit hook, the agent hooks and the daemon's background validation never
  create, sync to or exec on a CircleCI sidecar. A dim note ("running lint
  locally; remote validation needs the circleci backend") is printed once per
  run for commands marked remote. `validate --remote`, `validate variants` and
  `--mark-remote` are the exceptions: `--remote` and `variants` fail with an
  error explaining the opt-in rather than silently falling back, and
  `--mark-remote` (like `smelt sidecar setup`) opts the project in by setting
  `factory.backend` to `circleci`.
- On the circleci backend, validation commands run remotely by default. Set
  `local: true` on a command to run it in the local working tree. `remote: true`
  remains an explicit, backwards-compatible annotation; setting both fields is
  invalid.
- `validate --mark-remote` sets `remote: true` and clears `local: true` on commands
  in `.smelt/config.json`, then exits. With a `[name]` it marks that one command;
  without it every configured command **except `role: autofix`** ones, which it
  names as skipped — a formatter that runs on the sidecar rewrites files there and
  the edits never reach the local working tree. Naming an autofix command marks it
  anyway, for the caller who means it. Commands already marked are reported as no
  change. `smelt sidecar setup` marks install and gate commands automatically, so
  `--mark-remote` is for the rest: a sidecar set up by hand, or a command whose
  role does not qualify. Unmarking is still a hand edit of the config.
- On the circleci backend, command placement is independent of `validation.sidecarImage`: explicit local
  commands stay local, while unspecified and explicitly remote commands use the
  managed sidecar pool. `--local` remains the whole-run local override.
- A run of several commands starts with the local `role: autofix` ones, then the
  remote batch, then the remaining local commands, keeping configured order
  within each. The remote batch syncs the working tree when it starts, so a
  formatter has to finish first for its rewrites to reach the gates.
- **Within the remote batch, independent commands run on separate sidecars, not
  one after another on a single one.** A batch with several remote commands
  (`test`, `lint`, `acceptance-test`, ...) uses up to `defaultValidateParallelism`
  (3) sidecars at once by default, each pulling the next unstarted command from
  the list, matching how `mutate --parallel` and `validate variants --parallel`
  already spread work across a pool. `--parallel <n>` overrides the cap in
  either direction; `PlanCommands` still clamps it to the number of remote
  commands actually selected, so a single-command run never provisions more
  than one. Sidecars a run creates to fill out that pool are kept for reuse by
  later runs rather than torn down when a smaller run only needs one of them.
  Recreating a sidecar costs a full initial sync, so shrinking on every small
  run would make the common case pay for the parallel one.
- **Snapshot selection.** When a sidecar has to be created and no
  `validation.sidecarImage` is recorded (project-level or per-command), `smelt`
  picks one of the org's snapshots instead of booting the bare default image.
  A snapshot matches on its name and tag, split into tokens: the repo name
  (from the git remote, then `vcs.repo`, then the directory name) outranks the
  detected stack, and the org's own snapshot outranks an equivalent system one.
  Matching is token-based, so `go` matches `go-base` but not `mongo-api`. When
  nothing matches, the default image is used and the reason is printed —
  guessing the wrong prepared environment produces failures that look like the
  repo's own. Selection never fails a run: an unreachable snapshot API is
  warned about and treated as no match.
- **`upgrade` follows this fork's releases.** It reads the latest release of
  `smeltdev/smelt` (the one place the repository is named is
  `internal/upgrade`), downloads `smelt_<Os>_<arch>.tar.gz` and verifies it
  against that release's `checksums.txt` before replacing the binary; a missing
  or mismatched checksum aborts with the installed binary untouched. It refuses
  on a source build (`version` is `dev`, or the binary is a symlink as
  `task dev-install` makes) and says to `git pull && task build`, and on a
  Homebrew-managed binary (that is CircleCI's build) and points at the install
  script. Stop the daemon afterwards. The update notice and `smelt watch`'s
  footer use the same source and are silent for `dev` builds.
- Telemetry is opt-out, and off unless the binary was built with a Segment
  write key (`-X .../internal/telemetry.WriteKey=...`; release builds set it
  only if the `SMELT_TELEMETRY_WRITE_KEY` secret exists). Without a key nothing
  is created or sent whatever the preference says, and `smelt config show` says
  so. When a key is present: events are anonymous until you authenticate with
  CircleCI; after that they include your CircleCI user ID, and earlier
  anonymous events from the same install are linked to it. They never include
  flag values, argument values, names or email addresses. It's disabled by the
  `SMELT_NO_TELEMETRY` / `NO_ANALYTICS` / `DO_NOT_TRACK` / `CI` environment
  variables (first match wins, in that order), or `smelt config set telemetry false`.
- **Org ID resolution** for `sidecar create`, `sidecar list`, and other sidecar
  subcommands that need an org (in order): `--org-id` flag → `CIRCLECI_ORG_ID`
  env var → `orgID` in `.smelt/config.json` → interactive org picker (TTY only).
  Non-interactive sessions (agents, CI) should set `orgID` in project config or
  pass `--org-id` / `CIRCLECI_ORG_ID`.
- **`conflicts` is advisory and always exits 0.** It reports what the watch
  daemon already computed and never previews a merge itself, so it returns
  immediately. It is deliberately incapable of failing: it runs from the same
  commit hook group as `smelt validate`, whose non-zero exit blocks a commit,
  and a merge conflict with the default branch is information rather than a
  reason to stop. With no daemon running, `--hook` prints nothing at all —
  the daemon is optional, and announcing its absence on every commit would be
  noise — while a manual run says why there is no answer.
- **`conflicts --json` always emits one shape.** Output is a `ConflictReport`
  whether or not there is an answer, so `.known` is always present and a
  consumer never has to tell a report apart from an error object. `.conflict` is
  a pointer and is omitted in one case, so a script must guard it before
  dereferencing:

  | | `known` | `conflict` |
  |---|---|---|
  | No daemon, or a socket it cannot reach | `false` | reason in `conflict.unavailable` |
  | Daemon up, project not tracked | `false` | reason in `conflict.unavailable` |
  | Daemon up, tracked, no check has run yet | `true` | **absent** |
  | Tracked and checked, but not comparable | `true` | reason in `conflict.unavailable` |
  | Tracked and checked | `true` | `conflict.conflicted` is the answer |

  So `known: false` always carries its reason in `conflict.unavailable`, and the
  only report without a `conflict` object at all is the tracked project whose
  first check has not finished. `conflict.unavailable` is the one field for
  every "there is no answer, and here is why": no daemon, an untracked project,
  a detached HEAD, an unfetched target, or a branch that is its own merge
  target.
- **What `conflicts` compares.** The daemon previews merging each project's
  branch into the default branch on `origin`, falling back to `upstream`
  (`DefaultRemoteBranchIn`), re-checking every 60s and refreshing the target's
  remote-tracking ref every 3 minutes. The comparison covers **committed
  history only** — uncommitted work in the tree is invisible to it. A detached
  HEAD, a repo with no recorded default branch, and a branch that is itself the
  merge target all produce "no answer" rather than a clean bill of health.
- **`watch` can show a remote daemon.** With `SMELT_DAEMON_REMOTE_ADDR` and
  `SMELT_DAEMON_TCP_TOKEN` set, `watch` reads the daemon at that TCP address instead
  of the local socket, and never starts a local daemon. The header names the
  daemon (`local` or `remote host:port`) and turns red with `unreachable` when the
  last poll failed; a rejected token says so and names `SMELT_DAEMON_TCP_TOKEN`.
  The local machine's working directory and project registry describe different
  repos than the daemon's, so they are not used: the default is every project the
  daemon tracks, and `watch <path>` / `--focus <path>` take paths **on the daemon's
  host**, sent as typed. `--focus` with no path is an error in this mode.
- **`factory build` takes defaults from the `factory` section of `.smelt/config.json`.**
  `{"factory": {"backend": "docker", "image": "smelt-sandbox:local", "maxAttempts": 5, "log": true}}`.
  Precedence, highest first: an explicitly set flag, then `factory.*`, then the built-in default
  (circleci, 10 attempts, no log). An explicit `--max-attempts 3` beats `factory.maxAttempts`. The docker
  backend uses `--image` or `factory.image` and never `validation.sidecarImage`, which is a CircleCI
  snapshot ID; with neither (and no `SMELT_DOCKER_IMAGE`) the command fails before the run starts.
- **A round is plan → implement → gates → reviews.** Before round 1 the implementer writes a plan
  (schema: summary, files, acceptance, risks, questions, `cannot_verify_here`) with read-only tools and a
  5-minute timeout; the `requirements` review (and `adversarial`, when the project has it) checks the plan
  against the request. High or medium findings send the plan back for one revision; if the revision is still
  found wanting the run goes ahead, with the findings in front of the implementer. The approved plan is
  prepended to the implementation prompt and the PR-notes prompt, and is in `factory status --json` as
  `factory.plan`, in the run log and in the run's record. `cannot_verify_here` items seed the implementer's
  `UNVERIFIED:` lines. `--no-plan` or `factory.plan: false` skips it; `--continue` never plans.
- **The plan turn also scouts: the context pack.** The plan carries `pack` entries (`edit`, `reference`,
  `test_pattern`, `rule`, `new_file`; path, 1-based range, reason, priority 1–3) and `rejected_hints`. smelt
  validates each against the run's baseline commit (`git cat-file blob <baseline>:<path>`): entries whose file or
  range does not exist are dropped and logged (`pack: dropped internal/x.go:900-950 — beyond end of file (612
  lines)`), and `reference`/`test_pattern` excerpts are read from the commit, never from the model's answer.
  `edit` entries and `new_file` entries stay pointers; excerpts are inlined in the implementer's prompt (after
  the plan, as `## Context pack`) and the reviewers' prompts (after the request). Size is estimated at 4
  characters per token; over `factory.pack.maxTokens` (default 25000) excerpts become pointers, priority 3
  first. The request may have a `## Context` section of author hints (bullets or lines until the next
  heading); each must appear in the pack or in `rejected_hints`, and a miss is a medium plan-review finding
  (one revision, as for any plan finding). The plan review also asks whether every acceptance item has its
  code in the pack. An empty or failed pack never fails the run (`pack: none, continuing with the plan
  alone`). `factory status --json` has `factory.pack: {entries, dropped, rejected_hints, tokens}`;
  `--continue` reuses the recorded pack, re-anchoring ranges that moved by their first line, else pointing
  to the whole file. `--no-pack` or `factory.pack: {"enabled": false}` skips it; `factory.pack.maxTokens`
  sets the cap.
  `factory.gatesFirst` (default `true`) runs the validation commands first; if any fails, the round's reviews
  are reported as `skipped` (`gates failed`) and only the failures are fed back. `gatesFirst: false` runs them
  side by side. A skipped round still counts towards `--max-attempts`.
  `factory.focusedRechecks` (default `true`) makes rounds after the first work like an integration run's
  second pass: a review whose last round had no high or medium findings is carried forward (status `mode:
  "carried"`) and not run again, a review that had findings re-checks only those (`mode: "recheck"`), and one
  that errored or was skipped gets a full pass (`mode: "broad"`). A re-check's new medium finding that matches
  none of the review's earlier findings (same file, within 5 lines) and lies on no line changed since the tree
  that review last saw is downgraded to low (`downgraded: true` and `downgraded_reason` in status, a line in
  the run log). High findings are never downgraded. Downgraded findings are listed under "Low findings" in
  the PR body and commit message, across all rounds. `focusedRechecks: false` restores full reviews every
  round that are only asked to hold back.
- **Changed-command templates.** A project may add an optional `changed` string next to
  `run` in a validation command, for example
  `{"name":"test","run":"go test -race ./...","changed":"go test -race {dirs}","role":"gate"}`.
  Factory rounds run this variant instead of `run`. Standalone validation keeps using `run`.
  `{files}` expands to sorted repository-relative Git paths, each shell-quoted.
  `{dirs}` expands to their sorted distinct directories, each shell-quoted and prefixed
  with `./`; a file at the repository root contributes `./`. Unknown identifier
  placeholders such as `{pkgs}` are rejected; shell brace expressions remain available.
  Paths include all changes since the previous completed check snapshot (the factory
  baseline in round one), including deletions and both sides of renames. There is no
  language detection or path filter: docs-only edits also supply paths. Projects own
  tool-specific filtering and handling of deleted paths. Snapshots include gate edits,
  so those edits are not attributed to the next implementation round.
  With no changed paths, the variant produces no check and runs no command.
  After all gates, reviews, and quality checks allow a round to pass, factory runs the
  full `run` of every command declaring `changed`, even with no changed paths.
  Full failures, timeouts, and SARIF ratchet failures fail the round with normal
  output and feedback; a later candidate-passing round retries the full commands.
  Both phases count as gate time. Logs and status retain labels such as
  `test (changed: 3 dirs)` and `test (full)`. Commands without `changed` retain
  their names and run once per round.
  `smelt init` does not invent templates. Pilot language packs may provision them
  through Pilot's `--overlay`; smelt's factory build overlay does not merge project config.
- **Ratchet gates (`format: "sarif"`).** A gate whose command prints SARIF 2.1.0 fails a factory run only on
  findings the change introduced, so existing debt never blocks a run and no run adds to it, for any linter:

  ```json
  {"name": "lint", "run": "ruff check --output-format sarif .", "role": "gate",
   "format": "sarif", "timeout": 180}
  ```

  `format` is `""` (the default: the exit code decides) or `"sarif"`. `sarifPath`, valid only with
  `format: "sarif"`, is a repo-relative file the tool writes instead of stdout; smelt reads it after each
  run and deletes it, so it never ends up in the change. Tools that can print SARIF: ESLint
  (`--format @microsoft/eslint-formatter-sarif`), Ruff (`--output-format sarif`), golangci-lint
  (`--output.sarif.path`, with `sarifPath`), Semgrep (`--sarif`), clippy (via `clippy-sarif`).
  - *Baseline.* In `Sidecars.Prepare`, right after the baseline commit and before the implementer edits
    anything, the command runs once in the repo (so installed dependencies are there) and its findings are
    kept for the run. If that fails the first round makes them from a temporary worktree of the baseline
    commit; if that fails too the gate is `errored` and blocks the round.
  - *Each round* the command runs again on the change. The exit code is ignored (linters exit non-zero when
    they find anything); a timeout or a failure to run is handled as for any gate. Only stdout is parsed (or
    `sarifPath`); stderr is kept for the failure message. Output over 64 MiB fails the gate. A command that
    prints no valid SARIF fails the gate (`failed`, fed back) with the parse error and the tail of stderr.
    Results a tool marks as suppressed are ignored.
  - *Matching* (`internal/sarif`, pure): findings with `partialFingerprints`/`fingerprints` match the
    baseline finding sharing one. Otherwise a finding is identified by rule ID, file, the trimmed text of the
    flagged line and its occurrence among findings sharing those, so a finding that only moved lines is still
    the same. A finding with no line (or no readable file) matches by rule, file and occurrence alone.
  - *Result.* The gate fails when the change has findings with no match in the baseline, and the feedback
    lists only those, as `file:line rule message`. Otherwise it passes with the note
    `lint: 0 new, 3 fixed, 214 existing`. In the status JSON each such check in a round's `checks` carries
    `"ratchet": {"new": 0, "fixed": 3, "existing": 214}`.
  - Only factory runs ratchet. `smelt validate` keeps the exit-code behaviour for these commands.
- **The implementer runs quick checks; smelt runs the gates.** Its prompt asks it, after its last edit, to
  build and run the tests of the packages it changed (for Go `go build ./...` and `go test ./<changed
  packages>/...`) and not to run the full gates itself: smelt runs them right after the turn and sends back any
  failure, so a round does not pay for them twice. The prompt lists the project's `role: gate` commands under
  "smelt runs these after your turn". A command may set `quick`, free text shown as that gate's quick check
  (`"quick": "go test ./<package>/... for each package you changed, without -race"`); smelt never runs it.
  A gate whose configured `timeout` is 60 seconds or less is quick enough to be run in full after the last
  edit, and the prompt says so. Long commands run in the foreground: the prompt says not to background them
  and poll, and for Claude Code the turn sets `BASH_DEFAULT_TIMEOUT_MS` (the longest gate timeout plus 2
  minutes, at least 10) and `BASH_MAX_TIMEOUT_MS` (the implementer timeout), so one tool call can outlast a
  full test run. Codex has no equivalent setting (its shell tool takes a timeout per call, set by the model),
  so it gets the prompt line alone. If the summary claims passing tests, lint or gates but the turn's activity
  shows no gate (its command, or the `task` target that wraps it) was run after the last edit, that is noted on
  the round (`unproven_gates`, the log and the status line) and nothing more happens: the round's own gates
  decide. Only `role: gate` commands count, never `install` or other setup commands. Tests a gate's output
  reports as `SKIP` (`go test -v`, gotestsum) are listed in the gate's detail, as `skipped_tests` in
  `factory status --json`, and in the PR notes under "Skipped in the sandbox".
  Grok: a bounded `grok --version` probe checks the host; kit v5's harness label
  includes Grok in the image. `XAI_API_KEY` takes precedence (`source: "api_key"`).
  Otherwise `~/.grok/auth.json` supplies `source: "oauth"` and RFC3339 `expires_at`:
  the latest readable expiry strictly after now is `ok`, otherwise `expired`
  with an instruction to run `grok login`. Unreadable sibling expiries are skipped;
  no readable expiry or credential is `missing`. Status never renews credentials.
  Grok exposes session costs (`total_cost_usd`, and `grok usage` per session),
  but no published rate-limit windows, so `rate_limits: []` stays empty, including
  when a run recorded a signal. No daily cost aggregate is reported: smelt's
  persisted implementer-turn costs do not cover all Grok turns.
- **The docker backend keeps a warm cache.** Every sidecar of a project mounts the named volume
  `smelt-cache-<project hash>-<uid>` at the image user's `~/.cache` (Go's build cache lives there), and, for a Go
  project (`environment.stack: go`), `smelt-gomod-<project hash>-<uid>` at the Go module cache (`GOMODCACHE`, else
  `GOPATH/pkg/mod`, else `~/go/pkg/mod`). The home directory comes from the image. The volumes outlive
  containers and runs, so a second run's gates start warm. `factory.cache: false` turns both off;
  `smelt factory clean --cache` removes them for every uid, and older volumes without a uid, which runs no
  longer use (a volume in use by a running sidecar is kept). The `<uid>` is the image's runtime user, read once
  per image with `id -u`, so a project that switches to an image with another user gets fresh caches instead
  of files it cannot write. At container start a self-check looks for cache content the user does not own
  (to a depth of three) and for a mount point it cannot write; if it finds either, one warning is logged and
  `GOCACHE`, `GOMODCACHE`, `UV_CACHE_DIR` and `XDG_CACHE_HOME` point at `/tmp/smelt-cache-<uid>` in the
  container for that member's commands, so the run goes on, with a cold cache. The CircleCI backend is
  unaffected.
- **Cache limits and disk safety (Docker only).** `factory.cache` accepts the old boolean or
  `{"enabled":true,"maxGB":10}`. `maxGB` caps each volume independently; `factory.minFreeGB`
  defaults to 10 too. Both use decimal GB and must be positive. Before mounting a workload,
  smelt measures each volume with `du` in a throwaway container, at most hourly, with successful
  measurements persisted by engine and volume identity. Processes coordinate maintenance and mounting.
  A recreated volume is measured again. Above its cap, the home cache runs `go clean -cache`,
  `uv cache prune`, and `golangci-lint cache clean` against explicit paths in that order, stopping
  when the target is reached, then evicts remaining entries by access time (ties by path) to 70%
  of the cap. Module volumes use access-time eviction only. Missing cache tools are reported;
  trimming requires Python 3, `du` and a shell in the workload image. Symlinks are never followed
  outside the cache, and mount roots are retained. Measured bytes freed are logged. Trimming
  refuses volumes held by another container; stop its runs before retrying.
  Hourly pre-mount maintenance is not a live quota: an active build can exceed the cap or fill a disk.
- `factory clean --caches [--project <12-character project hash>]` removes all recognized factory
  caches, or exactly the selected project's, including legacy names. Singular `--cache` retains
  current-project selection. Both support `--yes` and `--dry-run`, confirmation, and non-forced
  Docker volume removal; attached volumes are refused. They accept no run arguments or `--force`.
- `factory image` and `factory doctor` share a storage report: volume size, effective configured cap,
  measurement age (or explicit unavailable status), endpoint, daemon root, and backing-storage evidence.
  The current project uses its configured cap; other projects show their last measured policy
  (or the default when no measurement is available).
  Before image work or provisioning, smelt checks the project, workspace, Docker filesystem and
  the host filesystem holding Docker's backing storage. Any measured filesystem below `minFreeGB`
  refuses startup with `disk almost full: <free> free; run smelt factory clean --caches`.
  A local Linux engine uses its inspected Docker root. Local Docker Desktop on macOS/Linux uses
  `settings-store.json` (legacy `settings.json`) and its configured `dataFolder`/`DataFolder` to
  validate `Docker.raw` or `Docker.qcow2`, resolving symlinks before checking the containing
  filesystem. Relocated images are honored; smelt never substitutes the project filesystem.
  Missing, inaccessible, ambiguous or unsupported storage discovery doesn't stop a run: it
  prints one `⚠ couldn't check Docker's free disk space (<reason>)` warning, still checks the
  project, workspace and Docker VM filesystems, and `factory doctor` reports the failure. When macOS
  denies reading Desktop's settings, `factory doctor` advises granting Full Disk Access to the app
  running smelt. Remote engines and Windows
  Desktop are unsupported by host storage discovery. Docker's filesystem probe uses
  `alpine:3.21`, pulling this small helper when absent after the host checks pass. This
  supports an empty daemon and does not depend on workload images providing a shell.
  Offline, load that image with `docker load` before retrying.
  Desktop references: [settings files](https://docs.docker.com/desktop/settings-and-maintenance/settings/),
  [disk image locations](https://docs.docker.com/desktop/settings-and-maintenance/backup-and-restore/),
  [legacy settings example](https://github.com/docker/enable-macos-virtualization-framework/blob/main/settings.json).
- Ordinary factory containers are removed through pool teardown on completion, failure or cancellation;
  `--keep-sidecars` explicitly retains them. Verification and storage helpers have cancellation-independent,
  bounded cleanup. Deletion errors can still leave containers behind; this does not sweep unrelated orphans.
- `factory status --json` lists the implementer's tool calls as `factory.activity`: `{round, tool, detail, outcome, exit_code, error}`, newest last, the latest 500. `outcome` is `ok`, `failed` or `""` when the agent does not say; `exit_code` and `error` (the first line of the output, up to 200 characters) are set for failed calls where the agent reports them (claude and codex).
- **Where the time went.** At the end of a run `factory build` prints, the run log records and
  `factory status --json` carries (as `factory.timing`, in milliseconds, with the `line`) one line:
  `Time: plan 1m02s · implement 3m40s (commands 1m10s) · gates 0m52s · reviews 0m21s · notes 0m15s · total 6m10s`.
  `commands` is the implementer's time inside tool calls, taken from the activity stream (the gap between a
  call and the next activity, so a slight overestimate). Any single tool call over 2 minutes is logged as a
  warning with its command. `timing.gate_only_rounds`, and `gates_only_failed` on a round, name the rounds that
  failed on gates alone.
- **Peak context per turn.** When any turn's size is known, a second line follows the `Time:` line, e.g.
  `Context: implementer peak 88k (round 1), 52k (round 2) · reviews peak 41k · plan 31k` (`(round N)` only with
  more than one round; `n/a` for a phase that ran with no known size). The peak is the largest prompt
  (`input + cache read + cache creation` tokens) the agent sent in one model call of the turn. Status JSON has
  `implementer.context_peak` and `model_calls` per round, `context_peak` per review, and `timing.context_peak_max`
  (the largest of any turn, notes included); the run log has one line per turn, such as
  `context peak 88,412 tokens (24 model calls)`. Nothing goes in the PR notes. The implementer is streamed, so
  its peak is exact. Plan, review and notes turns are not streamed and only report their total usage, so they
  show a peak only when they made a single model call and `n/a (not streamed)` otherwise. Codex reports only a
  turn's total usage, not per request, so all its turns are `n/a`. Plan reviews are not measured.
- **Blocked is not unverified.** `blocked` means part of the request was not done (a credential it needs, a
  manual step, an open decision): the implementer ends its summary with `BLOCKED: <what and why>`, the run
  ends `blocked` and is never published. `UNVERIFIED: <what to check and how>` lines mean the work is done
  and passed the gates and reviews, but something can only be proven outside the sandbox (Docker, real
  credentials, the keychain, a real network service). They never change the result: a run with all checks
  passing and only `UNVERIFIED:` lines ends `passed` and is published as usual. The items are in
  `factory status --json` as `unverified` (omitted when empty, next to `blocked`), listed on the terminal
  under `Verify outside the sandbox:`, logged as `unverified: ...` in the run log and activity stream, and
  rendered in the PR body as a `## Verify before merging` checklist. With `--continue`, the list is the
  latest turn's, not accumulated.
- **`factory stop` ends the agent's work within seconds on the Docker backend.** Every command a run
  executes in a sidecar (agent turns, gates, reviews) runs in its own process group. On stop the group
  gets SIGTERM, then SIGKILL after `factory.stopGrace` seconds (default 10), and the run ends
  `cancelled`, never `failed`. The session's `stopped_after_ms` is the time from the stop request to
  the run ending. `factory stop` waits for that, up to the grace plus 5 s, then says the run is still
  winding down. On the CircleCI backend the sidecar API has no stop for a running command, so stop
  waits for the current turn to end. A command in an image without `setsid` cannot be signalled and
  runs until the grace has passed and the stream is closed.
- **`factory.cpus` limits the CPUs each run container may use** (Docker backend; a number, fractions
  allowed). Unset, the limit is the Docker engine's CPU count divided by `factory.maxConcurrent` runs,
  at least 2 and never more than the engine has. `0` turns the limit off; negative values are rejected.
  It applies to the implementer's and every reviewer's container. Commands see it as `SMELT_CPUS`
  (for example `pytest -n $SMELT_CPUS`), which is not set when there is no limit. The run log's start
  line and `factory status --json` (`cpus`) show it, such as `cpus 2.0 (8 engine CPUs / 4 runs)`.
- **`factory.maxConcurrent` allows several active runs per project.** The default, and `0` or a
  missing value, is 1: a second `factory build` is refused with `project already has N active run(s)
  (limit N; raise factory.maxConcurrent)`, with code `project.busy` and exit **75** (retryable). Negative values are rejected. A continued run (`--continue`)
  counts as one active run. Each run has its own worktree and container pool, so runs share no state, but
  each is one implementer container plus one reviewer container per review prompt: cap the reviewers
  with `--reviewers N` when running several at once. `factory status` with no run ID still shows the
  newest run; `factory list` and `smelt watch` show every active one. Finished runs are kept until
  more than `maxConcurrent + 8` sessions are retained, and active ones are never evicted.
- **Machine-wide limits live in the user config** (`~/.config/smelt/config.json`), under `factory`, and
  are read when the daemon starts (restart it to change them). `factory.maxRuns` is the most unfinished
  runs across all projects; unset or `0` is no limit. Over it, a start or `--continue` is refused with
  code `daemon.busy`, exit **75**, and a message naming the holders: `smelt is running 5 of 5 runs
  (smelt 2, smelt-pilot 3); try again when one finishes`. `factory.holdOverLoad` (load average per CPU,
  such as `1.5`) refuses new starts with `daemon.busy` (`machine load 7.8 per CPU`) while the host's
  1-minute load per CPU is above it; runs already going are never affected, and a host that cannot report
  its load never holds. Negative values are rejected. The machine check comes before the project one.
  `smelt daemon status` shows `runs: 5 of 5 (machine limit)`, and `--json` has `max_runs`.
  `smelt init` asks where factory runs execute and writes `factory.backend` (and, for docker,
  `factory.dockerfile`).
- **`factory build` needs a coding agent.** `--harness claude|codex|grok`, then `factory.harness`, then (in a
  terminal) a question that marks the agents that are logged in and in the image and offers to save the
  answer as `factory.harness`. With none of them and no terminal it fails with a `userError` listing the
  options and the config key; there is no silent default. `factory.models.<harness>` fills `--model`
  when it is not given. The daemon refuses a harness with no credential before starting anything, with
  the login hint, and a run needs only its own harness's credential. The run-start line shows
  `harness=… model=…`. An image labelled `smelt.harnesses` that lacks the chosen agent is refused with a
  pointer to `smelt factory image`; an image without the label is not checked.
- **Codex credentials never carry a refresh token into a container.** `auth login codex` runs
  `codex login` with `CODEX_HOME` set to a private temp directory (so `~/.codex` is untouched), keeps the
  whole `auth.json` in the keychain (`smelt:codex:chatgpt`), and deletes the directory. A run gets a copy
  with `tokens.refresh_token` set to `""` (the field must stay; codex rejects the file without it), passed
  base64 in the exec environment of the one-time setup script only, and written to
  `/home/circleci/.smelt-codex/auth.json` (directory 700, file 600). The access token lasts about ten days; with under
  24 hours left, a run first tries to renew it on the host (one `codex exec` turn against a temp
  `CODEX_HOME` holding the full file) and stores the result; if the token did not change it stops with
  "Your Codex login expires soon; run `smelt refresh codex`". `auth set codex` stores an API key
  (`smelt:codex:api-key`), which sidecars get as `CODEX_API_KEY` and `OPENAI_API_KEY`. The environment
  overrides the keychain: `SMELT_CODEX_AUTH_JSON` (base64 of `auth.json`), then `CODEX_API_KEY`.
  Unverified: the host renewal path could not be exercised with fresh tokens, so it is defensive until a
  stored token first gets within 24 hours of expiry.
- **Grok uses the host's `grok login`, or `XAI_API_KEY`.** There is no `auth login grok`: run `grok login`
  on this machine. A run sends the sidecars a copy of `~/.grok/auth.json` with every `refresh_token`
  set to `""`, base64 in the setup-only variable `SMELT_GROK_AUTH_JSON`; setup writes it to
  `$HOME/.grok/auth.json` (directory 700, file 600). The access token lasts 6 hours; with under 2 hours
  left a run first runs `grok models` on the host to renew it, and stops with "run `grok login`" if the
  login is still short. `XAI_API_KEY` (from the environment only) wins over the login and is passed to
  each turn. A turn runs `grok --output-format streaming-messages-json --always-approve --max-turns 200`
  with the prompt in `--prompt-file`; the system prompt goes under `## Rules` in that prompt, and the
  same git guard as Codex refuses `git commit`, `stash`, `reset`, `checkout`, `switch` and `rebase`. A
  review runs `grok --json-schema … --tools read_file,grep,list_dir` (the reviewer's tool list is fixed)
  and its answer is read from `structuredOutput`, never from `text`; like Codex, a review that changed
  the working tree fails. Kit v5 installs the pinned `grok` (`sandboximage.GrokVersion`, SHA-256 per
  architecture) into `~/.local/bin`. Unverified: grok itself, `grok models` renewing the token, and a
  token expiring mid-run could not be exercised in the sandbox.
- **Agent versions and refresh.** `harness status` reports `host_version` from each host CLI's
  `--version` and `image_version` from `/opt/smelt/manifest.json` in the sandbox, in text and JSON.
  `harness refresh` pulls and selects the current published kit; `harness refresh --kit-ref
  ghcr.io/smeltdev/smelt-kit:6` selects another published kit without upgrading smelt. Run
  `smelt factory image` afterward to assemble it with the existing toolchain. Future factory
  builds reuse that digest until another refresh or a CLI upgrade requires a newer kit;
  host agents remain managed by their own installers.
- **`harness status` reports, it does not choose.** Codex: `account/read` and `account/rateLimits/read`
  over `codex app-server`, with the stored login's refresh token blanked in a temp `CODEX_HOME`; windows
  are named from `windowDurationMins` (300 → `5h`, 10080 → `7d`, 43200 → `30d`, else `<n>m`). Claude:
  `claude auth status --json` for login and plan, and by default a `GET /api/oauth/usage` with Claude
  Code's own login (macOS keychain item `Claude Code-credentials`, read-only, never refreshed; smelt's
  `setup-token` token is rejected by that endpoint). `limits` is parsed first (`session` → `5h`,
  `weekly_all` → `7d`, `weekly_scoped` → `7d:<model>`), then `five_hour` and `seven_day`. Results are
  cached 60 seconds in `~/.local/share/smelt/harness/claude.json`, a 429 backs off 5 minutes, and any
  failure falls back to the last reading plus the last rate-limit signal a run saw (`source: "run"`).
  Anthropic has said third-party tools should not call the endpoint and it can change without notice;
  `harness.claude.usage_probe: false` in `~/.config/smelt/config.json` turns it off. `installed_image`
  comes from the image label `smelt.harnesses`, and stays a bool. Each harness also has `image_status`:
  `labeled` (label present), `unlabeled` (the image exists without the label: built with agent kit v1)
  or `no_image` (not on the host; `installed_image` is false, with a note to run `smelt factory image`).
  An unlabeled image is run once (`sh -c 'command -v claude …'`, `--rm`, no network, 20 s timeout) and
  the answer cached per image ID in `~/.local/share/smelt/harness/images.json`; the note says `image has
  no smelt.harnesses label (agent kit v1); checked by running it` and points at `smelt factory image`. A probe
  that fails or times out gives `installed_image: false` and a note saying so. The human output has an
  `image:` line, e.g. `image: smelt-x:f81c… (kit v1, unlabeled; claude found, codex not)`.
- **The docker backend's image is resolved, highest first, from `--image`, `factory.image`,
  `factory.dockerfile`, then `SMELT_DOCKER_IMAGE`.** `factory.image` is explicit and user-managed: it is
  never built, so a repo with its own `docker build` flow keeps it. `factory.dockerfile` (a
  repo-relative path, `Dockerfile.smelt` when `smelt init` wrote it) is managed: the image tag is
  `smelt-<repo>:<12 hex of sha256(Dockerfile + each COPY source's path and content)>`, so a change to
  the Dockerfile or a copied lockfile is a new tag, and `factory build` builds it first when it is
  missing. The build context is exactly the Dockerfile's `COPY` sources (absolute paths, `..` and
  wildcards are rejected), never the repo. A failed build is an error pointing at `smelt factory image`.
- **`--sandbox-dockerfile PATH` takes the toolchain Dockerfile from outside the repo.** On `factory build`
  and `factory image` (docker backend only; refused with `--image`). The Dockerfile comes from, highest
  first, the flag or build overlay (`caller`), `factory.dockerfile` (`config`), then the repo's own `Dockerfile.smelt`
  (`repo`, used only when `factory.image` and `SMELT_DOCKER_IMAGE` name no image). A relative PATH is
  taken against the current directory. The repo stays the build context, so `COPY go.mod go.sum` finds the
  repo's files; the file is sent to Docker under the reserved name `.smelt-toolchain.Dockerfile` (a COPY of
  that name is rejected) and is never copied into the repo or the run's worktree. The tag follows the
  Dockerfile text and the COPY sources as before. `factory image` never repairs or regenerates such a file
  (`--regenerate` is refused with the flag). `factory status --json` shows the choice as
  `factory.sandbox`: `{"dockerfile", "source", "toolchain_tag", "kit": "<version>@<digest>"}`; for a kit built with
  `--kit-from-source` the digest reads `source:<image id>`. `factory build --overlay refinement/run-overlay` reads JSON such as
  `{"sandbox":{"dockerfile":"/data/toolchains/go.Dockerfile"}}`. An explicitly supplied
  `--sandbox-dockerfile` wins over the overlay. Both the overlay file path and its Dockerfile path
  resolve against the invocation directory, not the overlay directory. Unrelated keys are ignored;
  malformed JSON and incompatible field types are rejected. No overlay is discovered implicitly,
  and `--overlay` is available only on `factory build`.
- **`smelt init` and `smelt factory image` verify what they build.** The image runs in a throwaway
  container over a copy of the repo: first the agent kit (`git`, `claude`, `codex`, `codex-code-mode-host`, `sha256sum`, `pkill`, then `lizard --version`, which only warns;
  plus one `codex exec --skip-git-repo-check "reply ok"` with a 60-second timeout when this machine has a
  Codex login, otherwise the output says `binary check only`; a usage-limit or 401 reply is reported as
  "could not confirm", not a failure), then the
  install step, then the cheapest gate (lint, format, typecheck, test). If the build or a check fails in a
  terminal with `claude` installed, smelt offers to let Claude edit `Dockerfile.smelt` (up to 3 attempts,
  only that file may change) and prints the diff on success. `factory build` never verifies or repairs.
- **The docker backend runs the install step once per sidecar** (the detected `install` setup step, else
  the `install` command) after the first sync and before the agent starts, streaming its output into the
  run log. Existing `pnpm install &&` prefixes in validation commands are left alone.
- **The agent kit is versioned (`sandboximage.KitVersion`) and added by smelt, not by `Dockerfile.smelt`.**
  `Dockerfile.smelt` holds the toolchain only; `smelt init` writes no kit section. An older file's section,
  which opens with `# --- smelt agent kit (vN) ---`, is ignored: the toolchain image is built from the text
  above the marker. `factory build` (for a `factory.dockerfile` image) and `factory image` print one note
  that the section is unused, once per project (a `kit-warned` stamp in the project's data dir; `factory
  image` prints it every time), record it in the run log and as `kit_outdated: true` in the `factory`
  section of `smelt factory status --json`, and never block. Kit v3 adds `codex-code-mode-host`, the
  companion program Codex runs every command through (pinned by SHA-256 next to `codex`,
  `sandboximage.CodexVersion`); a v2 image has Codex but it cannot run anything. Kit v4 adds lizard
  (`sandboximage.LizardVersion`) on a relocatable Python; without it code quality is `not measured` and
  nothing fails. Kit v5 adds the Grok CLI.
- **A Codex turn that cannot start its command host fails the run.** Output saying
  `codex-code-mode-host is missing` or `command host failed to start` (agent message, failed command or
  error event) becomes `codex.ErrHostMissing`; the run ends `failed`, not `no_change`, with "Codex can't
  run commands in this sandbox image: rebuild it with `smelt factory image`". That error and
  message are the stable hook for tooling (pilot) that wants to stop using Codex for the session.
- **Codex reviews run in bypass mode and are held read-only by a check.** `--sandbox read-only` needs
  bubblewrap and user namespaces on Linux, which containers often lack, so the review script uses
  `--dangerously-bypass-approvals-and-sandbox` like the implementer, keeps the git guard on `PATH`, and
  compares `git status --porcelain` and `git diff HEAD` before and after: a review that changed the tree
  exits non-zero and its answer is dropped. Verified on 2026-10-13: a review in the sandbox image runs in
  bypass mode with the git guard (`--sandbox read-only` is not used).
- **The sandbox image is built in two stages.** The agent kit is an image,
  `ghcr.io/smeltdev/smelt-kit:<KitVersion>` (built from `kit/`, published and cosign-signed by the
  release for amd64 and arm64, also as `:<KitVersion>-<smelt version>`). It holds `/opt/smelt` with `bin/`
  (`claude`, `codex`, `codex-code-mode-host`, `grok`, `lizard`), `env.sh` and `manifest.json`. Stage 1
  builds the toolchain image from `Dockerfile.smelt` (everything above any kit marker) as
  `smelt-<repo>-toolchain:<hash>`. Stage 2 builds a generated Dockerfile: `FROM` the toolchain,
  `COPY --from=<kit>@<digest> /opt/smelt /opt/smelt`, `ENV SMELT_KIT` and `PATH`, and the
  `smelt.harnesses`/`smelt.kit` labels, with no `USER` and no `CMD`, so the sandbox runs as the toolchain's
  user (root or not). The sandbox tag is a hash of the toolchain tag and the kit digest: a new kit changes
  only that tag and rebuilds only stage 2; a toolchain change changes both. This is unconditional: a
  hand-written Dockerfile gets the kit too, and it needs `git`, `bash`, `pkill` and `sha256sum` in the base
  (the verify step says which is missing). Kit resolution first checks local `smelt-kit:<kit>-<revision>` and `smelt-kit:<kit>` tags,
  resolving them to immutable image IDs without pulling. Otherwise it pulls the registry tag by digest.
  If the registry is denied, missing, or offline, smelt automatically builds its embedded `kit/Dockerfile`
  and `runtime.sh`, tags the result with both local tags, and reuses it on subsequent runs.
  A provenance notice appears once per image and is recorded on every affected run;
  explicit `smelt factory image` displays it again. `--kit-from-source` forces an embedded rebuild
  from any project, retaining Docker layer caching. An explicitly refreshed kit remains selected.
  If building fails, the last cached kit is a final fallback. Cold offline builds still need cached
  base images and download dependencies. Forks must publish their own kit or will build locally.
  After the first kit publish to GHCR, a repository owner must open the package settings
  and set its visibility to Public, then rerun the release job. Each fork must do the same
  for its own package or rely on the automatic local source build. The workflow
  checks visibility and fails until this is done. Repository permission inheritance does not
  inherit public visibility. GitHub's supported Packages API has no visibility update operation
  ([visibility settings](https://docs.github.com/en/packages/learn-github-packages/configuring-a-packages-access-control-and-visibility),
  [Packages API](https://docs.github.com/en/rest/packages/packages)); visibility is a one-time
  administrator bootstrap, not an automated release step.
  Follow-up: implement supported GHCR visibility automation when GitHub provides a
  documented mechanism. Before replacing the manual bootstrap, verify the operation
  and its required credentials against GitHub's documentation, exercise it on a
  newly published private test package, and confirm anonymous pulls succeed. Keep
  the release visibility check, make the operation idempotent for public packages,
  and report actionable permission failures. Automatic first-publish visibility is
  outside the acceptance criteria for the local kit fallback.
  `--update-kit` and
  `--yes` are retired: they print the same notice every time and do nothing. <a id="updating-the-agent-kit"></a>
  There is nothing to update in `Dockerfile.smelt`: run `smelt factory image`, and delete a leftover kit
  section if you like (`--regenerate` writes a file without one).
- **`Dockerfile.smelt` is generated from detection, never overwritten silently.** The base image is the
  one every CI gate job uses (`.circleci/config.yml`), else the detected stack's image. Dependencies
  are baked as the package manager's store (pnpm, npm, yarn, uv, pip, Go, Cargo), not as an installed
  tree, because the workspace is bind-mounted over the repo at run time. An unknown stack gets a
  minimal Debian image with a comment to add a toolchain.
- **The factory branch's commits are attributed to the developer.** They use `factory.author`
  (`"Name <email>"`) when set, else the repo's git `user.name`/`user.email`, else `smelt`. These are
  the commits a published PR carries. `.smelt/config.json` is committed, so `factory.author` applies to
  everyone in the repo: `smelt init` writes it only when you choose someone other than your git
  identity. `smelt factory author` shows the author and its source; with an argument it sets
  `factory.author`, and `--unset` removes it.
- **`factory build --plan FILE` reads the prompt from a file**, as if piped on stdin. It cannot be combined
  with a prompt argument or redirected stdin; with `--continue` it is optional guidance, like an argument.
- **`factory build` runs on the local daemon, in the background.** `smelt factory build "<prompt>"` (or
  `smelt factory build < prompt.md`; `smelt factory build - < prompt.md` is the explicit
  form; a prompt argument with a non-empty file on stdin is refused, so a
  stray word such as `--log run.log < prompt.md` cannot drop the file) checks
  the project's review prompts and validation commands, starts the daemon if
  needed, and starts a factory session, which the daemon starts tracking the
  project for if it has not seen it before; it follows the run
  until it ends. Ctrl-C stops the run and waits for it to commit the work done
  so far; the run belongs to the daemon, so if `smelt factory build` loses contact it
  keeps going and `smelt watch` still shows it. It uses the daemon's
  Claude and CircleCI credentials, resolved when the daemon started, and the
  project's configured org and image unless `--org-id` or `--image` say
  otherwise. It is refused when `SMELT_DAEMON_REMOTE_ADDR` is set, since a run
  works from this machine's files. The project needs a git remote named
  `origin` (the sidecars clone from it); `factory build` fails up front without one.
  A project has one active run at a time. Runs are kept in memory: a daemon
  restart loses the record, but not the work committed on the run's branch. `--reviews` must name a directory inside the project. Every reviewer is
  given the original factory prompt as the requested-change specification, so
  the files in that directory can be reusable review lenses rather than copies
  of a particular request. Every reviewer is also given a severity rubric: only
  a problem it can demonstrate (it breaks the requested behavior or existing
  behavior, degrades run time, loses data or is a security problem) may be high
  or medium and so send the implementer back; style remarks, tests that could
  be more thorough and other suggestions are low and never block. From the
  second round, each reviewer is shown its own earlier blocking findings and the
  implementer's reply to them, so it checks whether they were fixed rather than
  raising them again or contradicting itself. As it follows the run it prints the sidecars being
  prepared and each round's findings, with the end of any failed validation
  command's output.
  Factory branches are named `smelt/<description>/<run id>`. The
  description is a short slug made from the prompt's first line, while the
  final segment remains the stable run ID. When the run ends it prints that
  branch, how to keep it, how long the run took and what the implementer cost,
  and exits non-zero unless every check passed. `--log` has the run keep a
  plain-text log in `~/.smelt/factory/run-<start time>.log`, and
  `--log-file <path>` keeps it in a file of your choosing instead (it implies
  `--log`), with the full context the display leaves
  out — each prompt the implementer is sent, what it did and said, every
  review's findings and prose, and failed commands' output — for a person or an
  agent to judge the run and its prompts by. The daemon writes it, so a
  relative path is resolved against the caller's directory first. `--verbose`
  adds the review prompts, passing commands' output, and a check each round
  that every reviewer has the implementer's change, also shows the log in the
  terminal as the run writes it, and implies `--log`.
  `--implementer-instructions` appends run-specific guidance to the implementer's
  fixed safety prompt, which is useful for controlled harness experiments.
- **`factory build --from <ref>` bases a run on a branch or commit, not on the checkout.**
  The ref (`epic/x`, `origin/epic/x`, a SHA) is resolved with
  `git rev-parse --verify <ref>^{commit}`. A branch is first fetched from `origin`
  (`git fetch origin <branch>`; a failed fetch is a warning and uses what is local),
  and `origin`'s copy is used when the local branch is behind it; a diverged local
  branch wins. A ref that is neither a branch nor a commit fails with
  `no branch or commit <ref>`. No working-tree snapshot is taken: the baseline is the
  resolved commit, uncommitted changes in the checkout are left out (the run says so
  once when there are any), and the checkout is never touched. The run branch is still
  `smelt/<description>/<run id>`. `Worktree.BaseBranch` becomes the branch `--from`
  named (without `origin/`; empty for a commit), so `factory publish --pr` targets it
  by default, and the status JSON gains `from: {ref, commit}`. It is per run, not a
  config key, and `--from` with `--continue` is a user error (a continued run keeps its
  base). It works with both backends.
- **A run's PR targets the repository's default branch unless it was started from a base on purpose.**
  `Worktree.PRBase` (`pr_base` in the record and the status JSON) is decided once, when the
  run starts: the `--from` branch (an integrate run's `--against` branch), else the default
  branch from `gh repo view --json defaultBranchRef` (cached per repository under the data
  dir, falling back to the remote HEAD git recorded). `factory publish --pr` and
  `factory update` use it, so a run started while the checkout sits on a leftover feature
  branch still opens its PR against the default branch. `base_branch` still records the
  branch the run started from. Without `--from`, on a branch other than the default, the run
  starts from that commit and says `starting from <branch> (not <default>); the PR will
  target <default>` (terminal and run log). Publish says `Opened PR #62 → main`. Records
  from before `pr_base` have none, so they publish to the default branch.
- **`factory build --integrate` checks a finished branch against a whole spec.** It needs
  `--from` and a prompt (usually `--plan epics/x.md`), and is a user error with
  `--continue` or without `--from`; `--against` without `--integrate` is one too. The
  change under review is `merge-base(--against, --from)..--from`, so everything the
  branch added; `--against` defaults to the repository's default branch (an error when
  none is recorded). The worktree is checked out at the `--from` tip with that merge
  base as its baseline, and the sidecars see the branch's work as uncommitted changes,
  as in a continuation. There is no plan and no implementer turn at first. Round 1 runs
  the gates and every review together (`factory.gatesFirst` is ignored), each review
  preceded by an integration preamble that asks whether the combined change meets every
  requirement and acceptance item of the spec and whether the parts fit together. A
  pass ends the run `passed` with no implementer turn. Otherwise the implementer gets
  one fix turn, told "This branch combines several finished tickets. Fix exactly these
  findings; do not refactor beyond them.", followed by the findings. Round 2 runs the
  gates and only the reviews that had high or medium findings, each told to check only
  those findings and what the fix may have broken; reviews that passed are carried
  forward as passed, and a review that errored in round 1 gets its broad pass then.
  A failing round 2 ends the run `exhausted`. The attempts are fixed at 2;
  `--max-attempts` is ignored with a note (the daemon ignores `attempts` too). The PR
  base is `--against`'s branch unless `--base` says otherwise; the PR head is the run's
  `smelt/*` branch, which equals the epic's tip when no fix was needed. A run that
  makes no fix commit does not amend the epic's tip commit; the notes go in the record
  and the PR body. The status JSON has `mode: "integrate"`, `against`, and for each
  round `kind` (`broad` or `recheck`), with `implement.fix` set on the fix turn.
- **`factory build --continue` picks up a finished run's work.** A run whose checks
  still fail when it ends (exhausted or stuck) prints
  `smelt factory build --continue <run id>`. The continued run reopens the earlier
  run's worktree (adding it back from its branch if it was removed, and first
  committing any edits made in it by hand), adds a commit to the same branch,
  and measures the work from the same baseline, so reviewers see the whole
  change rather than only what the continuation adds: the sidecars are synced
  from the baseline's files and the work is then laid on the implementer's as
  uncommitted changes. The implementer is a new Claude session and is told the
  original request and where the work is. A prompt is optional and adds to the
  request, for the implementer and the reviewers alike. With one, the
  implementer starts on it; without one, the run checks the work as it is
  first and sends the implementer what failed, and that round counts toward
  `--max-attempts`. Given no prompt in a terminal, it asks what to do
  differently first; Enter skips that and checks first. Without a terminal it
  does not ask. Each run keeps a record (its request, worktree, branch
  and baseline) in the project's data directory beside its worktree, which is
  what `--continue` reads, so a run can be continued after the daemon
  restarts; runs from before records were kept cannot be continued. The
  session's record names the run it continues as `continues_run_id`. A run
  whose branch holds no work (it ended `no_change`, or was cancelled or failed
  before its first turn's changes came back) has nothing to check, so
  `--continue` implements the original request, plus any guidance, with a line
  saying the earlier run produced no work, and `factory status` reports
  `continued_from_empty: true`. A branch with work is still checked first.
- **`watch` is organized around runs.** Factory runs are listed at the top of
  the left pane, live ones first, then up to five that ended in the last day;
  sidecars are listed below them, and `↑/↓` moves through both as one list. The
  first poll selects the newest run. A selected run's pane shows where it stands
  and where its work is (prompt, worktree, branch, log), then each round: the
  implementer's turn (duration, cost, diff stat), each validation command, and
  each review with its findings, followed by the run's latest progress and its
  sidecars. A factory review is a check: it passes unless it found something
  worth changing (high or medium severity), so its row shows ✓ with any lesser
  findings noted, or ✗ with how many are worth changing. A factory run's own
  sidecars are shown there rather than as rows of their own. `→` moves into
  the pane, `↑/↓` picks a row and `Enter` opens it: a review's live log, a
  failed check's output, or the implementer's summary. The summary and a
  check's output are word-wrapped to the screen; a log keeps one line per line
  of output. The footer only lists keys that do something for what is
  selected, so `x` is gone once a run has ended. **`q` only detaches**: the run
  belongs to the daemon and carries on. The one key that stops it is `x`, and it
  needs a second `x` on the same run to confirm.
- `watch` requires a TTY — it exits with an error if stdout is not a terminal. It polls sidecar state every 5 seconds and keeps an in-memory window of the 300 most recent event log entries. Use `j`/`k` or `↑`/`↓` to select a sidecar, `q` or `Esc` to quit. By default it watches every project it knows about; pass `--focus` to watch only the current directory. Running `watch` in a project also registers that project so future runs find it. `--all` is deprecated — it is now the default.
- **Run results are read from disk, not sent to the daemon.** Every `validate` run
  writes its events to the project's event log and registers the project (a
  `project-root` breadcrumb in the same data directory) whether or not a daemon
  is running. The daemon discovers projects from those breadcrumbs and replays
  each log from the start, so a sidecar run made with no dashboard open is there
  in full the next time `watch` opens. A purely local run has no sidecar, so it
  gets no row. (Remote command *output* is the exception: it is registered with
  the daemon live and buffered in memory, keyed by the same project root, so it
  too is only listed for a project that has been registered.)
- **`watch` can show a command's output.** In the activity pane, an invocation
  marked `▤` has output the daemon still holds; `Enter` opens a scrollback view
  of it. A command that is still running tails live — the pane polls every 200 ms
  while it is open, and drops back to the 5 s snapshot tick when closed, so a tail
  never speeds up everything else. `↑`/`↓` and `PgUp`/`PgDn` scroll, `g` jumps to
  the top, `G` re-follows the end, and `Esc` closes the pane rather than quitting
  the dashboard. Scrolling up detaches from the bottom so arriving output does not
  yank the view away from what you are reading; scrolling back to the end
  re-follows automatically.
  - Output is capped at 256 KiB per command, keeping the **tail** — the end of a
    failed run is the part worth reading. When earlier output has been dropped the
    pane says `(earlier output dropped)` rather than presenting a partial run as if
    it were whole.
  - The daemon holds at most 20 commands per project, evicting the oldest
    *finished* one first. A running command is never evicted.
  - Buffers are in memory only, so restarting the daemon loses them. `Enter` on an
    invocation the daemon no longer knows about says so.
  - Output streaming needs a CircleCI token. Without one the dashboard still works
    and shows a one-line hint in the footer; the daemon never prompts.
- **`smelt sidecar logs <command-id>`** is the non-TUI door onto the same output,
  for scripts and for agents with no terminal. Command IDs appear in the `watch`
  dashboard. It reads the smelt daemon's buffer when the daemon has the command
  and falls back to streaming from the API when it does not, so it works either
  way. `-f`/`--follow` keeps printing until the command exits.
  - Output goes to stdout and nothing else does, so it can be piped. A non-zero
    remote status is reported on **stderr** as `exit status N`.
  - `logs` itself exits non-zero only when *reading* failed. A failing command is
    not a failing read — conflating the two would make the exit status useless to
    a caller.
- **`watch` shows live resource usage** for the selected sidecar — CPU, memory and
  disk, sampled every 2 seconds. Sampling only runs while a dashboard is attached,
  since each sampled sidecar is running a shell loop for it; close `watch` and it
  stops. A
  sample older than three intervals renders dimmed and marked `(stale)` rather
  than disappearing, so a stalled sampler looks stalled instead of looking like an
  idle sidecar. Memory is read from the sidecar's cgroup, not `/proc/meminfo`,
  because the latter reports the host's memory.
- **`watch` rows are per branch, except when they cannot be.** A branch's local runs
  are folded into its sidecar's row, so one row shows both kinds of run along with
  sync state. A branch with more than one sidecar — two agent sessions in one
  working tree, which is the normal shape (see **Sidecar state is per agent
  session**) — keeps a row each, with local runs as their own row, since folding
  them together hid a session's row and listed its activity under the other
  session's name. Rows that share a branch carry a label naming the session
  (`⧉ session d7d915b2`), because the branch name no longer tells them apart and
  generated sidecar names shorten to the repo name for all of them.
- `smelt init` uses Claude to auto-detect the test command for the project.
  It generates `.claude/settings.json` with pre-commit hooks. It never touches
  CircleCI — tokens are prompted inline only when a command actually needs them.
- `sidecar sync` mirrors the working tree to the sidecar with rsync, honouring
  `.gitignore`. The branch does not need to be pushed to GitHub. Pooled
  `validate` and `review` use the same mechanism.
- `sidecar current` prints the owning session (`session <8 chars>`) alongside the
  name and ID. A sidecar keeps the name it was created with, and adoption transfers
  it without renaming, so the name often carries the session that created it rather
  than the one using it now — the session line is what tells two parallel sessions
  apart. `--json` carries `session_id` and `owner_pid` in full.
- `sidecar ssh -- <cmd>` forwards stdin when the process stdin is a pipe, enabling
  patterns like `cat bundle | smelt sidecar ssh -- git fetch ...`.
- **`smelt --version` prints the build.** `smelt version dev (dabe728, 2026-10-18)`:
  the version, then the VCS revision (7 characters, `-dirty` for an uncommitted
  tree) and commit date from the binary's build info. A binary built without VCS
  information (`go test`, a build outside a checkout) prints the version alone.
- **`daemon restart --when-idle` hands over the main listener.** The old daemon
  moves its Unix listener to `smeltd-<pid>.sock`, annotates its live sessions
  with that socket, releases the main socket and PID file, and returns. The next
  command starts the build on disk; new runs start immediately while existing
  runs finish in their original process. Session commands, including status,
  stop, follow, update, publish and `--continue`, discover the owner. Factory
  list merges sessions from all local daemons. A continuation uses its original
  daemon while the session remains discoverable; afterwards it uses persisted
  history on the main daemon. The old daemon exits after its last run winds down
  and removes its own socket. Discovery removes sockets left by crashed daemons.
  Older daemons without handover support use `POST /drain` and wait: starts
  receive exit **75** (`daemon.draining`) until runs finish. For this legacy
  fallback, `--timeout` cancels draining (`DELETE /drain`), leaves runs intact,
  and exits 75 (`daemon.restart_timeout`). The default waits indefinitely;
  requester cancellation leaves draining enabled. Builds predating drain mode
  need a one-time `daemon stop --force`. Plain restart without `--when-idle`
  remains refused (exit 2).
- **`daemon stop` refuses while a factory run is active**, and when it cannot tell
  (exit 75, code `daemon.stop_refused`); `--force` stops it regardless.
  `scripts/restart-daemon.sh` and `task build` use `restart --when-idle`; `task
  daemon:restart` uses `stop --force`.
- **`daemon status` lists every local daemon**, main first, then draining owners.
  Text includes PID, revision (marked `(this binary)` when matching), active run
  count, and `exits when they finish` for draining daemons. `--json` emits a
  deterministic array of `{pid, revision, draining, active_runs, socket}` with
  every field present. With none, text is `not running` and JSON is `[]` (exit 0).
  Stop remains scoped to the main daemon. Daemon commands do not auto-launch
  and refuse `SMELT_DAEMON_REMOTE_ADDR`; remote lifecycles remain externally managed.
- **`sidecar ssh` exits with the remote status**, for both a command and an
  interactive shell, and prints nothing of its own, the same as `ssh` and
  `sidecar exec`. A remote status can therefore coincide with one of smelt's own
  exit codes (2–5); smelt's own failures are the ones that print an error. A
  process killed by a signal additionally reports the signal on stderr.
- **Sidecar state is per agent session.** Two agent sessions open in the same
  working tree must not share a sidecar: they would sync into the same remote
  workspace and reset the checkout under each other's test run. State files are
  keyed by session and branch, where the session is the Stop hook payload's
  `session_id`, or `CLAUDE_CODE_SESSION_ID` (overridable with `SMELT_SESSION_ID`)
  for the commands an agent runs itself. Every session always creates its own
  sidecar — adoption was removed to prevent two sessions from syncing into the
  same remote workspace and resetting each other's checkout. Parallel sessions
  cost one sidecar each; sequential ones reuse the same one (same session ID,
  same state file).
- **Abandoned sidecars are reaped automatically.** Local sidecar state is one file
  per session and branch, and `validate` sweeps them before resolving a sidecar:
  state naming a sidecar absent from the org listing is deleted, and a sidecar
  still running whose state has not been touched for 5 days is deleted through the
  API and its state dropped. The sidecar the current run is about to use is never
  deleted by age, and one that is alive with recently touched state is left alone
  so a concurrent session on another branch keeps its own. The sweep needs an org
  ID that resolves without prompting (see **Org ID resolution**), skips silently
  without one, and deletes nothing if the listing fails, since an empty listing is
  not proof of absence. A sidecar the API rejects as out of date (410) is deleted
  when a sync hits it, because no listing reveals that state.
- **The `validate variants` pool is outside that scheme.** Variants are queued
  across temporary pool members, which are not written to active-pool state —
  workers would race on it and leave the user's own session pointing at a sidecar
  about to be deleted. That also makes them invisible to the reaper above, so the
  command cleans up after itself instead: it deletes each sidecar when the pool
  shuts down, catches SIGINT/SIGTERM so an interrupt still unwinds through those
  deletes, and sweeps stranded `variant-*` sidecars from an earlier crashed run
  before starting a new one. Each name carries that sidecar's own creation time
  (`variant-<base36 seconds>--<id>`), which is what lets the sweep spare a
  concurrent run's in-flight sidecars — two runs at once, in two worktrees or two
  repos, is a normal shape for the mutation-testing skill — and only collect ones
  too old for any live run to own. Per-sidecar rather than per-run, because a run
  of a hundred variants takes hours and its newest sidecar must not inherit the
  age of the run that booted it. The delimiter is two dashes and variant IDs are
  sanitised to collapse dash runs, so the timestamp is recoverable by a plain
  split rather than by guessing which segment is a date. A name without one is
  reported and left alone. For the same reason `validate variants` syncs
  without persisting a workspace and therefore requires a resolvable workdir; it
  resolves one through `sidecar.ResolveWorkspace`, the same
  `--workdir` → active sidecar → `<sidecarHome>/<repo>` order as every other
  sidecar command, and fails before booting anything when none of the three
  applies.
- **`validate variants` never reports an environmental failure as a caught
  mutant.** A killed variant means the validate command ran and exited non-zero
  on its own terms. Exit codes 126 and 127 are the shell failing to run the
  command at all, and a command that outruns its timeout never returned a verdict;
  both are recorded as errors instead, and errors are neither kills nor survivors.
  Commands are template-expanded locally before being shipped, since a literal
  `{{CHANGED_PACKAGES}}` reaching the remote shell would otherwise fail every
  variant the same way. The command warns when every variant was killed, because
  that pattern is more often one broken command than a fully covered codebase.
- Commands that require a CircleCI token (`task run`, `task config`, `sidecar *`,
  `validate --sidecar-id`) prompt for it inline at the point of need rather than
  failing with an error.
- `smelt auth set github` stores a GitHub token in the config file; previously
  only the `GITHUB_TOKEN` environment variable was supported.
- `smelt hook disable` creates a `.smelt/hooks-disabled` sentinel file inspected by the `smelt validate` Stop hook; `hook enable` removes it. Stop-hook validation is also disabled when `SMELT_HOOKS_DISABLED` is set in the environment.
- **Hook runs may be validated in the background.** When the smelt daemon is
  running, a Stop-hook `smelt validate` offers the daemon the choice of running
  the checks after the hook has exited. The daemon takes the offer for a change
  under 500 lines, or one confined to docs and text files, and holds the caller
  for anything larger, anything it cannot measure, and any project whose last
  run failed. Size is measured against the last state that passed its checks
  rather than against `HEAD`, so consecutive small changes do not accumulate
  into a large one and a commit neither hides a change nor resets the count. Results of a background run reach the agent on its next turn, via
  the `smelt validate results` hook. Only hook runs are ever released — a
  `smelt validate` typed at a terminal always waits. Set `asyncValidate` in
  `.smelt/config.json` to `never` or `always` to override the judgement, and
  `asyncValidateMaxLines` to move the threshold, `asyncValidateInert` and
  `asyncValidateBlocking` to say which paths it applies to. See
  [HOOKS.md](HOOKS.md#background-validation).
- `smelt validate` caches successful runs in hook mode only, keyed by
  `.smelt/config.json`, the execution target, the HEAD SHA, and the contents of
  all changed files; a repeat hook invocation with nothing changed prints
  `skipped` instead of re-running. Manual runs, `--cmd` inline commands, and
  repos whose state cannot be hashed never cache. Entries expire after 7 days.
  See [HOOKS.md](HOOKS.md#result-caching).
- **`validate --async` and `validate results` are two halves of one flow.**
  `--async` hands the run to the smelt daemon and returns immediately; `results`
  reads what finished and prints it to stdout. Nothing installs a hook to call
  `results` yet: a background run is something you ask for explicitly, so
  reading its answer back is too. Wiring it into a hook waits for the change
  that makes background runs happen on their own. A result is reported once and
  then forgotten. A live-tree run whose working tree changed while it was in
  flight has its verdict discarded: a pass describing code that is no longer on
  disk reads as a green light for work already changed. The discard itself is
  reported, without an exit code or output, because the commonest reason the
  tree moved is the run — output written, a golden regenerated, a lockfile
  touched — and staying silent about that is indistinguishable from no run
  having happened, which leaves such a project getting nothing with no way to
  tell why. `TaskState.Passed` consults `Stale`, so a stripped verdict cannot
  read as a clean pass. Stopping the tree moving under the run in the first
  place is a matter of where the run happens, not how its result is judged —
  which is what `asyncValidateWorktree` does: a run that validated a snapshot
  keeps its verdict and is reported with the state it describes named.
- **`validate results` is a subcommand, not a flag, because it validates
  nothing.** It reads daemon state. A flag on `validate` would read as a
  modifier of a run that never happens.
- Every command checks GitHub for a newer release in the background and prints a
  notice to stderr once it finishes. The result is cached in the app state dir
  (`update-check.json`) for 24 h to stay inside GitHub's unauthenticated rate
  limit of 60 requests/hour. The window is claimed before the request is made,
  so a command that exits mid-flight does not send the next invocation back to
  GitHub — the notice simply appears on a later run.
- The update check never delays a command: if the check has not finished by the
  time the command has, the notice is dropped rather than waited for. It is
  skipped entirely in CI (`CI` set), when the user sets
  `SMELT_NO_UPDATE_CHECK`, under `go test`, and for `completion`,
  `receive-telemetry`, `upgrade`, and shell-completion requests, which either run
  on every keypress or report versions themselves. `watch` runs its own check and
  renders the notice in the TUI footer instead.
- The notice suggests the install script when the running binary resolves into
  a Homebrew prefix (that is CircleCI's build, not this project's), and
  `smelt upgrade` otherwise. `smelt upgrade` refuses to replace a
  Homebrew-managed binary in place. No notice is shown for `dev` builds.
- The notice names both versions (`v1.0.0 → v1.0.1`) and links the new
  release's GitHub page, so the reason to upgrade is one click away. The `watch`
  footer keeps its one-line form: it has no room for a URL.

## Quality (`smelt quality`)

`smelt quality` runs the measurement every factory run makes, in a throwaway
container from the project's image (Docker, whatever the project's factory
backend; the image is built if it is missing), with the limits from
`factory.quality` (see below). It needs no daemon or credentials. The project is
synced into the container, so uncommitted work is measured.

- Any language the provider knows. The default provider is lizard, installed
  by agent kit v4: an image built before it shows `not measured` until
  `smelt factory image`. `factory.quality.command` replaces it
  with a command that prints the metrics contract as JSON (see
  `internal/quality/contract.go`); output that is not the contract is an error.
- A measurement that cannot be made (provider missing, exit 127, output that is
  not the contract) is a `not_measured` check: it counts as passed, is shown as
  `⚠ quality not measured: <reason>`, appears in the PR notes, and the status
  JSON carries `quality: {"measured": false, "reason": "..."}`. Only quality
  findings (medium) can fail a round; `blocksRound` in `internal/factory/check.go` decides what blocks
  (gates, reviews and quality findings, never a measurement error).
- Left out: what git ignores, `vendor/`, `node_modules/`, `third_party/`,
  `testdata/`, test files and directories (`*_test.go`, `test_*.py`,
  `*.test.ts`, `*.spec.ts`, `tests/`, `spec/`, ...) and generated files (a
  `Code generated`, `@generated`, `DO NOT EDIT` or `autogenerated` marker in the
  first lines). `factory.quality.exclude` adds globs, `include` takes paths back
  out of the defaults and `defaultExcludes: false` turns them off.
- Per function: cyclomatic complexity as the provider counts it, and lines
  (non-blank, non-comment). Closures are separate functions for lizard.
  Counting differs per language and tool, so numbers are only comparable within
  one project over time.
- **Erosion** is the share, in percent, of Σ(complexity × √lines) held by
  functions with complexity above 10.
- **Functions over N** counts functions above `maxFunctionComplexity`.
- **Clone lines** are the lines of copies of blocks of at least `minCloneLines`
  lines (it counted statements before), counted once per extra copy. If the
  duplicate report cannot be read, clones are listed as `not measured`.
- `--base <rev>` also measures that revision (in a temporary git worktree in the
  container) and lists what a run would send
  back: a new or changed function over the complexity limit that is new or grew
  (medium), erosion up by more than `maxErosionRise` points (medium), and
  copied blocks the change adds (low). With `--base`, any medium finding makes
  the command exit non-zero, for CI. Without it the exit code is 0.
- `--json` prints `measured`, `erosion`, `limit`, `over_threshold`,
  `clone_lines`, `worst` (`func`, `file`, `language`, `cc`, `lines`),
  `not_measured` and, with `--base`, `base` and `findings`.

## Review patterns (`smelt review patterns`)

`smelt review patterns [--language L]... [--area A] [--json]` prints smelt's catalogue of review
patterns: named ways a change goes wrong, described as behaviour, with an is/isn't example per
language. It needs no project, login or network.

- `--area` is one of `requirements`, `correctness`, `maintainability`, `testing`; `adversarial`
  is another name for `requirements`. Any other value is a usage error listing the valid areas.
- `--language` takes lizard's language names (the ones `smelt quality` reports), lower case; repeat
  it or separate names with commas. Examples are limited to those languages, and a pattern that
  does not exist in any of them (`applies_to`) is left out. A language the catalogue has no
  example for gets the definition alone: another language's example is never substituted. Without
  `--language` every example is printed.
- Text output is Markdown, a `##` heading per area and a `###` section per pattern.

`--json` is a contract other tools may read. It prints one object:

```json
{
  "version": 1,
  "patterns": [
    {
      "name": "silent-fallback",
      "area": "correctness",
      "definition": "Code that turns a failure into a plausible default, ...",
      "severity": "medium",
      "applies_to": ["all"],
      "examples": {"python": {"is": "...", "isnt": "..."}}
    }
  ]
}
```

- `version` is 1. A change that removes or renames a field, or changes what one means, bumps it;
  new fields and new patterns do not.
- `patterns` is ordered by area (requirements, correctness, maintainability, testing) and then by
  the catalogue's own order, so the output is the same on every run. It is `[]` when nothing
  matches.
- `name` is unique, lowercase words joined by hyphens, and is the name a finding carries in
  brackets, such as `[silent-fallback]`.
- `severity` is the usual severity of a finding of the pattern: `high`, `medium`, `low` or `info`.
- `applies_to` is `["all"]` or the languages the pattern exists in, such as the ones that have
  interfaces for `lonely-interface`.
- `examples` maps a language name to `{"is", "isnt"}`: text showing the pattern, and text that looks
  similar but is not it. It holds only the requested languages that have an example, and is `{}`
  when there are none.

A prompts directory named `patterns` must be passed to `smelt review` as `./patterns`.

## Config keys

| Key | Scope | Description |
|-----|-------|-------------|
| `harness.claude.usage_probe` | user config (`~/.config/smelt/config.json`) | Whether `harness status` may call Claude's usage endpoint (`true`/`false`, default: `true`) |
| `factory.harness` | `.smelt/config.json` | The coding agent runs use: `claude`, `codex` or `grok` |
| `factory.models.<harness>` | `.smelt/config.json` | The model that agent uses when `--model` is not given |
| `factory.quality` | `.smelt/config.json` | The quality check of factory runs and `smelt quality`: `{"enabled": true, "maxFunctionComplexity": 15, "maxErosionRise": 1.0, "minCloneLines": 15, "command": "", "exclude": [], "include": [], "defaultExcludes": true}`; `enabled: false` turns the check off, `command` replaces lizard, `minCloneLines` counts lines |
| `model` | user config (`~/.config/smelt/config.json`) | Claude model override |
| `telemetry` | user config (`~/.config/smelt/config.json`) | Usage telemetry preference (`true`/`false`, default: `true`); only matters to a build that has a telemetry write key, which default builds do not |
| `notifications` | user config (`~/.config/smelt/config.json`) | OS desktop notification after validate completes (`true`/`false`, default: `false`) |
| `orgID` | `.smelt/config.json` | CircleCI organization ID for sidecar subcommands |
| `validation.sidecarImage` | `.smelt/config.json` | Snapshot or image ID for sidecar bootstrap and validate (unset: a matching org snapshot is selected automatically) |
| `asyncValidate` | `.smelt/config.json` | Whether hook runs may be validated in the background: `auto` (default), `always`, `never` |
| `asyncValidateMaxLines` | `.smelt/config.json` | Largest change, in lines, still validated in the background under `auto` (default: 500) |
| `asyncValidateWorktree` | `.smelt/config.json` | Run background checks in a checked-out snapshot so edits cannot make the result stale (`true`/`false`, default: `false` — a snapshot holds nothing gitignored) |
| `asyncValidateInert` | `.smelt/config.json` | Extensions (`.sql`) or file names (`NOTICE`) this project also counts as prose, validated in the background at any size; adds to the built-in list |
| `asyncValidateBlocking` | `.smelt/config.json` | Extensions or file names that always block, over an inert default, the line threshold, and `asyncValidate: always` |

`smelt config show` displays resolved user credentials and, when run from a
project directory, the resolved `orgID` (env var takes precedence over project
config).

## Plugins

`smelt <name> [args…]`, when `<name>` is not a built-in command and a program
named `smelt-<name>` is on `PATH`, runs that program with `args`, the same
stdin, stdout and stderr, and exits with its exit code.

- `<name>` must match `^[a-z][a-z0-9-]*$`. The lookup is `PATH` only, never the
  current directory, and a built-in command always wins over a plugin of the
  same name.
- The plugin gets `SMELT_BIN` (the absolute path of the running smelt, so it
  calls the same build) and `SMELT_VERSION`.
- Without such a program the error is the usual unknown-command one, plus
  `No smelt-<name> plugin is installed either.`
- `smelt --help` ends with a `Plugins:` section listing the `smelt-*` programs
  found on `PATH`.

## Run control

`smelt factory build --detach` starts a run and returns once the run has its
branch, so another program can drive it: `status` polls, `stop` cancels (as
Ctrl-C does) and `publish` pushes or opens the PR once it has finished. Run IDs
are accepted as the full ID or the branch's final segment. `publish` applies the
end-of-run guards: only `smelt/` branches, never forced, never the default
branch. A run the daemon no longer knows, after a restart, is reported from its
record, with `"state":"done"`.

### Writing a project's first review prompts: `factory bootstrap`

`smelt factory bootstrap [--harness claude|codex] [--model M] [--timeout 30m] [--output DIR] [--json]`
starts one sandbox, has the coding agent read the project there (read-only: `Read`, `Grep`, `Glob`
and `git log`/`show`/`ls-files` for Claude; Codex is held read-only by the review script's
working-tree check) and writes 2–4 review prompts. It runs in-process, not on the daemon.

- **Where it writes.** `.smelt/reviews/<name>.md`, or `--output DIR`. It refuses at once, before
  any sandbox starts, when that directory already has `.md` or `.txt` prompts (suggestion: pass
  `--output` to write elsewhere and compare), and never overwrites a file: each is created
  exclusively, so a file that appears meanwhile fails that prompt, not silently.
- **Backend, image and agent** are resolved as `factory build` resolves them: `factory.backend`
  (Docker by default; CircleCI only when the project chose it, never because a token is present),
  the project's image (a missing one gets `factory build`'s error, pointing at `smelt factory image`),
  and `--harness`, then `factory.harness`. The agent's credential is looked up on this machine, and a
  stored Codex login is renewed first. The sandbox is its own pool, `factory-bootstrap`, and is
  always deleted when the command ends.
- **What the agent is told.** The validation commands from the config (so reviews leave to them what
  they enforce) and, when `smelt build-prompt` wrote it, `.smelt/context/review-prompt.md` (capped at
  64 KiB).
- **Patterns to look for.** Each prompt carries an `area` (the schema's `area` field: the four
  catalogue areas or `other`). smelt, not the agent, appends a `## Patterns to look for` section
  with that area's patterns from `smelt review patterns`, with examples in the project's languages:
  those of the files `git ls-files` lists (tracked or not ignored), found with the extension map and
  default exclusions `smelt quality` uses. Languages with no examples, or a project whose languages
  cannot be listed, get definitions alone. A prompt in area `other` gets no section. The section is
  capped at 24 KiB, after the 32 KiB check of the agent's own text.
- **While it runs**, each tool call is shown as a status line; its prose is not. Then the summary,
  the files written, any dropped prompts and the cost when the agent reports it.
- **The answer is untrusted text that becomes files.** At most 6 prompts are kept; names must be
  lowercase words joined by hyphens, with no repeats (the first wins); bodies must be non-empty and
  at most 32 KiB. The rest are dropped and counted. With none left it fails with "the agent gave no
  usable review prompts".
- **`--json`** prints `{"dir", "prompts": [{"name", "path"}], "summary", "dropped", "cost_usd",
  "harness"}` (`cost_usd` is left out when the agent does not report it). Errors are reported as
  `factory build --json` reports them.
- `--timeout` bounds the agent's run and defaults to 30 minutes: reading a large repository takes
  longer than a review.

### Telling a live run something: `factory note`

`smelt factory note <run-id> [text|-] [--json]` leaves a note for a run that is still
going (text from the argument, or stdin when it is `-` or omitted; at most 4096 bytes, not
empty). At the start of the run's next round, the implementer's prompt and every review
prompt of that round get the notes it has not read yet under `## Notes from the operator`,
oldest first. Each note reaches one round only; it is not copied into the record or the PR
notes. Notes are files in `<data dir>/factory/<run-id>.notes/`, so the daemon's runs and
runs in their own process read them the same way.

- `--json` prints `{"run_id","note_id","queued":true}`.
- A finished run refuses a note: `Run <id> has ended; notes reach live runs only.`
- `smelt factory status --json` lists them as `operator_notes`, each with `id`, `text`,
  `received_at` and `delivered_round` (null until a round has read it).
- A note left before round 1 is read in round 1. One left while the last round is checked
  is never read, and its `delivered_round` stays null. A run that crashed leaves a record
  that looks live, so a note to it is accepted and never read.

### Keeping a run mergeable: `factory update`

Runs started together all branch from the same `main`, so once one PR merges the others
may conflict. `smelt factory update <run-id> [--base BRANCH] [--push] [--json]` fetches
`origin/<base>` (the run's base branch, else the default branch) and merges it into the
run's branch, in the run's worktree, as the run's commit author. The implementer cannot do
this itself: its container has no `main` to merge against. It refuses a run that is still
active.

- **Clean merge:** the merge commit lands on the branch (`result: "clean"`); an
  already-merged base reports `"up-to-date"` and adds no commit. Nothing is pushed unless
  `--push` is given, which pushes only after a clean result, only a `smelt/` branch, never
  forced.
- **Conflicts:** the conflicted files are committed with their markers in place
  (`Merge <base> into <branch> (conflicts to resolve: a.go, b.go)`), listed in the run's
  record as `conflicts`, and reported (`"result":"conflicts"`). The worktree is left clean,
  so `smelt factory build --continue <run-id> "Resolve the merge conflict markers in …"` works.
  That run's prompt names the files and asks for the markers to be removed; the record's
  `conflicts` are cleared when it ends.
- **The baseline moves:** after a clean, conflicted or already-merged update the run's
  `baseline` becomes the merge base of the branch and the base (the base's tip), so a
  continuation's change, reviews, quality, ratchet and PR notes cover only the ticket's own
  work. The record keeps `original_baseline` for history and `updated_from`
  (`{"branch","commit"}`); `smelt factory status --json` reports `baseline` and
  `updated_from`, and `update --json` the new `baseline`. The continuation's prompt and the
  reviewers' request say "This branch was updated from main at <sha>; review only the
  changes since then made for this ticket."
- **The `deletions` check** runs in a continuation of an updated run: it fails while the
  change deletes a file of the baseline whose base name the request (prompt plus guidance)
  does not mention, and lists the files. A run whose last round still fails it is not
  committed; its work stays uncommitted in the worktree.
- **The `markers` check** runs in every factory run: it fails while any tracked file has a
  conflict marker line (`git grep -nE '^(<<<<<<<|=======|>>>>>>>)( |$)'`), so a
  half-resolved merge never passes.

#### Merging into a live run: `--next-round`

`smelt factory update <run-id> --next-round [--base BRANCH] [--json]` on a run that is
still active queues the merge instead of refusing. Without the flag an active run is still
refused. The merge is applied at the end of the current round, after its checks and before
the next implementer turn: the work is set aside, `origin/<base>` is merged the way a
finished run's update does, the implementer and the reviewers are put on the new baseline
(so the next round's change excludes the base's files), and the work is put back.

- **Clean merge:** it lands; the next prompt says which base the branch was updated from
  and that those files are not part of the request.
- **Conflicts:** committed with markers; the next prompt lists the files, and the `markers`
  check holds until they are resolved. Work that does not apply on the merged branch is
  left with markers in the same way.
- The round log gets `round N: synced main at <sha> (k files, m conflicts)`.
- `smelt factory status [--json]` shows each sync as `{"base","state","round","commit",
  "files","conflicts","reason"}`, where `state` is `queued`, `done` or `dropped`.
- A run that ends before a round boundary (it passed, failed or was cancelled in that
  round) drops the queued sync (`state: "dropped"`, with the reason); status says so.
  Asking again for a base that is still queued changes nothing.
- `--next-round --json` prints the queued sync.

Usage errors (an unknown command, the wrong number of arguments) exit with
`ExitBadArgs` and cobra's own message.

## Flag Conventions

- Required flags use cobra's `MarkFlagRequired()`
- Comma-separated lists are split with `strings.Split(s, ",")`
- Dates use `YYYY-MM-DD` format, parsed with `time.Parse("2006-01-02", s)`
- Boolean toggles default to `false`
- Model flags fall back to config file values, then built-in defaults

Factory harness failover accepts `--fallback claude,codex`: these are tried in
caller order after `--harness`, without ranking. `factory.fallback` supplies the
default; an explicitly supplied flag replaces it (including `--fallback=` to
clear it). Duplicate names, including the primary harness, are removed. Only
registered harnesses are accepted; Grok is not registered in this checkout.

Each candidate uses its own daemon credential and `factory.models.<harness>`
model, or that CLI's default. An explicit `--model` applies to the primary agent.
Candidates need their binary installed in the execution image and a valid login.
Rate limits, rejected authentication, missing or crashed agents trigger a retry
of the same turn on the next eligible harness. A first turn deadline retries the
same harness; two consecutive deadlines in that logical turn permit failover.
Caller cancellation, gate failures, review findings and `BLOCKED:` never swap.
Reviews retry individually before any fatal cancellation of the pass.

Before a turn, the highest reported window utilization is compared as a fraction
against `factory.swapAtUtilization` (default `0.95`, valid range greater than zero
and at most one). At or above the threshold, the run swaps before executing.
Failed harnesses are skipped for the rest of that run. Implementer swaps start a
fresh session with the ticket, approved plan, re-anchored context pack, current
partial work and findings; retries do not consume another round.

If no harness remains, the result is `failed`, with reason `no harness available`
and `reset_at` set to the earliest known rate-limit reset. This also applies to
planning and notes; generated fallback notes do not hide exhaustion. Status JSON
and the durable run record retain `harness_swaps` (`round`, `turn`, `from`, `to`,
`kind`, `reset_at`) and per-attempt `turns` with actual harness/model attribution.
Planning, plan reviews and notes use round zero. Logs and the `Time:` line include
phrases such as `swapped codex → claude (rate_limited) in round 2`.

A harness that is at capacity (Codex: "Selected model is at capacity") is busy, not
failing. The turn waits and retries on the same harness after 30s, 1m, 2m and 4m,
logging `round N implementer: codex at capacity, retrying in 1m (2 of 4)`. Then it
moves to the next `--fallback` candidate as for a rate limit. On the last candidate it
keeps waiting, at most 5m between tries, until `factory.capacityWait` (a Go duration,
default `30m`; it includes the first four waits) is spent. Then the result is
`unavailable` with reason such as `codex at capacity for 30m` and `"stop":
"harness_unavailable"` in status JSON and the run record, so a caller can retry the
run later. The round is not counted against `--max-attempts`. Only Codex classifies
capacity today; Claude and grok are left out until a real capture of their messages
exists.

`--continue <run> --harness <name>` uses the requested harness and its credentials
and model. Its fresh session receives the original request, approved plan and
re-anchored pack with the branch's existing work and any new guidance.
