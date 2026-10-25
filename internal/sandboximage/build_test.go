package sandboximage

import (
	"archive/tar"
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/containerd/errdefs"
	"github.com/docker/docker/api/types/build"
	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/image"
	"github.com/docker/docker/api/types/network"
	"github.com/docker/docker/client"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	"gotest.tools/v3/assert"
)

func TestReadBuildStream(t *testing.T) {
	stream := `{"stream":"Step 1/3 : FROM x\n"}
{"stream":" ---> abc\n"}
{"aux":{"ID":"sha256:1"}}
{"errorDetail":{"message":"boom"},"error":"The command '/bin/sh -c false' returned a non-zero code: 1"}
`
	var lines []string
	err := readBuildStream(strings.NewReader(stream), func(l string) { lines = append(lines, l) })
	assert.ErrorContains(t, err, "non-zero code")
	assert.DeepEqual(t, lines, []string{"Step 1/3 : FROM x", " ---> abc", "The command '/bin/sh -c false' returned a non-zero code: 1"})

	lines = nil
	assert.NilError(t, readBuildStream(strings.NewReader(`{"stream":"ok\n"}`+"\n"), func(l string) { lines = append(lines, l) }))
	assert.DeepEqual(t, lines, []string{"ok"})
}

func TestTailKeepsLastLines(t *testing.T) {
	var tl tail
	for i := range logTailLines + 10 {
		tl.add(strings.Repeat("x", i%3+1))
	}
	assert.Equal(t, len(tl.lines), logTailLines)
}

// fakeDocker is a daemon that has some images and answers builds with a canned stream.
type fakeDocker struct {
	images   map[string]bool
	stream   string
	builds   []build.ImageBuildOptions
	context  []byte   // the last build context
	contexts [][]byte // every build context, in order
}

func (f *fakeDocker) ImageBuild(_ context.Context, r io.Reader, o build.ImageBuildOptions) (build.ImageBuildResponse, error) {
	f.builds = append(f.builds, o)
	f.context, _ = io.ReadAll(r)
	f.contexts = append(f.contexts, f.context)
	return build.ImageBuildResponse{Body: io.NopCloser(strings.NewReader(f.stream))}, nil
}

func (f *fakeDocker) ImageInspect(_ context.Context, ref string, _ ...client.ImageInspectOption) (image.InspectResponse, error) {
	if f.images[ref] {
		return image.InspectResponse{}, nil
	}
	return image.InspectResponse{}, errdefs.ErrNotFound
}

func (f *fakeDocker) ImagePull(context.Context, string, image.PullOptions) (io.ReadCloser, error) {
	return nil, errors.New("no network")
}

func goPlan(t *testing.T) Plan {
	t.Helper()
	dir := writeRepo(t, map[string]string{
		"Dockerfile.smelt": "FROM x\nCOPY go.mod /tmp/\n",
		"go.mod":           "module a\n",
	})
	p, err := NewPlan(dir, "Dockerfile.smelt")
	assert.NilError(t, err)
	return p
}

func TestEnsureSkipsAnExistingTag(t *testing.T) {
	plan := goPlan(t)
	f := &fakeDocker{images: map[string]bool{plan.Tag: true}}
	built, err := NewBuilder(f).Ensure(context.Background(), plan, EnsureOptions{})
	assert.NilError(t, err)
	assert.Assert(t, !built)
	assert.Equal(t, len(f.builds), 0)
}

func TestEnsureBuildsAMissingTagWithLabelAndLog(t *testing.T) {
	plan := goPlan(t)
	f := &fakeDocker{stream: `{"stream":"Step 1/2 : FROM x\n"}` + "\n"}
	logPath := filepath.Join(t.TempDir(), "logs", "build.log")
	var told string
	built, err := NewBuilder(f).Ensure(context.Background(), plan, EnsureOptions{LogPath: logPath, Building: func(tag string) { told = tag }})
	assert.NilError(t, err)
	assert.Assert(t, built)
	assert.Equal(t, told, plan.Tag)
	assert.DeepEqual(t, f.builds[0].Tags, []string{plan.Tag})
	assert.Equal(t, f.builds[0].Labels[LabelSandbox], plan.Slug)
	assert.Equal(t, f.builds[0].Dockerfile, "Dockerfile.smelt")
	assert.Assert(t, !f.builds[0].NoCache)
	logged, err := os.ReadFile(logPath)
	assert.NilError(t, err)
	assert.Assert(t, strings.Contains(string(logged), "Step 1/2"))
	assert.Assert(t, strings.Contains(string(f.context), "module a"))
}

