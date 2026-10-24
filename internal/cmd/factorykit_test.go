package cmd

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

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

func TestNoteKitOutdatedWarnsOncePerDay(t *testing.T) {
	root := kitProject(t, kitV1Dockerfile)
	streams, out := capturedStreams()
	day := time.Date(2026, 10, 12, 9, 0, 0, 0, time.Local)
	n := kitNotice{Root: root, Dockerfile: "Dockerfile.smelt", Now: day}

	got := noteKitOutdated(streams, n)
	assert.Assert(t, strings.Contains(got, "agent kit v1; the current kit is v5"), got)
	assert.Equal(t, strings.Count(out.String(), "Dockerfile.smelt was generated with agent kit v1"), 1)
	assert.Assert(t, strings.Contains(out.String(), "smelt factory image --regenerate"))

	// The same day it is still reported to the caller, but not printed again.
	out.Reset()
	assert.Assert(t, noteKitOutdated(streams, n) != "")
	assert.Equal(t, out.String(), "")

	// Another day prints it again; so does asking for it every time.
	n.Now = day.AddDate(0, 0, 1)
	noteKitOutdated(streams, n)
	assert.Assert(t, out.Len() > 0)
	out.Reset()
	n.EveryTime = true
	noteKitOutdated(streams, n)
	assert.Assert(t, out.Len() > 0)
}

func TestNoteKitOutdatedSaysNothingForACurrentOrNewerKit(t *testing.T) {
	for _, body := range []string{
		"FROM x\n# --- smelt agent kit (v5) ---\n",
		"FROM x\n# --- smelt agent kit (v9) ---\n",
	} {
		root := kitProject(t, body)
		streams, out := capturedStreams()
		got := noteKitOutdated(streams, kitNotice{Root: root, Dockerfile: "Dockerfile.smelt", Now: time.Now(), EveryTime: true})
		assert.Equal(t, got, "")
		assert.Equal(t, out.String(), "")
	}
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

func TestWithAgentKitLeavesADockerfileWithoutAKitSectionAlone(t *testing.T) {
	root := kitProject(t, "FROM x\nRUN true\n")
	plan, err := sandboximage.NewPlan(root, "Dockerfile.smelt")
	assert.NilError(t, err)
	// No kit section: the builder is never used, so nil is fine.
	got, err := withAgentKit(t.Context(), iostream.Streams{}, nil, plan, kitSource{Root: root})
	assert.NilError(t, err)
	assert.Equal(t, got.Tag, plan.Tag)
	assert.Equal(t, got.Context.Content, "FROM x\nRUN true\n")
}
