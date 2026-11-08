package validate

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"gotest.tools/v3/assert"
)

func writeTree(t *testing.T, files map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	for name, body := range files {
		p := filepath.Join(dir, name)
		assert.NilError(t, os.MkdirAll(filepath.Dir(p), 0o755))
		assert.NilError(t, os.WriteFile(p, []byte(body), 0o644))
	}
	return dir
}

func TestDetectPython(t *testing.T) {
	devGroup := "[dependency-groups]\ndev = [\"pytest>=8\"]\n"
	optional := "[project]\nname = \"x\"\n[project.optional-dependencies]\ndev = [\"pytest\"]\n"
	poetry := "[tool.poetry.group.dev.dependencies]\npytest = \"^8\"\n"
	cases := []struct {
		name  string
		files map[string]string
		gate  bool
	}{
		{"no tests, no pytest", map[string]string{"pyproject.toml": "[project]\nname = \"x\"\n", "app.py": ""}, false},
		{"tests, pytest not declared", map[string]string{"pyproject.toml": "[project]\nname = \"x\"\n", "tests/test_x.py": ""}, false},
		{"pytest declared, no tests", map[string]string{"pyproject.toml": devGroup, "app.py": ""}, false},
		{"dependency group", map[string]string{"pyproject.toml": devGroup, "tests/test_x.py": ""}, true},
		{"optional dependencies", map[string]string{"pyproject.toml": optional, "tests/test_x.py": ""}, true},
		{"poetry group", map[string]string{"pyproject.toml": poetry, "tests/test_x.py": ""}, true},
		{"requirements-dev.txt and _test.py", map[string]string{"requirements-dev.txt": "pytest==8.0\n", "pkg/x_test.py": ""}, true},
		{"similar name is not pytest", map[string]string{"requirements.txt": "pytest-cov\n", "test_x.py": ""}, false},
		{"hidden dirs ignored", map[string]string{"requirements.txt": "pytest\n", ".venv/test_x.py": ""}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cmds, note := detectPython(writeTree(t, tc.files))
			if tc.gate {
				assert.Equal(t, len(cmds), 1)
				assert.Equal(t, cmds[0].Name, "test")
				assert.Equal(t, cmds[0].Run, "pytest")
				assert.Equal(t, note, "")
				return
			}
			assert.Equal(t, len(cmds), 0)
			assert.Equal(t, note, "no test gate: no tests found (add tests, then run smelt init again)")
		})
	}
}

func TestDetectPythonUsesTrackedFiles(t *testing.T) {
	dir := writeTree(t, map[string]string{"requirements.txt": "pytest\n", "tests/test_x.py": ""})
	for _, args := range [][]string{{"init"}, {"add", "requirements.txt"}} {
		c := exec.Command("git", args...)
		c.Dir = dir
		out, err := c.CombinedOutput()
		assert.NilError(t, err, string(out))
	}
	// The test file exists but is untracked, so a fresh clone would not have it.
	cmds, _ := detectPython(dir)
	assert.Equal(t, len(cmds), 0)

	c := exec.Command("git", "add", "tests/test_x.py")
	c.Dir = dir
	out, err := c.CombinedOutput()
	assert.NilError(t, err, string(out))
	cmds, _ = detectPython(dir)
	assert.Equal(t, len(cmds), 1)
}
