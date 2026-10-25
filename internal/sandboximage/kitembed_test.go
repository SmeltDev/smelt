package sandboximage

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

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
	assert.DeepEqual(t, a.Bytes(), b.Bytes())
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
	assert.Equal(t, len(docker.pulls), 1)
	_, err = ResolveKit(t.Context(), builder, state)
	assert.NilError(t, err)
	assert.Equal(t, len(docker.builds), 1)
	assert.Equal(t, len(docker.pulls), 1)
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
