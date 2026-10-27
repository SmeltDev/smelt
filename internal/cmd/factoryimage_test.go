package cmd

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
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
			"smelt-sandbox:local has agent kit none; this smelt expects " + sandboximage.KitVersionLabel() + ": rebuild it (docker build -t smelt-sandbox:local -f Dockerfile.smelt .)"},
		"lower": {sandboximage.ImageInfo{Status: sandboximage.ImageLabeled, Kit: 3, KitLabeled: true},
			nil, `{"factory":{"backend":"docker","image":"smelt-sandbox:local"}}`,
			"smelt-sandbox:local has agent kit 3; this smelt expects " + sandboximage.KitVersionLabel() + ": rebuild it (docker build -t smelt-sandbox:local -f Dockerfile.smelt .)"},
		"equal":   {sandboximage.ImageInfo{Status: sandboximage.ImageLabeled, Kit: sandboximage.KitVersion, KitLabeled: true}, nil, `{"factory":{"backend":"docker","image":"smelt-sandbox:local"}}`, ""},
		"missing": {sandboximage.ImageInfo{Status: sandboximage.ImageMissing}, nil, `{"factory":{"backend":"docker","image":"smelt-sandbox:local"}}`, ""},
		"error":   {sandboximage.ImageInfo{}, errors.New("no daemon"), `{"factory":{"backend":"docker","image":"smelt-sandbox:local"}}`, ""},
		"custom dockerfile hint": {sandboximage.ImageInfo{Status: sandboximage.ImageUnlabeled, Kit: 1},
			nil, `{"factory":{"backend":"docker","image":"x:1","dockerfile":"ci/Dockerfile.dev"}}`,
			"x:1 has agent kit none; this smelt expects " + sandboximage.KitVersionLabel() + ": rebuild it (docker build -t x:1 -f ci/Dockerfile.dev .)"},
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

// A Docker whose backing storage can't be found (here: no Desktop settings in a
// temp home) is not refused: the build goes on to the image work.
func TestImageStorageDiscoveryFailureDoesNotBlockImageWork(t *testing.T) {
	root := t.TempDir()
	assert.NilError(t, os.MkdirAll(filepath.Join(root, ".smelt"), 0o700))
	assert.NilError(t, os.WriteFile(filepath.Join(root, ".smelt", "config.json"), []byte(`{}`), 0o600))
	assert.NilError(t, os.WriteFile(filepath.Join(root, "Dockerfile.smelt"), []byte("FROM alpine:3.20\n"), 0o600))
	imageWork := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.HasSuffix(r.URL.Path, "/_ping"):
			w.Header().Set("API-Version", "1.47")
			_, _ = w.Write([]byte("OK"))
		case strings.HasSuffix(r.URL.Path, "/info"):
			_, _ = w.Write([]byte(`{"ID":"id","ServerVersion":"27.0.0","OperatingSystem":"Docker Desktop","DockerRootDir":"/var/lib/docker"}`))
		case strings.HasSuffix(r.URL.Path, "/images/json"):
			_, _ = w.Write([]byte(`[]`))
		default:
			imageWork++
			http.Error(w, "unexpected image work", 500)
		}
	}))
	defer server.Close()
	t.Setenv("DOCKER_HOST", server.URL)
	t.Setenv("DOCKER_API_VERSION", "1.47")
	_, err := buildSandboxImage(context.Background(), discardStreams(), root, imageBuild{Dockerfile: "Dockerfile.smelt"})
	if err != nil {
		assert.Assert(t, !strings.Contains(err.Error(), "backing-storage discovery failed"), err.Error())
	}
	assert.Assert(t, imageWork > 0)
}

