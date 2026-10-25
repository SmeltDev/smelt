#!/usr/bin/env bash
# Publish the kit under both tags and sign it by digest. The release job and the
# CI registry job run this same script, so what CI checks is what a release does.
#
#   KIT_REPO=ghcr.io/smeltdev/smelt-kit kit/publish.sh SMELT_VERSION [PLATFORMS]
#
# COSIGN_ARGS adds flags to `cosign sign` (CI passes a key and a registry
# without a transparency log; a release signs keyless).
set -euo pipefail

root=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
version=${1:?usage: kit/publish.sh SMELT_VERSION [PLATFORMS]}
platforms=${2:-linux/amd64,linux/arm64}
repo=${KIT_REPO:?KIT_REPO is required}

kit=$(cd "$root" && go run ./kit/pins | sed -n 's/^KIT_VERSION=//p')
"$root/kit/build.sh" --push --smelt-version "$version" --platform "$platforms" --tag "$repo:$kit"
docker buildx imagetools create --tag "$repo:$kit-$version" "$repo:$kit"
digest=$(docker buildx imagetools inspect "$repo:$kit" --format '{{ .Manifest.Digest }}')
# shellcheck disable=SC2086 # COSIGN_ARGS is a word list
cosign sign --yes ${COSIGN_ARGS:-} "$repo@$digest"
echo "Published $repo:$kit and :$kit-$version at $digest"
