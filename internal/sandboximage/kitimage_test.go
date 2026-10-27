package sandboximage

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/docker/docker/api/types/image"
	"github.com/docker/docker/client"
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
	assert.Check(t, cmp.Contains(kit, `'SSL_CERT_FILE="$SMELT_KIT/libexec/runtime/ca-certificates.crt"'`))
	assert.Check(t, cmp.Contains(kit, `'NODE_EXTRA_CA_CERTS="$SSL_CERT_FILE"'`))
	got := SandboxDockerfile("t:1", "k")
	assert.Check(t, cmp.Contains(got, "ENV SSL_CERT_FILE="+KitDir+"/libexec/runtime/ca-certificates.crt\n"))
	assert.Check(t, cmp.Contains(got, "ENV NODE_EXTRA_CA_CERTS="+KitDir+"/libexec/runtime/ca-certificates.crt\n"))
	assert.Check(t, cmp.Contains(got, "ENV SMELT_KIT="+KitDir+"\n"))
	assert.Check(t, cmp.Contains(got, "ENV PATH="+KitDir+"/bin:$PATH\n"))
}

type fakePuller struct {
	ref       string
	pullErr   error
	have      bool
	requested *string
}

func (f fakePuller) PullKit(_ context.Context, ref string) (string, error) {
	if f.requested != nil {
		*f.requested = ref
	}
	return f.ref, f.pullErr
}
func (f fakePuller) Exists(context.Context, string) (bool, error) { return f.have, nil }

func TestResolveKit(t *testing.T) {
	state := filepath.Join(t.TempDir(), "kit-image.json")
	ref := KitRegistryRepo + "@sha256:1111"

	_, err := ResolveKit(context.Background(), fakePuller{pullErr: errors.New("no network")}, state)
	assert.Check(t, errors.Is(err, ErrKitUnavailable), "offline with nothing remembered")

	var requested string
	c, err := ResolveKit(context.Background(), fakePuller{ref: ref, requested: &requested}, state)
	assert.Equal(t, requested, KitTagRef())
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
	assert.NilError(t, RememberKit(state, ref))
	got, ok := plan.LastKitSandboxPlan(state)
	assert.Check(t, ok)
	assert.Check(t, cmp.Equal(got.Tag, plan.SandboxPlan(ref).Tag))
	assert.Check(t, got.Tag != plan.Tag)
	assert.Check(t, cmp.Contains(got.Context.Content, ref))
}

func TestOfflineKitAcrossUpgrade(t *testing.T) {
	state := KitStatePath(t.TempDir())
	old := lastKit{Kit: KitVersion - 1, Ref: KitRegistryRepo + "@sha256:old"}
	raw, err := json.Marshal(old)
	assert.NilError(t, err)
	assert.NilError(t, os.WriteFile(state, raw, 0o600))
	choice, err := ResolveKit(t.Context(), fakePuller{pullErr: errors.New("offline"), have: true}, state)
	assert.NilError(t, err)
	assert.Assert(t, choice.Offline)
	assert.Equal(t, choice.Kit, old.Kit)
	assert.Equal(t, choice.Ref, old.Ref)
	plan := (Plan{Tag: "tool:1"}).SandboxPlanWithKit(choice)
	assert.Assert(t, strings.Contains(plan.Context.Content, "smelt.kit="+strconv.Itoa(old.Kit)))
	cached, ok := plan.Toolchain.LastKitSandboxPlan(state)
	assert.Assert(t, ok)
	assert.Equal(t, cached.Kit, old.Kit)
	assert.Equal(t, cached.Tag, plan.Tag)
}

func TestPlanningIgnoresOldKitCopySources(t *testing.T) {
	dir := writeRepo(t, map[string]string{"Dockerfile.smelt": "FROM debian:12\n# --- smelt agent kit ---\nCOPY agents/*.sh /opt/agents/\nCOPY ../outside /outside\n"})
	relative, err := NewPlan(dir, "Dockerfile.smelt")
	assert.NilError(t, err)
	absolute, err := NewPlanFromFile(dir, filepath.Join(dir, "Dockerfile.smelt"))
	assert.NilError(t, err)
	assert.Equal(t, relative.Tag, absolute.Tag)
	assert.Equal(t, relative.Context.Content, "FROM debian:12\n")
	assert.Equal(t, len(relative.Context.Sources), 0)
}

type refreshingKit struct {
	fakePuller
	manifest    KitManifest
	manifestErr error
}

func (f refreshingKit) ReadKitManifest(context.Context, string) (KitManifest, error) {
	return f.manifest, f.manifestErr
}

func TestRefreshSelectsNewKitWithoutCLIUpgrade(t *testing.T) {
	state := KitStatePath(t.TempDir())
	tag := KitRegistryRepo + ":next"
	var requested string
	provider := refreshingKit{fakePuller: fakePuller{ref: "digest:new", requested: &requested}, manifest: KitManifest{Kit: KitVersion + 1}}
	choice, err := RefreshKit(t.Context(), provider, state, tag)
	assert.NilError(t, err)
	assert.Equal(t, requested, tag)
	assert.Equal(t, choice.Kit, KitVersion+1)
	assert.Equal(t, choice.Ref, "digest:new")
	_, err = ResolveKit(t.Context(), provider, state)
	assert.NilError(t, err)
	assert.Equal(t, requested, choice.Ref, "a moving tag must not change the selected kit")
	offline, err := ResolveKit(t.Context(), fakePuller{pullErr: errors.New("offline"), have: true}, state)
	assert.NilError(t, err)
	assert.Equal(t, offline.Kit, KitVersion+1)
	assert.Equal(t, offline.Ref, choice.Ref)
	provider.manifestErr = errors.New("invalid manifest")
	_, err = RefreshKit(t.Context(), provider, state, "bad:kit")
	assert.ErrorContains(t, err, "invalid manifest")
	last, ok := readLastKit(state)
	assert.Assert(t, ok)
	assert.Equal(t, last.Selected, tag)
}

