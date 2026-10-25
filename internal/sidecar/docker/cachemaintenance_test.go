package docker

import (
	"bytes"
	"context"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/docker/docker/client"
	"gotest.tools/v3/assert"
)

func TestTrimDecision(t *testing.T) {
	for _, tc := range []struct {
		size, cap, target int64
		trim              bool
	}{{9, 10, 7, false}, {10, 10, 7, false}, {11, 10, 7, true}, {100, 11, 7, true}, {11e9, 10e9, 7e9, true}} {
		target, trim := trimTarget(tc.size, tc.cap)
		assert.Equal(t, target, tc.target)
		assert.Equal(t, trim, tc.trim)
	}
}

func TestMeasurementSchedule(t *testing.T) {
	now := time.Now()
	for _, tc := range []struct {
		name     string
		record   cacheMeasurement
		identity string
		due      bool
	}{
		{"failed or absent", cacheMeasurement{}, "v1", true},
		{"fresh", cacheMeasurement{Identity: "v1", At: now.Add(-time.Hour + time.Nanosecond)}, "v1", false},
		{"hour boundary", cacheMeasurement{Identity: "v1", At: now.Add(-time.Hour)}, "v1", true},
		{"recreated", cacheMeasurement{Identity: "v1", At: now}, "v2", true},
		{"clock rollback", cacheMeasurement{Identity: "v1", At: now.Add(time.Second)}, "v1", true},
	} {
		t.Run(tc.name, func(t *testing.T) { assert.Equal(t, measurementDue(tc.record, tc.identity, now), tc.due) })
	}
}

func TestCoordinatedMaintenance(t *testing.T) {
	b, f, _ := storageProvider(t)
	b.CacheMaxBytes = 10 * 1024
	b.CacheKey = "project"
	f.cacheKB = 20
	var wg sync.WaitGroup
	errs := make(chan error, 8)
	for range 8 {
		wg.Go(func() {
			// Separate providers emulate independent callers sharing persisted state.
			other := NewWithClient(b.cli, "local-image", b.workRoot)
			other.CacheMaxBytes = b.CacheMaxBytes
			other.CacheKey = b.CacheKey
			unlock, err := other.storageLock(context.Background())
			if err != nil {
				errs <- err
				return
			}
			defer unlock()
			errs <- other.maintainVolume(context.Background(), "engine", "local-image", "smelt-cache-project-1001")
		})
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		assert.NilError(t, err)
	}
	assert.Equal(t, f.measurements, 2)
	assert.Equal(t, f.trims, 1)
	assert.Equal(t, f.removed, 3)
	record := readMeasurement(b.measurementPath("engine", "smelt-cache-project-1001"))
	assert.Equal(t, record.Bytes, int64(4096))
	assert.Assert(t, !record.At.IsZero())
	// Recreation invalidates the successful measurement even in the same second.
	f.mu.Lock()
	delete(f.volumes, "smelt-cache-project-1001")
	f.mu.Unlock()
	unlock, err := b.storageLock(context.Background())
	assert.NilError(t, err)
	assert.NilError(t, b.maintainVolume(context.Background(), "engine", "local-image", "smelt-cache-project-1001"))
	unlock()
	assert.Equal(t, f.measurements, 3)
}

func TestMaintenanceRefusesConsumerAndRetriesFailedMeasurement(t *testing.T) {
	b, f, _ := storageProvider(t)
	b.CacheMaxBytes = 10 * 1024
	for _, scenario := range []string{"measurement fails", "consumer attached", "success"} {
		f.failMeasure = scenario == "measurement fails"
		f.active = scenario == "consumer attached"
		f.cacheKB = 20
		unlock, err := b.storageLock(context.Background())
		assert.NilError(t, err)
		err = b.maintainVolume(context.Background(), "engine", "local-image", "smelt-cache-project-1")
		unlock()
		if scenario == "success" {
			assert.NilError(t, err)
		} else {
			assert.Assert(t, err != nil)
			assert.Equal(t, f.trims, 0)
			assert.Assert(t, readMeasurement(b.measurementPath("engine", "smelt-cache-project-1")).At.IsZero())
		}
	}
	assert.Equal(t, f.trims, 1)
}

