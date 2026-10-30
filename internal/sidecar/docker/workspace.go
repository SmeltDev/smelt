package docker

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	cerrdefs "github.com/containerd/errdefs"
	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/filters"
	"github.com/docker/docker/api/types/mount"
	"github.com/docker/docker/api/types/volume"

	"github.com/smeltdev/smelt/internal/brand"
	"github.com/smeltdev/smelt/internal/config"
	"github.com/smeltdev/smelt/internal/iostream"
)

// WorkspaceMode is where a member's workspace lives.
type WorkspaceMode string

const (
	// WorkspaceVolume keeps the workspace on a Docker volume inside the engine.
	// Nothing is bind-mounted from the host, so a VM-backed engine (Docker
	// Desktop) does not carry every file access across the host file share.
	WorkspaceVolume WorkspaceMode = config.DockerWorkspaceVolume
	// WorkspaceBind keeps the workspace in a host directory mounted into the
	// container, which is native speed on a Linux engine.
	WorkspaceBind WorkspaceMode = config.DockerWorkspaceBind
)

const (
	// workVolumePrefix and snapVolumePrefix name a member's workspace volume
	// and a snapshot of one.
	workVolumePrefix = brand.Command + "-work-"
	snapVolumePrefix = brand.Command + "-snap-"
	// labelImage records, on a snapshot volume, the image its seed ran on.
	labelImage = brand.LabelPrefix + "image"
	// orphanAge is how old an unused workspace or snapshot volume must be
	// before cache maintenance removes it, so one a run is about to mount is
	// never taken.
	orphanAge = 24 * time.Hour
	// desktopEngine is what a Docker Desktop engine reports as its operating
	// system.
	desktopEngine = "Docker Desktop"
)

// ParseWorkspaceMode reads the docker.workspace setting. Empty means auto.
func ParseWorkspaceMode(s string) (WorkspaceMode, error) {
	switch mode := WorkspaceMode(s); mode {
	case "", WorkspaceVolume, WorkspaceBind:
		return mode, nil
	}
	return "", fmt.Errorf("docker workspace %q is not %q or %q", s, WorkspaceVolume, WorkspaceBind)
}

// decideWorkspace picks the workspace mode: the setting when there is one,
// otherwise a volume on Docker Desktop (whose bind mounts cross the VM) and a
// bind mount elsewhere. The reason is what factory doctor shows.
func decideWorkspace(configured WorkspaceMode, operatingSystem string) (WorkspaceMode, string) {
	switch {
	case configured != "":
		return configured, "set by docker.workspace"
	case strings.Contains(operatingSystem, desktopEngine):
		return WorkspaceVolume, "the engine is " + desktopEngine + ", whose bind mounts are slow"
	}
	return WorkspaceBind, "the engine is not " + desktopEngine
}

// ResolveWorkspace is the mode new members get, and why.
func (b *Provider) ResolveWorkspace(ctx context.Context) (WorkspaceMode, string, error) {
	if b.Workspace != "" {
		mode, reason := decideWorkspace(b.Workspace, "")
		return mode, reason, nil
	}
	info, err := b.cli.Info(ctx)
	if err != nil {
		return "", "", fmt.Errorf("docker: inspect the engine: %w", err)
	}
	mode, reason := decideWorkspace("", info.OperatingSystem)
	return mode, reason, nil
}

// ownedLabels are the labels of a volume smelt created, with extra added.
func ownedLabels(extra map[string]string) map[string]string {
	labels := map[string]string{label: "1"}
	for k, v := range extra {
		labels[k] = v
	}
	return labels
}

// volumeSafe replaces what a Docker volume name cannot hold.
func volumeSafe(s string) string {
	return strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '_', r == '.', r == '-':
			return r
		}
		return '-'
	}, s)
}

func workVolumeName(containerName string) string { return workVolumePrefix + volumeSafe(containerName) }

func snapshotVolumeName(name string) string { return snapVolumePrefix + volumeSafe(name) }

// snapshotSource is what a snapshot ref names: a volume or a host directory.
type snapshotSource struct {
	volume, dir string
}

