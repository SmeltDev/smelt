package docker

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"path"
	"sort"
	"strings"
	"time"

	cerrdefs "github.com/containerd/errdefs"
	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/filters"
	"github.com/docker/docker/api/types/mount"
	"github.com/docker/docker/api/types/volume"
	"github.com/docker/docker/pkg/stdcopy"
	dockerspec "github.com/moby/docker-image-spec/specs-go/v1"

	"github.com/smeltdev/smelt/internal/brand"
)

// Cache volumes are per project and image user, and shared by every member of
// every run, so a new sidecar starts with the build cache earlier ones filled.
// The user is part of the name because files in a volume belong to whoever
// wrote them: another user could not write them, and the gates would fail.
const (
	cacheVolumePrefix = brand.Command + "-cache-"
	gomodVolumePrefix = brand.Command + "-gomod-"
	// labelCacheKey marks a cache volume with the project it belongs to.
	labelCacheKey = brand.LabelPrefix + "cache-key"
)

// CacheVolumeNames are the names of a project's cache volumes for the image
// user with the given uid: the one at the user's ~/.cache and the one at the Go
// module cache.
func CacheVolumeNames(key, uid string) (cache, gomod string) {
	return cacheVolumePrefix + key + "-" + uid, gomodVolumePrefix + key + "-" + uid
}

// cacheVolume is a named volume and where it is mounted in a container.
type cacheVolume struct {
	Name, Target string
}

// cacheLayout is the volumes a member mounts, and who owns their mount points.
type cacheLayout struct {
	Volumes []cacheVolume
	// UID is the image's runtime uid, which the volume names carry.
	UID string
	// Owner is the user the image runs as, "" for root. A fresh volume's root
	// directory belongs to root, so a non-root user must be given it.
	Owner string
}

// layoutCaches works out where an image keeps its caches. The home directory
// cannot be written as $HOME in a mount, so it comes from the image: its HOME
// variable, else /root or /home/<user>. An image whose home cannot be known
// gets no volumes: mounted where nothing reads them they would do nothing,
// silently.
func layoutCaches(key, uid string, goStack bool, cfg *dockerspec.DockerOCIImageConfig) cacheLayout {
	if key == "" || cfg == nil {
		return cacheLayout{}
	}
	env := map[string]string{}
	for _, kv := range cfg.Env {
		if k, v, ok := strings.Cut(kv, "="); ok {
			env[k] = v
		}
	}
	user, _, _ := strings.Cut(cfg.User, ":")
	owner := ""
	if user != "" && user != "root" && user != "0" {
		owner = cfg.User
	}
	home := env["HOME"]
	switch {
	case home != "":
	case owner == "":
		home = "/root"
	case !isNumeric(user):
		home = "/home/" + user
	default:
		return cacheLayout{}
	}
	cacheName, gomodName := CacheVolumeNames(key, uid)
	layout := cacheLayout{Owner: owner, UID: uid, Volumes: []cacheVolume{{cacheName, path.Join(home, ".cache")}}}
	if goStack {
		layout.Volumes = append(layout.Volumes, cacheVolume{gomodName, goModCache(env, home)})
	}
	return layout
}

// goModCache is where the image's Go keeps downloaded modules.
func goModCache(env map[string]string, home string) string {
	if dir := env["GOMODCACHE"]; dir != "" {
		return dir
	}
	if gopath := env["GOPATH"]; gopath != "" {
		return path.Join(strings.Split(gopath, ":")[0], "pkg", "mod")
	}
	return path.Join(home, "go", "pkg", "mod")
}

func isNumeric(s string) bool {
	return s != "" && strings.Trim(s, "0123456789") == ""
}

// cacheLayout inspects the image and lays out the member's cache volumes; none
// when caching is off, the image cannot be inspected or its user is unknown.
func (b *Provider) cacheLayout(ctx context.Context, image string) cacheLayout {
	if b.CacheKey == "" {
		return cacheLayout{}
	}
	insp, err := b.cli.ImageInspect(ctx, image)
	if err != nil || insp.Config == nil {
		return cacheLayout{}
	}
	imageID := insp.ID
	if imageID == "" {
		imageID = image
	}
	uid, err := b.imageUID(ctx, imageID, image)
	if err != nil {
		log.Printf("docker: no cache volumes for %s: %v", image, err)
		return cacheLayout{}
	}
	return layoutCaches(b.CacheKey, uid, b.GoStack, insp.Config)
}

