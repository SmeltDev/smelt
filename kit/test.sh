#!/usr/bin/env bash
# Check a kit image: its tools run on glibc and musl bases, and a tampered
# download fails the build.
#
#   kit/test.sh IMAGE
set -euo pipefail

image=${1:?usage: kit/test.sh IMAGE}
bases=(debian:12-slim ubuntu:24.04 alpine:3.22 fedora:42)
root=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)

for base in "${bases[@]}"; do
  echo "== $base"
  tmp=$(mktemp -d)
  printf 'FROM %s\nCOPY --from=%s /opt/smelt /opt/smelt\n' "$base" "$image" > "$tmp/Dockerfile"
  docker build -q -t "smelt-kit-test:$$" "$tmp" >/dev/null
  # As root, and as an ordinary user whose only writable place is /tmp: a mode
  # or ownership the kit relies on shows up in the second run.
  for user in 0:0 1000:1000; do
    echo "   user $user"
    docker run --rm --user "$user" -e HOME=/tmp "smelt-kit-test:$$" sh -c '
      set -e
      . /opt/smelt/env.sh
      git --version
      git init /tmp/runtime-check
      git -C /tmp/runtime-check status
      test -x "$(git --exec-path)/git-remote-https"
      bash -c "echo bash-ok"
      pkill --version
      sha256sum --version
      claude --version
      codex --version
      command -v codex-code-mode-host
      grok --version
      lizard --version
      test -s /opt/smelt/manifest.json'
  done
  docker rmi -f "smelt-kit-test:$$" >/dev/null
  rm -rf "$tmp"
done

echo "== tamper check"
if docker build -q --build-arg GROK_SHA_X86_64=0000000000000000000000000000000000000000000000000000000000000000 \
  --build-arg GROK_SHA_AARCH64=0000000000000000000000000000000000000000000000000000000000000000 \
  -t smelt-kit-tamper:$$ "$root/kit" >/dev/null 2>&1; then
  echo "a wrong checksum did not fail the kit build" >&2
  exit 1
fi
echo "ok"
