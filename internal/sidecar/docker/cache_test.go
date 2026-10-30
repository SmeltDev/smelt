package docker

import (
	"bufio"
	"context"
	"errors"
	"io"
	"log"
	"net"
	"os"
	"os/exec"
	"slices"
	"strconv"
	"strings"
	"testing"

	cerrdefs "github.com/containerd/errdefs"
	"github.com/docker/docker/api/types"
	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/mount"
	"github.com/docker/docker/api/types/volume"
	dockerspec "github.com/moby/docker-image-spec/specs-go/v1"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	"gotest.tools/v3/assert"
)

func (f *fakeDocker) ContainerExecCreate(_ context.Context, _ string, opts container.ExecOptions) (container.ExecCreateResponse, error) {
	f.execs = append(f.execs, opts)
	return container.ExecCreateResponse{ID: "exec"}, nil
}

func (f *fakeDocker) ContainerExecAttach(context.Context, string, container.ExecAttachOptions) (types.HijackedResponse, error) {
	conn, peer := net.Pipe()
	var framed string
	if f.execOut != nil {
		if out := f.execOut(f.execs[len(f.execs)-1]); out != "" {
			framed = string([]byte{1, 0, 0, 0, 0, 0, 0, byte(len(out))}) + out
		}
	}
	var src io.Reader = strings.NewReader(framed)
	if f.execs[len(f.execs)-1].AttachStdin {
		// The output waits for the script written to stdin, so a test can read
		// it as soon as the exec returns.
		got := make(chan struct{})
		go func() {
			buf := make([]byte, 1<<20)
			n, _ := peer.Read(buf)
			f.stdinMu.Lock()
			f.stdins = append(f.stdins, string(buf[:n]))
			f.stdinMu.Unlock()
			close(got)
		}()
		src = io.MultiReader(readerFunc(func([]byte) (int, error) { <-got; return 0, io.EOF }), src)
	}
	return types.HijackedResponse{Conn: conn, Reader: bufio.NewReader(src)}, nil
}

// readerFunc adapts a function to io.Reader.
type readerFunc func([]byte) (int, error)

func (r readerFunc) Read(p []byte) (int, error) { return r(p) }

// sentScripts are the stdin scripts execs received so far.
func (f *fakeDocker) sentScripts() []string {
	f.stdinMu.Lock()
	defer f.stdinMu.Unlock()
	return slices.Clone(f.stdins)
}

func (f *fakeDocker) ContainerExecInspect(context.Context, string) (container.ExecInspect, error) {
	code := f.execCode
	if f.execCodeFor != nil {
		code = f.execCodeFor(f.execs[len(f.execs)-1])
	}
	return container.ExecInspect{ExitCode: code}, nil
}

func (f *fakeDocker) ContainerWait(context.Context, string, container.WaitCondition) (<-chan container.WaitResponse, <-chan error) {
	ch := make(chan container.WaitResponse, 1)
	ch <- container.WaitResponse{}
	return ch, make(chan error)
}

// ContainerLogs answers the probe with f.uid in Docker's multiplexed framing.
func (f *fakeDocker) ContainerLogs(context.Context, string, container.LogsOptions) (io.ReadCloser, error) {
	out := f.uid + "\n"
	hdr := []byte{1, 0, 0, 0, 0, 0, 0, byte(len(out))}
	return io.NopCloser(strings.NewReader(string(hdr) + out)), nil
}

func (f *fakeDocker) VolumeList(context.Context, volume.ListOptions) (volume.ListResponse, error) {
	var res volume.ListResponse
	for _, n := range f.volumes {
		res.Volumes = append(res.Volumes, &volume.Volume{Name: n})
	}
	return res, nil
}

