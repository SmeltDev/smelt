# The smelt agent kit: every tool the factory's agents need, in one directory,
# /opt/smelt, that any Linux base image can COPY in. Built and published by the
# release workflow (see kit/build.sh); `smelt factory image` copies it into the
# sandbox image in place of the old kit section of Dockerfile.smelt.
#
# The ARG defaults below are the pins in internal/sandboximage (KitPins); a test
# keeps them equal, and kit/build.sh passes them anyway. Change them together.
#
# Nothing here is a shell script run inside the kit: the final image is
# `scratch`. The tools need only a Linux kernel and /tmp:
#   - codex, codex-code-mode-host and grok are static executables;
#   - claude and python (for lizard) are linked to musl, so the kit ships musl's
#     loader in lib/ and the wrappers in bin/ start them through it. That runs
#     on glibc and musl bases alike, with no gcompat and no apk or apt.
#   - git, bash, procps and coreutils use the same loader and bundled libraries.
# Kept free of heredocs and BuildKit-only syntax so the classic builder that
# --kit-from-source uses builds it too.

ARG BUILDER=alpine:3.22
FROM ${BUILDER} AS builder

ARG KIT_VERSION=5
ARG SMELT_VERSION=dev
ARG CODEX_VERSION=0.162.0
ARG CODEX_URL_BASE=https://github.com/openai/codex/releases/download/rust-v
ARG CODEX_SHA_X86_64=8daf67f6261161aa5939d8d42a516032d760406216779d7ce6040f242140ff73
ARG CODEX_SHA_AARCH64=15162a9b59edf8e512b27414ec0b7db22e6424a1262b0eee8666643e0d295c2f
ARG CODEX_HOST_SHA_X86_64=dee8bf3df637c68e1495b928d643b76d4ad44f94e4f0ed79fe8482fa79e23d97
ARG CODEX_HOST_SHA_AARCH64=18d99b055d2ef4a2af51f1144919bd8a208f4f79ace389b7775341d6903cf7e9
ARG GROK_VERSION=1.0.62
ARG GROK_URL_BASE=https://x.ai/cli/grok-
ARG GROK_SHA_X86_64=2a46d9e4d7ebd7a9cff7f95716714b19dad5f21dd2a497822400b1fb128969aa
ARG GROK_SHA_AARCH64=0b808a2209728788d2e37c34ea1846d460b41528a9fc7edc7698b9e7dec12fe1
ARG PYTHON_VERSION=3.12.15
ARG PBS_RELEASE=20261021
ARG PBS_SHA_X86_64=b208f54027fd6503879723f8dea7ba4f1fdfb9c79f5d4f8aa431435109788b79
ARG PBS_SHA_AARCH64=b232d56e2aad0ddc20387f9618bbcf1fe9c61e73e33bddf3392954024b1a47e0
ARG LIZARD_VERSION=1.24.1
ARG LIZARD_SHA256=746060a3e9a375af7340b2d4c6aab307b8a8b4199339c36ddc7afc4188b53d1c
ARG PYGMENTS_VERSION=2.21.0
ARG PYGMENTS_SHA256=2363c69b61c4a97c838da3b130dcd6468f4848992b21a82f2a63ec34377137d9
ARG PATHSPEC_VERSION=1.1.1
ARG PATHSPEC_SHA256=a00ce642f577bf7f473932318056212bc4f8bfdf53128c78bbd5af0b9b20b189
ARG CLAUDE_VERSION=2.1.310
ARG CLAUDE_SHA_X86_64=2ceb05e0a97b09be4c14717464a2156e8d087dea9867a399dd75d3b898ca4824
ARG CLAUDE_SHA_AARCH64=adfd1f0ef12a3bcafa66e33491fc3e4204e41aeb4a7305ffc311c16c26793825
ARG CLAUDE_URL_BASE=https://downloads.claude.ai/claude-code-releases

RUN apk add --no-cache bash curl jq ca-certificates git procps coreutils
COPY runtime.sh /work/runtime.sh

# fetch URL DEST SHA256: download, then fail the build unless the checksum
# matches. kitenv sets $a (x86_64|aarch64) and $k (X86_64|AARCH64) for this
# machine. record NAME VERSION SHA256 adds a tool to the manifest.
RUN mkdir -p /opt/smelt/bin /opt/smelt/lib /opt/smelt/libexec /work \
 && printf '%s\n' '#!/bin/sh' 'set -eu' \
      'curl -fsSL --retry 3 -o "$2" "$1"' \
      'echo "$3  $2" | sha256sum -c -' > /usr/local/bin/fetch \
 && printf '%s\n' \
      'case "$(uname -m)" in' \
      '  x86_64) a=x86_64; k=X86_64 ;;' \
      '  aarch64|arm64) a=aarch64; k=AARCH64 ;;' \
      '  *) echo "unsupported architecture: $(uname -m)" >&2; exit 1 ;;' \
      'esac' > /usr/local/bin/kitenv \
 && printf '%s\n' '#!/bin/sh' 'set -eu' \
      'jq -cn --arg name "$1" --arg version "$2" --arg sha256 "$3" '"'"'{name:$name,version:$version,sha256:$sha256}'"'"' >> /work/tools.ndjson' > /usr/local/bin/record \
 && chmod +x /usr/local/bin/fetch /usr/local/bin/record \
 && cp -L /lib/ld-musl-*.so.1 /opt/smelt/lib/