// parseSnapshotRef reads a ref made by Snapshot. A ref of the other workspace
// mode, such as one from before volumes, is refused with a way forward rather
// than failing later on a path that is not there.
func parseSnapshotRef(ref string, mode WorkspaceMode) (snapshotSource, error) {
	rest := strings.TrimPrefix(ref, snapshotScheme)
	isVolume := strings.HasPrefix(rest, snapVolumePrefix) && !filepath.IsAbs(rest)
	switch {
	case mode == WorkspaceVolume && isVolume:
		return snapshotSource{volume: rest}, nil
	case mode == WorkspaceBind && !isVolume && filepath.IsAbs(rest):
		return snapshotSource{dir: rest}, nil
	}
	return snapshotSource{}, fmt.Errorf("docker: snapshot %s was not taken with the %s workspace; take a new snapshot", ref, mode)
}

// snapshotRestore is a snapshot opened for a clone: where its files are, and
// the image its seed ran.
type snapshotRestore struct {
	source snapshotSource
	image  string
}

func (b *Provider) openSnapshot(ctx context.Context, mode WorkspaceMode, ref string) (snapshotRestore, error) {
	source, err := parseSnapshotRef(ref, mode)
	if err != nil {
		return snapshotRestore{}, err
	}
	if source.volume == "" {
		recorded, err := os.ReadFile(filepath.Join(source.dir, snapshotImageFile))
		if err != nil {
			return snapshotRestore{}, fmt.Errorf("docker: read snapshot image: %w", err)
		}
		source.dir = filepath.Join(source.dir, snapshotWorkspaceDir)
		return snapshotRestore{source: source, image: strings.TrimSpace(string(recorded))}, nil
	}
	vol, err := b.cli.VolumeInspect(ctx, source.volume)
	if err != nil {
		return snapshotRestore{}, fmt.Errorf("docker: read snapshot %s: %w", source.volume, err)
	}
	if vol.Labels[labelImage] == "" {
		return snapshotRestore{}, fmt.Errorf("docker: snapshot %s records no image", source.volume)
	}
	return snapshotRestore{source: source, image: vol.Labels[labelImage]}, nil
}

// staged is the storage prepared for a new member's workspace: how the
// container mounts it and how to take it back if creation fails.
type staged struct {
	member member
	binds  []string
	mounts []mount.Mount
	undo   func()
}

// stageWorkspace prepares the member's workspace storage, restored from a
// snapshot when restore is set.
func (b *Provider) stageWorkspace(ctx context.Context, mode WorkspaceMode, containerName, repoPath, containerImage string, restore *snapshotRestore) (staged, error) {
	if mode == WorkspaceBind {
		return b.stageBind(ctx, containerName, repoPath, restore)
	}
	return b.stageVolume(ctx, containerName, repoPath, containerImage, restore)
}

func (b *Provider) stageBind(ctx context.Context, containerName, repoPath string, restore *snapshotRestore) (staged, error) {
	hostDir := filepath.Join(b.workRoot, containerName)
	if err := os.MkdirAll(hostDir, 0o777); err != nil {
		return staged{}, fmt.Errorf("docker: create workspace dir: %w", err)
	}
	undo := func() { _ = os.RemoveAll(hostDir) }
	if restore != nil {
		if err := copyTree(ctx, restore.source.dir, hostDir); err != nil {
			undo()
			return staged{}, fmt.Errorf("docker: restore snapshot: %w", err)
		}
	}
	return staged{member: member{hostDir: hostDir}, binds: []string{hostDir + ":" + repoPath}, undo: undo}, nil
}