// imageUID is the uid the image's containers run as, read once per image ID
// with a throwaway `id -u` container.
func (b *Provider) imageUID(ctx context.Context, imageID, image string) (string, error) {
	b.uidMu.Lock()
	defer b.uidMu.Unlock()
	if uid, ok := b.uids[imageID]; ok {
		return uid, nil
	}
	uid, err := b.probeUID(ctx, image)
	if err != nil {
		return "", err
	}
	if b.uids == nil {
		b.uids = map[string]string{}
	}
	b.uids[imageID] = uid
	return uid, nil
}

// probeUID runs `id -u` in a container of image and returns what it prints.
func (b *Provider) probeUID(ctx context.Context, image string) (string, error) {
	created, err := b.cli.ContainerCreate(ctx, &container.Config{
		Image: image, Entrypoint: []string{"id"}, Cmd: []string{"-u"},
	}, &container.HostConfig{}, nil, nil, "")
	if err != nil {
		return "", fmt.Errorf("probe the user of %s: %w", image, err)
	}
	defer func() {
		cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
		defer cancel()
		_ = b.cli.ContainerRemove(cleanup, created.ID, container.RemoveOptions{Force: true})
	}()
	if err := b.cli.ContainerStart(ctx, created.ID, container.StartOptions{}); err != nil {
		return "", fmt.Errorf("probe the user of %s: %w", image, err)
	}
	waitCh, errCh := b.cli.ContainerWait(ctx, created.ID, container.WaitConditionNotRunning)
	select {
	case res := <-waitCh:
		if res.StatusCode != 0 {
			return "", fmt.Errorf("probe the user of %s: id exited %d", image, res.StatusCode)
		}
	case err := <-errCh:
		return "", fmt.Errorf("probe the user of %s: %w", image, err)
	case <-ctx.Done():
		return "", fmt.Errorf("probe the user of %s: %w", image, ctx.Err())
	}
	logs, err := b.cli.ContainerLogs(ctx, created.ID, container.LogsOptions{ShowStdout: true})
	if err != nil {
		return "", fmt.Errorf("probe the user of %s: %w", image, err)
	}
	defer func() { _ = logs.Close() }()
	var out bytes.Buffer
	if _, err := stdcopy.StdCopy(&out, io.Discard, logs); err != nil {
		return "", fmt.Errorf("probe the user of %s: read output: %w", image, err)
	}
	uid := strings.TrimSpace(out.String())
	if !isNumeric(uid) {
		return "", fmt.Errorf("probe the user of %s: unexpected output %q", image, uid)
	}
	return uid, nil
}

// cacheMounts are the container mounts for a layout.
func (b *Provider) cacheMounts(l cacheLayout) []mount.Mount {
	out := make([]mount.Mount, 0, len(l.Volumes))
	for _, v := range l.Volumes {
		out = append(out, mount.Mount{
			Type: mount.TypeVolume, Source: v.Name, Target: v.Target,
			// Copying image contents into a fresh volume would fill it after
			// maintenance measured it, bypassing the pre-mount cap check.
			VolumeOptions: &mount.VolumeOptions{NoCopy: true, Labels: map[string]string{label: "1", labelCacheKey: b.CacheKey}},
		})
	}
	return out
}

// ownCaches hands the volumes' mount points to the image's user, which a fresh
// volume would otherwise keep for root and the build cache could not be
// written. Only the mount point is changed, not the cache inside it, so it is
// quick on a volume that is already full.
func (b *Provider) ownCaches(ctx context.Context, id string, l cacheLayout) error {
	if l.Owner == "" || len(l.Volumes) == 0 {
		return nil
	}
	args := make([]string, len(l.Volumes))
	for i, v := range l.Volumes {
		args[i] = shellQuote(v.Target)
	}
	var out strings.Builder
	code, err := b.execAs(ctx, id, "root", "chown "+shellQuote(l.Owner)+" "+strings.Join(args, " "),
		func(_ string, data []byte) { out.Write(data) })
	switch {
	case err != nil:
		return fmt.Errorf("docker: hand the cache volumes to %s: %w", l.Owner, err)
	case code != 0:
		return fmt.Errorf("docker: hand the cache volumes to %s: chown exited %d: %s", l.Owner, code, strings.TrimSpace(out.String()))
	}
	return nil
}

