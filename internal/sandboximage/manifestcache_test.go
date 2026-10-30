package sandboximage

import (
	"archive/tar"
	"bytes"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/docker/docker/client"
	"gotest.tools/v3/assert"
)

// countingDocker is a Docker API that lists the given image IDs, serves one
// kit manifest from any container and counts the containers created.
type countingDocker struct {
	creates atomic.Int32
	images  []string
	labels  map[string]string // labels of the inspected image
}

const testManifestJSON = `{"kit":6,"tools":[{"name":"claude","version":"2.1.310","sha256":"pinned"}]}`

func (c *countingDocker) builder(t *testing.T) *Builder {
	t.Helper()
	var archive bytes.Buffer
	tw := tar.NewWriter(&archive)
	assert.NilError(t, tw.WriteHeader(&tar.Header{Name: "manifest.json", Mode: 0o644, Size: int64(len(testManifestJSON))}))
	_, err := tw.Write([]byte(testManifestJSON))
	assert.NilError(t, err)
	assert.NilError(t, tw.Close())
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.HasSuffix(r.URL.Path, "/containers/create"):
			c.creates.Add(1)
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`{"Id":"c1"}`))
		case strings.HasSuffix(r.URL.Path, "/containers/c1/archive"):
			w.Header().Set("Content-Type", "application/x-tar")
			w.Header().Set("X-Docker-Container-Path-Stat", base64.StdEncoding.EncodeToString([]byte(`{"name":"manifest.json"}`)))
			_, _ = w.Write(archive.Bytes())
		case r.Method == http.MethodDelete:
			w.WriteHeader(http.StatusNoContent)
		case strings.HasSuffix(r.URL.Path, "/images/json"):
			list := make([]map[string]any, len(c.images))
			for i, id := range c.images {
				list[i] = map[string]any{"Id": id}
			}
			_ = json.NewEncoder(w).Encode(list)
		case strings.HasSuffix(r.URL.Path, "/json") && strings.Contains(r.URL.Path, "/images/"):
			_ = json.NewEncoder(w).Encode(map[string]any{"Id": "sha256:a", "Config": map[string]any{"Labels": c.labels}})
		default:
			t.Errorf("unexpected Docker call: %s %s", r.Method, r.URL)
			http.Error(w, "unexpected", http.StatusBadRequest)
		}
	}))
	t.Cleanup(server.Close)
	cli, err := client.NewClientWithOpts(client.WithHost(server.URL), client.WithVersion("1.47"))
	assert.NilError(t, err)
	t.Cleanup(func() { _ = cli.Close() })
	return NewBuilder(cli)
}

func cachedIDs(t *testing.T, path string) []string {
	t.Helper()
	raw, err := os.ReadFile(path)
	assert.NilError(t, err)
	var m map[string]KitManifest
	assert.NilError(t, json.Unmarshal(raw, &m))
	ids := make([]string, 0, len(m))
	for id := range m {
		ids = append(ids, id)
	}
	return ids
}

func TestCachedKitManifestReadsOncePerImageID(t *testing.T) {
	d := &countingDocker{images: []string{"sha256:a", "sha256:b"}}
	b := d.builder(t)
	path := filepath.Join(t.TempDir(), "state", "kit-manifests.json")

	first, err := b.CachedKitManifest(t.Context(), path, "sha256:a")
	assert.NilError(t, err)
	second, err := b.CachedKitManifest(t.Context(), path, "sha256:a")
	assert.NilError(t, err)
	assert.DeepEqual(t, first, second)
	assert.Equal(t, first.Kit, 6)
	assert.Equal(t, d.creates.Load(), int32(1))

	_, err = b.CachedKitManifest(t.Context(), path, "sha256:b")
	assert.NilError(t, err)
	assert.Equal(t, d.creates.Load(), int32(2))
}

