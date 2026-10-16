# Real lizard captures

`*.version.txt`, `*.csv` and `*.dup.txt` are the output of `lizard --version`,
`lizard --csv` and `lizard -Eduplicate` over `fixtures/<name>/`, written by
`scripts/capture-lizard-fixtures.sh`. Never edit them by hand: the adapter tests
in `internal/quality/lizard` parse them, and they must be what lizard prints.
Run the script again after changing `sandboximage.LizardVersion`, the fixture
sources or what the adapter relies on.

- `python` and `typescript` are small projects with one complex function, a
  copied block, a test file and a generated file.
- `go` is a snapshot of seven of `internal/quality`'s own sources, taken by the
  script. The committed `go.csv` and `go.dup.txt` are the original capture with
  the lines for the dropped files removed; the next run rewrites them in full.

These were captured with lizard 1.24.1 (the version the kit pins).

## This repo against the old `go/ast` measurement

Measured on the same tree (the commit before the provider contract) with the
old `smelt quality --json` and with lizard 1.24.1 through the new pipeline:

| | `go/ast` (before) | lizard (after) |
|---|---|---|
| erosion | 39.9 % | 35.3 % (32.8 % for the named Go functions alone) |
| functions over 15 | 61 | 54 (53 in Go) |
| clone lines | 401 | 69 |

Why they differ: `go/ast` counted a closure toward the function that holds it
and lizard reports it as a function of its own, which lowers the holder's
complexity. Folding lizard's anonymous functions into their holder, and
measuring length from brace to brace, gives 39.6 % and 59 functions over 15, so
that is the whole gap in erosion. Clone lines differ because lizard finds
duplicates token by token (`minCloneLines` is now a number of lines) where
`go/ast` compared runs of statements. The thresholds were not adjusted.
