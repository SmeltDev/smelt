package harness

import (
	"fmt"
	"strings"
)

// GuardedGit are the git commands that would move the implementer's work out
// of the working tree the loop collects it from.
var GuardedGit = []string{"commit", "stash", "reset", "checkout", "switch", "rebase"}

// GitGuardScript is the git wrapper, installed as <guardDir>/git ahead of the
// real git on PATH, that refuses the GuardedGit commands and logs each attempt
// to <guardDir>/attempts.log. It stands in for a CLI that cannot disallow
// commands itself.
func GitGuardScript(guardDir string) string {
	return fmt.Sprintf(`#!/bin/sh
# Refuses the git commands that move work out of the working tree.
sub=
skip=
for a in "$@"; do
  if [ -n "$skip" ]; then skip=; continue; fi
  case "$a" in
    -C|-c|--git-dir|--work-tree|--namespace) skip=1 ;;
    -*) ;;
    *) sub=$a; break ;;
  esac
done
case "$sub" in
  %s)
    echo "$sub $*" >> %s/attempts.log
    echo "smelt: git $sub is not allowed here; leave your changes uncommitted in the working tree" >&2
    exit 1 ;;
esac
PATH=${PATH#%s:}
exec git "$@"
`, strings.Join(GuardedGit, "|"), guardDir, guardDir)
}
