package quality

import (
	"archive/tar"
	"bytes"
	"testing"

	"gotest.tools/v3/assert"
)

func tarOf(t *testing.T, files map[string]string) []byte {
	t.Helper()
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	for _, name := range []string{"b.py", "a.go", "c.ts"} {
		content, ok := files[name]
		if !ok {
			continue
		}
		assert.NilError(t, tw.WriteHeader(&tar.Header{Name: "./" + name, Typeflag: tar.TypeReg, Size: int64(len(content)), Mode: 0o644}))
		_, err := tw.Write([]byte(content))
		assert.NilError(t, err)
	}
	assert.NilError(t, tw.WriteHeader(&tar.Header{Name: "dir/", Typeflag: tar.TypeDir}))
	assert.NilError(t, tw.Close())
	return buf.Bytes()
}

func TestReadTarSortsAndCleansPaths(t *testing.T) {
	files, skipped, err := ReadTar(tarOf(t, map[string]string{"b.py": "b", "a.go": "a"}), 100)
	assert.NilError(t, err)
	assert.Equal(t, len(skipped), 0)
	assert.Equal(t, len(files), 2)
	assert.Equal(t, files[0].Path, "a.go")
	assert.Equal(t, string(files[1].Content), "b")
}

func TestReadTarSkipsFilesPastTheLimit(t *testing.T) {
	files, skipped, err := ReadTar(tarOf(t, map[string]string{"b.py": "123456", "a.go": "123456", "c.ts": "1"}), 8)
	assert.NilError(t, err)
	assert.Equal(t, len(files), 2)
	assert.DeepEqual(t, skipped, []string{"a.go"})
}

func TestReadTarRejectsGarbage(t *testing.T) {
	_, _, err := ReadTar(bytes.Repeat([]byte("x"), 600), 100)
	assert.ErrorContains(t, err, "source archive")
}
