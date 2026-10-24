package cmd

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"gotest.tools/v3/assert"

	"github.com/smeltdev/smelt/internal/config"
	"github.com/smeltdev/smelt/internal/iostream"
	"github.com/smeltdev/smelt/internal/sandboximage"
	"github.com/smeltdev/smelt/internal/sidecar/docker"
	"github.com/smeltdev/smelt/internal/smeltd"
)

func cfgWith(t *testing.T, body string) *config.ProjectConfig {
	t.Helper()
	var cfg config.ProjectConfig
	assert.NilError(t, json.Unmarshal([]byte(body), &cfg))
	return &cfg
}

func TestResolveBuildSettingsBuildsOnlyDockerfileImages(t *testing.T) {
	t.Setenv("DOCKER_HOST", "tcp://127.0.0.1:1")
	root := t.TempDir()
	streams := discardStreams()

	// A user-managed image passes through untouched, with no Docker involved.
	cfg := cfgWith(t, `{"factory":{"backend":"docker","image":"smelt-sandbox:local","dockerfile":"Dockerfile.smelt"}}`)
	got, err := resolveBuildSettings(context.Background(), streams, root, factorySettings{Attempts: 3}, factoryExplicit{}, cfg)
	assert.NilError(t, err)
	assert.Equal(t, got.Image, "smelt-sandbox:local")
	assert.Equal(t, got.ImageSource, imageFromConfig)

	// A managed one is built, which needs its Dockerfile.
	cfg = cfgWith(t, `{"factory":{"backend":"docker","dockerfile":"Dockerfile.smelt"}}`)
	_, err = resolveBuildSettings(context.Background(), streams, root, factorySettings{Attempts: 3}, factoryExplicit{}, cfg)
	assert.Equal(t, userMsg(err), "Could not read Dockerfile.smelt.")

	// With the Dockerfile present it gets as far as Docker, which is unreachable here.
	assert.NilError(t, os.WriteFile(filepath.Join(root, "Dockerfile.smelt"), []byte("FROM x\n"), 0o644))
	// The wait for Docker is cut to a second so the test does not sit out the
	// default three minutes.
	begin := time.Now()
	_, err = resolveBuildSettings(context.Background(), streams, root, factorySettings{Attempts: 3, ReadyTimeout: time.Second}, factoryExplicit{}, cfg)
	assert.ErrorContains(t, err, "did not finish starting")
	assert.Assert(t, time.Since(begin) < 10*time.Second, "took %s", time.Since(begin))
}

func TestPrepareDockerfileKeepsAnExistingFile(t *testing.T) {
	t.Setenv("DOCKER_HOST", "tcp://127.0.0.1:1")
	root := t.TempDir()
	path := filepath.Join(root, "Dockerfile.smelt")
	assert.NilError(t, os.WriteFile(path, []byte("FROM mine\n"), 0o644))
	assert.NilError(t, prepareDockerfile(context.Background(), discardStreams(), root, "Dockerfile.smelt", cfgWith(t, `{}`), false))
	got, err := os.ReadFile(path)
	assert.NilError(t, err)
	assert.Equal(t, string(got), "FROM mine\n")
}

func TestPrepareDockerfileRegenerateNeedsDocker(t *testing.T) {
	t.Setenv("DOCKER_HOST", "tcp://127.0.0.1:1")
	root := t.TempDir()
	path := filepath.Join(root, "Dockerfile.smelt")
	assert.NilError(t, os.WriteFile(path, []byte("FROM mine\n"), 0o644))
	err := prepareDockerfile(context.Background(), discardStreams(), root, "Dockerfile.smelt", cfgWith(t, `{}`), true)
	assert.Assert(t, err != nil)
	got, readErr := os.ReadFile(path)
	assert.NilError(t, readErr)
	assert.Equal(t, string(got), "FROM mine\n") // never overwritten on failure
}

