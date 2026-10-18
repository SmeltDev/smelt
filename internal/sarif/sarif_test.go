package sarif

import (
	"os"
	"path/filepath"
	"testing"

	"gotest.tools/v3/assert"
)

func read(t *testing.T, name string) []byte {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", name))
	assert.NilError(t, err)
	return data
}

func TestParseGolangciLint(t *testing.T) {
	fs, err := Parse(read(t, "real/golangci-lint.sarif"), "")
	assert.NilError(t, err)
	assert.Equal(t, len(fs), 2)
	assert.Equal(t, fs[0].String(), "main.go:8 errcheck Error return value is not checked")
	assert.Equal(t, fs[1].Line, 9)
}

func TestParseRuffShape(t *testing.T) {
	fs, err := Parse(read(t, "samples/ruff.sarif"), "/work/proj")
	assert.NilError(t, err)
	assert.Equal(t, len(fs), 2)
	assert.Equal(t, fs[0].File, "app/main.py")
	assert.Equal(t, fs[1].File, "app/my mod.py")
	assert.Equal(t, fs[0].RuleID, "F401")
}

func TestParseESLintShape(t *testing.T) {
	fs, err := Parse(read(t, "samples/eslint.sarif"), "")
	assert.NilError(t, err)
	// The suppressed result is left out; the rule id comes from rule.id.
	assert.Equal(t, len(fs), 1)
	assert.Equal(t, fs[0].RuleID, "no-unused-vars")
	assert.Equal(t, fs[0].Snippet, "const x = 1;")
}

func TestParseFingerprints(t *testing.T) {
	fs, err := Parse([]byte(`{"version":"2.1.0","runs":[{"results":[{"ruleId":"r","message":{"text":"m"},
		"partialFingerprints":{"h":"1"},"fingerprints":{"f":"2"}}]}]}`), "")
	assert.NilError(t, err)
	assert.DeepEqual(t, fs[0].Fingerprints, []string{"f=2", "h=1"})
}

func TestParseErrors(t *testing.T) {
	for name, tc := range map[string]struct{ in, want string }{
		"empty":       {"  \n", "empty output"},
		"not json":    {"error: no such flag", "not valid JSON"},
		"no runs":     {`{"version":"2.1.0"}`, "no \"runs\""},
		"bad version": {`{"version":"1.0.0","runs":[]}`, "unsupported SARIF version"},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := Parse([]byte(tc.in), "")
			assert.ErrorContains(t, err, tc.want)
		})
	}
	fs, err := Parse([]byte(`{"version":"2.1.0","runs":[]}`), "")
	assert.NilError(t, err)
	assert.Equal(t, len(fs), 0)
}