func (b *Provider) stageVolume(ctx context.Context, containerName, repoPath, containerImage string, restore *snapshotRestore) (staged, error) {
	name := workVolumeName(containerName)
	if _, err := b.cli.VolumeCreate(ctx, volume.CreateOptions{Name: name, Labels: ownedLabels(nil)}); err != nil {
		return staged{}, fmt.Errorf("docker: create workspace volume: %w", err)
	}
	undo := func() { b.removeVolume(context.WithoutCancel(ctx), name) }
	if restore != nil {
		if err := b.cloneVolume(ctx, containerImage, restore.source.volume, name); err != nil {
			undo()
			return staged{}, fmt.Errorf("docker: restore snapshot: %w", err)
		}
	}
	return staged{
		member: member{volume: name},
		// A fresh volume must not be filled from the image at the mount point:
		// the workspace is what Sync sends, and the cache volumes do likewise.
		mounts: []mount.Mount{{
			Type: mount.TypeVolume, Source: name, Target: repoPath,
			VolumeOptions: &mount.VolumeOptions{NoCopy: true, Labels: ownedLabels(nil)},
		}},
		undo: undo,
	}, nil
}

// removeVolume removes a volume, best effort: a leftover is collected by cache
// maintenance once it is old enough.
func (b *Provider) removeVolume(ctx context.Context, name string) {
	if err := b.cli.VolumeRemove(ctx, name, false); err != nil && !cerrdefs.IsNotFound(err) {
		log.Printf("docker: remove volume %s: %v", name, err)
	}
}

// cloneVolume copies the contents of volume from into volume to, attributes
// included, with a short-lived container of image.
func (b *Provider) cloneVolume(ctx context.Context, image, from, to string) error {
	_, err := b.helper(ctx, image, []mount.Mount{
		{Type: mount.TypeVolume, Source: from, Target: "/from", ReadOnly: true},
		{Type: mount.TypeVolume, Source: to, Target: "/to"},
	}, "cp -a /from/. /to/")
	if err != nil {
		return fmt.Errorf("copy volume %s to %s: %w", from, to, err)
	}
	return nil
}

// snapshotVolume clones a member's workspace volume to a snapshot volume that
// records the member's image, replacing an earlier snapshot of the same name.
func (b *Provider) snapshotVolume(ctx context.Context, m member, name string) (string, error) {
	snap := snapshotVolumeName(name)
	if err := b.cli.VolumeRemove(ctx, snap, false); err != nil && !cerrdefs.IsNotFound(err) {
		return "", fmt.Errorf("docker: replace snapshot %s: %w", snap, err)
	}
	if _, err := b.cli.VolumeCreate(ctx, volume.CreateOptions{Name: snap, Labels: ownedLabels(map[string]string{labelImage: m.image})}); err != nil {
		return "", fmt.Errorf("docker: create snapshot volume: %w", err)
	}
	if err := b.cloneVolume(ctx, m.image, m.volume, snap); err != nil {
		b.removeVolume(context.WithoutCancel(ctx), snap)
		return "", fmt.Errorf("docker: snapshot copy: %w", err)
	}
	return snapshotScheme + snap, nil
}

// owner is the uid and gid the image runs as.
type owner struct {
	uid, gid int
	known    bool
}

// parseOwner reads the "uid:gid" a probe printed.
func parseOwner(out string) (owner, error) {
	uid, gid, ok := strings.Cut(strings.TrimSpace(out), ":")
	u, uerr := strconv.Atoi(uid)
	g, gerr := strconv.Atoi(gid)
	if !ok || uerr != nil || gerr != nil || u < 0 || g < 0 {
		return owner{}, fmt.Errorf("unexpected user %q", strings.TrimSpace(out))
	}
	return owner{uid: u, gid: g, known: true}, nil
}

