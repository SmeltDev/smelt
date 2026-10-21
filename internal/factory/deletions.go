package factory

import (
	"fmt"

	"github.com/smeltdev/smelt/internal/config"
	"github.com/smeltdev/smelt/internal/sidecar"
)

// DeletionsGateName names the gate that fails a round of an updated run whose
// change deletes files of the base it was updated from.
const DeletionsGateName = "deletions"

// DeletionsGate is the check an updated run's continuation has: it fails while
// the change deletes a file from the baseline (the base's tip, when the run was
// updated) whose name the request does not mention. Files that came in with
// the base are not the ticket's to remove; a deletion the ticket asked for
// names the file, so a file's base name appearing in the request lets it
// through.
func DeletionsGate(request string, from UpdatedFrom) config.Command {
	script := fmt.Sprintf(`request=%s
gone=$(git diff --name-only --diff-filter=D HEAD | while IFS= read -r path; do
  case "$request" in *"$(basename -- "$path")"*) ;; *) printf '%%s\n' "$path" ;; esac
done)
if [ -n "$gone" ]; then
  printf 'These files exist on %%s at %%s and are deleted by the change, but the request does not mention them. Restore them:\n%%s\n' %s %s "$gone"
  exit 1
fi`, sidecar.ShellEscape(request), sidecar.ShellEscape(from.Branch), sidecar.ShellEscape(shortSHA(from.Commit)))
	return config.Command{Name: DeletionsGateName, Run: script}
}

// failedGate reports whether the check named name failed in checks.
func failedGate(checks []Check, name string) bool {
	for _, c := range checks {
		if c.Kind == KindValidate && c.Name == name && c.Status == StatusFailed {
			return true
		}
	}
	return false
}