func TestResolveBuildSettingsWarnsAboutAStaleUserImage(t *testing.T) {
	for name, tt := range map[string]struct {
		info sandboximage.ImageInfo
		err  error
		cfg  string
		want string
	}{
		"label missing": {sandboximage.ImageInfo{Status: sandboximage.ImageUnlabeled, Kit: 1},
			nil, `{"factory":{"backend":"docker","image":"smelt-sandbox:local"}}`,
			"smelt-sandbox:local has agent kit none; this smelt expects 5: rebuild it (docker build -t smelt-sandbox:local -f Dockerfile.smelt .)"},
		"lower": {sandboximage.ImageInfo{Status: sandboximage.ImageLabeled, Kit: 3, KitLabeled: true},
			nil, `{"factory":{"backend":"docker","image":"smelt-sandbox:local"}}`,
			"smelt-sandbox:local has agent kit 3; this smelt expects 5: rebuild it (docker build -t smelt-sandbox:local -f Dockerfile.smelt .)"},
		"equal":   {sandboximage.ImageInfo{Status: sandboximage.ImageLabeled, Kit: sandboximage.KitVersion, KitLabeled: true}, nil, `{"factory":{"backend":"docker","image":"smelt-sandbox:local"}}`, ""},
		"missing": {sandboximage.ImageInfo{Status: sandboximage.ImageMissing}, nil, `{"factory":{"backend":"docker","image":"smelt-sandbox:local"}}`, ""},
		"error":   {sandboximage.ImageInfo{}, errors.New("no daemon"), `{"factory":{"backend":"docker","image":"smelt-sandbox:local"}}`, ""},
		"custom dockerfile hint": {sandboximage.ImageInfo{Status: sandboximage.ImageUnlabeled, Kit: 1},
			nil, `{"factory":{"backend":"docker","image":"x:1","dockerfile":"ci/Dockerfile.dev"}}`,
			"x:1 has agent kit none; this smelt expects 5: rebuild it (docker build -t x:1 -f ci/Dockerfile.dev .)"},
		"not docker": {sandboximage.ImageInfo{Status: sandboximage.ImageUnlabeled, Kit: 1}, nil, `{"factory":{"backend":"circleci","image":"x:1"}}`, ""},
	} {
		t.Run(name, func(t *testing.T) {
			old := inspectUserImage
			t.Cleanup(func() { inspectUserImage = old })
			inspectUserImage = func(context.Context, string) (sandboximage.ImageInfo, error) { return tt.info, tt.err }
			got, err := resolveBuildSettings(context.Background(), discardStreams(), t.TempDir(), factorySettings{Attempts: 3}, factoryExplicit{}, cfgWith(t, tt.cfg))
			assert.NilError(t, err)
			assert.Equal(t, got.KitWarning, tt.want)
		})
	}
}

func TestAbsSandboxDockerfile(t *testing.T) {
	got, err := absSandboxDockerfile("")
	assert.NilError(t, err)
	assert.Equal(t, got, "")

	dir := t.TempDir()
	file := filepath.Join(dir, "Toolchain.Dockerfile")
	assert.NilError(t, os.WriteFile(file, []byte("FROM x\n"), 0o644))
	got, err = absSandboxDockerfile(file)
	assert.NilError(t, err)
	assert.Equal(t, got, file)

	t.Chdir(dir)
	got, err = absSandboxDockerfile("Toolchain.Dockerfile")
	assert.NilError(t, err)
	assert.Equal(t, got, file)

	for _, bad := range []string{filepath.Join(dir, "missing"), dir} {
		_, err = absSandboxDockerfile(bad)
		assert.Assert(t, err != nil, bad)
		assert.Equal(t, userMsg(err), "Could not read "+bad+".")
	}
}

