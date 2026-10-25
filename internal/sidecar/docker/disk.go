package docker

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/docker/docker/api/types/image"
	"github.com/docker/docker/api/types/mount"
	"github.com/docker/docker/api/types/volume"
	"github.com/shirou/gopsutil/v4/disk"
)

// StorageReport shares diagnostics between image preparation and factory doctor.
type StorageReport struct {
	Host        HostStorage
	HostError   string
	Filesystems []FilesystemSpace
	Caches      []CacheUsage
}

// FilesystemSpace describes an independently checked storage location.
type FilesystemSpace struct {
	Name, Path string
	Free       int64
}

// CacheUsage describes the latest successful persisted cache measurement.
type CacheUsage struct {
	Name        string
	Bytes, Cap  int64
	MeasuredAt  string
	Unavailable string
}

func availableBytes(ctx context.Context, path string) (int64, error) {
	usage, err := disk.UsageWithContext(ctx, path)
	if err != nil {
		return 0, err
	}
	if usage.Free > math.MaxInt64 {
		return math.MaxInt64, nil
	}
	return int64(usage.Free), nil
}

func existingParent(path string) (string, error) {
	for {
		_, err := os.Stat(path)
		if err == nil {
			return path, nil
		}
		if !os.IsNotExist(err) {
			return "", err
		}
		parent := filepath.Dir(path)
		if parent == path {
			return "", err
		}
		path = parent
	}
}

func diskRefusal(free, minimum int64) error {
	if free < minimum {
		return fmt.Errorf("disk almost full: %.2f GB free; run smelt factory clean --caches", float64(free)/1e9)
	}
	return nil
}

func (b *Provider) hostStorage(ctx context.Context) (HostStorage, error) {
	info, err := b.cli.Info(ctx)
	if err != nil {
		return HostStorage{}, fmt.Errorf("inspect Docker daemon storage: %w", err)
	}
	discovery, err := localDiscovery()
	if err != nil {
		return HostStorage{}, err
	}
	if b.discovery != nil {
		discovery = *b.discovery
	}
	endpoint := b.cli.DaemonHost()
	if b.endpoint != "" {
		endpoint = b.endpoint
	}
	return discovery.discover(endpoint, info.ID, info.DockerRootDir, info.OperatingSystem)
}

func (b *Provider) helperImage(ctx context.Context) (string, error) {
	// Use a small, known shell/df image rather than an arbitrary workload image.
	// This is called only after all host filesystems have passed the guard.
	const ref = "alpine:3.21"
	if _, err := b.cli.ImageInspect(ctx, ref); err == nil {
		return ref, nil
	}
	stream, err := b.cli.ImagePull(ctx, ref, image.PullOptions{})
	if err != nil {
		return "", fmt.Errorf("pull storage probe image: %w", err)
	}
	defer func() { _ = stream.Close() }()
	decoder := json.NewDecoder(stream)
	for {
		var event struct{ Error string }
		err := decoder.Decode(&event)
		if err == io.EOF {
			return ref, nil
		}
		if err != nil {
			return "", fmt.Errorf("read storage probe pull: %w", err)
		}
		if event.Error != "" {
			return "", fmt.Errorf("pull storage probe image: %s", event.Error)
		}
	}
}

// CheckStorage refuses image work and provisioning before they can write data.
func (b *Provider) CheckStorage(ctx context.Context) error {
	if b.MinFreeBytes <= 0 {
		return nil
	}
	_, err := b.storageReport(ctx, storageGuard)
	return err
}

// ReportStorage returns evidence even when discovery or measurement fails.
func (b *Provider) ReportStorage(ctx context.Context) (StorageReport, error) {
	return b.storageReport(ctx, storageDiagnostic)
}

type storageMode int

const (
	storageGuard storageMode = iota
	storageDiagnostic
)

