# The agent kit image

`/opt/smelt` holds the tools factory agents use: `claude`, `codex`,
`codex-code-mode-host`, `grok`, `lizard`, `git`, `bash`, `pkill` and `sha256sum` in `bin/`, `env.sh` to put them on
`PATH`, and `manifest.json` naming each tool, version and checksum. The
Dockerfile builds it into a `scratch` image; `smelt factory image` copies it
into the sandbox image with `COPY --from=<kit image> /opt/smelt /opt/smelt`.

Nothing in it needs apk or apt. Codex and Grok are static. Claude and Python
(for lizard) are linked to musl, so `lib/` carries musl's loader and the
wrappers in `bin/` start them through it; that works on glibc and musl bases.

## Bumping a tool

1. Change the pin in `internal/sandboximage` (`render.go` for Codex, Grok and
   lizard; `kitimage.go` for Python and Claude) with its checksum.
2. Run `go test ./internal/sandboximage/`. A test fails until the ARG defaults
   in `kit/Dockerfile` equal the Go pins; copy them over (`go run ./kit/pins`
   prints them).
3. Raise `KitVersion` in `internal/sandboximage/kit.go`. It names the tag the
   release publishes (`:<KitVersion>`), so every change of content needs a new
   one.
4. `task kit` builds it locally and `task test:kit` runs `kit/test.sh`.

Claude's version and both musl architecture checksums are pinned in `KitPins`,
passed through both build paths, and recorded in `manifest.json`.

`smelt harness status` shows host CLI versions alongside sandbox versions from
the manifest. To select a newer published kit without upgrading smelt, run
`smelt harness refresh --kit-ref ghcr.io/smeltdev/smelt-kit:6`, then
`smelt factory image`. The selection persists for future factory runs.

`smelt factory image --kit-from-source` builds this Dockerfile locally instead
of pulling the published image.

The kit bundles the runtime prerequisites with musl libraries and wrappers;
copying it onto a minimal Debian or Alpine toolchain needs no package installs.
A source-built kit is remembered and reused when registry pulls fail, including
after a CLI upgrade. The fallback reports the cached kit's actual version.
