package sandboximage

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/docker/docker/client"
	"gotest.tools/v3/assert"
)

type localKitFake struct {
	fakePuller
	images        map[string]string
	pulls, builds int
}

func (f *localKitFake) LocalKit(_ context.Context, tag string) (string, error) {
	if ref := f.images[tag]; ref != "" {
		return ref, nil
	}
	return "", errors.New("missing")
}
func (f *localKitFake) PullKit(ctx context.Context, ref string) (string, error) {
	f.pulls++
	return f.fakePuller.PullKit(ctx, ref)
}
func (f *localKitFake) BuildKit(_ context.Context, dir string, _ BuildOptions) (string, error) {
	f.builds++
	c, err := ReadContext(dir, "Dockerfile")
	if err != nil {
		return "", err
	}
	var buf bytes.Buffer
	if err := c.Tar(&buf); err != nil {
		return "", err
	}
	f.images[KitRevisionRef()], f.images[KitLocalRef()] = "sha256:built", "sha256:built"
	return "sha256:built", nil
}
func TestEmbeddedKitResolution(t *testing.T) {
	for _, failure := range []string{"denied", "not found", "offline"} {
		t.Run(failure, func(t *testing.T) {
			state := KitStatePath(t.TempDir())
			f := &localKitFake{fakePuller: fakePuller{pullErr: errors.New(failure)}, images: map[string]string{}}
			choice, err := ResolveKit(t.Context(), f, state)
			assert.NilError(t, err)
			assert.Equal(t, choice.Ref, "sha256:built")
			assert.Assert(t, choice.Notice != "")
			again, err := ResolveKit(t.Context(), f, state)
			assert.NilError(t, err)
			assert.Equal(t, again.Notice, choice.Notice)
			assert.Equal(t, f.pulls, 1)
			assert.Equal(t, f.builds, 1)
			assert.Assert(t, ClaimKitNotice(state, choice.Ref))
			assert.Assert(t, !ClaimKitNotice(state, choice.Ref))
			assert.Assert(t, ClaimKitNotice(state, "sha256:different"))
		})
	}
}
func TestKitLocalPrecedence(t *testing.T) {
	f := &localKitFake{images: map[string]string{KitRevisionRef(): "revision", KitLocalRef(): "version"}}
	choice, err := ResolveKit(t.Context(), f, KitStatePath(t.TempDir()))
	assert.NilError(t, err)
	assert.Equal(t, choice.Ref, "revision")
	assert.Equal(t, f.pulls, 0)
	delete(f.images, KitRevisionRef())
	choice, err = ResolveKit(t.Context(), f, KitStatePath(t.TempDir()))
	assert.NilError(t, err)
	assert.Equal(t, choice.Ref, "version")
	assert.Equal(t, f.pulls, 0)
}
func TestEmbeddedKitContextParity(t *testing.T) {
	dir, err := EmbeddedKitContext()
	assert.NilError(t, err)
	defer func() { _ = os.RemoveAll(dir) }()
	embedded, err := ReadContext(dir, "Dockerfile")
	assert.NilError(t, err)
	tree, err := ReadContext(filepath.Join("..", "..", "kit"), "Dockerfile")
	assert.NilError(t, err)
	var a, b bytes.Buffer
	assert.NilError(t, embedded.Tar(&a))
	assert.NilError(t, tree.Tar(&b))
	assert.Equal(t, sha256.Sum256(a.Bytes()), sha256.Sum256(b.Bytes()))
}

func TestDockerEmbeddedFallbackTagsAndReuse(t *testing.T) {
	docker := &fakeDocker{imageIDs: map[string]string{}}
	builder := NewBuilder(docker)
	state := KitStatePath(t.TempDir())
	choice, err := ResolveKit(t.Context(), builder, state)
	assert.NilError(t, err)
	assert.Equal(t, choice.Ref, "sha256:built")
	assert.Equal(t, len(docker.builds), 1)
	assert.DeepEqual(t, docker.builds[0].Tags, []string{KitRevisionRef(), KitLocalRef()})
	for _, pin := range KitPins() {
		assert.Equal(t, *docker.builds[0].BuildArgs[pin.Name], pin.Value)
	}
	assert.Equal(t, len(docker.pulls), 1)
	_, err = ResolveKit(t.Context(), builder, state)
	assert.NilError(t, err)
	assert.Equal(t, len(docker.builds), 1)
	assert.Equal(t, len(docker.pulls), 1)
}

