package docker

import (
	"context"
	"errors"
	"fmt"
	"path"
	"strings"

	cerrdefs "github.com/containerd/errdefs"
	"github.com/docker/docker/api/types/mount"
	dockerspec "github.com/moby/docker-image-spec/specs-go/v1"

	"github.com/smeltdev/smelt/internal/brand"
)

// Cache volumes are per project and shared by every member of every run, so a
// new sidecar starts with the build cache earlier ones filled.
const (
	cacheVolumePrefix = brand.Command + "-cache-"
	gomodVolumePrefix = brand.Command + "-gomod-"
	// labelCacheKey marks a cache volume with the project it belongs to.
	labelCacheKey = brand.LabelPrefix + "cache-key"
)

// CacheVolumeNames are the names of a project's cache volumes: the one at the
// user's ~/.cache and the one at the Go module cache.
func CacheVolumeNames(key string) (cache, gomod string) {
	return cacheVolumePrefix + key, gomodVolumePrefix + key
}

// cacheVolume is a named volume and where it is mounted in a container.
type cacheVolume struct {
	Name, Target string
}

// cacheLayout is the volumes a member mounts, and who owns their mount points.
type cacheLayout struct {
	Volumes []cacheVolume
	// Owner is the user the image runs as, "" for root. A fresh volume's root
	// directory belongs to root, so a non-root user must be given it.
	Owner string
}

// layoutCaches works out where an image keeps its caches. The home directory
// cannot be written as $HOME in a mount, so it comes from the image: its HOME
// variable, else /root or /home/<user>. An image whose home cannot be known
// gets no volumes: mounted where nothing reads them they would do nothing,
// silently.
func layoutCaches(key string, goStack bool, cfg *dockerspec.DockerOCIImageConfig) cacheLayout {
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
	cacheName, gomodName := CacheVolumeNames(key)
	layout := cacheLayout{Owner: owner, Volumes: []cacheVolume{{cacheName, path.Join(home, ".cache")}}}
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
// when caching is off or the image cannot be inspected.
func (b *Provider) cacheLayout(ctx context.Context, image string) cacheLayout {
	if b.CacheKey == "" {
		return cacheLayout{}
	}
	insp, err := b.cli.ImageInspect(ctx, image)
	if err != nil {
		return cacheLayout{}
	}
	return layoutCaches(b.CacheKey, b.GoStack, insp.Config)
}

// cacheMounts are the container mounts for a layout.
func (b *Provider) cacheMounts(l cacheLayout) []mount.Mount {
	out := make([]mount.Mount, 0, len(l.Volumes))
	for _, v := range l.Volumes {
		out = append(out, mount.Mount{
			Type: mount.TypeVolume, Source: v.Name, Target: v.Target,
			VolumeOptions: &mount.VolumeOptions{Labels: map[string]string{label: "1", labelCacheKey: b.CacheKey}},
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
	code, err := b.execAs(ctx, id, "root", "chown "+shellQuote(l.Owner)+" "+strings.Join(args, " "), nil,
		func(_ string, data []byte) { out.Write(data) }, nil)
	switch {
	case err != nil:
		return fmt.Errorf("docker: hand the cache volumes to %s: %w", l.Owner, err)
	case code != 0:
		return fmt.Errorf("docker: hand the cache volumes to %s: chown exited %d: %s", l.Owner, code, strings.TrimSpace(out.String()))
	}
	return nil
}

// shellQuote single-quotes s for sh.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// RemoveCacheVolumes removes a project's cache volumes and returns the names it
// removed. A volume that does not exist is skipped; one a container still uses
// is an error, and the others are still removed.
func (b *Provider) RemoveCacheVolumes(ctx context.Context, key string) ([]string, error) {
	cache, gomod := CacheVolumeNames(key)
	var removed []string
	var errs []error
	for _, name := range []string{cache, gomod} {
		err := b.cli.VolumeRemove(ctx, name, false)
		switch {
		case err == nil:
			removed = append(removed, name)
		case cerrdefs.IsNotFound(err):
		default:
			errs = append(errs, fmt.Errorf("docker: remove volume %s: %w", name, err))
		}
	}
	return removed, errors.Join(errs...)
}