func TestResolveBuildSettingsWithAnOutsideDockerfile(t *testing.T) {
	t.Setenv("DOCKER_HOST", "tcp://127.0.0.1:1")
	t.Setenv("SMELT_DOCKER_IMAGE", "")
	root := t.TempDir()
	assert.NilError(t, os.WriteFile(filepath.Join(root, "go.mod"), []byte("module a\n"), 0o644))
	outside := filepath.Join(t.TempDir(), "Toolchain.Dockerfile")
	assert.NilError(t, os.WriteFile(outside, []byte("FROM x\nCOPY go.mod /tmp/\n"), 0o644))
	cfg := cfgWith(t, `{"factory":{"backend":"docker","image":"cfg:img","dockerfile":"Dockerfile.smelt"}}`)

	// The caller's file wins over factory.image, reads the repo's go.mod as
	// context and gets as far as Docker, which is unreachable here.
	_, err := resolveBuildSettings(context.Background(), discardStreams(), root,
		factorySettings{Attempts: 3, SandboxDockerfile: outside, ReadyTimeout: time.Second}, factoryExplicit{}, cfg)
	assert.ErrorContains(t, err, "did not finish starting")

	// A COPY source the repo lacks is reported before Docker is involved.
	assert.NilError(t, os.Remove(filepath.Join(root, "go.mod")))
	_, err = resolveBuildSettings(context.Background(), discardStreams(), root,
		factorySettings{Attempts: 3, SandboxDockerfile: outside}, factoryExplicit{}, cfg)
	assert.Equal(t, userMsg(err), "Could not read "+outside+".")

	// Nothing was written into the repo.
	entries, err := os.ReadDir(root)
	assert.NilError(t, err)
	assert.Equal(t, len(entries), 0)
}

// fakeEnsurer is an imageEnsurer that records the plan it was asked to build.
type fakeEnsurer struct {
	fakeKits
	plan sandboximage.Plan
}

func (f *fakeEnsurer) Ensure(_ context.Context, p sandboximage.Plan, _ sandboximage.EnsureOptions) (bool, error) {
	f.plan = p
	return true, nil
}

func TestEnsureSandboxImageRecordsWhereTheToolchainCameFrom(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	const ref = sandboximage.KitRegistryRepo + "@sha256:abcd"
	root := t.TempDir()
	assert.NilError(t, os.WriteFile(filepath.Join(root, "go.mod"), []byte("module a\n"), 0o644))
	assert.NilError(t, os.WriteFile(filepath.Join(root, "Dockerfile.smelt"), []byte("FROM x\nCOPY go.mod /tmp/\n"), 0o644))
	outside := filepath.Join(t.TempDir(), "Toolchain.Dockerfile")
	assert.NilError(t, os.WriteFile(outside, []byte("FROM y\nCOPY go.mod /tmp/\n"), 0o644))

	for _, tt := range []struct {
		name string
		b    imageBuild
	}{
		{"caller", imageBuild{Dockerfile: outside, Kind: sandboximage.ToolchainCaller}},
		{"config", imageBuild{Dockerfile: "Dockerfile.smelt", Kind: sandboximage.ToolchainConfig}},
		{"repo", imageBuild{Dockerfile: "Dockerfile.smelt", Kind: sandboximage.ToolchainRepo}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			plan, err := tt.b.toolchainPlan(root)
			assert.NilError(t, err)
			f := &fakeEnsurer{fakeKits: fakeKits{ref: ref}}
			got, err := ensureSandboxImage(t.Context(), discardStreams(), root, tt.b, plan, f)
			assert.NilError(t, err)
			assert.Equal(t, got.Tag, f.plan.Tag)
			assert.Assert(t, got.Tag != plan.Tag, "the sandbox tag is not the toolchain's")
			assert.DeepEqual(t, *got.Sandbox, smeltd.FactorySandbox{
				Dockerfile:   tt.b.Dockerfile,
				Source:       tt.name,
				ToolchainTag: plan.Tag,
				Kit:          sandboximage.KitVersionLabel() + "@sha256:abcd",
			})
		})
	}
}

func TestResolveBuildSettingsKeepsTheBuiltSandbox(t *testing.T) {
	t.Setenv(docker.EnvImage, "")
	root := t.TempDir()
	want := &smeltd.FactorySandbox{Dockerfile: "Dockerfile.smelt", Source: "repo", ToolchainTag: "t:1", Kit: "5@sha256:x"}
	old := buildImage
	buildImage = func(_ context.Context, _ iostream.Streams, _ string, b imageBuild) (builtImage, error) {
		assert.Equal(t, b.Kind, sandboximage.ToolchainRepo)
		return builtImage{Tag: "sandbox:1", Sandbox: want}, nil
	}
	t.Cleanup(func() { buildImage = old })
	assert.NilError(t, os.WriteFile(filepath.Join(root, "Dockerfile.smelt"), []byte("FROM x\n"), 0o644))

	got, err := resolveBuildSettings(t.Context(), discardStreams(), root, factorySettings{Attempts: 3}, factoryExplicit{}, cfgWith(t, `{"factory":{"backend":"docker"}}`))
	assert.NilError(t, err)
	assert.Equal(t, got.Image, "sandbox:1")
	assert.Equal(t, got.Sandbox, want)
}

