package docker

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

// HostStorage records the evidence used to locate the daemon's host filesystem.
type HostStorage struct {
	Endpoint       string
	DaemonID       string
	DockerRoot     string
	Settings       string
	BackingImage   string
	FilesystemPath string
}

type hostDiscovery struct {
	platform, home string
	readFile       func(string) ([]byte, error)
	resolve        func(string) (string, error)
	stat           func(string) (os.FileInfo, error)
}

const platformLinux = "linux"

func localDiscovery() (hostDiscovery, error) {
	home, err := os.UserHomeDir()
	return hostDiscovery{runtime.GOOS, home, os.ReadFile, filepath.EvalSymlinks, os.Stat}, err
}

func (d hostDiscovery) discover(endpoint, id, root, operatingSystem string) (HostStorage, error) {
	report := HostStorage{Endpoint: endpoint, DaemonID: id, DockerRoot: root}
	if !strings.HasPrefix(endpoint, "unix://") {
		return report, fmt.Errorf("unsupported storage discovery for endpoint %q: select a local Unix-socket engine whose host storage can be inspected", endpoint)
	}
	if id == "" || !filepath.IsAbs(root) {
		return report, fmt.Errorf("daemon identity or Docker root is missing; check Docker engine diagnostics")
	}
	desktop := strings.Contains(strings.ToLower(operatingSystem), "docker desktop")
	if !desktop {
		if d.platform != platformLinux {
			return report, fmt.Errorf("cannot establish host storage for %q on %s; use factory doctor and inspect the selected engine", operatingSystem, d.platform)
		}
		resolved, err := d.resolve(root)
		if err != nil {
			return report, fmt.Errorf("inspect local Docker root %s: %w", root, err)
		}
		info, err := d.stat(resolved)
		if err != nil {
			return report, fmt.Errorf("inspect Docker root: %w", err)
		}
		if !info.IsDir() {
			return report, fmt.Errorf("docker root %s is not a directory", resolved)
		}
		report.FilesystemPath = resolved
		return report, nil
	}
	return d.desktop(report)
}

func (d hostDiscovery) desktop(report HostStorage) (HostStorage, error) {
	endpoint := report.Endpoint
	var settingsDir string
	switch d.platform {
	case "darwin":
		settingsDir = filepath.Join(d.home, "Library", "Group Containers", "group.com.docker")
	case platformLinux:
		settingsDir = filepath.Join(d.home, ".docker", "desktop")
	default:
		return report, fmt.Errorf("docker Desktop backing-storage discovery is unsupported on %s; inspect Settings > Resources > Advanced", d.platform)
	}
	// Docker documents settings-store.json (settings.json before Desktop 4.35).
	// dataFolder: github.com/docker/enable-macos-virtualization-framework/blob/main/settings.json
	// DataFolder: github.com/docker/desktop-feedback/issues/484 (relocated 4.79 image).
	socketPath := filepath.Join(d.home, ".docker", "run", "docker.sock")
	if d.platform == platformLinux {
		socketPath = filepath.Join(d.home, ".docker", "desktop", "docker.sock")
	}
	selected, err := d.resolve(strings.TrimPrefix(endpoint, "unix://"))
	if err != nil {
		return report, fmt.Errorf("resolve selected Desktop socket: %w", err)
	}
	expected, err := d.resolve(socketPath)
	if err != nil || selected != expected {
		return report, fmt.Errorf("selected endpoint %s cannot be associated with Desktop settings for %s; select the local Desktop socket %s", endpoint, d.home, socketPath)
	}
	var data []byte
	for _, name := range []string{"settings-store.json", "settings.json"} {
		report.Settings = filepath.Join(settingsDir, name)
		var err error
		data, err = d.readFile(report.Settings)
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return report, fmt.Errorf("read Desktop settings %s: %w", report.Settings, err)
		}
		break
	}
	if data == nil {
		return report, fmt.Errorf("desktop settings missing in %s; open Settings > Resources > Advanced and check Disk image location", settingsDir)
	}
	folder, err := desktopDataFolder(data, report.Settings)
	if err != nil {
		return report, err
	}
	return d.desktopImage(report, folder)
}

func (d hostDiscovery) desktopImage(report HostStorage, folder string) (HostStorage, error) {
	// Only validated image files in the configured directory count. Never fall
	// back to the default directory when the image has been relocated.
	for _, name := range []string{"Docker.raw", "Docker.qcow2"} {
		candidate := filepath.Join(folder, name)
		resolved, err := d.resolve(candidate)
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return report, fmt.Errorf("resolve backing image %s: %w", candidate, err)
		}
		info, err := d.stat(resolved)
		if err != nil {
			return report, fmt.Errorf("inspect backing image %s: %w", resolved, err)
		}
		if !info.Mode().IsRegular() {
			return report, fmt.Errorf("desktop backing image %s is not a regular file", resolved)
		}
		if report.BackingImage != "" {
			return report, fmt.Errorf("ambiguous Desktop backing images in %s; establish the active Disk image location", folder)
		}
		report.BackingImage = resolved
	}
	if report.BackingImage == "" {
		return report, fmt.Errorf("no Docker.raw or Docker.qcow2 in configured Desktop dataFolder %s; check Disk image location and access permissions", folder)
	}
	report.FilesystemPath = filepath.Dir(report.BackingImage)
	return report, nil
}

func desktopDataFolder(data []byte, settingsPath string) (string, error) {
	var settings map[string]json.RawMessage
	if err := json.Unmarshal(data, &settings); err != nil {
		return "", fmt.Errorf("decode %s: %w", settingsPath, err)
	}
	folder := ""
	for _, key := range []string{"dataFolder", "DataFolder"} {
		if raw, ok := settings[key]; ok {
			var value string
			if err := json.Unmarshal(raw, &value); err != nil {
				return "", fmt.Errorf("unsupported %s in %s: %w", key, settingsPath, err)
			}
			if folder != "" && folder != value {
				return "", fmt.Errorf("ambiguous Desktop dataFolder in %s", settingsPath)
			}
			folder = value
		}
	}
	if !filepath.IsAbs(folder) {
		return "", fmt.Errorf("desktop dataFolder is missing or not absolute in %s; check Disk image location in Settings > Resources > Advanced", settingsPath)
	}

	return folder, nil
}