func TestImageBuildReportsExistingCacheSizeAndCap(t *testing.T) {
	root := t.TempDir()
	assert.NilError(t, os.MkdirAll(filepath.Join(root, ".smelt"), 0o700))
	assert.NilError(t, os.WriteFile(filepath.Join(root, ".smelt", "config.json"), []byte(`{"factory":{"minFreeGB":0.000001,"cache":{"maxGB":3}}}`), 0o600))
	assert.NilError(t, os.WriteFile(filepath.Join(root, "Dockerfile.smelt"), []byte("FROM alpine:3.20\n"), 0o600))
	socket := filepath.Join(t.TempDir(), "docker.sock")
	listener, err := net.Listen("unix", socket)
	assert.NilError(t, err)
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.HasSuffix(r.URL.Path, "/_ping"):
			w.Header().Set("API-Version", "1.47")
			_, _ = w.Write([]byte("OK"))
		case strings.HasSuffix(r.URL.Path, "/info"):
			_ = json.NewEncoder(w).Encode(map[string]string{"ID": "image-report-engine", "ServerVersion": "27.0.0", "OperatingSystem": "Linux", "DockerRootDir": root})
		case strings.HasSuffix(r.URL.Path, "/images/json"):
			_, _ = w.Write([]byte(`[]`))
		case strings.HasSuffix(r.URL.Path, "/volumes"):
			_, _ = w.Write([]byte(`{"Volumes":[{"Name":"smelt-cache-abcdef123456","CreatedAt":"today"}]}`))
		case strings.Contains(r.URL.Path, "/volumes/"):
			_, _ = w.Write([]byte(`{"Name":"smelt-cache-abcdef123456","CreatedAt":"today"}`))
		case strings.HasSuffix(r.URL.Path, "/json") && strings.Contains(r.URL.Path, "/images/"):
			_, _ = w.Write([]byte(`{"Id":"sha256:local"}`))
		case strings.HasSuffix(r.URL.Path, "/containers/create"):
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`{"Id":"helper"}`))
		case strings.HasSuffix(r.URL.Path, "/start"), r.Method == http.MethodDelete:
			w.WriteHeader(http.StatusNoContent)
		case strings.HasSuffix(r.URL.Path, "/wait"):
			_, _ = w.Write([]byte(`{"StatusCode":0}`))
		case strings.HasSuffix(r.URL.Path, "/logs"):
			out := "20000000"
			w.Header().Set("Content-Type", "application/vnd.docker.raw-stream")
			_, _ = w.Write(append([]byte{1, 0, 0, 0, 0, 0, 0, byte(len(out))}, []byte(out)...))
		default:
			// Stop subsequent kit/image work; storage output must already exist.
			http.Error(w, `{"message":"stop image work"}`, http.StatusInternalServerError)
		}
	}))
	server.Listener.Close()
	server.Listener = listener
	server.Start()
	defer server.Close()
	t.Setenv("DOCKER_HOST", "unix://"+socket)
	t.Setenv("DOCKER_API_VERSION", "1.47")
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	var output bytes.Buffer
	streams := discardStreams()
	streams.Err = &output
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	_, _ = buildSandboxImage(ctx, streams, root, imageBuild{Dockerfile: "Dockerfile.smelt"})
	assert.Assert(t, strings.Contains(output.String(), "smelt-cache-abcdef123456"), output.String())
	assert.Assert(t, strings.Contains(output.String(), "20.48 GB"), output.String())
	assert.Assert(t, strings.Contains(output.String(), "3.00 GB"), output.String())
}

func TestEnsureSandboxImagePreservesKitProvenance(t *testing.T) {
	root := kitProject(t, "FROM x\n")
	plan, err := sandboximage.NewPlan(root, "Dockerfile.smelt")
	assert.NilError(t, err)
	for range 2 {
		f := &fakeEnsurer{fakeKits: fakeKits{pullErr: errors.New("denied")}}
		got, err := ensureSandboxImage(t.Context(), discardStreams(), root, imageBuild{Dockerfile: "Dockerfile.smelt"}, plan, f)
		assert.NilError(t, err)
		assert.Assert(t, strings.Contains(got.Sandbox.KitNotice, "published kit isn't available (denied)"))
	}
}

