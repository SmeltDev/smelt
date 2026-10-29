package docker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log"
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
	Host      HostStorage
	HostError string
	// HostPermission is set when HostError is macOS (or the OS) denying a read.
	HostPermission bool
	// Warning is set when the run guard went ahead without knowing where
	// Docker keeps its data.
	Warning     string
	Filesystems []FilesystemSpace
	Caches      []CacheUsage
	// Workspace is the mode new members get and WorkspaceReason why; Workspaces
	// are the workspace and snapshot volumes that exist.
	Workspace       WorkspaceMode
	WorkspaceReason string
	Workspaces      []WorkspaceUsage
}

// WorkspaceUsage is a workspace or snapshot volume and the space it holds.
type WorkspaceUsage struct {
	Name        string
	Bytes       int64
	Unavailable string
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

// daemonStorage is what `docker info` says about where the daemon keeps its
// data. It needs no host permission, unlike the discovery that follows it.
func (b *Provider) daemonStorage(ctx context.Context) (HostStorage, string, error) {
	info, err := b.cli.Info(ctx)
	if err != nil {
		return HostStorage{}, "", fmt.Errorf("inspect Docker daemon storage: %w", err)
	}
	endpoint := b.cli.DaemonHost()
	if b.endpoint != "" {
		endpoint = b.endpoint
	}
	return HostStorage{Endpoint: endpoint, DaemonID: info.ID, DockerRoot: info.DockerRootDir}, info.OperatingSystem, nil
}

func (b *Provider) hostStorage(ctx context.Context) (HostStorage, error) {
	base, operatingSystem, err := b.daemonStorage(ctx)
	if err != nil {
		return HostStorage{}, err
	}
	discovery, err := localDiscovery()
	if err != nil {
		return HostStorage{}, err
	}
	if b.discovery != nil {
		discovery = *b.discovery
	}
	return discovery.discover(base.Endpoint, base.DaemonID, base.DockerRoot, operatingSystem)
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
	_, err := b.GuardStorage(ctx)
	return err
}

// GuardStorage runs the CheckStorage guard and returns the warning shown when
// it went ahead without being able to check Docker's free disk space; the
// warning is empty when the guard measured the disks.
func (b *Provider) GuardStorage(ctx context.Context) (string, error) {
	if b.MinFreeBytes <= 0 {
		return "", nil
	}
	report, err := b.storageReport(ctx, storageGuard)
	if err != nil {
		return "", err
	}
	if report.Warning != "" {
		b.storageWarned.Do(func() { log.Printf("smelt: %s", report.Warning) })
	}
	return report.Warning, nil
}

// StorageWarning is the one line shown when Docker's free disk space can't be checked.
func StorageWarning(reason string) string {
	return fmt.Sprintf("⚠ couldn't check Docker's free disk space (%s); runs aren't protected from a full disk. See smelt factory doctor", reason)
}

// IsPermissionDenied reports whether err is the operating system refusing a read.
func IsPermissionDenied(err error) bool {
	return errors.Is(err, fs.ErrPermission)
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
			err := errors.Join(b.appendCacheUsage(ctx, &report), b.appendWorkspaceUsage(ctx, &report))
			if resultErr == nil {
				resultErr = err
			}
		}
	}()
	if mode == storageGuard {
		if done, err := b.guardFromDaemonInfo(ctx, &report); done {
			return report, err
		}
		report = StorageReport{}
	}
	host, err := b.hostStorage(ctx)
	report.Host = host
	if err != nil {
		report.HostError = err.Error()
		report.HostPermission = IsPermissionDenied(err)
		if mode == storageDiagnostic {
			return report, fmt.Errorf("docker backing-storage discovery failed: %w; run smelt factory doctor", err)
		}
		// Finding Docker's backing storage is best effort: macOS privacy rules
		// can hide Docker Desktop's settings, and a remote engine has no local
		// disk to find. Only a measured low-space result refuses a run.
		report.Warning = StorageWarning(report.HostError)
	}
	if err := b.reportHostFilesystems(ctx, &report, mode); err != nil {
		return report, err
	}
	if report.Warning != "" && (!strings.HasPrefix(host.Endpoint, "unix://") || !filepath.IsAbs(host.DockerRoot)) {
		return report, nil
	}
	if err := b.reportDockerVM(ctx, &report, host.DockerRoot, mode); err != nil {
		if report.Warning != "" {
			// Nothing could be measured and the user is already warned.
			return report, nil
		}
		return report, err
	}
	return report, nil
}