func TestStorageLockCancellation(t *testing.T) {
	b, _, _ := storageProvider(t)
	unlock, err := b.storageLock(context.Background())
	assert.NilError(t, err)
	defer unlock()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err = b.storageLock(ctx)
	assert.ErrorIs(t, err, context.Canceled)
}

func runTrim(t *testing.T, root string, target int64, kind cacheKind) {
	t.Helper()
	cmd := exec.Command("sh", "-c", trimScript(root, target, kind))
	out, err := cmd.CombinedOutput()
	assert.NilError(t, err, string(out))
}

func TestTrimToolsThenOldestEntries(t *testing.T) {
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("python3 unavailable")
	}
	root, bin := t.TempDir(), t.TempDir()
	order := filepath.Join(t.TempDir(), "order")
	t.Setenv("PATH", bin+":"+os.Getenv("PATH"))
	t.Setenv("SMELT_TEST_ORDER", order)
	for _, tool := range []string{"go", "uv", "golangci-lint"} {
		script := "#!/bin/sh\necho " + shellQuote(tool) + " >> \"$SMELT_TEST_ORDER\"\n"
		switch tool {
		case "go":
			script += "rm -rf -- \"$GOCACHE\"\n"
		case "uv":
			script += "rm -rf -- \"$4\"\n"
		case "golangci-lint":
			script += "rm -rf -- \"$GOLANGCI_LINT_CACHE\"\n"
		}
		assert.NilError(t, os.WriteFile(filepath.Join(bin, tool), []byte(script), 0o700))
	}
	for _, name := range []string{"go-build", "uv", "golangci-lint"} {
		assert.NilError(t, os.MkdirAll(filepath.Join(root, name), 0o700))
		assert.NilError(t, os.WriteFile(filepath.Join(root, name, "cache"), []byte(strings.Repeat("x", 8192)), 0o600))
	}
	for _, name := range []string{"old", "new"} {
		assert.NilError(t, os.WriteFile(filepath.Join(root, name), []byte(strings.Repeat("x", 8192)), 0o600))
	}
	assert.NilError(t, os.Chtimes(filepath.Join(root, "old"), time.Unix(10, 0), time.Now()))
	assert.NilError(t, os.Chtimes(filepath.Join(root, "new"), time.Unix(20, 0), time.Now()))
	outside := filepath.Join(t.TempDir(), "safe")
	assert.NilError(t, os.WriteFile(outside, []byte("keep"), 0o600))
	assert.NilError(t, os.Symlink(filepath.Dir(outside), filepath.Join(root, "go-build", "external")))
	runTrim(t, root, 12*1024, homeCache)
	data, err := os.ReadFile(order)
	assert.NilError(t, err)
	assert.Equal(t, string(data), "go\nuv\ngolangci-lint\n")
	_, err = os.Stat(filepath.Join(root, "old"))
	assert.Assert(t, os.IsNotExist(err))
	_, err = os.Stat(filepath.Join(root, "new"))
	assert.NilError(t, err)
	data, err = os.ReadFile(outside)
	assert.NilError(t, err)
	assert.Equal(t, string(data), "keep")
	_, err = os.Stat(root)
	assert.NilError(t, err)
}

func TestTrimModuleVolumePreservesRootAndDoesNotRunTools(t *testing.T) {
	root := t.TempDir()
	assert.NilError(t, os.WriteFile(filepath.Join(root, "module"), []byte(strings.Repeat("x", 8192)), 0o600))
	runTrim(t, root, 4096, moduleCache)
	entries, err := os.ReadDir(root)
	assert.NilError(t, err)
	assert.Equal(t, len(entries), 0)
}

func TestOverCapTrimFinishesBeforeWorkloadMount(t *testing.T) {
	b, f, _ := storageProvider(t)
	b.MinFreeBytes = 0
	b.CacheKey = "project"
	b.CacheMaxBytes = 10 * 1024
	f.cacheKB = 20
	_, err := b.Create(context.Background(), "workload", "local-image", "/repo")
	assert.NilError(t, err)
	assert.Equal(t, f.trims, 1)
	assert.Equal(t, f.workloads, 1)
	assert.Equal(t, f.workloadKB, int64(4))
}

