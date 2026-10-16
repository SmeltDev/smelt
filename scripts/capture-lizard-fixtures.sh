#!/usr/bin/env bash
# Capture real lizard output for internal/quality/testdata/real/.
#
# The adapter tests parse these captures, so they must come from lizard itself:
# never edit them by hand. Run this after changing LizardVersion, the adapter's
# expectations or the fixture sources, and commit what it writes.
#
#   scripts/capture-lizard-fixtures.sh            # installs lizard in a fresh venv
#   LIZARD=/path/to/lizard scripts/capture-lizard-fixtures.sh   # use this lizard
#
# For each fixture project it writes <name>.version.txt, <name>.csv (lizard
# --csv) and <name>.dup.txt (lizard -Eduplicate), the same three runs the
# adapter's script makes. The "go" fixture is a copy of a few of internal/quality's
# own sources, refreshed on every run.
set -euo pipefail

root=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
out="$root/internal/quality/testdata/real"
fixtures="$out/fixtures"

version=$(sed -n 's/^[[:space:]]*LizardVersion = "\(.*\)"$/\1/p' "$root/internal/sandboximage/render.go")
if [ -z "$version" ]; then
	echo "could not read LizardVersion from internal/sandboximage/render.go" >&2
	exit 1
fi

if [ -z "${LIZARD:-}" ]; then
	venv=$(mktemp -d)
	trap 'rm -rf "$venv"' EXIT
	python3 -m venv "$venv"
	"$venv/bin/pip" install --quiet "lizard==$version"
	LIZARD="$venv/bin/lizard"
fi

rm -rf "${fixtures:?}/go"
mkdir -p "$fixtures/go"
(cd "$root" && for f in assess clones compare exclude hash metrics source; do cp --parents "internal/quality/$f.go" "$fixtures/go/"; done)

for dir in "$fixtures"/*/; do
	name=$(basename "$dir")
	(
		cd "$dir"
		find . -type f | sed 's|^\./||' | LC_ALL=C sort | tr '\n' '\0' >"$out/.files"
		"$LIZARD" --version >"$out/$name.version.txt"
		xargs -0 "$LIZARD" --csv <"$out/.files" >"$out/$name.csv"
		xargs -0 "$LIZARD" -Eduplicate <"$out/.files" >"$out/$name.dup.txt"
	)
	echo "captured $name ($("$LIZARD" --version))"
done
rm -f "$out/.files"