// guardFromDaemonInfo checks the guard without Docker Desktop's settings file:
// the project and workspace disks, then the Docker VM disk through a helper
// container at the DockerRootDir `docker info` reports. done is false when that
// can't be established and the caller must fall back to reading the settings.
func (b *Provider) guardFromDaemonInfo(ctx context.Context, report *StorageReport) (done bool, err error) {
	base, _, infoErr := b.daemonStorage(ctx)
	if infoErr != nil || !strings.HasPrefix(base.Endpoint, "unix://") || base.DaemonID == "" || !filepath.IsAbs(base.DockerRoot) {
		return false, nil
	}
	if err := b.reportHostFilesystems(ctx, report, storageGuard); err != nil {
		return true, err
	}
	probe := &StorageReport{}
	if b.reportDockerVM(ctx, probe, base.DockerRoot, storageDiagnostic) != nil {
		return false, nil
	}
	report.Host = base
	report.Filesystems = append(report.Filesystems, probe.Filesystems...)
	return true, b.refuseVM(probe)
}

// refuseVM turns a low Docker VM disk into the refusal error.
func (b *Provider) refuseVM(report *StorageReport) error {
	for _, fsp := range report.Filesystems {
		if err := diskRefusal(fsp.Free, b.MinFreeBytes); err != nil {
			return err
		}
	}
	return nil
}

// reportDockerVM measures the disk the Docker daemon writes to, from inside it.
func (b *Provider) reportDockerVM(ctx context.Context, report *StorageReport, root string, mode storageMode) error {
	img, err := b.helperImage(ctx)
	if err != nil {
		return err
	}
	out, err := b.helper(ctx, img, []mount.Mount{{Type: mount.TypeBind, Source: root, Target: "/smelt-disk", ReadOnly: true}}, "df -Pk /smelt-disk | tail -1 | awk '{print $4}'")
	if err != nil {
		return fmt.Errorf("probe Docker filesystem: %w", err)
	}
	kb, err := strconv.ParseInt(strings.TrimSpace(out), 10, 64)
	if err != nil || kb < 0 {
		return fmt.Errorf("probe Docker filesystem: invalid df output %q", out)
	}
	report.Filesystems = append(report.Filesystems, FilesystemSpace{Name: "Docker VM", Path: root, Free: kb * 1024})
	if mode == storageGuard {
		return diskRefusal(kb*1024, b.MinFreeBytes)
	}
	return nil
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

// appendWorkspaceUsage adds the workspace mode and the sizes of the workspace
// and snapshot volumes, which count against Docker's disk like the caches. A
// volume that can't be measured is listed with the reason.
func (b *Provider) appendWorkspaceUsage(ctx context.Context, report *StorageReport) error {
	if mode, reason, err := b.ResolveWorkspace(ctx); err == nil {
		report.Workspace, report.WorkspaceReason = mode, reason
	}
	volumes, err := b.listWorkspaceVolumes(ctx)
	if err != nil || len(volumes) == 0 {
		return err
	}
	var img string
	if report.HostError == "" {
		img, err = b.helperImage(ctx)
	}
	for _, vol := range volumes {
		usage := WorkspaceUsage{Name: vol.Name}
		switch {
		case report.HostError != "":
			usage.Unavailable = "backing storage unavailable: " + report.HostError
		case err != nil:
			usage.Unavailable = "measurement failed: " + err.Error()
		default:
			if usage.Bytes, usage.Unavailable = b.measureWorkspace(ctx, img, vol.Name); usage.Unavailable != "" {
				usage.Unavailable = "measurement failed: " + usage.Unavailable
			}
		}
		report.Workspaces = append(report.Workspaces, usage)
	}
	return nil
}

func (b *Provider) measureWorkspace(ctx context.Context, img, name string) (int64, string) {
	size, err := b.measureVolume(ctx, img, name)
	if err != nil {
		return 0, err.Error()
	}
	return size, ""
}

// workspaceInVolume tells whether new members get a volume workspace, in which
// case no host directory is written and its filesystem needs no check.
func (b *Provider) workspaceInVolume(ctx context.Context) bool {
	mode, _, err := b.ResolveWorkspace(ctx)
	return err == nil && mode == WorkspaceVolume
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
		if loc.Path == "" || loc.Name == "workspace" && b.workspaceInVolume(ctx) {
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
