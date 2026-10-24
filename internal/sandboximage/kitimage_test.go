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

func TestSandboxDockerfile(t *testing.T) {
	for _, f := range fixtures {
		t.Run(f.name, func(t *testing.T) {
			tool, err := planFor(t, f.toolchain+f.old)
			assert.NilError(t, err)
			got := SandboxDockerfile(tool.Tag, "ghcr.io/x/kit@sha256:abc")
			assert.Check(t, strings.HasPrefix(got, "FROM "+tool.Tag+"\n"))
			assert.Check(t, cmp.Contains(got, "COPY --from=ghcr.io/x/kit@sha256:abc /opt/smelt /opt/smelt\n"))
			assert.Check(t, cmp.Contains(got, "ENV SMELT_KIT=/opt/smelt\n"))
			assert.Check(t, cmp.Contains(got, "ENV PATH=/opt/smelt/bin:$PATH\n"))
			assert.Check(t, cmp.Contains(got, "LABEL smelt.harnesses=claude,codex,grok smelt.kit="))
			for _, banned := range []string{"USER ", "CMD ", "RUN ", "ENTRYPOINT "} {
				assert.Check(t, !strings.Contains(got, banned), "%q in %s", banned, got)
			}
		})
	}
}

// planFor plans a toolchain image from a Dockerfile's text.
func planFor(t *testing.T, dockerfile string) (Plan, error) {
	t.Helper()
	dir := writeRepo(t, map[string]string{"Dockerfile.smelt": dockerfile})
	return NewPlan(dir, "Dockerfile.smelt")
}

// tagHash is the digest part of a tag; the repository part follows the
// directory the fixture is written to.
func tagHash(tag string) string { return tag[strings.LastIndex(tag, ":")+1:] }

func TestTags(t *testing.T) {
	const kitA, kitB = KitRegistryRepo + "@sha256:aaaa", KitRegistryRepo + "@sha256:bbbb"
	a, err := planFor(t, fixtures[0].toolchain)
	assert.NilError(t, err)
	old, err := planFor(t, fixtures[1].toolchain+fixtures[1].old)
	assert.NilError(t, err)
	changed, err := planFor(t, fixtures[0].toolchain+"RUN true\n")
	assert.NilError(t, err)

	assert.Check(t, cmp.Equal(tagHash(old.Tag), tagHash(a.Tag)), "an old kit section does not change the toolchain tag")
	assert.Check(t, strings.Contains(a.Tag, toolchainSuffix+":"))
	sa, sb := a.SandboxPlan(kitA), a.SandboxPlan(kitB)
	assert.Check(t, cmp.Equal(sa.Tag, SandboxTag(a.Tag, kitA)))
	assert.Check(t, sa.Tag != sb.Tag, "a new kit changes the sandbox tag")
	assert.Check(t, cmp.Equal(sa.Toolchain.Tag, sb.Toolchain.Tag), "and only that")
	assert.Check(t, tagHash(a.Tag) != tagHash(changed.Tag))
	assert.Check(t, sa.Tag != changed.SandboxPlan(kitA).Tag, "a toolchain change changes both")
	assert.Check(t, !strings.Contains(sa.Tag, toolchainSuffix))
	assert.Check(t, strings.HasPrefix(sa.Tag, ImageName(a.Slug)+":"))
	assert.Check(t, cmp.Equal(sa.Slug, a.Slug))
}

// TestSandboxEnvEqualsTheKitsEnvSh keeps the ENV lines of the sandbox Dockerfile
// equal to what kit/Dockerfile writes into env.sh.
func TestSandboxEnvEqualsTheKitsEnvSh(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "..", "kit", "Dockerfile"))
	assert.NilError(t, err)
	kit := string(raw)
	assert.Check(t, cmp.Contains(kit, "'SMELT_KIT=${SMELT_KIT:-"+KitDir+"}'"))
	assert.Check(t, cmp.Contains(kit, `'PATH="$SMELT_KIT/bin:$PATH"'`))
	got := SandboxDockerfile("t:1", "k")
	assert.Check(t, cmp.Contains(got, "ENV SMELT_KIT="+KitDir+"\n"))
	assert.Check(t, cmp.Contains(got, "ENV PATH="+KitDir+"/bin:$PATH\n"))
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

func TestLastKitSandboxPlanMatchesTheTagOfTheBuild(t *testing.T) {
	plan, err := planFor(t, Render(NewRecipe(&envspec.Environment{Stack: "unknown"}, nil, nil)))
	assert.NilError(t, err)

	state := KitStatePath(t.TempDir())
	_, ok := plan.LastKitSandboxPlan(state)
	assert.Check(t, !ok, "no kit pulled yet")

	ref := KitRegistryRepo + "@sha256:" + strings.Repeat("a", 64)
	assert.NilError(t, saveLastKit(state, ref))
	got, ok := plan.LastKitSandboxPlan(state)
	assert.Check(t, ok)
	assert.Check(t, cmp.Equal(got.Tag, plan.SandboxPlan(ref).Tag))
	assert.Check(t, got.Tag != plan.Tag)
	assert.Check(t, cmp.Contains(got.Context.Content, ref))
}
