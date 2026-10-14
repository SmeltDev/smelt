// Package checkscripts tests the repository guard scripts against throwaway git
// repositories holding a copy of the real script.
package checkscripts

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"gotest.tools/v3/assert"
)

func TestGuards(t *testing.T) {
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash not available")
	}
	// The old name is spelled in parts so these guards do not flag this file.
	oldName := "chu" + "nk"
	slug := "CircleCI-Public/" + oldName + "-cli"
	tests := []struct {
		name, script, path, content string
		wantOK                      bool
	}{
		{"names allows UPSTREAM.md", "check-names.sh", "UPSTREAM.md", "fork of " + slug + "\n", true},
		{"names allows the upstream script", "check-names.sh", "scripts/upstream-check.sh", oldName + "-cli\n", true},
		{"names rejects other files", "check-names.sh", "notes.txt", "the " + oldName + " tool\n", false},
		{"circleci allows UPSTREAM.md", "check-circleci.sh", "UPSTREAM.md", slug + "\n", true},
		{"circleci allows the upstream script", "check-circleci.sh", "scripts/upstream-check.sh", slug + "\n", true},
		{"circleci rejects other files", "check-circleci.sh", "notes.txt", slug + "\n", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			src, err := os.ReadFile(filepath.Join("..", "..", "scripts", tt.script))
			assert.NilError(t, err)
			write := func(rel string, b []byte) {
				p := filepath.Join(dir, rel)
				assert.NilError(t, os.MkdirAll(filepath.Dir(p), 0o755))
				assert.NilError(t, os.WriteFile(p, b, 0o755))
			}
			write("scripts/"+tt.script, src)
			write(tt.path, []byte(tt.content))
			for _, args := range [][]string{{"init", "-q"}, {"add", "-A"}} {
				cmd := exec.Command("git", args...)
				cmd.Dir = dir
				out, err := cmd.CombinedOutput()
				assert.NilError(t, err, string(out))
			}
			cmd := exec.Command("bash", "scripts/"+tt.script)
			cmd.Dir = dir
			out, err := cmd.CombinedOutput()
			if tt.wantOK {
				assert.NilError(t, err, string(out))
			} else {
				assert.Assert(t, err != nil, string(out))
			}
		})
	}
}