func (f *fakeDocker) VolumeRemove(_ context.Context, name string, force bool) error {
	if force {
		return errors.New("forced cache removal is forbidden")
	}
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
			cacheLayout{UID: "1001", Volumes: []cacheVolume{{"smelt-cache-k1-1001", "/root/.cache"}}}},
		{"named user's home is /home/<user>", "k1", false, imageCfg("circleci"),
			cacheLayout{Owner: "circleci", UID: "1001", Volumes: []cacheVolume{{"smelt-cache-k1-1001", "/home/circleci/.cache"}}}},
		{"HOME in the image wins", "k1", false, imageCfg("1000:1000", "HOME=/work/me"),
			cacheLayout{Owner: "1000:1000", UID: "1001", Volumes: []cacheVolume{{"smelt-cache-k1-1001", "/work/me/.cache"}}}},
		{"go adds the module cache at the default path", "k1", true, imageCfg("circleci"),
			cacheLayout{Owner: "circleci", UID: "1001", Volumes: []cacheVolume{
				{"smelt-cache-k1-1001", "/home/circleci/.cache"}, {"smelt-gomod-k1-1001", "/home/circleci/go/pkg/mod"}}}},
		{"GOMODCACHE wins over GOPATH", "k1", true, imageCfg("", "GOPATH=/go", "GOMODCACHE=/mods"),
			cacheLayout{UID: "1001", Volumes: []cacheVolume{{"smelt-cache-k1-1001", "/root/.cache"}, {"smelt-gomod-k1-1001", "/mods"}}}},
		{"GOPATH gives the module cache", "k1", true, imageCfg("", "GOPATH=/go"),
			cacheLayout{UID: "1001", Volumes: []cacheVolume{{"smelt-cache-k1-1001", "/root/.cache"}, {"smelt-gomod-k1-1001", "/go/pkg/mod"}}}},
		{"a numeric user with no HOME has no known home", "k1", true, imageCfg("1000"), cacheLayout{}},
		{"no key, no volumes", "", true, imageCfg(""), cacheLayout{}},
		{"an image without config has none", "k1", true, nil, cacheLayout{}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.DeepEqual(t, layoutCaches(tt.key, "1001", tt.goStack, tt.cfg), tt.want)
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
		assert.Assert(t, m.VolumeOptions.NoCopy, "image cache contents must not populate a volume after maintenance")
		got[m.Source] = m.Target
	}
	return got
}

func TestCreateMountsTheCacheVolumes(t *testing.T) {
	f := createWith(t, "abc", false, imageCfg("circleci"))
	assert.DeepEqual(t, volumeMounts(t, f), map[string]string{"smelt-cache-abc-1001": "/home/circleci/.cache"})
	assert.Equal(t, len(f.hostConfigs[0].Binds), 1, "the workspace is still bind-mounted")
	labels := f.hostConfigs[0].Mounts[0].VolumeOptions.Labels
	assert.Equal(t, labels["smelt.cache-key"], "abc")
}

func TestCreateMountsTheModuleCacheOnlyForGo(t *testing.T) {
	f := createWith(t, "abc", true, imageCfg("circleci"))
	assert.DeepEqual(t, volumeMounts(t, f), map[string]string{
		"smelt-cache-abc-1001": "/home/circleci/.cache",
		"smelt-gomod-abc-1001": "/home/circleci/go/pkg/mod",
	})
}

func TestCreateMountsNothingWithoutAKey(t *testing.T) {
	f := createWith(t, "", true, imageCfg("circleci"))
	assert.Equal(t, len(f.hostConfigs[0].Mounts), 0)
	assert.Equal(t, len(f.execs), 0)
}

func scripts(f *fakeDocker) []string {
	var out []string
	for _, e := range f.execs {
		out = append(out, e.Cmd[2])
	}
	return out
}

func TestCreateChecksThenHandsTheMountPointsToTheImagesUser(t *testing.T) {
	f := createWith(t, "abc", true, imageCfg("circleci"))
	got := scripts(f)
	assert.Equal(t, len(got), 3)
	assert.Equal(t, f.execs[0].User, "root")
	assert.Assert(t, strings.Contains(got[0], "find"), "the content check runs before the chown: %q", got[0])
	assert.Equal(t, f.execs[1].User, "root")
	assert.Equal(t, got[1], "chown 'circleci' '/home/circleci/.cache' '/home/circleci/go/pkg/mod'")
	assert.Equal(t, f.execs[2].User, "")
	assert.Equal(t, got[2], "test -w '/home/circleci/.cache' && test -w '/home/circleci/go/pkg/mod'")
}

func TestCreateLeavesRootsVolumesAlone(t *testing.T) {
	f := newFakeDocker()
	f.uid, f.imgConfig = "0", imageCfg("")
	p := f.provider()
	p.workRoot = t.TempDir()
	p.CacheKey = "abc"
	_, err := p.Create(context.Background(), "m", "", "/repo")
	assert.NilError(t, err)
	assert.Equal(t, len(f.hostConfigs[0].Mounts), 1)
	assert.Equal(t, len(f.execs), 0)
}

func TestCreateFailsWhenTheVolumesCannotBeHandedOver(t *testing.T) {
	f := newFakeDocker()
	f.imgConfig = imageCfg("circleci")
	f.execCodeFor = func(o container.ExecOptions) int {
		if strings.HasPrefix(o.Cmd[2], "chown") {
			return 1
		}
		return 0
	}
	p := f.provider()
	p.workRoot = t.TempDir()
	p.CacheKey = "abc"
	_, err := p.Create(context.Background(), "m", "", "/repo")
	assert.ErrorContains(t, err, "chown exited 1")
}