# codex and its command host: static musl executables.
RUN set -eu; . kitenv; \
    for t in codex codex-code-mode-host; do \
      case "$t" in codex) sha=$(printenv "CODEX_SHA_$k") ;; *) sha=$(printenv "CODEX_HOST_SHA_$k") ;; esac; \
      name="$t-$a-unknown-linux-musl"; \
      fetch "$CODEX_URL_BASE$CODEX_VERSION/$name.tar.gz" "/work/$name.tar.gz" "$sha"; \
      tar -xzf "/work/$name.tar.gz" -C /work; \
      mv "/work/$name" "/opt/smelt/bin/$t"; \
      record "$t" "$CODEX_VERSION" "$sha"; \
    done

# grok: a static executable.
RUN set -eu; . kitenv; \
    sha=$(printenv "GROK_SHA_$k"); \
    fetch "$GROK_URL_BASE$GROK_VERSION-linux-$a" /opt/smelt/bin/grok "$sha"; \
    chmod +x /opt/smelt/bin/grok; \
    record grok "$GROK_VERSION" "$sha"

# claude: the musl build, run through the bundled loader.
RUN set -eu; . kitenv; \
    case "$a" in x86_64) p=linux-x64-musl ;; *) p=linux-arm64-musl ;; esac; \
    v="$CLAUDE_VERSION"; \
    sha=$(printenv "CLAUDE_SHA_$k"); \
    fetch "$CLAUDE_URL_BASE/$v/$p/claude" /opt/smelt/libexec/claude "$sha"; \
    chmod +x /opt/smelt/libexec/claude; \
    record claude "$v" "$sha"

# python (relocatable, musl) and lizard from hash-pinned wheels.
RUN set -eu; . kitenv; \
    sha=$(printenv "PBS_SHA_$k"); \
    fetch "https://github.com/astral-sh/python-build-standalone/releases/download/$PBS_RELEASE/cpython-$PYTHON_VERSION%2B$PBS_RELEASE-$a-unknown-linux-musl-install_only_stripped.tar.gz" /work/python.tar.gz "$sha"; \
    tar -xzf /work/python.tar.gz -C /opt/smelt/libexec; \
    record python "$PYTHON_VERSION" "$sha"; \
    printf '%s\n' \
      "lizard==$LIZARD_VERSION --hash=sha256:$LIZARD_SHA256" \
      "pygments==$PYGMENTS_VERSION --hash=sha256:$PYGMENTS_SHA256" \
      "pathspec==$PATHSPEC_VERSION --hash=sha256:$PATHSPEC_SHA256" > /work/requirements.txt; \
    /opt/smelt/libexec/python/bin/python3 -m pip install --no-cache-dir --require-hashes --only-binary=:all: -r /work/requirements.txt; \
    record lizard "$LIZARD_VERSION" "$LIZARD_SHA256"

# Wrappers: each finds the kit from its own path, so the directory can sit
# anywhere. They are the only scripts, and need only /bin/sh.
RUN set -eu; \
    wrap() { \
      printf '%s\n' '#!/bin/sh' \
        'd=$(cd "$(dirname "$0")/.." && pwd -P)' \
        'export PYTHONDONTWRITEBYTECODE=1 PYTHONNOUSERSITE=1' \
        "exec \"\$d\"/lib/ld-musl-*.so.1 $2 \"\$@\"" > "/opt/smelt/bin/$1"; \
      chmod +x "/opt/smelt/bin/$1"; \
    }; \
    wrap claude '"$d/libexec/claude"'; \
    wrap lizard '"$d/libexec/python/bin/python3" -m lizard'; \
    printf '%s\n' \
      '# Source this to put the smelt agent kit on PATH.' \
      'SMELT_KIT=${SMELT_KIT:-/opt/smelt}' \
      'export SMELT_KIT' \
      'PATH="$SMELT_KIT/bin:$PATH"' \
      'export PATH' > /opt/smelt/env.sh; \
    jq -s --arg kit "$KIT_VERSION" --arg smelt "$SMELT_VERSION" --arg arch "$(uname -m)" \
      '{kit:($kit|tonumber),smelt:$smelt,arch:$arch,tools:.}' /work/tools.ndjson > /opt/smelt/manifest.json; \
    /opt/smelt/bin/lizard --version; \
    /opt/smelt/bin/claude --version

RUN sh /work/runtime.sh

FROM scratch
ARG KIT_VERSION=5
ARG SMELT_VERSION=dev
LABEL org.opencontainers.image.title="smelt agent kit" \
      org.opencontainers.image.source="https://github.com/smeltdev/smelt" \
      smelt.kit=$KIT_VERSION \
      org.opencontainers.image.version=$SMELT_VERSION
COPY --from=builder /opt/smelt /opt/smelt
