package sandboximage

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"gotest.tools/v3/assert"
	cmp "gotest.tools/v3/assert/cmp"
	"gotest.tools/v3/golden"

	"github.com/smeltdev/smelt/internal/config"
	"github.com/smeltdev/smelt/internal/envspec"
	"github.com/smeltdev/smelt/internal/iostream"
)

// TestRepositorySandboxLive exercises the actual repository toolchain and gates.
// Run with -timeout 90m: a cold build downloads Go modules and the agent kit.
func TestRepositorySandboxLive(t *testing.T) {
	if testing.Short() || os.Getenv("SMELT_REPOSITORY_SANDBOX_NESTED") == "1" {
		t.Skip("repository sandbox gates: short mode or already inside this test's sandbox")
	}
	b, err := Connect()
	if err != nil {
		t.Skipf("no docker: %v", err)
	}
	defer func() { _ = b.Close() }()
	ctx, cancel := context.WithTimeout(context.Background(), 80*time.Minute)
	defer cancel()
	if _, err := b.Exists(ctx, "smelt-live-probe:none"); err != nil {
		t.Skipf("docker daemon unreachable: %v", err)
	}
	docker, err := exec.LookPath("docker")
	assert.NilError(t, err)
	root, err := filepath.Abs(filepath.Join("..", ".."))
	assert.NilError(t, err)
	cfg, err := config.LoadProjectConfig(root)
	assert.NilError(t, err)
	assert.Assert(t, cfg != nil)
	assert.Equal(t, cfg.Factory.Image, "")
	toolchain, err := NewPlan(root, cfg.Factory.Dockerfile)
	assert.NilError(t, err)
	kit, err := b.BuildKit(ctx, filepath.Join(root, "kit"), BuildOptions{Tag: KitLocalRef()})
	assert.NilError(t, err)
	plan := toolchain.SandboxPlan(kit)
	t.Logf("toolchain=%s kit=%s sandbox=%s", toolchain.Tag, kit, plan.Tag)
	_, err = b.Ensure(ctx, plan, EnsureOptions{
		LogPath:  filepath.Join(t.TempDir(), "repository-sandbox-build.log"),
		Progress: func(_ iostream.Level, msg string) { t.Log(msg) },
	})
	assert.NilError(t, err)
	// Copy inside the container so gates can write reports and caches without
	// changing the checkout. No daemon is forwarded to nested Docker tests.
	out, err := exec.CommandContext(ctx, docker, "run", "--rm",
		"--env", "SMELT_REPOSITORY_SANDBOX_NESTED=1",
		"--mount", "type=bind,src="+root+",dst=/source,readonly",
		plan.Tag, "bash", "-euc",
		"mkdir /tmp/smelt-gates; cp -a /source/. /tmp/smelt-gates/; cd /tmp/smelt-gates; task test; task lint").CombinedOutput()
	t.Logf("repository gates:\n%s", out)
	assert.NilError(t, err, string(out))
}

func TestRenderGolden(t *testing.T) {
	env := func(stack, image, version string, setup ...envspec.Step) *envspec.Environment {
		return &envspec.Environment{Stack: stack, Image: image, ImageVersion: version, Setup: setup}
	}
	tests := []struct {
		name    string
		env     *envspec.Environment
		tracked []string
		user    string
		base    string // overrides the base, as a CI image would
	}{
		{"go", env("go", "cimg/go", "1.24"), []string{"go.mod", "go.sum"}, "circleci", ""},
		{"pnpm", env("typescript", "cimg/node", "22.12", envspec.Step{Name: "system", Command: "sudo npm install -g pnpm"}),
			[]string{"package.json", "pnpm-lock.yaml", "pnpm-workspace.yaml", "packages/a/package.json"}, "circleci", "cimg/node:24.21.0"},
		{"pnpm-detected", env("typescript", "cimg/node", "22.12", envspec.Step{Name: "system", Command: "sudo npm install -g pnpm"}),
			[]string{"package.json", "pnpm-lock.yaml"}, "circleci", ""},
		{"npm", env("javascript", "cimg/node", "22.12"), []string{"package.json", "package-lock.json"}, "circleci", ""},
		{"uv", env("python", "python", "3.12"), []string{"pyproject.toml", "uv.lock"}, "", ""},
		{"pip", env("python", "cimg/python", "3.12"), []string{"requirements.txt"}, "circleci", ""},
		{"rust", env("rust", "cimg/rust", "1.80"), []string{"Cargo.toml", "Cargo.lock", "crates/a/Cargo.toml"}, "circleci", ""},
		{"unknown", env("unknown", "", ""), nil, "", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := NewRecipe(tt.env, nil, tt.tracked)
			if tt.base != "" {
				r.Base, r.BaseFrom = tt.base, BaseFromCI
			}
			r.User = tt.user
			got := Render(r)

			golden.Assert(t, got, tt.name+".Dockerfile")

			// What is rendered must be readable by the build context parser.
			_, err := CopySources(got)
			assert.NilError(t, err)
		})
	}
}

