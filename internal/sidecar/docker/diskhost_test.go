package docker

import (
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gotest.tools/v3/assert"
)

func desktopFixture(t *testing.T, legacy bool) (hostDiscovery, string, string) {
	t.Helper()
	home, tempErr := os.MkdirTemp("", "smelt-disk-")
	assert.NilError(t, tempErr)
	t.Cleanup(func() { _ = os.RemoveAll(home) })
	socket := filepath.Join(home, ".docker", "run", "docker.sock")
	assert.NilError(t, os.MkdirAll(filepath.Dir(socket), 0o700))
	listener, err := net.Listen("unix", socket)
	assert.NilError(t, err)
	t.Cleanup(func() { _ = listener.Close() })
	settingsDir := filepath.Join(home, "Library", "Group Containers", "group.com.docker")
	assert.NilError(t, os.MkdirAll(settingsDir, 0o700))
	name := "settings-store.json"
	if legacy {
		name = "settings.json"
	}
	relocated := t.TempDir()
	data, err := json.Marshal(map[string]string{"dataFolder": relocated})
	assert.NilError(t, err)
	settings := filepath.Join(settingsDir, name)
	assert.NilError(t, os.WriteFile(settings, data, 0o600))
	assert.NilError(t, os.WriteFile(filepath.Join(relocated, "Docker.raw"), []byte("disk"), 0o600))
	return hostDiscovery{"darwin", home, os.ReadFile, filepath.EvalSymlinks, os.Stat}, relocated, settings
}

func TestDiscoverDesktopStorage(t *testing.T) {
	for _, legacy := range []bool{false, true} {
		t.Run(map[bool]string{false: "current", true: "legacy"}[legacy], func(t *testing.T) {
			d, relocated, settings := desktopFixture(t, legacy)
			report, err := d.discover("unix://"+filepath.Join(d.home, ".docker", "run", "docker.sock"), "engine", "/var/lib/docker", "Docker Desktop")
			assert.NilError(t, err)
			assert.Equal(t, report.Settings, settings)
			assert.Equal(t, report.BackingImage, filepath.Join(relocated, "Docker.raw"))
			assert.Equal(t, report.FilesystemPath, relocated)
		})
	}
}

func TestDiscoverDesktopSymlink(t *testing.T) {
	d, folder, _ := desktopFixture(t, false)
	actual := filepath.Join(t.TempDir(), "disk.raw")
	assert.NilError(t, os.Rename(filepath.Join(folder, "Docker.raw"), actual))
	assert.NilError(t, os.Symlink(actual, filepath.Join(folder, "Docker.raw")))
	report, err := d.discover("unix://"+filepath.Join(d.home, ".docker", "run", "docker.sock"), "id", "/var/lib/docker", "Docker Desktop")
	assert.NilError(t, err)
	assert.Equal(t, report.BackingImage, actual)
	assert.Equal(t, report.FilesystemPath, filepath.Dir(actual))
}

func TestDiscoverStorageFailures(t *testing.T) {
	for _, scenario := range []string{"missing image", "ambiguous", "missing settings", "bad settings", "inaccessible", "remote", "unsupported", "missing identity", "relative folder", "conflicting folder"} {
		t.Run(scenario, func(t *testing.T) {
			d, folder, settings := desktopFixture(t, false)
			endpoint, id := "unix://"+filepath.Join(d.home, ".docker", "run", "docker.sock"), "id"
			switch scenario {
			case "missing image":
				assert.NilError(t, os.Remove(filepath.Join(folder, "Docker.raw")))
			case "ambiguous":
				assert.NilError(t, os.WriteFile(filepath.Join(folder, "Docker.qcow2"), nil, 0o600))
			case "missing settings":
				assert.NilError(t, os.Remove(settings))
			case "bad settings":
				assert.NilError(t, os.WriteFile(settings, []byte("{"), 0o600))
			case "inaccessible":
				d.readFile = func(string) ([]byte, error) { return nil, os.ErrPermission }
			case "remote":
				endpoint = "tcp://remote:2376"
			case "unsupported":
				d.platform = "windows"
			case "missing identity":
				id = ""
			case "relative folder":
				assert.NilError(t, os.WriteFile(settings, []byte(`{"dataFolder":"relative"}`), 0o600))
			case "conflicting folder":
				assert.NilError(t, os.WriteFile(settings, []byte(`{"dataFolder":"/one","DataFolder":"/two"}`), 0o600))
			}
			report, err := d.discover(endpoint, id, "/var/lib/docker", "Docker Desktop")
			assert.Assert(t, err != nil)
			assert.Equal(t, report.FilesystemPath, "")
		})
	}
}

func TestDiscoverNativeDocker(t *testing.T) {
	root := t.TempDir()
	d := hostDiscovery{"linux", t.TempDir(), os.ReadFile, filepath.EvalSymlinks, os.Stat}
	report, err := d.discover("unix:///socket", "id", root, "Ubuntu Linux")
	assert.NilError(t, err)
	assert.Equal(t, report.FilesystemPath, root)
	_, err = d.discover("ssh://remote", "id", root, "Ubuntu Linux")
	assert.Assert(t, strings.Contains(err.Error(), "unsupported storage discovery"))
}