// foreignContentScript exits 1 when a directory holds an entry, within a few
// levels, that the user with the given uid does not own. A cache another user
// filled cannot be written (go-build/ed, say), though its mount point can once
// it is handed over. The depth bound and -quit keep it cheap on a large cache.
func foreignContentScript(uid string, dirs []string) string {
	quoted := make([]string, len(dirs))
	for i, d := range dirs {
		quoted[i] = shellQuote(d)
	}
	return `for d in ` + strings.Join(quoted, " ") + `; do
  [ -d "$d" ] || continue
  if [ -n "$(find "$d" -mindepth 1 -maxdepth 3 ! -user ` + shellQuote(uid) + ` -print -quit)" ]; then exit 1; fi
done`
}

// writableScript exits non-zero when the user cannot write a directory.
func writableScript(dirs []string) string {
	tests := make([]string, len(dirs))
	for i, d := range dirs {
		tests[i] = "test -w " + shellQuote(d)
	}
	return strings.Join(tests, " && ")
}

// foreignCaches reports whether a cache volume holds content the image's user
// does not own. It runs as root before ownCaches, which would hide a root-owned
// mount point but not what is inside it.
func (b *Provider) foreignCaches(ctx context.Context, id string, l cacheLayout) (bool, error) {
	if l.Owner == "" || len(l.Volumes) == 0 {
		return false, nil
	}
	code, err := b.execAs(ctx, id, "root", foreignContentScript(l.UID, l.targets()), nil)
	if err != nil {
		return false, fmt.Errorf("docker: check the cache volumes: %w", err)
	}
	return code != 0, nil
}

// unwritableCaches reports whether the image's user cannot write a cache mount
// point, after ownCaches: what ownership cannot show, a read-only mount say.
func (b *Provider) unwritableCaches(ctx context.Context, id string, l cacheLayout) (bool, error) {
	if l.Owner == "" || len(l.Volumes) == 0 {
		return false, nil
	}
	code, err := b.execAs(ctx, id, "", writableScript(l.targets()), nil)
	if err != nil {
		return false, fmt.Errorf("docker: check the cache volumes: %w", err)
	}
	return code != 0, nil
}

func (l cacheLayout) targets() []string {
	out := make([]string, len(l.Volumes))
	for i, v := range l.Volumes {
		out[i] = v.Target
	}
	return out
}

// fallbackCacheEnv points every cache at a directory inside the container, for
// a run whose cache volumes cannot be used.
func fallbackCacheEnv(uid string) map[string]string {
	dir := "/tmp/" + brand.Command + "-cache-" + uid
	return map[string]string{
		"GOCACHE":        dir + "/go-build",
		"GOMODCACHE":     dir + "/go-mod",
		"UV_CACHE_DIR":   dir + "/uv",
		"XDG_CACHE_HOME": dir + "/xdg",
	}
}

// useFallbackCache warns once and makes the fallback directories, as the image's
// user; the returned env is what commands in the container then run with.
func (b *Provider) useFallbackCache(ctx context.Context, id string, l cacheLayout) (map[string]string, error) {
	names := make([]string, len(l.Volumes))
	for i, v := range l.Volumes {
		names[i] = v.Name
	}
	log.Printf("docker: cache volume %s is not writable by uid %s; using a temporary cache for this run: run `%s factory clean --cache` to reset it",
		strings.Join(names, ", "), l.UID, brand.Command)
	env := fallbackCacheEnv(l.UID)
	dirs := make([]string, 0, len(env))
	for _, d := range env {
		dirs = append(dirs, shellQuote(d))
	}
	sort.Strings(dirs)
	code, err := b.execAs(ctx, id, "", "mkdir -p "+strings.Join(dirs, " "), nil)
	switch {
	case err != nil:
		return nil, fmt.Errorf("docker: make the temporary cache: %w", err)
	case code != 0:
		return nil, fmt.Errorf("docker: make the temporary cache: mkdir exited %d", code)
	}
	return env, nil
}

