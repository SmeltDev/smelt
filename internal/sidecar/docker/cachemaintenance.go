package docker

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/filters"
	"github.com/docker/docker/api/types/mount"
	"github.com/docker/docker/api/types/volume"
	"github.com/docker/docker/pkg/stdcopy"
	"github.com/smeltdev/smelt/internal/brand"
	"github.com/smeltdev/smelt/internal/config"
)

const measurementInterval = time.Hour

type cacheMeasurement struct {
	Identity string
	Cap      int64
	Bytes    int64
	At       time.Time
}

func trimTarget(size, capBytes int64) (int64, bool) {
	return capBytes/10*7 + capBytes%10*7/10, size > capBytes
}

func measurementDue(record cacheMeasurement, identity string, now time.Time) bool {
	return record.Identity != identity || record.At.IsZero() || now.Before(record.At) || now.Sub(record.At) >= measurementInterval
}

func storageKey(parts ...string) string {
	sum := sha256.Sum256([]byte(strings.Join(parts, "\x00")))
	return hex.EncodeToString(sum[:])
}

func (b *Provider) storageDir() string {
	if b.storageRoot != "" {
		return b.storageRoot
	}
	return filepath.Join(b.workRoot, "storage")
}

func (b *Provider) measurementPath(engine, name string) string {
	return filepath.Join(b.storageDir(), storageKey(engine, name)+".json")
}

func readMeasurement(path string) cacheMeasurement {
	var record cacheMeasurement
	data, err := os.ReadFile(path)
	if err == nil {
		_ = json.Unmarshal(data, &record)
	}
	return record
}

func (b *Provider) cachedUsage(ctx context.Context, engine, name string) CacheUsage {
	usage := CacheUsage{Name: name, Cap: b.CacheMaxBytes}
	if usage.Cap == 0 {
		usage.Cap = config.DefaultStorageGB * 1e9
	}
	record := readMeasurement(b.measurementPath(engine, name))
	vol, err := b.cli.VolumeInspect(ctx, name)
	if err != nil {
		usage.Unavailable = "volume inspection failed: " + err.Error()
		return usage
	}
	if record.At.IsZero() {
		usage.Unavailable = "not measured"
		return usage
	}
	if record.Identity != volumeIdentity(engine, vol) {
		usage.Unavailable = "volume recreated; measurement unavailable"
		return usage
	}
	if record.Cap > 0 {
		usage.Cap = record.Cap
	}
	if _, current := matchCacheVolume(name, b.CacheKey); b.CacheKey != "" && current {
		usage.Cap = b.CacheMaxBytes
	}
	usage.Bytes, usage.MeasuredAt = record.Bytes, record.At.UTC().Format(time.RFC3339)
	return usage
}