func TestStorageHelperCleanupOnFailureAndCancellation(t *testing.T) {
	for _, scenario := range []string{"start fails", "cancelled"} {
		t.Run(scenario, func(t *testing.T) {
			b, f, _ := storageProvider(t)
			f.failStart = scenario == "start fails"
			f.cancelWait = scenario == "cancelled"
			ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
			defer cancel()
			_, err := b.helper(ctx, "local-image", nil, "du -sk /smelt-cache")
			assert.Assert(t, err != nil)
			f.mu.Lock()
			defer f.mu.Unlock()
			assert.Equal(t, f.removed, 1)
		})
	}
}

func TestTrimMissingToolsFallsBackToEviction(t *testing.T) {
	root, bin := t.TempDir(), t.TempDir()
	for _, name := range []string{"sh", "python3", "du"} {
		executable, err := exec.LookPath(name)
		if err != nil {
			t.Skipf("%s unavailable", name)
		}
		assert.NilError(t, os.Symlink(executable, filepath.Join(bin, name)))
	}
	assert.NilError(t, os.WriteFile(filepath.Join(root, "cache"), []byte(strings.Repeat("x", 8192)), 0o600))
	t.Setenv("PATH", bin)
	out, err := exec.Command("sh", "-c", trimScript(root, 4096, homeCache)).CombinedOutput()
	assert.NilError(t, err, string(out))
	for _, name := range []string{"go", "uv", "golangci-lint"} {
		assert.Assert(t, strings.Contains(string(out), name+" unavailable"), string(out))
	}
	entries, err := os.ReadDir(root)
	assert.NilError(t, err)
	assert.Equal(t, len(entries), 0)
}

func TestCacheMaintenanceProcessHelper(t *testing.T) {
	endpoint := os.Getenv("SMELT_TEST_STORAGE_ENDPOINT")
	if endpoint == "" {
		t.Skip("child process helper")
	}
	cli, err := client.NewClientWithOpts(client.WithHost(endpoint), client.WithVersion("1.47"))
	assert.NilError(t, err)
	defer func() { _ = cli.Close() }()
	b := NewWithClient(cli, "local-image", os.Getenv("SMELT_TEST_STORAGE_ROOT"))
	b.CacheMaxBytes = 10 * 1024
	unlock, err := b.storageLock(context.Background())
	assert.NilError(t, err)
	defer unlock()
	assert.NilError(t, b.maintainVolume(context.Background(), "engine", "local-image", "smelt-cache-project-1"))
}

func TestMaintenanceCoordinatesProcesses(t *testing.T) {
	b, f, _ := storageProvider(t)
	var wg sync.WaitGroup
	failures := make(chan string, 2)
	for range 2 {
		wg.Go(func() {
			cmd := exec.Command(os.Args[0], "-test.run=^TestCacheMaintenanceProcessHelper$")
			cmd.Env = append(os.Environ(), "SMELT_TEST_STORAGE_ENDPOINT="+b.cli.DaemonHost(), "SMELT_TEST_STORAGE_ROOT="+b.workRoot)
			out, err := cmd.CombinedOutput()
			if err != nil {
				failures <- string(out)
			}
		})
	}
	wg.Wait()
	close(failures)
	for failure := range failures {
		t.Error(failure)
	}
	assert.Equal(t, f.measurements, 1)
}