func TestFactoryImageCommandDisplaysClaimedKitNotice(t *testing.T) {
	root := kitProject(t, "FROM x\n")
	assert.NilError(t, exec.Command("git", "-C", root, "init", "-q").Run())
	assert.NilError(t, exec.Command("git", "-C", root, "remote", "add", "origin", "https://github.com/smeltdev/smelt.git").Run())
	t.Chdir(root)
	assert.NilError(t, os.MkdirAll(filepath.Join(root, ".smelt"), 0o755))
	assert.NilError(t, os.WriteFile(filepath.Join(root, ".smelt", "config.json"), []byte(`{"factory":{"backend":"docker"}}`), 0o644))
	assert.NilError(t, os.WriteFile(filepath.Join(root, "Dockerfile.smelt"), []byte("FROM x\n"), 0o644))
	kits := &fakeEnsurer{fakeKits: fakeKits{pullErr: errors.New("denied")}}
	streams, output := capturedStreams()
	choice, err := kitImageRef(t.Context(), streams, kits, kitSource{Root: root})
	assert.NilError(t, err)
	assert.Assert(t, choice.Notice != "")
	output.Reset()
	_, err = kitImageRef(t.Context(), streams, kits, kitSource{Root: root})
	assert.NilError(t, err)
	assert.Equal(t, output.String(), "", "automatic notice was already claimed")

	stop := errors.New("stop before verification")
	previous := buildImage
	t.Cleanup(func() { buildImage = previous })
	called := false
	buildImage = func(ctx context.Context, streams iostream.Streams, root string, b imageBuild) (builtImage, error) {
		called = true
		plan, err := b.toolchainPlan(root)
		if err != nil {
			return builtImage{}, err
		}
		built, err := ensureSandboxImage(ctx, streams, root, b, plan, kits)
		if err != nil {
			return builtImage{}, err
		}
		return built, stop
	}
	cmd := newFactoryImageCmd()
	cmd.SetContext(t.Context())
	cmd.SetOut(streams.Out)
	cmd.SetErr(streams.Err)
	cmd.SetArgs(nil)
	err = cmd.Execute()
	assert.ErrorIs(t, err, stop)
	assert.Assert(t, called)
	assert.Assert(t, strings.Contains(output.String(), choice.Notice), output.String())
}

// localKitEnsurer offers an existing local kit as well as source builds.
type localKitEnsurer struct{ fakeEnsurer }

func (*localKitEnsurer) LocalKit(context.Context, string) (string, error) {
	return "sha256:cached", nil
}

func TestFactoryImageCommandForcesKitRebuildWithLocalKit(t *testing.T) {
	root := kitProject(t, "FROM x\n")
	assert.NilError(t, exec.Command("git", "-C", root, "init", "-q").Run())
	assert.NilError(t, exec.Command("git", "-C", root, "remote", "add", "origin", "https://github.com/smeltdev/smelt.git").Run())
	t.Chdir(root)
	assert.NilError(t, os.MkdirAll(filepath.Join(root, ".smelt"), 0o755))
	assert.NilError(t, os.WriteFile(filepath.Join(root, ".smelt", "config.json"), []byte(`{"factory":{"backend":"docker"}}`), 0o644))

	for _, force := range []bool{false, true} {
		name := "reuse local kit"
		if force {
			name = "force source rebuild"
		}
		t.Run(name, func(t *testing.T) {
			kits := &localKitEnsurer{}
			stop := errors.New("stop before verification")
			previous := buildImage
			t.Cleanup(func() { buildImage = previous })
			called := false
			buildImage = func(ctx context.Context, streams iostream.Streams, root string, b imageBuild) (builtImage, error) {
				called = true
				assert.Equal(t, b.KitFromSource, force)
				plan, err := b.toolchainPlan(root)
				assert.NilError(t, err)
				built, err := ensureSandboxImage(ctx, streams, root, b, plan, kits)
				assert.NilError(t, err)
				return built, stop
			}
			streams, _ := capturedStreams()
			cmd := newFactoryImageCmd()
			cmd.SetContext(t.Context())
			cmd.SetOut(streams.Out)
			cmd.SetErr(streams.Err)
			args := []string{}
			if force {
				args = append(args, "--kit-from-source")
			}
			cmd.SetArgs(args)
			assert.ErrorIs(t, cmd.Execute(), stop)
			assert.Assert(t, called)
			if force {
				assert.Equal(t, kits.built, 1)
				assert.Assert(t, strings.Contains(kits.plan.Context.Content, "COPY --from=sha256:built"))
			} else {
				assert.Equal(t, kits.built, 0)
				assert.Assert(t, strings.Contains(kits.plan.Context.Content, "COPY --from=sha256:cached"))
			}
		})
	}
}