func TestCachedKitManifestPrunesVanishedImages(t *testing.T) {
	d := &countingDocker{images: []string{"sha256:a"}}
	b := d.builder(t)
	path := filepath.Join(t.TempDir(), "kit-manifests.json")
	writeManifestCache(path, map[string]KitManifest{"sha256:a": {Kit: 6}, "sha256:gone": {Kit: 5}})

	got, err := b.CachedKitManifest(t.Context(), path, "sha256:a")
	assert.NilError(t, err)
	assert.Equal(t, got.Kit, 6)
	assert.DeepEqual(t, cachedIDs(t, path), []string{"sha256:a"})
	assert.Equal(t, d.creates.Load(), int32(0))
}

func TestCachedKitManifestSurvivesACorruptFile(t *testing.T) {
	d := &countingDocker{images: []string{"sha256:a"}}
	b := d.builder(t)
	path := filepath.Join(t.TempDir(), "kit-manifests.json")
	assert.NilError(t, os.WriteFile(path, []byte("{not json"), 0o600))

	got, err := b.CachedKitManifest(t.Context(), path, "sha256:a")
	assert.NilError(t, err)
	assert.Equal(t, got.Kit, 6)
	assert.Equal(t, d.creates.Load(), int32(1))
	assert.DeepEqual(t, cachedIDs(t, path), []string{"sha256:a"})
}

func TestCachedKitManifestSkipsTheCacheWithoutAnID(t *testing.T) {
	d := &countingDocker{}
	b := d.builder(t)
	path := filepath.Join(t.TempDir(), "kit-manifests.json")
	for range 2 {
		_, err := b.CachedKitManifest(t.Context(), path, "")
		assert.NilError(t, err)
	}
	assert.Equal(t, d.creates.Load(), int32(2))
	_, err := os.Stat(path)
	assert.Assert(t, os.IsNotExist(err))
}

func TestInspectImageReadsTheManifestLabel(t *testing.T) {
	d := &countingDocker{labels: map[string]string{LabelHarnesses: "claude", LabelKitManifest: testManifestJSON}}
	info, err := d.builder(t).InspectImage(t.Context(), "tag")
	assert.NilError(t, err)
	assert.Assert(t, info.Manifest != nil)
	assert.DeepEqual(t, *info.Manifest, KitManifest{Kit: 6, Tools: []KitTool{{Name: "claude", Version: "2.1.310", SHA256: "pinned"}}})
	assert.Equal(t, d.creates.Load(), int32(0))
}

func TestInspectImageIgnoresAMalformedManifestLabel(t *testing.T) {
	d := &countingDocker{labels: map[string]string{LabelHarnesses: "claude", LabelKitManifest: "{nope"}}
	info, err := d.builder(t).InspectImage(t.Context(), "tag")
	assert.NilError(t, err)
	assert.Assert(t, info.Manifest == nil)
	assert.Equal(t, info.Status, ImageLabeled)
}

func TestSandboxDockerfileLabelsTheManifestOnlyWhenKnown(t *testing.T) {
	plain := sandboxDockerfile("tool:1", KitChoice{Ref: "kit@sha256:aa", Kit: 6})
	assert.Assert(t, !strings.Contains(plain, LabelKitManifest))

	m := KitManifest{Kit: 6, Tools: []KitTool{{Name: "claude", Version: "2.\"1\"", SHA256: "x"}}}
	labeled := sandboxDockerfile("tool:1", KitChoice{Ref: "kit@sha256:aa", Kit: 6, Manifest: &m})
	assert.Assert(t, strings.HasPrefix(labeled, plain))
	line := strings.TrimPrefix(labeled, plain)
	assert.Assert(t, strings.HasPrefix(line, "LABEL "+LabelKitManifest+"="), line)
	// The label value round-trips through Docker's quoting back to the manifest.
	value, err := strconv.Unquote(strings.TrimSpace(strings.TrimPrefix(line, "LABEL "+LabelKitManifest+"=")))
	assert.NilError(t, err)
	got := manifestLabel(value)
	assert.Assert(t, got != nil)
	assert.DeepEqual(t, *got, m)
}