func TestVolumeNamesCarryTheUID(t *testing.T) {
	mounts := map[string]map[string]string{}
	for _, uid := range []string{"0", "1001"} {
		f := newFakeDocker()
		f.uid, f.imgConfig = uid, imageCfg("")
		p := f.provider()
		p.workRoot = t.TempDir()
		p.CacheKey = "abc"
		_, err := p.Create(context.Background(), "m", "", "/repo")
		assert.NilError(t, err)
		mounts[uid] = volumeMounts(t, f)
	}
	assert.DeepEqual(t, mounts["0"], map[string]string{"smelt-cache-abc-0": "/root/.cache"})
	assert.DeepEqual(t, mounts["1001"], map[string]string{"smelt-cache-abc-1001": "/root/.cache"})
}

func TestUIDIsProbedOncePerImage(t *testing.T) {
	f := newFakeDocker()
	f.imgConfig = imageCfg("circleci")
	p := f.provider()
	p.workRoot = t.TempDir()
	p.CacheKey = "abc"
	for range 2 {
		_, err := p.Create(context.Background(), "m", "", "/repo")
		assert.NilError(t, err)
	}
	assert.Equal(t, f.probes, 1)
	f.imgID = "sha256:other"
	_, err := p.Create(context.Background(), "m", "", "/repo")
	assert.NilError(t, err)
	assert.Equal(t, f.probes, 2, "another image ID is probed again")
}

func TestForeignCacheContentUsesATemporaryCache(t *testing.T) {
	f := newFakeDocker()
	f.imgConfig = imageCfg("circleci")
	f.execCodeFor = func(o container.ExecOptions) int {
		if strings.Contains(o.Cmd[2], "find") {
			return 1
		}
		return 0
	}
	p := f.provider()
	p.workRoot = t.TempDir()
	p.CacheKey = "abc"
	var logged strings.Builder
	log.SetOutput(&logged)
	t.Cleanup(func() { log.SetOutput(os.Stderr) })
	inst, err := p.Create(context.Background(), "m", "", "/repo")
	assert.NilError(t, err)
	assert.Equal(t, strings.Count(logged.String(), "is not writable by uid 1001"), 1)
	assert.Assert(t, strings.Contains(logged.String(), "smelt-cache-abc-1001"))

	_, err = p.Exec(context.Background(), inst.ID, "task ci:test", map[string]string{"GOMODCACHE": "/mine"}, nil, nil)
	assert.NilError(t, err)
	got := f.execs[len(f.execs)-1].Env
	slices.Sort(got)
	assert.DeepEqual(t, got, []string{
		"GOCACHE=/tmp/smelt-cache-1001/go-build",
		"GOMODCACHE=/mine",
		"UV_CACHE_DIR=/tmp/smelt-cache-1001/uv",
		"XDG_CACHE_HOME=/tmp/smelt-cache-1001/xdg",
	})
}

func TestUnwritableMountPointUsesATemporaryCache(t *testing.T) {
	f := newFakeDocker()
	f.imgConfig = imageCfg("circleci")
	f.execCodeFor = func(o container.ExecOptions) int {
		if strings.HasPrefix(o.Cmd[2], "test -w") {
			return 1
		}
		return 0
	}
	p := f.provider()
	p.workRoot = t.TempDir()
	p.CacheKey = "abc"
	inst, err := p.Create(context.Background(), "m", "", "/repo")
	assert.NilError(t, err)
	_, err = p.Exec(context.Background(), inst.ID, "true", nil, nil, nil)
	assert.NilError(t, err)
	assert.Equal(t, len(f.execs[len(f.execs)-1].Env), 4)
}

func TestWritableCachesChangeNothing(t *testing.T) {
	f := newFakeDocker()
	f.imgConfig = imageCfg("circleci")
	p := f.provider()
	p.workRoot = t.TempDir()
	p.CacheKey = "abc"
	var logged strings.Builder
	log.SetOutput(&logged)
	t.Cleanup(func() { log.SetOutput(os.Stderr) })
	inst, err := p.Create(context.Background(), "m", "", "/repo")
	assert.NilError(t, err)
	_, err = p.Exec(context.Background(), inst.ID, "true", nil, nil, nil)
	assert.NilError(t, err)
	assert.Equal(t, len(f.execs[len(f.execs)-1].Env), 0)
	assert.Assert(t, !strings.Contains(logged.String(), "not writable"))
}