func TestEnsureRebuildIgnoresTheCache(t *testing.T) {
	plan := goPlan(t)
	f := &fakeDocker{images: map[string]bool{plan.Tag: true}}
	built, err := NewBuilder(f).Ensure(context.Background(), plan, EnsureOptions{Rebuild: true})
	assert.NilError(t, err)
	assert.Assert(t, built)
	assert.Assert(t, f.builds[0].NoCache)
}

func TestBuildFailureCarriesTheLogTail(t *testing.T) {
	plan := goPlan(t)
	f := &fakeDocker{stream: `{"stream":"E: Unable to locate package nodjs\n"}` + "\n" + `{"error":"returned a non-zero code: 100"}` + "\n"}
	_, err := NewBuilder(f).Ensure(context.Background(), plan, EnsureOptions{})
	var be *BuildError
	assert.Assert(t, errors.As(err, &be), "%v", err)
	assert.Equal(t, be.Tag, plan.Tag)
	assert.Assert(t, strings.Contains(be.LogTail, "nodjs"))
}

// TestBuildLive builds a tiny image on a real daemon; it skips without one.
func TestBuildLive(t *testing.T) {
	b, err := Connect()
	if err != nil {
		t.Skipf("no docker: %v", err)
	}
	defer func() { _ = b.Close() }()
	if _, err := b.Exists(context.Background(), "smelt-live-probe:none"); err != nil {
		t.Skipf("docker daemon unreachable: %v", err)
	}
	dir := writeRepo(t, map[string]string{
		"Dockerfile.smelt": "FROM busybox:stable\nCOPY go.mod /tmp/go.mod\nCMD [\"sleep\", \"infinity\"]\n",
		"go.mod":           "module tiny\n",
	})
	plan, err := NewPlan(dir, "Dockerfile.smelt")
	assert.NilError(t, err)
	_, err = b.Ensure(context.Background(), plan, EnsureOptions{})
	assert.NilError(t, err)
	exists, err := b.Exists(context.Background(), plan.Tag)
	assert.NilError(t, err)
	assert.Assert(t, exists)
}

// dockerfileOf is the Dockerfile of the i-th build the fake daemon ran, read
// from the context it received.
func (f *fakeDocker) dockerfileOf(t *testing.T, i int) string {
	t.Helper()
	tr := tar.NewReader(bytes.NewReader(f.contexts[i]))
	for {
		h, err := tr.Next()
		assert.NilError(t, err)
		if h.Name == f.builds[i].Dockerfile {
			raw, err := io.ReadAll(tr)
			assert.NilError(t, err)
			return string(raw)
		}
	}
}

func TestEnsureBuildsTheToolchainThenTheSandbox(t *testing.T) {
	const kit = KitRegistryRepo + "@sha256:aaaa"
	for _, fx := range fixtures {
		t.Run(fx.name, func(t *testing.T) {
			tool, err := planFor(t, fx.toolchain+fx.old)
			assert.NilError(t, err)
			plan := tool.SandboxPlan(kit)
			f := &fakeDocker{stream: `{"stream":"ok\n"}` + "\n"}
			var told []string
			built, err := NewBuilder(f).Ensure(context.Background(), plan, EnsureOptions{Building: func(tag string) { told = append(told, tag) }})
			assert.NilError(t, err)
			assert.Assert(t, built)
			assert.DeepEqual(t, told, []string{tool.Tag, plan.Tag})
			assert.Equal(t, len(f.builds), 2)
			assert.DeepEqual(t, f.builds[0].Tags, []string{tool.Tag})
			assert.DeepEqual(t, f.builds[1].Tags, []string{plan.Tag})
			assert.Equal(t, f.dockerfileOf(t, 0), fx.toolchain)
			assert.Equal(t, f.dockerfileOf(t, 1), SandboxDockerfile(tool.Tag, kit))
			assert.Equal(t, f.builds[1].Labels[LabelSandbox], plan.Slug)
		})
	}
}

func TestEnsureBuildsOnlyTheSandboxWhenTheToolchainExists(t *testing.T) {
	tool := goPlan(t)
	plan := tool.SandboxPlan(KitRegistryRepo + "@sha256:bbbb")
	f := &fakeDocker{images: map[string]bool{tool.Tag: true}}
	built, err := NewBuilder(f).Ensure(context.Background(), plan, EnsureOptions{})
	assert.NilError(t, err)
	assert.Assert(t, built)
	assert.Equal(t, len(f.builds), 1)
	assert.DeepEqual(t, f.builds[0].Tags, []string{plan.Tag})
}

func TestEnsureSkipsAnExistingSandboxWithoutItsToolchain(t *testing.T) {
	plan := goPlan(t).SandboxPlan(KitRegistryRepo + "@sha256:cccc")
	f := &fakeDocker{images: map[string]bool{plan.Tag: true}}
	built, err := NewBuilder(f).Ensure(context.Background(), plan, EnsureOptions{})
	assert.NilError(t, err)
	assert.Assert(t, !built)
	assert.Equal(t, len(f.builds), 0)
}

