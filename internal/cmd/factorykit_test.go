package cmd

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gotest.tools/v3/assert"

	"github.com/smeltdev/smelt/internal/iostream"
	"github.com/smeltdev/smelt/internal/sandboximage"
)

// kitProject is a project root with a Dockerfile.smelt holding body, and an
// isolated data dir.
func kitProject(t *testing.T, body string) string {
	t.Helper()
	isolateConfig(t)
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	root := t.TempDir()
	assert.NilError(t, os.WriteFile(filepath.Join(root, "Dockerfile.smelt"), []byte(body), 0o644))
	return root
}

func capturedStreams() (iostream.Streams, *bytes.Buffer) {
	var errOut bytes.Buffer
	return iostream.Streams{Out: &bytes.Buffer{}, Err: &errOut}, &errOut
}

const kitV1Dockerfile = "FROM base\nRUN my-toolchain\n\n# --- smelt agent kit ---\nRUN curl claude\nCMD [\"sleep\", \"infinity\"]\n"

func TestNoteKitOutdatedWarnsOncePerProject(t *testing.T) {
	root := kitProject(t, kitV1Dockerfile)
	streams, out := capturedStreams()
	n := kitNotice{Root: root, Dockerfile: "Dockerfile.smelt"}

	got := noteKitOutdated(streams, n)
	assert.Assert(t, strings.Contains(got, "(v1) is no longer used"), got)
	assert.Equal(t, strings.Count(out.String(), "Dockerfile.smelt (v1) is no longer used"), 1)
	assert.Assert(t, strings.Contains(out.String(), "smelt factory image --regenerate"))

	// Later it is still reported to the caller, but not printed again.
	out.Reset()
	assert.Assert(t, noteKitOutdated(streams, n) != "")
	assert.Equal(t, out.String(), "")

}

// Any kit section is unused now, whatever its version, so any of them is noted.
func TestNoteKitOutdatedWarnsForEveryVersionAndNotWithoutASection(t *testing.T) {
	for _, body := range []string{
		"FROM x\n# --- smelt agent kit (v5) ---\n",
		"FROM x\n# --- smelt agent kit (v9) ---\n",
	} {
		root := kitProject(t, body)
		streams, out := capturedStreams()
		assert.Assert(t, noteKitOutdated(streams, kitNotice{Root: root, Dockerfile: "Dockerfile.smelt"}) != "")
		assert.Assert(t, out.Len() > 0)
	}
	root := kitProject(t, "FROM x\nRUN true\n")
	streams, out := capturedStreams()
	assert.Equal(t, noteKitOutdated(streams, kitNotice{Root: root, Dockerfile: "Dockerfile.smelt"}), "")
	assert.Equal(t, out.String(), "")
}

func TestResolveBuildSettingsWarnsAboutAnOldKitOnceAndKeepsGoing(t *testing.T) {
	t.Setenv("DOCKER_HOST", "tcp://127.0.0.1:1")
	// Docker is unreachable here; a cancelled context ends the wait for it at
	// once, after the warning and before any build.
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	root := kitProject(t, kitV1Dockerfile)
	cfg := cfgWith(t, `{"factory":{"backend":"docker","dockerfile":"Dockerfile.smelt"}}`)

	streams, out := capturedStreams()
	got, err := resolveBuildSettings(ctx, streams, root, factorySettings{Attempts: 3}, factoryExplicit{}, cfg)
	assert.Assert(t, err != nil, "Docker is unreachable here, so the build is where it stops")
	assert.Assert(t, got.KitWarning != "")
	assert.Equal(t, strings.Count(out.String(), "smelt factory image --regenerate"), 1)

	out.Reset()
	got, _ = resolveBuildSettings(ctx, streams, root, factorySettings{Attempts: 3}, factoryExplicit{}, cfg)
	assert.Assert(t, got.KitWarning != "", "the run still records it")
	assert.Assert(t, !strings.Contains(out.String(), "--regenerate"), out.String())
}

func TestFactoryImageRejectsRegenerateWithUpdateKit(t *testing.T) {
	_, _, err := runCobra(t, newFactoryImageCmd(), "--regenerate", "--update-kit")
	assert.Equal(t, userMsg(err), "--regenerate and --update-kit cannot be used together.")
}

func TestNoteUpdateKitRetiredSaysWhy(t *testing.T) {
	streams, out := capturedStreams()
	noteUpdateKitRetired(streams)
	assert.Assert(t, strings.Contains(out.String(), "--update-kit does nothing now"), out.String())
	assert.Assert(t, strings.Contains(out.String(), "smelt factory image"), out.String())
}

func TestFactoryImageKitFlags(t *testing.T) {
	cmd := newFactoryImageCmd()
	for _, name := range []string{"update-kit", "kit-from-source"} {
		assert.Assert(t, cmd.Flags().Lookup(name) != nil, name)
	}
}

