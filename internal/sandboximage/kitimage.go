package sandboximage

import (
	"strconv"
	"strings"

	"github.com/smeltdev/smelt/internal/brand"
	"github.com/smeltdev/smelt/internal/hashutil"
)

// The agent kit is also published as an image of its own, built from kit/ by
// the release workflow. It holds one directory, KitDir, that any Linux base
// image can COPY in: the agent CLIs, a relocatable Python with lizard, an
// env.sh and a manifest.json. KitVersion numbers it.
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
	// ClaudeVersion pins Claude together with its architecture checksums.
	ClaudeVersion = "2.1.310"
	claudeSumX86  = "2ceb05e0a97b09be4c14717464a2156e8d087dea9867a399dd75d3b898ca4824"
	claudeSumARM  = "adfd1f0ef12a3bcafa66e33491fc3e4204e41aeb4a7305ffc311c16c26793825"
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
		{"CLAUDE_VERSION", ClaudeVersion},
		{"CLAUDE_SHA_X86_64", claudeSumX86},
		{"CLAUDE_SHA_AARCH64", claudeSumARM},
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

// toolchainSuffix ends the repository name of a toolchain image, which keeps
// its tags apart from the sandbox image's.
const toolchainSuffix = "-toolchain"

// ToolchainDockerfile is dockerfile as the toolchain image builds it: everything
// above the agent-kit marker of an older file, which smelt no longer uses, or
// the whole file when it has none. It is pure.
func ToolchainDockerfile(dockerfile string) string {
	off := kitMarkerOffset(dockerfile)
	if off < 0 {
		return dockerfile
	}
	return strings.TrimRight(dockerfile[:off], "\n") + "\n"
}

// kitHarnessLabel tells `smelt harness status` which agent CLIs the image has
// and which kit built it.
func kitHarnessLabel(version int) string {
	return "LABEL " + LabelHarnesses + "=claude,codex,grok " + LabelKit + "=" + strconv.Itoa(version) + "\n"
}

// SandboxDockerfile is the generated Dockerfile of the sandbox image: the
// toolchain image with the kit copied in and put on PATH. It has no USER and no
// package manager, so the sandbox runs as the toolchain's user. The ENV lines
// are what the kit's env.sh sets (a test keeps them equal). It is pure.
func SandboxDockerfile(toolchainTag, kitRef string) string {
	return sandboxDockerfile(toolchainTag, KitChoice{Ref: kitRef, Kit: KitVersion})
}

func sandboxDockerfile(toolchainTag string, choice KitChoice) string {
	return "FROM " + toolchainTag + "\n" +
		"COPY --from=" + choice.Ref + " " + KitDir + " " + KitDir + "\n" +
		"ENV SMELT_KIT=" + KitDir + "\n" +
		"ENV PATH=" + KitDir + "/bin:$PATH\n" +
		kitHarnessLabel(choice.Kit)
}

// SandboxTag names the sandbox image built from the toolchain image
// toolchainTag and the kit kitRef: the toolchain's repository without its
// suffix, and a digest of both, so a new kit changes only this tag. It is pure.
func SandboxTag(toolchainTag, kitRef string) string {
	repo, _, _ := strings.Cut(toolchainTag, ":")
	return strings.TrimSuffix(repo, toolchainSuffix) + ":" + hashutil.SumParts(toolchainTag, kitRef)[:tagHexLen]
}