// Render writes the toolchain only: smelt adds the kit when it builds the image.
func TestRenderHasNoKitSection(t *testing.T) {
	got := Render(NewRecipe(&envspec.Environment{Stack: "unknown"}, nil, nil))
	assert.Check(t, !strings.Contains(got, "smelt agent kit"))
	assert.Check(t, !strings.Contains(got, "LABEL "))
	assert.Check(t, !strings.Contains(got, "claude.ai/install.sh"))
	assert.Check(t, cmp.Equal(ToolchainDockerfile(got), got))
}

func TestGoldensHaveNoKitSection(t *testing.T) {
	files, err := filepath.Glob("testdata/*.Dockerfile")
	assert.NilError(t, err)
	assert.Check(t, len(files) > 0)
	for _, f := range files {
		raw, err := os.ReadFile(f)
		assert.NilError(t, err)
		assert.Check(t, !strings.Contains(string(raw), kitMarkerPrefix), f)
	}
}

// TestDockerfileSmeltIsToolchainOnly keeps smelt's own factory Dockerfile free
// of the kit, which smelt adds to every sandbox.
func TestDockerfileSmeltIsToolchainOnly(t *testing.T) {
	raw, err := os.ReadFile("../../Dockerfile.smelt")
	assert.NilError(t, err)
	_, found := KitVersionOf(string(raw))
	assert.Check(t, !found)
	assert.Check(t, cmp.Contains(string(raw), "procps"), "pkill comes from the toolchain")
	assert.Check(t, cmp.Equal(ToolchainDockerfile(string(raw)), string(raw)))
	for _, tool := range []string{"claude.ai/install.sh", KitDir, "lizard"} {
		assert.Check(t, !strings.Contains(string(raw), tool), "kit tool %s belongs in the kit", tool)
	}

	root := filepath.Join("..", "..")
	cfg, err := config.LoadProjectConfig(root)
	assert.NilError(t, err)
	assert.Assert(t, cfg != nil)
	assert.Check(t, cmp.Equal(cfg.Factory.Backend, config.BackendDocker))
	assert.Check(t, cmp.Equal(cfg.Factory.Dockerfile, "Dockerfile.smelt"))
	assert.Check(t, cmp.Equal(cfg.Factory.Image, ""), "an image override bypasses the managed toolchain")

	mod, err := os.ReadFile(filepath.Join(root, "go.mod"))
	assert.NilError(t, err)
	version := regexp.MustCompile(`(?m)^go (\S+)$`).FindStringSubmatch(string(mod))
	assert.Assert(t, cmp.Len(version, 2))
	assert.Check(t, cmp.Contains(string(raw), "FROM cimg/go:"+version[1]+"\n"))

	plan, err := NewPlan(root, cfg.Factory.Dockerfile)
	assert.NilError(t, err)
	assert.Check(t, cmp.Equal(plan.Context.Content, string(raw)))
	const kitA, kitB = KitRegistryRepo + "@sha256:aaaa", KitRegistryRepo + "@sha256:bbbb"
	a, b := plan.SandboxPlan(kitA), plan.SandboxPlan(kitB)
	assert.Check(t, cmp.Equal(a.Tag, SandboxTag(plan.Tag, kitA)))
	assert.Check(t, cmp.Equal(b.Tag, SandboxTag(plan.Tag, kitB)))
	assert.Check(t, a.Tag != b.Tag, "a kit change rebuilds the sandbox")
	assert.Check(t, cmp.Equal(a.Toolchain.Tag, plan.Tag))
	assert.Check(t, cmp.Equal(a.Toolchain.Tag, b.Toolchain.Tag), "a kit change reuses the toolchain")
}

// Each codex asset has a full SHA-256 for both architectures.
func TestCodexAssetsArePinnedForBothArchitectures(t *testing.T) {
	sha := regexp.MustCompile(`^[0-9a-f]{64}$`)
	seen := map[string]bool{}
	for _, a := range []codexAsset{codexMain, codexHost} {
		for _, sum := range []string{a.sumX86, a.sumARM} {
			assert.Check(t, sha.MatchString(sum), "%q", sum)
			assert.Check(t, !seen[sum], "duplicate checksum %s", sum)
			seen[sum] = true
		}
	}
	assert.Check(t, cmp.Len(seen, 4))
}

func TestLizardWheelsArePinnedByHash(t *testing.T) {
	sha := regexp.MustCompile(`^[0-9a-f]{64}$`)
	for _, w := range lizardWheels {
		assert.Check(t, sha.MatchString(w.sha256), w.name)
	}
}
