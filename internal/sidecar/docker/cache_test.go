package docker

import (
	"bufio"
	"context"
	"errors"
	"net"
	"strings"
	"testing"

	cerrdefs "github.com/containerd/errdefs"
	"github.com/docker/docker/api/types"
	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/mount"
	dockerspec "github.com/moby/docker-image-spec/specs-go/v1"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	"gotest.tools/v3/assert"
)

func (f *fakeDocker) ContainerExecCreate(_ context.Context, _ string, opts container.ExecOptions) (container.ExecCreateResponse, error) {
	f.execs = append(f.execs, opts)
	return container.ExecCreateResponse{ID: "exec"}, nil
}

func (f *fakeDocker) ContainerExecAttach(context.Context, string, container.ExecAttachOptions) (types.HijackedResponse, error) {
	conn, _ := net.Pipe()
	return types.HijackedResponse{Conn: conn, Reader: bufio.NewReader(strings.NewReader(""))}, nil
}

func (f *fakeDocker) ContainerExecInspect(context.Context, string) (container.ExecInspect, error) {
	return container.ExecInspect{ExitCode: f.execCode}, nil
}

func (f *fakeDocker) VolumeRemove(_ context.Context, name string, _ bool) error {
	if err := f.volumeErrs[name]; err != nil {
		return err
	}
	f.removedVolumes = append(f.removedVolumes, name)
	return nil
}

func imageCfg(user string, env ...string) *dockerspec.DockerOCIImageConfig {
	return &dockerspec.DockerOCIImageConfig{ImageConfig: ocispec.ImageConfig{User: user, Env: env}}
}

func TestLayoutCaches(t *testing.T) {
	tests := []struct {
		name    string
		key     string
		goStack bool
		cfg     *dockerspec.DockerOCIImageConfig
		want    cacheLayout
	}{
		{"root image", "k1", false, imageCfg(""),
			cacheLayout{Volumes: []cacheVolume{{"smelt-cache-k1", "/root/.cache"}}}},
		{"named user's home is /home/<user>", "k1", false, imageCfg("circleci"),
			cacheLayout{Owner: "circleci", Volumes: []cacheVolume{{"smelt-cache-k1", "/home/circleci/.cache"}}}},
		{"HOME in the image wins", "k1", false, imageCfg("1000:1000", "HOME=/work/me"),
			cacheLayout{Owner: "1000:1000", Volumes: []cacheVolume{{"smelt-cache-k1", "/work/me/.cache"}}}},
		{"go adds the module cache at the default path", "k1", true, imageCfg("circleci"),
			cacheLayout{Owner: "circleci", Volumes: []cacheVolume{
				{"smelt-cache-k1", "/home/circleci/.cache"}, {"smelt-gomod-k1", "/home/circleci/go/pkg/mod"}}}},
		{"GOMODCACHE wins over GOPATH", "k1", true, imageCfg("", "GOPATH=/go", "GOMODCACHE=/mods"),
			cacheLayout{Volumes: []cacheVolume{{"smelt-cache-k1", "/root/.cache"}, {"smelt-gomod-k1", "/mods"}}}},
		{"GOPATH gives the module cache", "k1", true, imageCfg("", "GOPATH=/go"),
			cacheLayout{Volumes: []cacheVolume{{"smelt-cache-k1", "/root/.cache"}, {"smelt-gomod-k1", "/go/pkg/mod"}}}},
		{"a numeric user with no HOME has no known home", "k1", true, imageCfg("1000"), cacheLayout{}},
		{"no key, no volumes", "", true, imageCfg(""), cacheLayout{}},
		{"an image without config has none", "k1", true, nil, cacheLayout{}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.DeepEqual(t, layoutCaches(tt.key, tt.goStack, tt.cfg), tt.want)
		})
	}
}

