#!/usr/bin/env bash
# Build the smelt agent kit image. The one entry for `task kit` and the release
# job: the pins come from internal/sandboximage, so they have one home.
#
#   kit/build.sh --tag IMAGE [--smelt-version V] [--platform P] [--push]
#
# Without --push the image is loaded into the local Docker daemon (one
# platform); with --push buildx publishes the platforms given (default
# linux/amd64,linux/arm64).
set -euo pipefail

root=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
tag=smelt-kit:local
smelt_version=dev
platform=
push=

while [ $# -gt 0 ]; do
  case "$1" in
    --tag) tag=$2; shift 2 ;;
    --smelt-version) smelt_version=$2; shift 2 ;;
    --platform) platform=$2; shift 2 ;;
    --push) push=1; shift ;;
    *) echo "unknown argument: $1" >&2; exit 2 ;;
  esac
done

args=(--build-arg "SMELT_VERSION=$smelt_version")
while IFS= read -r pin; do
  args+=(--build-arg "$pin")
done < <(cd "$root" && go run ./kit/pins)

if [ -n "$push" ]; then
  exec docker buildx build "${args[@]}" --platform "${platform:-linux/amd64,linux/arm64}" \
    --tag "$tag" --push "$root/kit"
fi
if [ -n "$platform" ]; then
  args+=(--platform "$platform")
fi
exec docker buildx build "${args[@]}" --tag "$tag" --load "$root/kit"
