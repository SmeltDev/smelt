package sandboximage

import (
	"errors"
	"strconv"
	"strings"
	"testing"

	"gotest.tools/v3/assert"
	"gotest.tools/v3/assert/cmp"
)

func TestKitVersionOf(t *testing.T) {
	tests := []struct {
		name      string
		in        string
		version   int
		found     bool
		wantNewer bool
	}{
		{"no marker", "FROM x\nRUN true\n", 1, false, false},
		{"v1 marker without number", "FROM x\n# --- smelt agent kit ------\nRUN true\n", 1, true, false},
		{"v2", "FROM x\n# --- smelt agent kit (v2) ------\n", 2, true, false},
		{"current", "FROM x\n# --- smelt agent kit (v" + kitVersionString() + ") ------\n", KitVersion, true, false},
		{"newer than the binary", "FROM x\n# --- smelt agent kit (v" + strconv.Itoa(KitVersion+1) + ") ------\n", KitVersion + 1, true, true},
		{"garbage after the marker", "# --- smelt agent kit (vX) ---\n", 1, true, false},
		{"huge number", "# --- smelt agent kit (v99999999999999999999) ---\n", 1, true, false},
		{"marker in a comment elsewhere", "FROM x\n# see the '# --- smelt agent kit (v2)' section\nRUN echo # --- smelt agent kit (v2)\n", 1, false, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			v, found := KitVersionOf(tt.in)
			assert.Equal(t, v, tt.version)
			assert.Equal(t, found, tt.found)
			assert.Equal(t, v > KitVersion, tt.wantNewer)
		})
	}
}

func TestKitOutdatedWarningSaysTheSectionIsUnused(t *testing.T) {
	got := KitOutdatedWarning("Dockerfile.smelt", 3)
	assert.Check(t, cmp.Contains(got, "Dockerfile.smelt (v3) is no longer used"))
	assert.Check(t, cmp.Contains(got, "delete"))
}

// fixtures are the Dockerfiles the sandbox is assembled from: a toolchain-only
// one and one with an old kit section, each on a non-root (cimg) and a root
// (debian) base.
var fixtures = []struct {
	name, base, toolchain, old string
}{
	{"cimg toolchain only", "cimg/base:stable", "FROM cimg/base:stable\nUSER circleci\nRUN go version\n", ""},
	{"cimg old section", "cimg/base:stable", "FROM cimg/base:stable\nUSER circleci\nRUN go version\n",
		"\n# --- smelt agent kit (v4) ------\nUSER root\nRUN apt-get install git\nUSER circleci\nRUN curl claude\nCMD [\"sleep\", \"infinity\"]\n"},
	{"debian toolchain only", "debian:bookworm-slim", "FROM debian:bookworm-slim\nRUN apt-get update && apt-get install -y git\n", ""},
	{"debian old section", "debian:bookworm-slim", "FROM debian:bookworm-slim\nRUN apt-get update && apt-get install -y git\n",
		"\n# --- smelt agent kit ------\nRUN curl claude\nLABEL smelt.kit=1\n"},
}

func TestToolchainDockerfileStopsAtTheKitMarker(t *testing.T) {
	for _, f := range fixtures {
		t.Run(f.name, func(t *testing.T) {
			assert.Check(t, cmp.Equal(ToolchainDockerfile(f.toolchain+f.old), f.toolchain))
			assert.Check(t, !strings.Contains(ToolchainDockerfile(f.toolchain+f.old), "agent kit"))
		})
	}
	assert.Check(t, cmp.Equal(ToolchainDockerfile("FROM x\n"), "FROM x\n"), "no marker: unchanged")
}

func TestStaleImageWarning(t *testing.T) {
	for name, tt := range map[string]struct {
		info ImageInfo
		err  error
		want string
	}{
		"missing":   {ImageInfo{Status: ImageMissing}, nil, ""},
		"error":     {ImageInfo{}, errors.New("x"), ""},
		"unlabeled": {ImageInfo{Status: ImageUnlabeled, Kit: 1}, nil, "t:1 has agent kit none; this smelt expects " + kitVersionString() + ": rebuild it (docker build -t t:1 -f Dockerfile.smelt .)"},
		"lower":     {ImageInfo{Status: ImageLabeled, Kit: 3, KitLabeled: true}, nil, "t:1 has agent kit 3; this smelt expects " + kitVersionString() + ": rebuild it (docker build -t t:1 -f Dockerfile.smelt .)"},
		"equal":     {ImageInfo{Status: ImageLabeled, Kit: KitVersion, KitLabeled: true}, nil, ""},
		"newer":     {ImageInfo{Status: ImageLabeled, Kit: KitVersion + 1, KitLabeled: true}, nil, ""},
	} {
		t.Run(name, func(t *testing.T) { assert.Equal(t, StaleImageWarning("t:1", tt.info, tt.err, ""), tt.want) })
	}
}
