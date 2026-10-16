package factory

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"gotest.tools/v3/assert"
)

func TestDetectLanguages(t *testing.T) {
	dir := t.TempDir()
	run := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		out, err := cmd.CombinedOutput()
		assert.NilError(t, err, string(out))
	}
	run("init", "-q")
	files := map[string]string{
		"app/main.py":         "",
		"app/util.py":         "",
		"web/index.tsx":       "",
		"vendor/lib/dep.go":   "",
		"app/main_test.go":    "",
		"README.md":           "",
		"ignored.rs":          "",
		".gitignore":          "ignored.rs\n",
		"node_modules/x/a.js": "",
	}
	for name, content := range files {
		assert.NilError(t, os.MkdirAll(filepath.Join(dir, filepath.Dir(name)), 0o755))
		assert.NilError(t, os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644))
	}
	run("add", "app/main.py")

	got, err := DetectLanguages(context.Background(), dir)
	assert.NilError(t, err)
	assert.DeepEqual(t, got, []string{"python", "typescript"})
}

func TestDetectLanguagesOutsideARepository(t *testing.T) {
	_, err := DetectLanguages(context.Background(), t.TempDir())
	assert.ErrorContains(t, err, "list the project's files")
}
