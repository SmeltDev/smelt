package sandboximage

import (
	"errors"
	"regexp"
	"strconv"
	"strings"
)

// KitVersion numbers the agent kit: the section of the rendered Dockerfile, and
// the kit image built from kit/ and published under this number as its tag (see
// kitimage.go). Bump it whenever the kit's contents change, so projects whose
// committed Dockerfile has an older kit are told, and a new kit image is
// published. Version 1 is a Dockerfile without a marker (Claude only);
// 2 adds Codex and the harness label; 3 adds codex-code-mode-host, which Codex
// runs every command through; 4 adds python3 and lizard, which measures code
// quality in any language; 5 adds the Grok CLI.
const KitVersion = 5

// kitMarkerPrefix starts the line that opens the agent-kit section.
const kitMarkerPrefix = "# --- smelt agent kit"

// kitMarkerRE parses a marker line: "# --- smelt agent kit (v2) ---...".
var kitMarkerRE = regexp.MustCompile(`^# --- smelt agent kit(?: \(v(\d+)\))?(?: |-|$)`)

// ErrNoKit means a Dockerfile has no agent-kit marker, so its kit cannot be
// told apart from the rest of the file.
var ErrNoKit = errors.New("no smelt agent kit marker")

func kitVersionString() string { return strconv.Itoa(KitVersion) }

// kitMarker is the line that opens the kit section, padded like the other
// section headers.
func kitMarker() string {
	head := kitMarkerPrefix + " (v" + kitVersionString() + ") "
	return head + strings.Repeat("-", max(3, 76-len(head))) + "\n"
}

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

// KitOutdated reports whether dockerfile carries an older kit than this
// binary's. A newer one is not outdated: the user may run a newer smelt
// elsewhere.
func KitOutdated(dockerfile string) bool {
	v, _ := KitVersionOf(dockerfile)
	return v < KitVersion
}

// UpdateKit replaces the agent-kit section of dockerfile, from its marker to
// the end of the file, with the current kit, and keeps everything above it. The
// base image's user is read from the old section, which switches to root around
// its package install and back. It returns ErrNoKit when there is no marker.
func UpdateKit(dockerfile string) (string, error) {
	off := kitMarkerOffset(dockerfile)
	if off < 0 {
		return "", ErrNoKit
	}
	return dockerfile[:off] + kitSection(kitUser(dockerfile[off:])), nil
}

// kitUser is the user an existing kit section restores after installing as
// root, "" when it does not switch.
func kitUser(section string) string {
	var users []string
	for line := range strings.SplitSeq(section, "\n") {
		if u, ok := strings.CutPrefix(strings.TrimSpace(line), "USER "); ok {
			users = append(users, strings.TrimSpace(u))
		}
	}
	if len(users) < 2 || users[0] != rootUser {
		return ""
	}
	return users[1]
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

// KitOutdatedWarning is the text shown when a Dockerfile's kit is older.
func KitOutdatedWarning(name string, have int) string {
	return name + " was generated with agent kit v" + strconv.Itoa(have) +
		"; the current kit is v" + kitVersionString() + "\n" +
		"  (smelt builds the image with the kit image in place of that section, so this file needs no change;\n" +
		"  smelt factory image --regenerate drops the section for good)"
}