// storageLock serializes cooperating processes across maintenance and mounting.
// flock is released by the kernel after a crash; waiting honors cancellation.
func (b *Provider) storageLock(ctx context.Context) (func(), error) {
	info, err := b.cli.Info(ctx)
	if err != nil {
		return nil, fmt.Errorf("inspect Docker identity for storage coordination: %w", err)
	}
	if info.ID == "" {
		return nil, fmt.Errorf("docker engine has no identity; cannot coordinate cache maintenance")
	}
	dir := b.storageDir()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(filepath.Join(dir, storageKey(info.ID)+".lock"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	for {
		locked, err := tryStorageLock(f)
		if err != nil {
			_ = f.Close()
			return nil, err
		}
		if locked {
			return func() { unlockStorage(f); _ = f.Close() }, nil
		}
		timer := time.NewTimer(50 * time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
			_ = f.Close()
			return nil, ctx.Err()
		case <-timer.C:
		}
	}
}

// helper never pulls an image. Detached, bounded removal also runs on cancel.
func (b *Provider) helper(ctx context.Context, image string, mounts []mount.Mount, script string) (string, error) {
	created, err := b.cli.ContainerCreate(ctx, &container.Config{Image: image, User: "0", Entrypoint: []string{"sh"}, Cmd: []string{"-c", script}}, &container.HostConfig{Mounts: mounts, NetworkMode: "none"}, nil, nil, "")
	if err != nil {
		return "", fmt.Errorf("create storage helper: %w", err)
	}
	defer func() {
		cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
		defer cancel()
		if err := b.cli.ContainerRemove(cleanup, created.ID, container.RemoveOptions{Force: true}); err != nil {
			log.Printf("docker: remove storage helper %s: %v", created.ID, err)
		}
	}()
	if err := b.cli.ContainerStart(ctx, created.ID, container.StartOptions{}); err != nil {
		return "", err
	}
	done, failed := b.cli.ContainerWait(ctx, created.ID, container.WaitConditionNotRunning)
	var exitCode int64
	select {
	case result := <-done:
		exitCode = result.StatusCode
	case err := <-failed:
		return "", err
	case <-ctx.Done():
		return "", ctx.Err()
	}
	logs, err := b.cli.ContainerLogs(ctx, created.ID, container.LogsOptions{ShowStdout: true, ShowStderr: true})
	if err != nil {
		return "", err
	}
	defer func() { _ = logs.Close() }()
	var out, stderr bytes.Buffer
	if _, err := stdcopy.StdCopy(&out, &stderr, logs); err != nil {
		return "", err
	}
	if stderr.Len() > 0 {
		log.Printf("docker storage helper: %s", strings.TrimSpace(stderr.String()))
	}
	if exitCode != 0 {
		return "", fmt.Errorf("storage helper exited %d (requires sh, du, and the agent kit Python runtime or python3 for trimming): %s", exitCode, strings.TrimSpace(stderr.String()))
	}
	return out.String(), nil
}

func (b *Provider) measureVolume(ctx context.Context, image, name string) (int64, error) {
	out, err := b.helper(ctx, image, []mount.Mount{{Type: mount.TypeVolume, Source: name, Target: "/smelt-cache", ReadOnly: true}}, "du -sk /smelt-cache | cut -f1")
	if err != nil {
		return 0, err
	}
	kb, err := strconv.ParseInt(strings.TrimSpace(out), 10, 64)
	if err != nil || kb < 0 {
		return 0, fmt.Errorf("invalid cache du output %q", out)
	}
	return kb * 1024, nil
}

// volumeUsers counts the containers, running or not, that mount a volume.
func (b *Provider) volumeUsers(ctx context.Context, name string) (int, error) {
	consumers, err := b.cli.ContainerList(ctx, container.ListOptions{All: true, Filters: filters.NewArgs(filters.Arg("volume", name))})
	return len(consumers), err
}

func (b *Provider) maintainCaches(ctx context.Context, image string, layout cacheLayout) error {
	if b.CacheMaxBytes <= 0 || len(layout.Volumes) == 0 {
		return nil
	}
	info, err := b.cli.Info(ctx)
	if err != nil {
		return err
	}
	for _, cache := range layout.Volumes {
		if err := b.maintainVolume(ctx, info.ID, image, cache.Name); err != nil {
			return fmt.Errorf("maintain cache %s: %w", cache.Name, err)
		}
	}
	return nil
}

func volumeIdentity(engine string, vol volume.Volume) string {
	return storageKey(engine, vol.Name, vol.CreatedAt, vol.Mountpoint, vol.Labels[labelCacheKey+"-identity"])
}

func (b *Provider) maintainVolume(ctx context.Context, engine, image, name string) error {
	nonce, err := randomSuffix()
	if err != nil {
		return err
	}
	vol, err := b.cli.VolumeCreate(ctx, volume.CreateOptions{Name: name, Labels: map[string]string{label: "1", labelCacheKey: b.CacheKey, labelCacheKey + "-identity": nonce}})
	if err != nil {
		return err
	}
	identity := volumeIdentity(engine, vol)
	recordPath := b.measurementPath(engine, name)
	record := readMeasurement(recordPath)
	due := measurementDue(record, identity, b.clock())
	if !due && record.Bytes <= b.CacheMaxBytes {
		return nil
	}
	size := record.Bytes
	if due {
		size, err = b.measureVolume(ctx, image, name)
		if err != nil {
			return err
		}
	}
	target, trim := trimTarget(size, b.CacheMaxBytes)
	if trim {
		users, err := b.volumeUsers(ctx, name)
		if err != nil {
			return fmt.Errorf("check consumers of %s: %w", name, err)
		}
		if users != 0 {
			// Trimming under a running workload would corrupt it; the saved
			// measurement keeps the volume over the cap for the next start.
			if _, logged := b.trimDeferred.LoadOrStore(name, true); !logged {
				//nolint:gosec // G706: the name is escaped; the rest is numeric.
				log.Printf("docker: cache %s is %.1f GB, over its %.0f GB cap; in use by %d containers, trim deferred", strconv.Quote(name), float64(size)/1e9, float64(b.CacheMaxBytes)/1e9, users)
			}
			return writeMeasurement(recordPath, cacheMeasurement{Identity: identity, Cap: b.CacheMaxBytes, Bytes: size, At: b.clock()})
		}
		_, err = b.helper(ctx, image, []mount.Mount{{Type: mount.TypeVolume, Source: name, Target: "/smelt-cache"}}, trimScript("/smelt-cache", target, volumeKind(name)))
		if err != nil {
			return err
		}
		after, err := b.measureVolume(ctx, image, name)
		if err != nil {
			return err
		}
		//nolint:gosec // G706: the name is escaped; byte counts use integer formatting.
		log.Printf("docker: cache %s freed %d bytes (%d remaining, cap %d)", strconv.Quote(name), size-after, after, b.CacheMaxBytes)
		if after > target {
			return fmt.Errorf("cache remains above trim target: %d > %d bytes", after, target)
		}
		size = after
	}
	return writeMeasurement(recordPath, cacheMeasurement{Identity: identity, Cap: b.CacheMaxBytes, Bytes: size, At: b.clock()})
}

func writeMeasurement(recordPath string, record cacheMeasurement) error {
	data, err := json.Marshal(record)
	if err != nil {
		return err
	}
	temp, err := os.CreateTemp(filepath.Dir(recordPath), ".measurement-*")
	if err != nil {
		return err
	}
	defer func() { _ = os.Remove(temp.Name()) }()
	if _, err := temp.Write(data); err != nil {
		_ = temp.Close()
		return err
	}
	if err := temp.Close(); err != nil {
		return err
	}
	return os.Rename(temp.Name(), recordPath)
}

// trimScript uses explicit paths and lstat. It never traverses symlinks, removes
// mount roots, or interprets filenames as shell input. Tools run in order;
// missing tools are reported before deterministic access-time eviction.
type cacheKind string

const (
	homeCache   cacheKind = "home"
	moduleCache cacheKind = "modules"
)

func volumeKind(name string) cacheKind {
	if strings.HasPrefix(name, gomodVolumePrefix) {
		return moduleCache
	}
	return homeCache
}

func trimScript(root string, target int64, kind cacheKind) string {
	// The kit Python needs its bundled musl loader, just like the lizard wrapper.
	prefix := "kit=\"${SMELT_KIT:-/opt/" + brand.Command + "}\"; python() { if [ -x \"$kit/libexec/python/bin/python3\" ]; then \"$kit\"/lib/ld-musl-*.so.1 --library-path \"$kit/libexec/runtime/lib\" \"$kit/libexec/python/bin/python3\" \"$@\"; else python3 \"$@\"; fi; }; "
	return prefix + "python - " + shellQuote(root) + " " + strconv.FormatInt(target, 10) + " " + shellQuote(string(kind)) + " <<'SMELT_TRIM'\n" + trimPython + "\nSMELT_TRIM"
}

const trimPython = `import os, sys, stat, subprocess, shutil
root, target, modules = sys.argv[1], int(sys.argv[2]), sys.argv[3] == 'modules'
def size():
    return int(subprocess.check_output(['du', '-sk', root]).split()[0]) * 1024
def tool(name, args, env):
    if shutil.which(name):
        subprocess.run([name] + args, env=dict(os.environ, **env), check=True, stdout=sys.stderr)
    else:
        print('cache maintenance: ' + name + ' unavailable; using access-time eviction', file=sys.stderr)
# Remove links before either tools or metadata eviction can traverse them.
for directory, dirs, files in os.walk(root, followlinks=False):
    for name in dirs + files:
        path = os.path.join(directory, name)
        if os.path.islink(path):
            os.chmod(directory, 0o700)
            os.unlink(path)
if not modules:
    if size() > target:
        tool('go', ['clean', '-cache'], {'GOCACHE': os.path.join(root, 'go-build')})
    if size() > target:
        tool('uv', ['cache', 'prune', '--cache-dir', os.path.join(root, 'uv')], {})
    if size() > target:
        tool('golangci-lint', ['cache', 'clean'], {'GOLANGCI_LINT_CACHE': os.path.join(root, 'golangci-lint')})
def remove(path):
    if os.path.isdir(path) and not os.path.islink(path):
        # Downloaded module trees are read-only.
        for directory, dirs, files in os.walk(path, followlinks=False):
            os.chmod(directory, 0o700)
        shutil.rmtree(path)
    elif os.path.lexists(path):
        os.unlink(path)
entries = []
covered = set()
if modules:
    # An extracted module and its download metadata form one eviction unit.
    # Leaving a partial tree or ziphash makes Go believe it is still complete.
    download = os.path.join(root, 'cache', 'download')
    for directory, dirs, files in os.walk(root, followlinks=False):
        if directory == os.path.join(root, 'cache'):
            dirs[:] = []
            continue
        for name in list(dirs):
            path = os.path.join(directory, name)
            if '@' in name and not os.path.islink(path):
                rel = os.path.relpath(path, root)
                module, version = rel.rsplit('@', 1)
                metadata = os.path.join(download, module, '@v')
                paths = [path] + [os.path.join(metadata, version + suffix) for suffix in
                                  ['.zip', '.ziphash', '.mod', '.info', '.partial']]
                entries.append((os.lstat(path).st_atime_ns, path, paths))
                covered.add(path)
                dirs.remove(name)
# Remaining download entries (including zip-only versions) can be discarded
# together; extracted versions above always lose their completion metadata.
for directory, dirs, files in os.walk(root, followlinks=False):
    for name in list(dirs):
        path = os.path.join(directory, name)
        if path in covered:
            dirs.remove(name)
        elif modules and directory == root and name == 'cache':
            entries.append((float('inf'), path, [path]))
            dirs.remove(name)
    for name in files + [n for n in dirs if os.path.islink(os.path.join(directory, n))]:
        path = os.path.join(directory, name)
        entries.append((os.lstat(path).st_atime_ns, path, [path]))
# Each eviction unit is traversed once; du runs only at the stage boundaries.
# Count hard-linked files only when removing their last link.
def allocated(path):
    try:
        info = os.lstat(path)
    except FileNotFoundError:
        return 0
    if stat.S_ISDIR(info.st_mode):
        return info.st_blocks * 512 + sum(allocated(os.path.join(path, n)) for n in os.listdir(path))
    return info.st_blocks * 512 if info.st_nlink <= 1 else 0
remaining = size()
for _, _, paths in sorted(entries):
    if remaining <= target:
        break
    for path in paths:
        freed = allocated(path)
        remove(path)
        remaining -= freed
# Empty directories consume blocks too; retain the mounted root.
for directory, dirs, files in os.walk(root, topdown=False, followlinks=False):
    if directory != root and not os.path.islink(directory):
        try:
            os.rmdir(directory)
        except OSError:
            pass
`