func TestTrimModuleEvictsWholeVersionAndMetadata(t *testing.T) {
	root := t.TempDir()
	module := filepath.Join(root, "example.com", "lib@v1.0.0")
	metadata := filepath.Join(root, "cache", "download", "example.com", "lib", "@v")
	assert.NilError(t, os.MkdirAll(module, 0o700))
	assert.NilError(t, os.MkdirAll(metadata, 0o700))
	for _, name := range []string{"a.go", "b.go"} {
		assert.NilError(t, os.WriteFile(filepath.Join(module, name), []byte(strings.Repeat("x", 8192)), 0o444))
	}
	for _, suffix := range []string{".zip", ".ziphash", ".mod", ".info"} {
		assert.NilError(t, os.WriteFile(filepath.Join(metadata, "v1.0.0"+suffix), []byte("downloaded"), 0o600))
	}
	assert.NilError(t, os.Chtimes(module, time.Unix(1, 0), time.Unix(1, 0)))
	runTrim(t, root, 20*1024, moduleCache)
	_, err := os.Stat(module)
	assert.Assert(t, os.IsNotExist(err))
	_, err = os.Stat(filepath.Join(metadata, "v1.0.0.ziphash"))
	assert.Assert(t, os.IsNotExist(err))
}

func TestTrimUsesKitRuntimeWithoutSystemPython(t *testing.T) {
	python, err := exec.LookPath("python3")
	assert.NilError(t, err)
	kit, bin, root := t.TempDir(), t.TempDir(), t.TempDir()
	for _, dir := range []string{"lib", "libexec/python/bin"} {
		assert.NilError(t, os.MkdirAll(filepath.Join(kit, dir), 0o700))
	}
	assert.NilError(t, os.Symlink(python, filepath.Join(kit, "libexec/python/bin/python3")))
	// A fake loader checks the same arguments as the kit wrapper before
	// invoking the real interpreter; PATH deliberately has no Python.
	loader := "#!/bin/sh\n[ \"$1\" = --library-path ] || exit 1\nshift 2\nexec \"$@\"\n"
	assert.NilError(t, os.WriteFile(filepath.Join(kit, "lib", "ld-musl-test.so.1"), []byte(loader), 0o700))
	for _, name := range []string{"sh", "du"} {
		executable, err := exec.LookPath(name)
		assert.NilError(t, err)
		assert.NilError(t, os.Symlink(executable, filepath.Join(bin, name)))
	}
	t.Setenv("SMELT_KIT", kit)
	t.Setenv("PATH", bin)
	assert.NilError(t, os.WriteFile(filepath.Join(root, "cache"), make([]byte, 8192), 0o600))
	runTrim(t, root, 4096, homeCache)
	entries, err := os.ReadDir(root)
	assert.NilError(t, err)
	assert.Equal(t, len(entries), 0)
}

func TestEvictionUsesBoundedMeasurements(t *testing.T) {
	root, bin := t.TempDir(), t.TempDir()
	du, err := exec.LookPath("du")
	assert.NilError(t, err)
	count := filepath.Join(t.TempDir(), "calls")
	script := "#!/bin/sh\necho measured >> " + shellQuote(count) + "\nexec " + shellQuote(du) + " \"$@\"\n"
	assert.NilError(t, os.WriteFile(filepath.Join(bin, "du"), []byte(script), 0o700))
	t.Setenv("PATH", bin+":"+os.Getenv("PATH"))
	for i := range 3000 {
		assert.NilError(t, os.WriteFile(filepath.Join(root, fmt.Sprint(i)), []byte("cache"), 0o600))
	}
	runTrim(t, root, 4096, moduleCache)
	calls, err := os.ReadFile(count)
	assert.NilError(t, err)
	assert.Equal(t, strings.Count(string(calls), "measured"), 1)
}

func TestMaintenanceReportsFreedBytes(t *testing.T) {
	b, f, _ := storageProvider(t)
	b.CacheMaxBytes = 10 * 1024
	f.cacheKB = 20
	var output bytes.Buffer
	old := log.Writer()
	log.SetOutput(&output)
	defer log.SetOutput(old)
	unlock, err := b.storageLock(context.Background())
	assert.NilError(t, err)
	defer unlock()
	assert.NilError(t, b.maintainVolume(context.Background(), "engine", "local-image", "smelt-cache-project-1"))
	assert.Assert(t, strings.Contains(output.String(), "smelt-cache-project-1"))
	assert.Assert(t, strings.Contains(output.String(), "freed 16384 bytes"), output.String())
}
