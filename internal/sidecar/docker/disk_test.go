package docker

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/volume"
	"github.com/docker/docker/client"
	"gotest.tools/v3/assert"
)

type storageEngine struct {
	probeImage                                       string
	mu                                               sync.Mutex
	scripts                                          map[string]string
	volumes                                          map[string]volume.Volume
	helpers, workloads, measurements, trims, removed int
	vmKB, cacheKB                                    int64
	workloadKB                                       int64
	active, failMeasure, failStart, cancelWait       bool
	emptyImages                                      bool
	pulls                                            int
}

func (f *storageEngine) serve(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	path := r.URL.Path
	if strings.HasPrefix(path, "/v1.") {
		path = path[strings.Index(path[1:], "/")+1:]
	}
	w.Header().Set("Content-Type", "application/json")
	switch {
	case path == "/info":
		_, _ = fmt.Fprint(w, `{"ID":"engine","DockerRootDir":"/var/lib/docker","OperatingSystem":"Docker Desktop"}`)
	case path == "/images/create":
		f.pulls++
		_, _ = fmt.Fprint(w, "{}")
	case strings.HasPrefix(path, "/images/") && strings.HasSuffix(path, "/json") && path != "/images/json":
		if f.emptyImages {
			http.Error(w, `{"message":"not found"}`, 404)
			return
		}
		_, _ = fmt.Fprint(w, `{"Id":"sha256:local-image","Config":{"Env":["HOME=/root"]}}`)
	case path == "/images/json":
		_, _ = fmt.Fprint(w, `[{"Id":"local-image"}]`)
	case path == "/volumes/create":
		var opts volume.CreateOptions
		_ = json.NewDecoder(r.Body).Decode(&opts)
		vol, ok := f.volumes[opts.Name]
		if !ok {
			vol = volume.Volume{Name: opts.Name, CreatedAt: "2026-10-25T00:00:00Z", Mountpoint: "/volumes/" + opts.Name, Labels: opts.Labels}
			f.volumes[opts.Name] = vol
		}
		_ = json.NewEncoder(w).Encode(vol)
	case path == "/volumes":
		var vols []*volume.Volume
		for _, v := range f.volumes {
			vol := v
			vols = append(vols, &vol)
		}
		_ = json.NewEncoder(w).Encode(volume.ListResponse{Volumes: vols})
	case strings.HasPrefix(path, "/volumes/"):
		_ = json.NewEncoder(w).Encode(f.volumes[strings.TrimPrefix(path, "/volumes/")])
	case path == "/containers/json":
		if f.active {
			_, _ = fmt.Fprint(w, `[{"Id":"consumer"}]`)
		} else {
			_, _ = fmt.Fprint(w, `[]`)
		}
	case path == "/containers/create":
		var cfg container.Config
		_ = json.NewDecoder(r.Body).Decode(&cfg)
		if cfg.Labels[label] == "1" {
			f.workloads++
			f.workloadKB = f.cacheKB
		}
		if strings.Contains(strings.Join(cfg.Cmd, " "), "df -Pk") {
			f.probeImage = cfg.Image
		}
		f.helpers++
		id := fmt.Sprint(f.helpers)
		f.scripts[id] = strings.Join(cfg.Cmd, " ")
		w.WriteHeader(http.StatusCreated)
		_, _ = fmt.Fprintf(w, `{"Id":%q}`, id)
	case strings.HasSuffix(path, "/start"):
		if f.failStart {
			http.Error(w, `{"message":"start failed"}`, 500)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	case strings.HasSuffix(path, "/wait"):
		if f.cancelWait {
			<-r.Context().Done()
			return
		}
		_, _ = fmt.Fprint(w, `{"StatusCode":0}`)
	case strings.HasSuffix(path, "/logs"):
		id := strings.Split(path, "/")[2]
		script := f.scripts[id]
		var out string
		switch {
		case strings.Contains(script, "df -Pk"):
			out = fmt.Sprint(f.vmKB)
		case strings.Contains(script, "du -sk"):
			f.measurements++
			if f.failMeasure {
				out = "invalid"
			} else {
				out = fmt.Sprint(f.cacheKB)
			}
		case strings.Contains(script, "SMELT_TRIM"):
			f.trims++
			f.cacheKB = 4
		default:
			out = "1001"
		}
		w.Header().Set("Content-Type", "application/vnd.docker.raw-stream")
		_, _ = w.Write(append([]byte{1, 0, 0, 0, 0, 0, 0, byte(len(out))}, []byte(out)...))
	case r.Method == http.MethodDelete:
		f.removed++
		w.WriteHeader(http.StatusNoContent)
	default:
		http.Error(w, "unexpected: "+path, http.StatusNotFound)
	}
}

func storageProvider(t *testing.T) (*Provider, *storageEngine, string) {
	t.Helper()
	f := &storageEngine{scripts: map[string]string{}, volumes: map[string]volume.Volume{}, vmKB: 20e6, cacheKB: 4}
	server := httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(server.Close)
	cli, err := client.NewClientWithOpts(client.WithHost(server.URL), client.WithVersion("1.47"))
	assert.NilError(t, err)
	t.Cleanup(func() { _ = cli.Close() })
	b := NewWithClient(cli, "local-image", t.TempDir())
	d, relocated, _ := desktopFixture(t, false)
	// Transport uses HTTP; discovery still sees the selected socket in production.
	b.discovery = &d
	b.MinFreeBytes = 10e9
	b.ProjectPath = t.TempDir()
	b.freeBytes = func(context.Context, string) (int64, error) { return 20e9, nil }
	return b, f, relocated
}

// The fake engine uses HTTP transport. Discovery input is overridden with a
// fixture resolver independently of transport, just as a Unix socket would be.
func TestDiskRefusal(t *testing.T) {
	for _, low := range []string{"project", "workspace", "backing", "VM", "discovery", "probe"} {
		t.Run(low, func(t *testing.T) {
			b, f, relocated := storageProvider(t)
			b.endpoint = "unix://" + filepath.Join(b.discovery.home, ".docker", "run", "docker.sock")
			if low == "VM" {
				f.vmKB = 1
			}
			if low == "discovery" {
				b.endpoint = "ssh://remote"
			}
			b.freeBytes = func(_ context.Context, path string) (int64, error) {
				if low == "probe" {
					return 0, fmt.Errorf("probe unavailable")
				}
				if low == "project" && path == b.ProjectPath || low == "workspace" && path == b.workRoot || low == "backing" && path == relocated {
					return 121e6, nil
				}
				return 20e9, nil
			}
			_, err := b.Create(context.Background(), "run", "image", "/repo")
			assert.Assert(t, err != nil)
			if low != "discovery" && low != "probe" {
				assert.Assert(t, strings.HasPrefix(err.Error(), "disk almost full: "), err.Error())
				assert.Assert(t, strings.HasSuffix(err.Error(), " free; run smelt factory clean --caches"))
			}
			assert.Equal(t, f.workloads, 0)
			if low != "VM" {
				assert.Equal(t, f.helpers, 0)
			}
			_, imageErr := b.resolveImage(context.Background(), "image", nil)
			assert.Assert(t, imageErr != nil)
			if low != "discovery" && low != "probe" {
				assert.Assert(t, strings.HasPrefix(imageErr.Error(), "disk almost full: "), imageErr.Error())
			}
		})
	}
}

func TestStorageReport(t *testing.T) {
	b, f, relocated := storageProvider(t)
	b.endpoint = "unix://" + filepath.Join(b.discovery.home, ".docker", "run", "docker.sock")
	name := "smelt-cache-abcdef123456-1001"
	vol := volume.Volume{Name: name, CreatedAt: "today"}
	f.volumes[name] = vol
	assert.NilError(t, os.MkdirAll(b.storageDir(), 0o700))
	data, err := json.Marshal(cacheMeasurement{Identity: volumeIdentity("engine", vol), Bytes: 12345, Cap: 6e9, At: time.Now()})
	assert.NilError(t, err)
	assert.NilError(t, os.WriteFile(b.measurementPath("engine", name), data, 0o600))
	report, err := b.ReportStorage(context.Background())
	assert.NilError(t, err)
	assert.Equal(t, report.Host.FilesystemPath, relocated)
	assert.Equal(t, report.Host.BackingImage, filepath.Join(relocated, "Docker.raw"))
	assert.Equal(t, len(report.Filesystems), 4)
	assert.Equal(t, f.probeImage, "alpine:3.21")
	assert.Equal(t, len(report.Caches), 1)
	assert.Equal(t, report.Caches[0].Name, name)
	assert.Equal(t, report.Caches[0].Bytes, int64(12345))
	assert.Equal(t, report.Caches[0].Cap, int64(6e9))
	assert.Equal(t, report.Caches[0].Unavailable, "")
}

func TestStorageProbeBootstrapsEmptyDaemon(t *testing.T) {
	b, f, _ := storageProvider(t)
	b.endpoint = "unix://" + filepath.Join(b.discovery.home, ".docker", "run", "docker.sock")
	f.emptyImages = true
	assert.NilError(t, b.CheckStorage(context.Background()))
	assert.Equal(t, f.pulls, 1)
	assert.Equal(t, f.probeImage, "alpine:3.21")
}

func TestDiagnosticsMeasureLegacyAndExpiredVolumesHourly(t *testing.T) {
	b, f, _ := storageProvider(t)
	b.endpoint = "unix://" + filepath.Join(b.discovery.home, ".docker", "run", "docker.sock")
	name := "smelt-cache-abcdef123456"
	f.volumes[name] = volume.Volume{Name: name, CreatedAt: "today"}
	for call, expected := range []int{1, 1, 2} {
		report, err := b.ReportStorage(context.Background())
		assert.NilError(t, err)
		assert.Equal(t, len(report.Caches), 1)
		assert.Equal(t, report.Caches[0].Bytes, int64(4096))
		assert.Equal(t, report.Caches[0].Unavailable, "")
		assert.Equal(t, f.measurements, expected)
		if expected == 1 {
			record := readMeasurement(b.measurementPath("engine", name))
			// Expire only after the second call.
			if call == 1 {
				record.At = record.At.Add(-time.Hour)
				assert.NilError(t, writeMeasurement(b.measurementPath("engine", name), record))
			}
		}
	}
	assert.Equal(t, f.trims, 0)
}

func TestMaintenanceReusesFreshDiagnosticMeasurement(t *testing.T) {
	b, f, _ := storageProvider(t)
	b.endpoint = "unix://" + filepath.Join(b.discovery.home, ".docker", "run", "docker.sock")
	b.CacheMaxBytes = 10 * 1024
	name := "smelt-cache-abcdef123456"
	f.volumes[name] = volume.Volume{Name: name, CreatedAt: "today"}
	f.cacheKB = 20
	_, err := b.ReportStorage(context.Background())
	assert.NilError(t, err)
	assert.Equal(t, f.measurements, 1)
	unlock, err := b.storageLock(context.Background())
	assert.NilError(t, err)
	defer unlock()
	assert.NilError(t, b.maintainVolume(context.Background(), "engine", "local-image", name))
	// Only the required post-trim measurement follows the diagnostic scan.
	assert.Equal(t, f.measurements, 2)
	assert.Equal(t, f.trims, 1)
}