// createWith creates a member on a fake daemon whose image has cfg, and returns
// the fake.
func createWith(t *testing.T, key string, goStack bool, cfg *dockerspec.DockerOCIImageConfig) *fakeDocker {
	t.Helper()
	f := newFakeDocker()
	f.imgConfig = cfg
	p := f.provider()
	p.workRoot = t.TempDir()
	p.CacheKey, p.GoStack = key, goStack
	_, err := p.Create(context.Background(), "m", "", "/repo")
	assert.NilError(t, err)
	return f
}

func volumeMounts(t *testing.T, f *fakeDocker) map[string]string {
	t.Helper()
	got := map[string]string{}
	for _, m := range f.hostConfigs[0].Mounts {
		assert.Equal(t, m.Type, mount.TypeVolume)
		got[m.Source] = m.Target
	}
	return got
}

func TestCreateMountsTheCacheVolumes(t *testing.T) {
	f := createWith(t, "abc", false, imageCfg("circleci"))
	assert.DeepEqual(t, volumeMounts(t, f), map[string]string{"smelt-cache-abc": "/home/circleci/.cache"})
	assert.Equal(t, len(f.hostConfigs[0].Binds), 1, "the workspace is still bind-mounted")
	labels := f.hostConfigs[0].Mounts[0].VolumeOptions.Labels
	assert.Equal(t, labels["smelt.cache-key"], "abc")
}

func TestCreateMountsTheModuleCacheOnlyForGo(t *testing.T) {
	f := createWith(t, "abc", true, imageCfg("circleci"))
	assert.DeepEqual(t, volumeMounts(t, f), map[string]string{
		"smelt-cache-abc": "/home/circleci/.cache",
		"smelt-gomod-abc": "/home/circleci/go/pkg/mod",
	})
}

func TestCreateMountsNothingWithoutAKey(t *testing.T) {
	f := createWith(t, "", true, imageCfg("circleci"))
	assert.Equal(t, len(f.hostConfigs[0].Mounts), 0)
	assert.Equal(t, len(f.execs), 0)
}

func TestCreateHandsTheMountPointsToTheImagesUser(t *testing.T) {
	f := createWith(t, "abc", true, imageCfg("circleci"))
	assert.Equal(t, len(f.execs), 1)
	assert.Equal(t, f.execs[0].User, "root")
	assert.DeepEqual(t, f.execs[0].Cmd, []string{"sh", "-c", "chown 'circleci' '/home/circleci/.cache' '/home/circleci/go/pkg/mod'"})
}

func TestCreateLeavesRootsVolumesAlone(t *testing.T) {
	f := createWith(t, "abc", false, imageCfg(""))
	assert.Equal(t, len(f.hostConfigs[0].Mounts), 1)
	assert.Equal(t, len(f.execs), 0)
}

func TestCreateFailsWhenTheVolumesCannotBeHandedOver(t *testing.T) {
	f := newFakeDocker()
	f.imgConfig, f.execCode = imageCfg("circleci"), 1
	p := f.provider()
	p.workRoot = t.TempDir()
	p.CacheKey = "abc"
	_, err := p.Create(context.Background(), "m", "", "/repo")
	assert.ErrorContains(t, err, "chown exited 1")
}

func TestRemoveCacheVolumes(t *testing.T) {
	f := newFakeDocker()
	f.volumeErrs = map[string]error{"smelt-gomod-abc": errors.Join(errors.New("no such volume"), cerrdefs.ErrNotFound)}
	removed, err := f.provider().RemoveCacheVolumes(context.Background(), "abc")
	assert.NilError(t, err)
	assert.DeepEqual(t, removed, []string{"smelt-cache-abc"})

	f.volumeErrs = map[string]error{"smelt-cache-abc": errors.New("volume is in use")}
	removed, err = f.provider().RemoveCacheVolumes(context.Background(), "abc")
	assert.ErrorContains(t, err, "volume is in use")
	assert.DeepEqual(t, removed, []string{"smelt-gomod-abc"})
}

func (f *fakeDocker) ContainerRemove(context.Context, string, container.RemoveOptions) error {
	return nil
}
