package sandboximage

import (
	"regexp"
	"strconv"
	"strings"
)

// KitVersion numbers the agent kit: the kit image built from kit/ and published
// under this number as its tag (see kitimage.go), and the label of every sandbox
// image. Bump it whenever the kit's contents change. Older Dockerfiles carried
// the kit as a section opened by a marker line; the number in that marker is
// the version it was written with: 1 is a Dockerfile without a marker (Claude
// only); 2 adds Codex and the harness label; 3 adds codex-code-mode-host, which
// Codex runs every command through; 4 adds python3 and lizard, which measures
// code quality in any language; 5 adds the Grok CLI.
const KitVersion = 5

// kitMarkerPrefix starts the line that opens the agent-kit section.
const kitMarkerPrefix = "# --- smelt agent kit"

// kitMarkerRE parses a marker line: "# --- smelt agent kit (v2) ---...".
var kitMarkerRE = regexp.MustCompile(`^# --- smelt agent kit(?: \(v(\d+)\))?(?: |-|$)`)

func kitVersionString() string { return strconv.Itoa(KitVersion) }

// kitMarkerOffset is the byte offset of the first marker line in dockerfile,
// or -1.
func kitMarkerOffset(dockerfile string) int {
	off := 0
	for line := range strings.SplitAfterSeq(dockerfile, "\n") {
		if kitMarkerRE.MatchString(strings.TrimRight(line, "\r\n")) {
			return off
		}
		off += len(line)
	}
	return -1
}

// KitVersionOf is the agent-kit version of dockerfile. found reports whether
// it has a marker; without one the version is 1, the kit before versions
// existed. A marker without a readable number also counts as 1. It is pure.
func KitVersionOf(dockerfile string) (version int, found bool) {
	off := kitMarkerOffset(dockerfile)
	if off < 0 {
		return 1, false
	}
	line, _, _ := strings.Cut(dockerfile[off:], "\n")
	m := kitMarkerRE.FindStringSubmatch(strings.TrimRight(line, "\r"))
	if m[1] == "" {
		return 1, true
	}
	v, err := strconv.Atoi(m[1])
	if err != nil || v < 1 {
		return 1, true
	}
	return v, true
}

// localDockerfile is the Dockerfile the rebuild hint names when the project
// does not set one.
const localDockerfile = "Dockerfile.smelt"

// StaleImageWarning is the text shown when a user-managed image tag carries an
// older agent kit than this smelt expects, "" when the image is absent, could
// not be inspected or is current. dockerfile is the file to rebuild it from;
// empty means Dockerfile.smelt.
func StaleImageWarning(tag string, info ImageInfo, inspectErr error, dockerfile string) string {
	if inspectErr != nil || info.Status == ImageMissing {
		return ""
	}
	have := "none"
	if info.KitLabeled {
		if info.Kit >= KitVersion {
			return ""
		}
		have = strconv.Itoa(info.Kit)
	}
	if dockerfile == "" {
		dockerfile = localDockerfile
	}
	return tag + " has agent kit " + have + "; this smelt expects " + kitVersionString() +
		": rebuild it (docker build -t " + tag + " -f " + dockerfile + " .)"
}

// KitOutdatedWarning is the text shown when a Dockerfile still has a kit
// section. smelt ignores it and adds its own kit to every sandbox image.
func KitOutdatedWarning(name string, have int) string {
	return "the agent kit section of " + name + " (v" + strconv.Itoa(have) + ") is no longer used: smelt adds its kit itself when it builds the image.\n" +
		"  You can delete everything from the '" + kitMarkerPrefix + "' line on (smelt factory image --regenerate writes a file without it)."
}
