#!/usr/bin/env bash
# Lists the commits upstream (CircleCI-Public/chunk-cli, which smelt forked)
# has made since the commit recorded in UPSTREAM.md. Read-only apart from adding
# the upstream remote, fetching it and pinning gh's default repository.
#
# Usage: task upstream:check -- [--since REV] [--remote NAME] [--branch NAME]
set -euo pipefail

# UPSTREAM_URL is a test seam.
upstream_url=${UPSTREAM_URL:-https://github.com/CircleCI-Public/chunk-cli.git}
remote=upstream
branch=main
since=

usage() {
	echo "usage: upstream-check.sh [--since REV] [--remote NAME] [--branch NAME]" >&2
	exit 2
}

while [ $# -gt 0 ]; do
	case $1 in
	--since | --remote | --branch)
		[ $# -ge 2 ] || usage
		case $1 in
		--since) since=$2 ;;
		--remote) remote=$2 ;;
		--branch) branch=$2 ;;
		esac
		shift 2
		;;
	*) usage ;;
	esac
done

cd "$(git rev-parse --show-toplevel)"

if ! git remote get-url "$remote" >/dev/null 2>&1; then
	echo "Adding remote '$remote' ($upstream_url)"
	git remote add "$remote" "$upstream_url"
fi

# gh prefers a remote named upstream over origin, so pin it to origin.
if git remote get-url origin >/dev/null 2>&1 && [ -z "$(git config --get remote.origin.gh-resolved || true)" ]; then
	git config remote.origin.gh-resolved base
	echo "Set remote.origin.gh-resolved=base so gh keeps targeting origin"
fi

git fetch --quiet "$remote"

if [ -z "$since" ]; then
	if [ ! -f UPSTREAM.md ]; then
		echo "upstream-check: UPSTREAM.md not found; pass --since REV" >&2
		exit 1
	fi
	since=$(sed -n 's/^Synced through:[[:space:]]*//p' UPSTREAM.md | head -n 1 | awk '{print $1}')
	if [ -z "$since" ]; then
		echo "upstream-check: no 'Synced through:' line in UPSTREAM.md; pass --since REV" >&2
		exit 1
	fi
fi

if ! full=$(git rev-parse --verify --quiet "${since}^{commit}"); then
	echo "upstream-check: '$since' is not a known (or is an ambiguous) commit" >&2
	exit 1
fi
tip="$remote/$branch"
if ! git rev-parse --verify --quiet "${tip}^{commit}" >/dev/null; then
	echo "upstream-check: $tip does not exist" >&2
	exit 1
fi
if ! git merge-base --is-ancestor "$full" "$tip"; then
	echo "upstream-check: $since is not in $tip" >&2
	exit 1
fi

short=$(git rev-parse --short=7 "$full")
count=0
while IFS=$'\t' read -r sha date title; do
	stat=$(git show --shortstat --format= "$sha" | sed 's/^ *//')
	printf '%s  %s  %s\n    %s\n' "$sha" "$date" "$title" "${stat:-no file changes}"
	count=$((count + 1))
done < <(git log --no-merges --reverse --format='%h%x09%as%x09%s' "$full..$tip")

if [ "$count" -eq 0 ]; then
	echo "Up to date with upstream ($short)"
else
	echo "$count new upstream commit(s) since $short"
fi