func TestEnsureRebuildIgnoresTheCacheForBothImages(t *testing.T) {
	tool := goPlan(t)
	plan := tool.SandboxPlan(KitRegistryRepo + "@sha256:dddd")
	f := &fakeDocker{images: map[string]bool{tool.Tag: true, plan.Tag: true}}
	_, err := NewBuilder(f).Ensure(context.Background(), plan, EnsureOptions{Rebuild: true})
	assert.NilError(t, err)
	assert.Equal(t, len(f.builds), 2)
	assert.Assert(t, f.builds[0].NoCache && f.builds[1].NoCache)
}

func TestEnsureStopsWhenTheToolchainFails(t *testing.T) {
	plan := goPlan(t).SandboxPlan(KitRegistryRepo + "@sha256:eeee")
	f := &fakeDocker{stream: `{"error":"boom"}` + "\n"}
	_, err := NewBuilder(f).Ensure(context.Background(), plan, EnsureOptions{})
	var be *BuildError
	assert.Assert(t, errors.As(err, &be), "%v", err)
	assert.Equal(t, len(f.builds), 1)
}

// TestSandboxLayerLive builds the four fixtures, a toolchain-only Dockerfile
// and one with an old kit section on a root (debian) and a non-root (cimg)
// base, with the kit built from source, and runs each sandbox image: the
// three agent CLIs answer and the user is the base's. It skips without Docker,
// and with -short: it downloads the agent CLIs.
func TestSandboxLayerLive(t *testing.T) {
	if testing.Short() {
		t.Skip("downloads the agent CLIs")
	}
	b, err := Connect()
	if err != nil {
		t.Skipf("no docker: %v", err)
	}
	defer func() { _ = b.Close() }()
	ctx := context.Background()
	if _, err := b.Exists(ctx, "smelt-live-probe:none"); err != nil {
		t.Skipf("docker daemon unreachable: %v", err)
	}
	docker, err := exec.LookPath("docker")
	if err != nil {
		t.Skip("no docker CLI to run the image with")
	}
	kitDir, err := filepath.Abs(filepath.Join("..", "..", "kit"))
	assert.NilError(t, err)
	kitID, err := b.BuildKit(ctx, kitDir, BuildOptions{Tag: KitLocalRef()})
	assert.NilError(t, err)

	live := []struct {
		name, toolchain, old string
		root                 bool
	}{
		{"debian toolchain only", "FROM debian:bookworm-slim\n", "", true},
		{"cimg toolchain only", "FROM cimg/base:stable\n", "", false},
	}
	for _, l := range live {
		for _, old := range []string{"", "\n# --- smelt agent kit (v4) ---\nRUN false\n"} {
			name := l.name
			if old != "" {
				name = strings.Replace(name, "toolchain only", "old section", 1)
			}
			t.Run(name, func(t *testing.T) {
				dir := writeRepo(t, map[string]string{"Dockerfile.smelt": l.toolchain + old})
				tool, err := NewPlan(dir, "Dockerfile.smelt")
				assert.NilError(t, err)
				plan := tool.SandboxPlan(kitID)
				_, err = b.Ensure(ctx, plan, EnsureOptions{})
				assert.NilError(t, err)
				out, err := exec.Command(docker, "run", "--rm", plan.Tag, "sh", "-c", kitCheck+" && git init /tmp/runtime-check && git -C /tmp/runtime-check status && bash -c 'echo bash-ok' && id -u").CombinedOutput()
				assert.NilError(t, err, string(out))
				uid := strings.TrimSpace(string(out[strings.LastIndex(strings.TrimSpace(string(out)), "\n")+1:]))
				if l.root {
					assert.Equal(t, uid, "0", string(out))
				} else {
					assert.Assert(t, uid != "0", string(out))
				}
			})
		}
	}
}

func (f *fakeDocker) ContainerCreate(context.Context, *container.Config, *container.HostConfig, *network.NetworkingConfig, *ocispec.Platform, string) (container.CreateResponse, error) {
	return container.CreateResponse{}, errors.New("no containers")
}

func (f *fakeDocker) ContainerStart(context.Context, string, container.StartOptions) error {
	return errors.New("no containers")
}

func (f *fakeDocker) ContainerWait(context.Context, string, container.WaitCondition) (<-chan container.WaitResponse, <-chan error) {
	return nil, nil
}

func (f *fakeDocker) ContainerLogs(context.Context, string, container.LogsOptions) (io.ReadCloser, error) {
	return nil, errors.New("no containers")
}

func (f *fakeDocker) ContainerRemove(context.Context, string, container.RemoveOptions) error {
	return nil
}