func TestFactoryBuildCommandPassesOverlayDockerfileToImageBuild(t *testing.T) {
	root := factoryProject(t)
	t.Setenv(docker.EnvImage, "")
	outside := t.TempDir()
	dockerfile := filepath.Join(outside, "Toolchain.Dockerfile")
	assert.NilError(t, os.WriteFile(dockerfile, []byte("FROM x\n"), 0o600))
	overlay := filepath.Join(outside, "run-overlay")
	body, err := json.Marshal(map[string]any{"sandbox": map[string]string{"dockerfile": dockerfile}})
	assert.NilError(t, err)
	assert.NilError(t, os.WriteFile(overlay, body, 0o600))

	// Stop at image building so the command needs neither Docker nor a daemon.
	stop := errors.New("image build reached")
	called := false
	old := buildImage
	t.Cleanup(func() { buildImage = old })
	buildImage = func(_ context.Context, _ iostream.Streams, repo string, b imageBuild) (builtImage, error) {
		called = true
		assert.Equal(t, repo, root)
		assert.Equal(t, b.Dockerfile, dockerfile)
		assert.Equal(t, b.Kind, sandboximage.ToolchainCaller)
		return builtImage{}, stop
	}
	_, _, err = runFactoryBuildCmd(t, "--backend", "docker", "--overlay", overlay, "add a flag")
	assert.ErrorIs(t, err, stop)
	assert.Assert(t, called, "the overlay must select a caller image build")
}

func TestOverlayBuildUsesCallerDockerfile(t *testing.T) {
	t.Setenv(docker.EnvImage, "env:image")
	repo, outside := t.TempDir(), t.TempDir()
	t.Chdir(outside)
	assert.NilError(t, os.WriteFile("Toolchain", []byte("FROM x\nCOPY go.mod /tmp/\n"), 0o600))
	assert.NilError(t, os.WriteFile(filepath.Join(repo, "go.mod"), []byte("module example\n"), 0o600))
	assert.NilError(t, os.WriteFile("overlay", []byte(`{"sandbox":{"dockerfile":"Toolchain"}}`), 0o600))
	selected, err := buildSandboxDockerfile("", false, "overlay")
	assert.NilError(t, err)
	old := buildImage
	t.Cleanup(func() { buildImage = old })
	buildImage = func(ctx context.Context, streams iostream.Streams, root string, b imageBuild) (builtImage, error) {
		assert.Equal(t, b.Kind, sandboximage.ToolchainCaller)
		assert.Equal(t, b.Dockerfile, filepath.Join(outside, "Toolchain"))
		plan, err := b.toolchainPlan(root)
		assert.NilError(t, err)
		assert.DeepEqual(t, plan.Context.Sources, []string{"go.mod"})
		return ensureSandboxImage(ctx, streams, root, b, plan, &fakeEnsurer{fakeKits: fakeKits{ref: sandboximage.KitRegistryRepo + "@sha256:abcd"}})
	}
	got, err := resolveBuildSettings(t.Context(), discardStreams(), repo,
		factorySettings{SandboxDockerfile: selected, Attempts: 3}, factoryExplicit{},
		cfgWith(t, `{"factory":{"backend":"docker","dockerfile":"missing","image":"config:image"}}`))
	assert.NilError(t, err)
	assert.Equal(t, got.Sandbox.Source, smeltd.SandboxFromCaller)
	assert.Equal(t, got.Sandbox.Dockerfile, filepath.Join(outside, "Toolchain"))
	entries, err := os.ReadDir(repo)
	assert.NilError(t, err)
	assert.Equal(t, len(entries), 1)
	assert.Equal(t, entries[0].Name(), "go.mod")
}
