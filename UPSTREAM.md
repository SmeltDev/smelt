# Upstream

smelt is a fork of <https://github.com/CircleCI-Public/chunk-cli>.

Synced through: 364f24c4db0bfecbc5e8ebd4c98bd84ee3f685ff

Fork point: `80fc0955ceeefed47b7f5efe97736120dd2389e1` ("Stop factory reviewers from blocking on nitpicks (#716)", 2026-10-10).

## Why we don't `git merge`

Our module path, names and agent harness differ throughout (`internal/brand`,
`internal/harness`), so an upstream merge conflicts in most factory and review
files. We review each upstream commit and port what's valuable.

## Decision log

Newest first, one row per upstream commit reviewed. Decisions: `taken`,
`ported`, `ticket`, `skipped`.

| Commit | Date | Title | Decision | Why |
|---|---|---|---|---|
| 364f24c | 2026-10-13 | Lock file maintenance (`harness/uv.lock`) | skipped | lock-file churn only; renovate refreshes ours |
| ec0fc4b | 2026-10-13 | golang.org/x/crypto 0.57.0 → 0.58.0 | taken | this ticket |
| 1957045 | 2026-10-10 | `factory bootstrap`: write a project's first review prompts (#709) | ported | `upstream-3-factory-bootstrap`; rebuilt on our harness seam and factory backend (PR number to add) |
| 0a407f8 | 2026-10-10 | Move the Claude Code helpers into `internal/claudecode` (#706) | ported | `internal/harness` already does the move for Claude and Codex; its "timed out after 0s" fix is `harness.TimeoutError` (PR number to add) |
| 3a1ce38 | 2026-10-10 | Start the factory implementer on the first ready sidecar (#715) | ported | `upstream-2-first-ready-sidecar` (PR number to add) |

## Next sync

1. Run `task upstream:check` to list upstream commits since `Synced through:`.
2. Add a row per commit: take dependency and bug fixes directly, write a ticket for features.
3. Move `Synced through:` to upstream's newest reviewed commit.