func TestUpgradeReplacesOlderRefreshSelection(t *testing.T) {
	state := KitStatePath(t.TempDir())
	old := lastKit{Kit: KitVersion - 1, Ref: "cached:old", Selected: KitRegistryRepo + ":4"}
	assert.NilError(t, writeLastKit(state, old))
	var requested string
	choice, err := ResolveKit(t.Context(), fakePuller{ref: "digest:current", requested: &requested}, state)
	assert.NilError(t, err)
	assert.Equal(t, requested, KitTagRef())
	assert.Equal(t, choice.Kit, KitVersion)
	last, ok := readLastKit(state)
	assert.Assert(t, ok)
	assert.Equal(t, last.Selected, "")
	assert.Equal(t, last.Ref, choice.Ref)
}

func TestPullKitStreamErrorUsesOfflineFallback(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/images/create"):
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"errorDetail":{"message":"registry unavailable"},"error":"registry unavailable"}` + "\n"))
		case strings.HasSuffix(r.URL.Path, "/json") && !strings.Contains(r.URL.Path, "cached-kit"):
			http.Error(w, "missing", http.StatusNotFound)
		case strings.HasSuffix(r.URL.Path, "/build"):
			http.Error(w, "build unavailable", http.StatusInternalServerError)
		case strings.HasSuffix(r.URL.Path, "/json"):
			assert.Equal(t, r.URL.Path, "/v1.47/images/cached-kit/json")
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"Id":"cached-kit"}`))
		default:
			t.Errorf("unexpected Docker call: %s %s", r.Method, r.URL)
			http.Error(w, "unexpected request", http.StatusBadRequest)
		}
	}))
	defer server.Close()
	cli, err := client.NewClientWithOpts(client.WithHost(server.URL), client.WithVersion("1.47"))
	assert.NilError(t, err)
	defer func() { _ = cli.Close() }()
	state := KitStatePath(t.TempDir())
	assert.NilError(t, RememberKit(state, "cached-kit"))
	choice, err := ResolveKit(t.Context(), NewBuilder(cli), state)
	assert.NilError(t, err)
	assert.Assert(t, choice.Offline)
	assert.Equal(t, choice.Ref, "cached-kit")
	assert.ErrorContains(t, choice.PullErr, "registry unavailable")
}

// labeledPuller is a fakePuller whose local images include some labelled kits.
type labeledPuller struct {
	fakePuller
	docker *fakeDocker
}

func (l labeledPuller) LabeledKit(ctx context.Context, version int) (string, error) {
	return NewBuilder(l.docker).LabeledKit(ctx, version)
}

func TestResolveKitFindsTheLabelledImageWhenTheRecordIsBad(t *testing.T) {
	kitLabel := func(v int) map[string]string { return map[string]string{LabelKit: strconv.Itoa(v)} }
	docker := &fakeDocker{labeled: []image.Summary{
		{ID: "sha256:other", Labels: map[string]string{"x": "y"}, Created: 9},
		{ID: "sha256:v5", Labels: kitLabel(KitVersion - 1), Created: 8},
		{ID: "sha256:old", Labels: kitLabel(KitVersion), Created: 1},
		{ID: "sha256:new", Labels: kitLabel(KitVersion), Created: 2},
	}}
	offline := fakePuller{pullErr: errors.New("denied")}
	for name, record := range map[string]string{
		"missing":    "",
		"corrupt":    "{not json",
		"image gone": `{"kit":6,"ref":"` + KitRegistryRepo + `@sha256:abcd"}`,
	} {
		t.Run(name, func(t *testing.T) {
			state := KitStatePath(t.TempDir())
			if record != "" {
				assert.NilError(t, os.WriteFile(state, []byte(record), 0o600))
			}
			c, err := ResolveKit(t.Context(), labeledPuller{fakePuller: offline, docker: docker}, state)
			assert.NilError(t, err)
			assert.Assert(t, c.Offline && c.FoundOnDisk)
			assert.Equal(t, c.Ref, "sha256:new", "the newest image carrying the label")
			assert.Equal(t, c.Kit, KitVersion)
			assert.ErrorContains(t, c.PullErr, "denied")
			last, ok := readLastKit(state)
			assert.Assert(t, ok, "record rewritten")
			assert.Equal(t, last.Ref, "sha256:new")
			assert.Equal(t, last.Kit, KitVersion)
		})
	}
}

func TestResolveKitWithoutALabelledImageIsUnavailable(t *testing.T) {
	docker := &fakeDocker{labeled: []image.Summary{{ID: "sha256:v5", Labels: map[string]string{LabelKit: strconv.Itoa(KitVersion - 1)}}}}
	state := KitStatePath(t.TempDir())
	_, err := ResolveKit(t.Context(), labeledPuller{fakePuller: fakePuller{pullErr: errors.New("denied")}, docker: docker}, state)
	assert.Check(t, errors.Is(err, ErrKitUnavailable))
	_, err = os.Stat(state)
	assert.Check(t, os.IsNotExist(err), "nothing recorded")
}