func TestKitNoticeConcurrentClaims(t *testing.T) {
	state := KitStatePath(t.TempDir())
	claims := make(chan bool, 16)
	var wg sync.WaitGroup
	for range cap(claims) {
		wg.Go(func() { claims <- ClaimKitNotice(state, "sha256:kit") })
	}
	wg.Wait()
	close(claims)
	count := 0
	for claimed := range claims {
		if claimed {
			count++
		}
	}
	assert.Equal(t, count, 1)
}

func TestLocalKitRecoversProvenanceAfterAnotherSelection(t *testing.T) {
	state := KitStatePath(t.TempDir())
	f := &localKitFake{fakePuller: fakePuller{pullErr: errors.New("denied")}, images: map[string]string{}}
	first, err := ResolveKit(t.Context(), f, state)
	assert.NilError(t, err)
	assert.NilError(t, RememberKit(state, "another-image"))
	next, err := ResolveKit(t.Context(), f, state)
	assert.NilError(t, err)
	assert.Equal(t, next.Notice, first.Notice)
	plan, ok := (Plan{Tag: "toolchain:1"}).LastKitSandboxPlan(state)
	assert.Assert(t, ok)
	assert.Equal(t, plan.KitNotice, first.Notice)
}

func TestRegistryStreamFailureBuildsEmbeddedKit(t *testing.T) {
	var built atomic.Bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.HasSuffix(r.URL.Path, "/images/create"):
			_, _ = w.Write([]byte("{\"error\":\"denied\"}\n"))
		case strings.HasSuffix(r.URL.Path, "/build"):
			built.Store(true)
			_, _ = w.Write([]byte("{\"stream\":\"built\"}\n"))
		case strings.HasSuffix(r.URL.Path, "/json") && built.Load():
			_, _ = w.Write([]byte(`{"Id":"sha256:embedded"}`))
		default:
			http.Error(w, "missing", http.StatusNotFound)
		}
	}))
	defer server.Close()
	cli, err := client.NewClientWithOpts(client.WithHost(server.URL), client.WithVersion("1.47"))
	assert.NilError(t, err)
	defer func() { _ = cli.Close() }()
	choice, err := ResolveKit(t.Context(), NewBuilder(cli), KitStatePath(t.TempDir()))
	assert.NilError(t, err)
	assert.Equal(t, choice.Ref, "sha256:embedded")
	assert.Assert(t, strings.Contains(choice.Notice, "published kit isn't available (denied)"))
}

func TestEmbeddedBuildIgnoresMetadataFailure(t *testing.T) {
	docker := &fakeDocker{imageIDs: map[string]string{}}
	blocked := filepath.Join(t.TempDir(), "file")
	assert.NilError(t, os.WriteFile(blocked, nil, 0o600))
	for range 2 {
		choice, err := BuildEmbeddedKit(t.Context(), NewBuilder(docker), filepath.Join(blocked, "state"), BuildOptions{}, nil)
		assert.NilError(t, err)
		assert.Equal(t, choice.Ref, "sha256:built")
	}
	assert.Equal(t, len(docker.builds), 2, "force builds bypass existing tags")
}
func TestKitCancellationDoesNotBuild(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	f := &localKitFake{fakePuller: fakePuller{pullErr: context.Canceled}, images: map[string]string{}}
	_, err := ResolveKit(ctx, f, KitStatePath(t.TempDir()))
	assert.Assert(t, errors.Is(err, context.Canceled))
	assert.Equal(t, f.builds, 0)
}

func TestLocalKitDoesNotOverrideRefresh(t *testing.T) {
	state := KitStatePath(t.TempDir())
	assert.NilError(t, writeLastKit(state, lastKit{Kit: KitVersion + 1, Ref: "digest:selected", Selected: "registry:next"}))
	f := &localKitFake{fakePuller: fakePuller{ref: "digest:selected"}, images: map[string]string{KitRevisionRef(): "local"}}
	choice, err := ResolveKit(t.Context(), f, state)
	assert.NilError(t, err)
	assert.Equal(t, choice.Ref, "digest:selected")
	assert.Equal(t, choice.Kit, KitVersion+1)
	assert.Equal(t, f.pulls, 1)
	assert.Equal(t, f.builds, 0)
}
