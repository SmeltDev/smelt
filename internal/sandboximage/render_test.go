package sandboximage

import (
	"os"
	"regexp"
	"strings"
	"testing"

	"gotest.tools/v3/assert"
	cmp "gotest.tools/v3/assert/cmp"
	"gotest.tools/v3/golden"

	"github.com/smeltdev/smelt/internal/envspec"
)

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

func TestKitInstallsBothAgentsAndLabelsTheImage(t *testing.T) {
	got := Render(NewRecipe(&envspec.Environment{Stack: "unknown"}, nil, nil))
	assert.Check(t, cmp.Contains(got, KitCodex()))
	assert.Check(t, cmp.Contains(got, "LABEL "+LabelHarnesses+"=claude,codex,grok"))
	assert.Check(t, cmp.Contains(got, "claude.ai/install.sh"))
}

// readDockerfileSmelt reads the committed factory sandbox recipe, which must
// carry the kit blocks verbatim.
func readDockerfileSmelt(t *testing.T) string {
	t.Helper()
	raw, err := os.ReadFile("../../Dockerfile.smelt")
	assert.NilError(t, err)
	return string(raw)
}

func TestDockerfileSmeltCarriesTheKitsCodexBlock(t *testing.T) {
	local := readDockerfileSmelt(t)
	assert.Check(t, cmp.Contains(local, KitCodex()))
	assert.Check(t, cmp.Contains(local, "LABEL "+LabelHarnesses+"=claude,codex,grok"))
}

func TestKitInstallsAPinnedGrokForBothArchitectures(t *testing.T) {
	kit := KitGrok()
	assert.Check(t, cmp.Contains(kit, "https://x.ai/cli/grok-"+GrokVersion+"-linux-${arch}"))
	assert.Check(t, cmp.Contains(kit, "x86_64) arch=x86_64; sum=2a46d9e4d7ebd7a9cff7f95716714b19dad5f21dd2a497822400b1fb128969aa"))
	assert.Check(t, cmp.Contains(kit, "aarch64|arm64) arch=aarch64; sum=0b808a2209728788d2e37c34ea1846d460b41528a9fc7edc7698b9e7dec12fe1"))
	assert.Check(t, cmp.Contains(kit, "sha256sum -c -"))
	assert.Check(t, cmp.Contains(kit, `test -x "$HOME/.local/bin/grok"`))
	assert.Check(t, cmp.Contains(Render(NewRecipe(&envspec.Environment{Stack: "unknown"}, nil, nil)), kit))
}

func TestDockerfileSmeltCarriesTheKitsGrokBlock(t *testing.T) {
	assert.Check(t, cmp.Contains(readDockerfileSmelt(t), KitGrok()))
}

func TestDockerfileSmeltCarriesTheKitsLizardBlock(t *testing.T) {
	local := readDockerfileSmelt(t)
	assert.Check(t, cmp.Contains(local, KitLizard()))
	assert.Check(t, cmp.Contains(local, "LABEL "+LabelHarnesses+"=claude,codex,grok "+LabelKit+"="+kitVersionString()))
	assert.Check(t, cmp.Contains(local, "python3-venv"))
}

// lizard is installed from hash-pinned wheels into a venv of its own, and an
// image without python3 skips the step.
func TestKitInstallsAPinnedLizard(t *testing.T) {
	got := Render(NewRecipe(&envspec.Environment{Stack: "unknown"}, nil, nil))
	assert.Check(t, cmp.Contains(got, KitLizard()))
	assert.Check(t, cmp.Contains(got, "python3 python3-venv"))
	assert.Check(t, cmp.Contains(got, "lizard=="+LizardVersion+" --hash=sha256:"))
	assert.Check(t, cmp.Contains(got, "pip\" install --no-cache-dir --require-hashes"))
	assert.Check(t, cmp.Contains(got, `python3 -m venv "$HOME/.local/share/smelt-tools"`))
	assert.Check(t, cmp.Contains(got, `ln -sf "$HOME/.local/share/smelt-tools/bin/lizard" "$HOME/.local/bin/lizard"`))
	assert.Check(t, cmp.Contains(got, "python3 is missing"))
	sha := regexp.MustCompile(`--hash=sha256:[0-9a-f]{64}'`)
	reqs := lizardRequirements()
	assert.Check(t, cmp.Len(sha.FindAllString(KitLizard(), -1), len(reqs)))
	for _, r := range reqs {
		assert.Check(t, sha.MatchString(r+"'"), r)
		assert.Check(t, cmp.Contains(r, "=="), "every requirement is pinned: %s", r)
	}
}

func TestKitInstallsTheCodexCommandHost(t *testing.T) {
	got := Render(NewRecipe(&envspec.Environment{Stack: "unknown"}, nil, nil))
	assert.Check(t, cmp.Contains(got, "(v5)"))
	assert.Check(t, cmp.Contains(got, `name="codex-code-mode-host-${arch}-unknown-linux-musl"`))
	assert.Check(t, cmp.Contains(got, `test -x "$HOME/.local/bin/codex-code-mode-host"`))
	assert.Check(t, cmp.Equal(strings.Count(got, "sha256sum -c -"), 3))
}

// The version lives in CodexVersion alone, and each asset has a full SHA-256
// for both architectures.
func TestCodexAssetsArePinnedForBothArchitectures(t *testing.T) {
	sha := regexp.MustCompile(`^[0-9a-f]{64}$`)
	seen := map[string]bool{}
	for _, a := range []codexAsset{codexMain, codexHost} {
		for _, sum := range []string{a.sumX86, a.sumARM} {
			assert.Check(t, sha.MatchString(sum), "%s: %q", a.prefix, sum)
			assert.Check(t, !seen[sum], "duplicate checksum %s", sum)
			seen[sum] = true
			assert.Check(t, cmp.Contains(a.install(), sum))
		}
		assert.Check(t, cmp.Contains(a.install(), codexReleaseURLBase+CodexVersion+"/"))
	}
	assert.Check(t, cmp.Len(seen, 4))
}
