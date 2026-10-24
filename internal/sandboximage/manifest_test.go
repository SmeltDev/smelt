package sandboximage

import (
	"archive/tar"
	"bytes"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/client"
	"gotest.tools/v3/assert"
)

func TestReadKitManifestWithoutStartingContainer(t *testing.T) {
	for _, malformed := range []bool{false, true} {
		t.Run(map[bool]string{false: "valid", true: "malformed"}[malformed], func(t *testing.T) {
			manifest := `{"kit":6,"tools":[{"name":"claude","version":"2.1.310","sha256":"pinned"}]}`
			if malformed {
				manifest = "invalid json"
			}
			var archive bytes.Buffer
			tw := tar.NewWriter(&archive)
			assert.NilError(t, tw.WriteHeader(&tar.Header{Name: "manifest.json", Mode: 0o644, Size: int64(len(manifest))}))
			_, err := tw.Write([]byte(manifest))
			assert.NilError(t, err)
			assert.NilError(t, tw.Close())
			var removed, copied atomic.Bool
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch {
				case strings.HasSuffix(r.URL.Path, "/containers/create"):
					var cfg container.Config
					if err := json.NewDecoder(r.Body).Decode(&cfg); err != nil {
						t.Error(err)
					}
					assert.Equal(t, cfg.Image, "kit@sha256:123")
					assert.Assert(t, cfg.NetworkDisabled)
					w.Header().Set("Content-Type", "application/json")
					w.WriteHeader(http.StatusCreated)
					_, _ = w.Write([]byte(`{"Id":"manifest-container"}`))
				case strings.HasSuffix(r.URL.Path, "/containers/manifest-container/archive"):
					assert.Equal(t, r.URL.Query().Get("path"), KitDir+"/manifest.json")
					copied.Store(true)
					w.Header().Set("Content-Type", "application/x-tar")
					w.Header().Set("X-Docker-Container-Path-Stat", base64.StdEncoding.EncodeToString([]byte(`{"name":"manifest.json"}`)))
					_, _ = w.Write(archive.Bytes())
				case r.Method == http.MethodDelete && strings.HasSuffix(r.URL.Path, "/containers/manifest-container"):
					removed.Store(true)
					w.WriteHeader(http.StatusNoContent)
				default:
					t.Errorf("unexpected Docker call (manifest must not execute tools): %s %s", r.Method, r.URL)
					http.Error(w, "unexpected request", http.StatusBadRequest)
				}
			}))
			defer server.Close()
			cli, err := client.NewClientWithOpts(client.WithHost(server.URL), client.WithVersion("1.47"))
			assert.NilError(t, err)
			defer func() { _ = cli.Close() }()
			got, err := NewBuilder(cli).ReadKitManifest(t.Context(), "kit@sha256:123")
			if malformed {
				assert.ErrorContains(t, err, "read kit manifest")
			} else {
				assert.NilError(t, err)
				assert.Equal(t, got.Kit, 6)
				assert.DeepEqual(t, got.Tools, []KitTool{{Name: "claude", Version: "2.1.310", SHA256: "pinned"}})
			}
			assert.Assert(t, copied.Load())
			assert.Assert(t, removed.Load(), "temporary container must be removed even when decoding fails")
		})
	}
}