// runScript runs script with the real sh.
func runScript(t *testing.T, script string) int {
	t.Helper()
	err := exec.Command("sh", "-c", script).Run()
	var exitErr *exec.ExitError
	switch {
	case err == nil:
		return 0
	case errors.As(err, &exitErr):
		return exitErr.ExitCode()
	}
	t.Fatal(err)
	return -1
}

func TestForeignContentScript(t *testing.T) {
	dir := t.TempDir()
	assert.NilError(t, os.MkdirAll(dir+"/go-build/ed", 0o755))
	assert.NilError(t, os.WriteFile(dir+"/go-build/ed/x-d", nil, 0o644))
	me := strconv.Itoa(os.Getuid())
	other := strconv.Itoa(os.Getuid() + 1)
	assert.Equal(t, runScript(t, foreignContentScript(me, []string{dir, dir + "/missing"})), 0, "owned content passes")
	assert.Equal(t, runScript(t, foreignContentScript(other, []string{dir})), 1, "content of another user is found")
	assert.Equal(t, runScript(t, foreignContentScript(other, []string{t.TempDir()})), 0, "an empty cache passes")
	assert.Equal(t, runScript(t, writableScript([]string{dir})), 0)
	assert.Equal(t, runScript(t, writableScript([]string{dir + "/missing"})), 1)
}

func TestMatchCacheVolume(t *testing.T) {
	for name, want := range map[string][2]bool{
		"smelt-cache-abc":        {true, true},
		"smelt-gomod-abc":        {true, true},
		"smelt-cache-abc-1001":   {false, true},
		"smelt-gomod-abc-0":      {false, true},
		"smelt-cache-abcd":       {false, false},
		"smelt-cache-abc-x":      {false, false},
		"smelt-cache-abc-1001-2": {false, false},
		"other":                  {false, false},
	} {
		legacy, ok := matchCacheVolume(name, "abc")
		assert.DeepEqual(t, [2]bool{legacy, ok}, want)
	}
}

func TestListAndRemoveCacheVolumes(t *testing.T) {
	f := newFakeDocker()
	f.volumes = []string{"smelt-cache-abc", "smelt-cache-abc-0", "smelt-cache-abc-1001", "smelt-gomod-abc-1001", "smelt-cache-abcd-1001"}
	provider := f.provider()
	provider.workRoot = t.TempDir()
	listed, err := provider.ListCacheVolumes(context.Background(), "abc")
	assert.NilError(t, err)
	assert.DeepEqual(t, listed, []CacheVolume{
		{"smelt-cache-abc", true}, {"smelt-cache-abc-0", false}, {"smelt-cache-abc-1001", false}, {"smelt-gomod-abc-1001", false}})

	f.volumeErrs = map[string]error{"smelt-gomod-abc-1001": errors.Join(errors.New("no such volume"), cerrdefs.ErrNotFound)}
	removed, err := provider.RemoveCacheVolumes(context.Background(), "abc")
	assert.NilError(t, err)
	assert.DeepEqual(t, removed, []string{"smelt-cache-abc", "smelt-cache-abc-0", "smelt-cache-abc-1001"})

	f.volumeErrs = map[string]error{"smelt-cache-abc-0": errors.New("volume is in use")}
	f.removedVolumes = nil
	removed, err = provider.RemoveCacheVolumes(context.Background(), "abc")
	assert.ErrorContains(t, err, "volume is in use")
	assert.DeepEqual(t, removed, []string{"smelt-cache-abc", "smelt-cache-abc-1001", "smelt-gomod-abc-1001"})
}

func (f *fakeDocker) ContainerRemove(context.Context, string, container.RemoveOptions) error {
	return nil
}

func TestAllProjectCacheInventory(t *testing.T) {
	f := &fakeDocker{volumes: []string{"smelt-cache-abc", "smelt-cache-abc-1001", "smelt-gomod-def", "smelt-gomod-def-0", "unrelated", "smelt-cache-abc-other"}}
	b := NewWithClient(f, "image", t.TempDir())
	vols, err := b.ListCacheVolumes(context.Background(), "")
	assert.NilError(t, err)
	assert.Equal(t, len(vols), 4)
	removed, err := b.RemoveCacheVolumes(context.Background(), "")
	assert.NilError(t, err)
	assert.Equal(t, len(removed), 4)
}

func (f *fakeDocker) DaemonHost() string { return "unix:///test-docker.sock" }