func (b *Provider) storageReport(ctx context.Context, mode storageMode) (report StorageReport, resultErr error) {
	defer func() {
		if mode == storageDiagnostic {
			err := b.appendCacheUsage(ctx, &report)
			if resultErr == nil {
				resultErr = err
			}
		}
	}()
	host, err := b.hostStorage(ctx)
	report.Host = host
	if err != nil {
		report.HostError = err.Error()
		return report, fmt.Errorf("docker backing-storage discovery failed: %w; run smelt factory doctor", err)
	}
	if err := b.reportHostFilesystems(ctx, &report, mode); err != nil {
		return report, err
	}
	img, err := b.helperImage(ctx)
	if err != nil {
		return report, err
	}
	out, err := b.helper(ctx, img, []mount.Mount{{Type: mount.TypeBind, Source: host.DockerRoot, Target: "/smelt-disk", ReadOnly: true}}, "df -Pk /smelt-disk | tail -1 | awk '{print $4}'")
	if err != nil {
		return report, fmt.Errorf("probe Docker filesystem: %w", err)
	}
	kb, err := strconv.ParseInt(strings.TrimSpace(out), 10, 64)
	if err != nil || kb < 0 {
		return report, fmt.Errorf("probe Docker filesystem: invalid df output %q", out)
	}
	report.Filesystems = append(report.Filesystems, FilesystemSpace{Name: "Docker VM", Path: host.DockerRoot, Free: kb * 1024})
	if mode == storageGuard {
		if err := diskRefusal(kb*1024, b.MinFreeBytes); err != nil {
			return report, err
		}
	}
	return report, nil
}

// appendCacheUsage refreshes stale diagnostics without trimming or mounting a workload.
func (b *Provider) appendCacheUsage(ctx context.Context, report *StorageReport) error {
	volumes, err := b.ListCacheVolumes(ctx, "")
	if err != nil {
		return err
	}
	if len(volumes) == 0 {
		return nil
	}
	unlock, err := b.storageLock(ctx)
	if err != nil {
		return err
	}
	defer unlock()
	for _, vol := range volumes {
		name := vol.Name
		recordPath := b.measurementPath(report.Host.DaemonID, name)
		record := readMeasurement(recordPath)
		inspected, inspectErr := b.cli.VolumeInspect(ctx, name)
		var refreshErr error
		if inspectErr == nil && measurementDue(record, volumeIdentity(report.Host.DaemonID, inspected), b.clock()) {
			refreshErr = b.refreshDiagnosticMeasurement(ctx, report, inspected, recordPath)
		}
		usage := b.cachedUsage(ctx, report.Host.DaemonID, name)
		if refreshErr != nil {
			usage.Unavailable = "measurement failed: " + refreshErr.Error()
		}
		report.Caches = append(report.Caches, usage)
	}
	return nil
}

// refreshDiagnosticMeasurement only reads the volume; diagnostics never trim it.
func (b *Provider) refreshDiagnosticMeasurement(ctx context.Context, report *StorageReport, vol volume.Volume, recordPath string) error {
	// Never pull a helper before establishing and checking host storage.
	if report.HostError != "" {
		return fmt.Errorf("backing storage unavailable: %s", report.HostError)
	}
	if err := b.reportHostFilesystems(ctx, &StorageReport{Host: report.Host}, storageGuard); err != nil {
		return err
	}
	img, err := b.helperImage(ctx)
	if err != nil {
		return err
	}
	size, err := b.measureVolume(ctx, img, vol.Name)
	if err != nil {
		return err
	}
	capBytes := b.cachedUsage(ctx, report.Host.DaemonID, vol.Name).Cap
	return writeMeasurement(recordPath, cacheMeasurement{Identity: volumeIdentity(report.Host.DaemonID, vol), Cap: capBytes, Bytes: size, At: b.clock()})
}

func (b *Provider) reportHostFilesystems(ctx context.Context, report *StorageReport, mode storageMode) error {
	probe := b.freeBytes
	if probe == nil {
		probe = availableBytes
	}
	for _, loc := range []FilesystemSpace{{Name: "project", Path: b.ProjectPath}, {Name: "workspace", Path: b.workRoot}, {Name: "Docker backing storage", Path: report.Host.FilesystemPath}} {
		if loc.Path == "" {
			continue
		}
		path := loc.Path
		var err error
		if loc.Name == "Docker backing storage" {
			_, err = os.Stat(path)
		} else {
			path, err = existingParent(path)
		}
		if err != nil {
			return fmt.Errorf("locate %s filesystem: %w", loc.Name, err)
		}
		loc.Free, err = probe(ctx, path)
		if err != nil {
			return fmt.Errorf("measure %s filesystem %s: %w", loc.Name, path, err)
		}
		report.Filesystems = append(report.Filesystems, loc)
		if mode == storageGuard {
			if err := diskRefusal(loc.Free, b.MinFreeBytes); err != nil {
				return err
			}
		}
	}
	return nil
}
