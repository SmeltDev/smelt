package factory

import (
	"os"
	"path/filepath"
	"testing"

	"gotest.tools/v3/assert"
)

func TestReadRunOverlay(t *testing.T) {
	for _, tt := range []struct {
		name, body, want string
		bad              bool
	}{
		{"valid", `{"sandbox":{"dockerfile":"../toolchain"}}`, "../toolchain", false},
		{"unrelated", `{"other":1,"sandbox":{"dockerfile":"/outside/D","other":true}}`, "/outside/D", false},
		{"empty object", "{}", "", false},
		{"empty sandbox", `{"sandbox":{}}`, "", false},
		{"empty path", `{"sandbox":{"dockerfile":""}}`, "", false},
		{"empty file", "", "", true},
		{"malformed", "{", "", true},
		{"trailing", "{} {}", "", true},
		{"array", "[]", "", true},
		{"null", "null", "", true},
		{"sandbox null", `{"sandbox":null}`, "", true},
		{"sandbox type", `{"sandbox":"x"}`, "", true},
		{"path null", `{"sandbox":{"dockerfile":null}}`, "", true},
		{"path type", `{"sandbox":{"dockerfile":42}}`, "", true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "overlay")
			assert.NilError(t, os.WriteFile(path, []byte(tt.body), 0o600))
			got, err := ReadRunOverlay(path)
			if tt.bad {
				assert.Assert(t, err != nil)
				return
			}
			assert.NilError(t, err)
			assert.Equal(t, got.Sandbox.Dockerfile, tt.want)
		})
	}
	_, err := ReadRunOverlay(filepath.Join(t.TempDir(), "missing"))
	assert.ErrorContains(t, err, "read run overlay")
}
