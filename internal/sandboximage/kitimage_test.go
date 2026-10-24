package sandboximage

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"gotest.tools/v3/assert"
	"gotest.tools/v3/assert/cmp"

	"github.com/smeltdev/smelt/internal/envspec"
)

// TestKitDockerfileDefaultsEqualThePins keeps the ARG defaults in kit/Dockerfile
// equal to the Go pins, which stay the one source.
func TestKitDockerfileDefaultsEqualThePins(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "..", "kit", "Dockerfile"))
	assert.NilError(t, err)
	defaults := map[string]string{}
	for _, m := range regexp.MustCompile(`(?m)^ARG ([A-Z0-9_]+)=(.*)$`).FindAllStringSubmatch(string(raw), -1) {
		defaults[m[1]] = m[2]
	}
	for _, pin := range KitPins() {
		got, ok := defaults[pin.Name]
		assert.Check(t, ok, "kit/Dockerfile has no ARG %s", pin.Name)
		assert.Check(t, cmp.Equal(got, pin.Value), "ARG %s", pin.Name)
	}
}

func TestKitRefs(t *testing.T) {
	assert.Check(t, cmp.Equal(KitTagRef(), KitRegistryRepo+":"+kitVersionString()))
	assert.Check(t, strings.HasSuffix(KitLocalRef(), ":"+kitVersionString()))
}

func TestComposeWithKitImage(t *testing.T) {
	base := "FROM debian:12\nRUN apt-get update\n" + kitMarker() + "RUN old kit\n"
	got := ComposeWithKitImage(base, "ghcr.io/x/kit@sha256:abc")
	assert.Check(t, strings.HasPrefix(got, "FROM debian:12\nRUN apt-get update\n"))
	assert.Check(t, cmp.Contains(got, "COPY --from=ghcr.io/x/kit@sha256:abc /opt/smelt /opt/smelt\n"))
	assert.Check(t, !strings.Contains(got, "RUN old kit"))
	assert.Check(t, cmp.Equal(ComposeWithKitImage(got, "other"), ComposeWithKitImage(got, "other")))
	assert.Check(t, cmp.Equal(ComposeWithKitImage("FROM x\n", "r"), "FROM x\n"))
}

type fakePuller struct {
	ref     string
	pullErr error
	have    bool
}

func (f fakePuller) PullKit(context.Context, string) (string, error) { return f.ref, f.pullErr }
func (f fakePuller) Exists(context.Context, string) (bool, error)    { return f.have, nil }

func TestResolveKit(t *testing.T) {
	state := filepath.Join(t.TempDir(), "kit-image.json")
	ref := KitRegistryRepo + "@sha256:1111"

	_, err := ResolveKit(context.Background(), fakePuller{pullErr: errors.New("no network")}, state)
	assert.Check(t, errors.Is(err, ErrKitUnavailable), "offline with nothing remembered")

	c, err := ResolveKit(context.Background(), fakePuller{ref: ref}, state)
	assert.NilError(t, err)
	assert.Check(t, cmp.Equal(c.Ref, ref))
	assert.Check(t, !c.Offline)

	c, err = ResolveKit(context.Background(), fakePuller{pullErr: errors.New("no network"), have: true}, state)
	assert.NilError(t, err)
	assert.Check(t, c.Offline)
	assert.Check(t, cmp.Equal(c.Ref, ref))
	assert.Check(t, c.PullErr != nil)

	_, err = ResolveKit(context.Background(), fakePuller{pullErr: errors.New("no network"), have: false}, state)
	assert.Check(t, errors.Is(err, ErrKitUnavailable), "remembered image is gone")
}

func TestDigestRef(t *testing.T) {
	want := KitRegistryRepo + "@sha256:aa"
	got, ok := digestRef([]string{"other/x@sha256:bb", want})
	assert.Check(t, ok)
	assert.Check(t, cmp.Equal(got, want))
	_, ok = digestRef([]string{"other/x@sha256:bb"})
	assert.Check(t, !ok)
}

func TestWithLastKitMatchesTheTagOfTheBuild(t *testing.T) {
	dir := t.TempDir()
	df := Render(NewRecipe(&envspec.Environment{Stack: "unknown"}, nil, nil))
	assert.NilError(t, os.WriteFile(filepath.Join(dir, "Dockerfile.smelt"), []byte(df), 0o644))
	plan, err := NewPlan(dir, "Dockerfile.smelt")
	assert.NilError(t, err)

	state := KitStatePath(t.TempDir())
	assert.Check(t, cmp.Equal(plan.WithLastKit(state).Tag, plan.Tag), "no kit pulled yet")

	ref := KitRegistryRepo + "@sha256:" + strings.Repeat("a", 64)
	assert.NilError(t, saveLastKit(state, ref))
	built, err := plan.WithKit(ref)
	assert.NilError(t, err)
	got := plan.WithLastKit(state)
	assert.Check(t, cmp.Equal(got.Tag, built.Tag))
	assert.Check(t, got.Tag != plan.Tag)
}
