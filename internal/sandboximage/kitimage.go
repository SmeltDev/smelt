package sandboximage

import (
	"strings"

	"github.com/smeltdev/smelt/internal/brand"
)

// The agent kit is also published as an image of its own, built from kit/ by
// the release workflow. It holds one directory, KitDir, that any Linux base
// image can COPY in: the agent CLIs, a relocatable Python with lizard, an
// env.sh and a manifest.json. KitVersion numbers it, like the Dockerfile
// section it replaces.
const (
	// KitDir is where the kit image keeps everything, and where a sandbox image
	// gets it.
	KitDir = "/opt/" + brand.Command
	// KitRegistryRepo is where the release publishes the kit, tagged with
	// KitVersion and with KitVersion-<smelt version>.
	KitRegistryRepo = "ghcr.io/smeltdev/" + brand.Command + "-kit"
	// kitLocalRepo is the repository of a kit built on this machine.
	kitLocalRepo = brand.Command + "-kit"
)

// Python for lizard is python-build-standalone's musl build, which is linked
// to its own C library and so runs on glibc and musl bases alike when started
// through the loader the kit ships. The release and the checksums of the two
// archives are pinned together: change them together (a test keeps kit/Dockerfile
// in step).
const (
	PythonVersion = "3.12.15"
	pbsRelease    = "20261021"
	pbsSumX86     = "b208f54027fd6503879723f8dea7ba4f1fdfb9c79f5d4f8aa431435109788b79"
	pbsSumARM     = "b232d56e2aad0ddc20387f9618bbcf1fe9c61e73e33bddf3392954024b1a47e0"
)

// KitTagRef is the registry reference the release publishes the kit under.
func KitTagRef() string { return KitRegistryRepo + ":" + kitVersionString() }

// KitLocalRef is the tag of a kit built on this machine (task kit, or
// --kit-from-source).
func KitLocalRef() string { return kitLocalRepo + ":" + kitVersionString() }

// KitArg is one build argument of kit/Dockerfile.
type KitArg struct{ Name, Value string }

// KitPins are the build arguments that pin the kit's downloads. kit/build.sh
// passes them, and a test keeps the defaults in kit/Dockerfile equal to them,
// so the constants here stay the one source.
func KitPins() []KitArg {
	pins := make([]KitArg, 0, 15+2*len(lizardWheels))
	pins = append(pins, []KitArg{
		{"KIT_VERSION", kitVersionString()},
		{"CODEX_VERSION", CodexVersion},
		{"CODEX_URL_BASE", codexReleaseURLBase},
		{"CODEX_SHA_X86_64", codexMain.sumX86},
		{"CODEX_SHA_AARCH64", codexMain.sumARM},
		{"CODEX_HOST_SHA_X86_64", codexHost.sumX86},
		{"CODEX_HOST_SHA_AARCH64", codexHost.sumARM},
		{"GROK_VERSION", GrokVersion},
		{"GROK_URL_BASE", grokURLBase},
		{"GROK_SHA_X86_64", grokSumX86},
		{"GROK_SHA_AARCH64", grokSumARM},
		{"PYTHON_VERSION", PythonVersion},
		{"PBS_RELEASE", pbsRelease},
		{"PBS_SHA_X86_64", pbsSumX86},
		{"PBS_SHA_AARCH64", pbsSumARM},
	}...)
	for _, w := range lizardWheels {
		up := strings.ToUpper(w.name)
		pins = append(pins, KitArg{up + "_VERSION", w.version}, KitArg{up + "_SHA256", w.sha256})
	}
	return pins
}

// kitLayerBootstrap installs the few system tools the factory's own scripts
// use, only when the base image lacks them and has apt. The kit itself needs
// none of it; this keeps a Debian base without git or procps working.
const kitLayerBootstrap = `RUN missing=; for t in git pkill sha256sum bash; do command -v "$t" >/dev/null 2>&1 || missing=1; done; \
    if [ -n "$missing" ] && command -v apt-get >/dev/null 2>&1; then \
      apt-get update && apt-get install -y --no-install-recommends git coreutils procps ca-certificates bash \
      && rm -rf /var/lib/apt/lists/*; \
    fi; \
    if command -v git >/dev/null 2>&1; then git config --system safe.directory '*'; fi
`

// ComposeWithKitImage is dockerfile with its agent-kit section replaced by a
// layer that copies the kit image imageRef into the image: everything above the
// marker is kept as the toolchain. A Dockerfile with no marker is returned as it
// is, since smelt cannot tell where its own kit would go. It is pure.
func ComposeWithKitImage(dockerfile, imageRef string) string {
	off := kitMarkerOffset(dockerfile)
	if off < 0 {
		return dockerfile
	}
	user := kitUser(dockerfile[off:])
	return dockerfile[:off] + kitMarker() +
		asUser(rootUser, user, kitLayerBootstrap) +
		"COPY --from=" + imageRef + " " + KitDir + " " + KitDir + "\n" +
		"ENV PATH=" + KitDir + "/bin:$PATH\n" +
		kitHarnessLabel() + kitCmd
}