// fakeKits is a kitProvider with canned answers.
type fakeKits struct {
	ref     string
	pullErr error
	have    bool
	built   int
}

func (f *fakeKits) PullKit(context.Context, string) (string, error) { return f.ref, f.pullErr }
func (f *fakeKits) Exists(context.Context, string) (bool, error)    { return f.have, nil }
func (f *fakeKits) BuildKit(context.Context, string, sandboximage.BuildOptions) (string, error) {
	f.built++
	return "sha256:built", nil
}

func TestWithAgentKitAddsTheKitWhetherOrNotTheFileHasASection(t *testing.T) {
	const ref = sandboximage.KitRegistryRepo + "@sha256:1111"
	for name, body := range map[string]string{
		"no section": "FROM x\nRUN true\n",
		"a section":  "FROM x\nRUN true\n" + kitV1Dockerfile[len("FROM base\nRUN my-toolchain\n"):],
	} {
		t.Run(name, func(t *testing.T) {
			root := kitProject(t, body)
			toolchain, err := sandboximage.NewPlan(root, "Dockerfile.smelt")
			assert.NilError(t, err)
			got, _, err := withAgentKit(t.Context(), iostream.Streams{}, &fakeKits{ref: ref}, toolchain, kitSource{Root: root})
			assert.NilError(t, err)
			assert.Equal(t, got.Tag, sandboximage.SandboxTag(toolchain.Tag, ref))
			assert.Equal(t, got.Toolchain.Tag, toolchain.Tag)
			assert.Assert(t, strings.Contains(got.Context.Content, "COPY --from="+ref+" /opt/smelt /opt/smelt"))
			assert.Assert(t, !strings.Contains(got.Context.Content, "agent kit"))
		})
	}
}

func TestWithAgentKitOffline(t *testing.T) {
	const ref = sandboximage.KitRegistryRepo + "@sha256:2222"
	offline := errors.New("no network")
	root := kitProject(t, "FROM x\n")
	toolchain, err := sandboximage.NewPlan(root, "Dockerfile.smelt")
	assert.NilError(t, err)

	// Nothing cached: the error names the flag that works without a registry.
	_, _, err = withAgentKit(t.Context(), iostream.Streams{}, &fakeKits{pullErr: offline}, toolchain, kitSource{Root: root})
	var ue *userError
	assert.Assert(t, asUserError(err, &ue), "%v", err)
	assert.Equal(t, ue.code, "factory.kit_unavailable")
	assert.Assert(t, strings.Contains(ue.suggestion, "--kit-from-source"), ue.suggestion)

	// Cached: it is used, with a note.
	_, _, err = withAgentKit(t.Context(), iostream.Streams{}, &fakeKits{ref: ref}, toolchain, kitSource{Root: root})
	assert.NilError(t, err)
	streams, out := capturedStreams()
	got, _, err := withAgentKit(t.Context(), streams, &fakeKits{pullErr: offline, have: true}, toolchain, kitSource{Root: root})
	assert.NilError(t, err)
	assert.Equal(t, got.Tag, sandboximage.SandboxTag(toolchain.Tag, ref))
	assert.Assert(t, strings.Contains(out.String(), "using the kit pulled last: "+ref), out.String())
}

func TestWithAgentKitFromSource(t *testing.T) {
	root := kitProject(t, "FROM x\n")
	toolchain, err := sandboximage.NewPlan(root, "Dockerfile.smelt")
	assert.NilError(t, err)

	_, _, err = withAgentKit(t.Context(), iostream.Streams{}, &fakeKits{}, toolchain, kitSource{Root: root, FromSource: true})
	var ue *userError
	assert.Assert(t, asUserError(err, &ue), "%v", err)
	assert.Equal(t, ue.code, "command.invalid_args")

	assert.NilError(t, os.MkdirAll(filepath.Join(root, "kit"), 0o755))
	assert.NilError(t, os.WriteFile(filepath.Join(root, "kit", "Dockerfile"), []byte("FROM scratch\n"), 0o644))
	kits := &fakeKits{}
	streams, _ := capturedStreams()
	got, _, err := withAgentKit(t.Context(), streams, kits, toolchain, kitSource{Root: root, FromSource: true})
	assert.NilError(t, err)
	assert.Equal(t, kits.built, 1)
	assert.Equal(t, got.Tag, sandboximage.SandboxTag(toolchain.Tag, "sha256:built"))
	kits.pullErr, kits.have = errors.New("offline"), true
	reused, _, err := withAgentKit(t.Context(), streams, kits, toolchain, kitSource{Root: root})
	assert.NilError(t, err)
	assert.Equal(t, reused.Tag, got.Tag)
	assert.Equal(t, kits.built, 1)

}
