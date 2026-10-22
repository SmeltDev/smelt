`codex-exec-host-missing.jsonl` is **reconstructed**, not captured: it follows the
event shapes of `../../../testdata/real/codex-exec-pass.jsonl` (codex-cli 0.162.0) and
quotes the summary of the pilot run that hit the missing `codex-code-mode-host`
(agent message, path scrubbed to `/home/circleci/...`). The failed command's output
line is an assumption about how Codex words the error. Swap in the real scrubbed log
when there is one; the parser test only needs the phrase to appear.

`codex-exec-at-capacity.jsonl` is **reconstructed**, not captured. The repo holds only
the message text from run `20261022-075541` (`codex exited 1: Selected model is at
capacity. Please try a different model.`), not the event JSON. The `error` and
`turn.failed` events follow the shape of the usage-limit failure that
`TestRealConsecutiveUsageFailuresAreClassified` quotes. Swap in the real scrubbed log
when there is one; the tests only need the phrase to appear.

Only Codex classifies a capacity refusal. Claude's `overloaded_error`/529 and grok's
equivalent are deliberately not classified until a real capture of each exists.