// owner is who the member's commands run as, found once with `id`.
func (b *Provider) owner(ctx context.Context, id string) (owner, error) {
	m, ok := b.member(id)
	if !ok {
		return owner{}, fmt.Errorf("docker: unknown container %s", id)
	}
	if m.owner.known {
		return m.owner, nil
	}
	out, err := b.capture(ctx, id, "", `printf '%s:%s' "$(id -u)" "$(id -g)"`)
	if err != nil {
		return owner{}, fmt.Errorf("docker: find the container user: %w", err)
	}
	o, err := parseOwner(string(out))
	if err != nil {
		return owner{}, fmt.Errorf("docker: find the container user: %w", err)
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if m, ok := b.members[id]; ok {
		m.owner = o
		b.members[id] = m
	}
	return o, nil
}

// ownWorkspace hands the workspace mount point, which a fresh volume keeps for
// root, to the image's user so it can write the files Sync sends.
func (b *Provider) ownWorkspace(ctx context.Context, id, repoPath string) error {
	o, err := b.owner(ctx, id)
	if err != nil {
		return err
	}
	if o.uid == 0 && o.gid == 0 {
		return nil
	}
	if _, err := b.capture(ctx, id, "root", fmt.Sprintf("chown %d:%d %s", o.uid, o.gid, shellQuote(repoPath))); err != nil {
		return fmt.Errorf("docker: hand the workspace to %d:%d: %w", o.uid, o.gid, err)
	}
	return nil
}

// capture runs a short script as user (the image's own when empty) and returns
// its stdout. A failing script's stderr is in the error.
func (b *Provider) capture(ctx context.Context, id, user, script string) ([]byte, error) {
	var stdout, stderr bytes.Buffer
	code, err := b.execAs(ctx, id, user, script, func(stream string, data []byte) {
		if stream == iostream.StreamStdout {
			stdout.Write(data)
			return
		}
		stderr.Write(data)
	})
	if err != nil {
		return nil, err
	}
	if code != 0 {
		return nil, fmt.Errorf("exited %d: %s", code, strings.TrimSpace(stderr.String()))
	}
	return stdout.Bytes(), nil
}

// Upload extracts a tar stream into dir of the container, giving the files to
// the container's user so its commands can change them. The stream is passed
// through, never held whole.
func (b *Provider) Upload(ctx context.Context, id, dir string, content io.Reader) error {
	o, err := b.owner(ctx, id)
	if err != nil {
		return err
	}
	pr, pw := io.Pipe()
	go func() { pw.CloseWithError(chownTar(pw, content, o.uid, o.gid)) }()
	err = b.cli.CopyToContainer(ctx, id, dir, pr, container.CopyToContainerOptions{CopyUIDGID: true})
	// Frees the goroutine above when the copy stopped before reading it all.
	_ = pr.CloseWithError(err)
	if err != nil {
		return fmt.Errorf("docker: upload to %s: %w", dir, err)
	}
	return nil
}

// gitInitStep creates the workspace's repository where git exists and there is
// none yet, so the baseline commit and `git diff HEAD` work.
const gitInitStep = `{ [ -d .git ] || ! command -v git >/dev/null 2>&1 || git init -q; } || exit 1`

// listingScript prints, NUL-separated, the files in repoPath that a mirror
// carries: git's tracked and not-ignored files, or every file but .git when
// there is no git to ask.
func listingScript(repoPath, prelude string) string {
	return "cd " + shellQuote(repoPath) + ` || exit 1
` + prelude + `
if command -v git >/dev/null 2>&1 && git rev-parse --git-dir >/dev/null 2>&1; then
  git ls-files -z --cached --others --exclude-standard
else
  find . -path ./.git -prune -o \( -type f -o -type l \) -print0
fi`
}

// syncVolume sends the tree at cwd into the member's workspace volume as a tar
// stream, then removes the files the volume has that cwd no longer does.
func (b *Provider) syncVolume(ctx context.Context, id, repoPath, cwd string) error {
	files, err := listLocalFiles(ctx, cwd)
	if err != nil {
		return err
	}
	pr, pw := io.Pipe()
	go func() { pw.CloseWithError(writeWorkspaceTar(pw, cwd, files)) }()
	err = b.Upload(ctx, id, repoPath, pr)
	_ = pr.CloseWithError(err)
	if err != nil {
		return err
	}
	// Extraction only adds and overwrites. The listing comes after it, so the
	// repository exists and a file Docker could not write shows up as missing.
	out, err := b.capture(ctx, id, "", listingScript(repoPath, gitInitStep))
	if err != nil {
		return fmt.Errorf("list the workspace: %w", err)
	}
	stale := staleFiles(parseListing(out), files)
	if len(stale) == 0 {
		return nil
	}
	return b.pruneVolume(ctx, id, repoPath, stale)
}

func (b *Provider) pruneVolume(ctx context.Context, id, repoPath string, stale []string) error {
	var stderr bytes.Buffer
	// The stale list can be long, so the script goes on stdin as Exec sends it.
	code, err := b.Exec(ctx, id, pruneScript(repoPath, stale), nil, func(stream string, data []byte) {
		if stream == iostream.StreamStderr {
			stderr.Write(data)
		}
	}, nil)
	if err != nil {
		return fmt.Errorf("remove stale files: %w", err)
	}
	if code != 0 {
		return fmt.Errorf("remove stale files: exited %d: %s", code, strings.TrimSpace(stderr.String()))
	}
	return nil
}

// pullVolume mirrors the member's workspace volume into localDir: it writes the
// files git would carry, and removes local ones the volume no longer has.
func (b *Provider) pullVolume(ctx context.Context, id, repoPath, localDir string) error {
	out, err := b.capture(ctx, id, "", listingScript(repoPath, ""))
	if err != nil {
		return fmt.Errorf("list the workspace: %w", err)
	}
	listed := parseListing(out)
	eligible := make(map[string]bool, len(listed))
	for _, name := range listed {
		eligible[name] = true
	}
	if err := os.MkdirAll(localDir, 0o777); err != nil {
		return err
	}
	tarball, _, err := b.cli.CopyFromContainer(ctx, id, repoPath+"/.")
	if err != nil {
		return fmt.Errorf("copy from %s: %w", repoPath, err)
	}
	defer func() { _ = tarball.Close() }()
	written, err := extractWorkspace(tarball, localDir, eligible)
	if err != nil {
		return err
	}
	// Names that match nothing mean the archive is not laid out as expected.
	// Deleting every local file on that evidence would be the worst answer.
	if len(written) == 0 && len(eligible) > 0 {
		return errors.New("the workspace archive held none of the workspace's files")
	}
	local, err := listLocalFiles(ctx, localDir)
	if err != nil {
		return err
	}
	return removeMissing(localDir, local, written)
}

// orphanedVolume tells whether a workspace or snapshot volume is old enough to
// collect when nothing uses it. An unreadable creation time is not old.
func orphanedVolume(v *volume.Volume, now time.Time) bool {
	if v == nil || v.Labels[label] != "1" {
		return false
	}
	if !strings.HasPrefix(v.Name, workVolumePrefix) && !strings.HasPrefix(v.Name, snapVolumePrefix) {
		return false
	}
	created, err := time.Parse(time.RFC3339, v.CreatedAt)
	return err == nil && now.Sub(created) >= orphanAge
}

// pruneOrphanWorkspaces removes the workspace and snapshot volumes a crashed
// run left, which no container uses. Best effort: it never fails a run.
func (b *Provider) pruneOrphanWorkspaces(ctx context.Context) {
	volumes, err := b.listWorkspaceVolumes(ctx)
	if err != nil {
		log.Print(err)
		return
	}
	for _, v := range volumes {
		if !orphanedVolume(v, b.clock()) {
			continue
		}
		users, err := b.volumeUsers(ctx, v.Name)
		if err != nil || users > 0 {
			continue
		}
		b.removeVolume(ctx, v.Name)
	}
}

// listWorkspaceVolumes lists every workspace and snapshot volume.
func (b *Provider) listWorkspaceVolumes(ctx context.Context) ([]*volume.Volume, error) {
	res, err := b.cli.VolumeList(ctx, volume.ListOptions{Filters: filters.NewArgs(
		filters.Arg("name", workVolumePrefix), filters.Arg("name", snapVolumePrefix))})
	if err != nil {
		return nil, fmt.Errorf("docker: list workspace volumes: %w", err)
	}
	var out []*volume.Volume
	for _, v := range res.Volumes {
		if v != nil && v.Labels[label] == "1" && (strings.HasPrefix(v.Name, workVolumePrefix) || strings.HasPrefix(v.Name, snapVolumePrefix)) {
			out = append(out, v)
		}
	}
	return out, nil
}