// prepareCaches readies the cache volumes of a started container for its
// user. The content check comes first: handing over the mount points would hide
// a root-owned one, though not what the previous user left inside. When the
// volumes cannot be used it returns the env that moves the caches inside the
// container, so the run goes on, slower, instead of failing.
func (b *Provider) prepareCaches(ctx context.Context, id string, l cacheLayout) (map[string]string, error) {
	foreign, err := b.foreignCaches(ctx, id, l)
	if err != nil {
		return nil, err
	}
	if err := b.ownCaches(ctx, id, l); err != nil {
		return nil, err
	}
	if !foreign {
		if foreign, err = b.unwritableCaches(ctx, id, l); err != nil {
			return nil, err
		}
	}
	if !foreign {
		return nil, nil
	}
	return b.useFallbackCache(ctx, id, l)
}

// shellQuote single-quotes s for sh.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// CacheVolume is an existing cache volume of a project.
type CacheVolume struct {
	Name string
	// Legacy marks a volume from before the names carried the image user's uid.
	// Runs no longer use it; only a clean removes it.
	Legacy bool
}

// ListCacheVolumes lists the project's cache volumes, for every uid and the
// legacy ones without a uid. Another project's volumes whose key merely starts
// with this one are not matched.
func (b *Provider) ListCacheVolumes(ctx context.Context, key string) ([]CacheVolume, error) {
	res, err := b.cli.VolumeList(ctx, volume.ListOptions{Filters: filters.NewArgs(
		filters.Arg("name", cacheVolumePrefix+key), filters.Arg("name", gomodVolumePrefix+key))})
	if err != nil {
		return nil, fmt.Errorf("docker: list volumes: %w", err)
	}
	var out []CacheVolume
	for _, v := range res.Volumes {
		if v == nil {
			continue
		}
		if legacy, ok := matchCacheVolume(v.Name, key); ok {
			out = append(out, CacheVolume{Name: v.Name, Legacy: legacy})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// matchCacheVolume tells whether name is one of the project's cache volumes,
// and whether it is a legacy one.
func matchCacheVolume(name, key string) (legacy, ok bool) {
	if key == "" {
		for _, prefix := range []string{cacheVolumePrefix, gomodVolumePrefix} {
			if rest, found := strings.CutPrefix(name, prefix); found {
				project, _, _ := strings.Cut(rest, "-")
				if project != "" {
					return matchCacheVolume(name, project)
				}
			}
		}
		return false, false
	}
	for _, prefix := range []string{cacheVolumePrefix, gomodVolumePrefix} {
		rest, found := strings.CutPrefix(name, prefix+key)
		switch {
		case !found:
		case rest == "":
			return true, true
		case strings.HasPrefix(rest, "-") && isNumeric(rest[1:]):
			return false, true
		}
	}
	return false, false
}

// RemoveCacheVolumes removes a project's cache volumes, whatever their uid, and
// returns the names it removed. A volume that does not exist is skipped; one a
// container still uses is an error, and the others are still removed.
func (b *Provider) RemoveCacheVolumes(ctx context.Context, key string) ([]string, error) {
	unlock, err := b.storageLock(ctx)
	if err != nil {
		return nil, err
	}
	defer unlock()
	vols, err := b.ListCacheVolumes(ctx, key)
	if err != nil {
		return nil, err
	}
	var removed []string
	var errs []error
	for _, v := range vols {
		err := b.cli.VolumeRemove(ctx, v.Name, false)
		switch {
		case err == nil:
			removed = append(removed, v.Name)
		case cerrdefs.IsNotFound(err):
		default:
			errs = append(errs, fmt.Errorf("docker: remove volume %s: %w", v.Name, err))
		}
	}
	return removed, errors.Join(errs...)
}
