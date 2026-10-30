package docker

import (
	"archive/tar"
	"bytes"
	"context"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	cerrdefs "github.com/containerd/errdefs"
	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/mount"
	"github.com/docker/docker/api/types/volume"
	"gotest.tools/v3/assert"
)

func (f *fakeDocker) VolumeCreate(_ context.Context, opts volume.CreateOptions) (volume.Volume, error) {
	if f.made == nil {
		f.made = map[string]volume.CreateOptions{}
	}
	f.made[opts.Name] = opts
	return volume.Volume{Name: opts.Name, Labels: opts.Labels}, nil
}

func (f *fakeDocker) VolumeInspect(_ context.Context, name string) (volume.Volume, error) {
	opts, ok := f.made[name]
	if !ok {
		return volume.Volume{}, cerrdefs.ErrNotFound
	}
	return volume.Volume{Name: name, Labels: opts.Labels}, nil
}

func (f *fakeDocker) CopyToContainer(_ context.Context, _, dst string, content io.Reader, _ container.CopyToContainerOptions) error {
	data, err := io.ReadAll(content)
	if f.uploads == nil {
		f.uploads = map[string][]byte{}
	}
	f.uploads[dst] = data
	return err
}

func (f *fakeDocker) CopyFromContainer(context.Context, string, string) (io.ReadCloser, container.PathStat, error) {
	return io.NopCloser(bytes.NewReader(f.download)), container.PathStat{}, nil
}

// madeWith finds the one created volume whose name starts with prefix; a
// workspace volume's name carries a suffix that keeps same-named members apart.
func madeWith(t *testing.T, f *fakeDocker, prefix string) (string, volume.CreateOptions) {
	t.Helper()
	for name, opts := range f.made {
		if strings.HasPrefix(name, prefix) {
			return name, opts
		}
	}
	t.Fatalf("no volume named %s*: %v", prefix, f.made)
	return "", volume.CreateOptions{}
}

func TestDecideWorkspace(t *testing.T) {
	tests := []struct {
		name       string
		configured WorkspaceMode
		os         string
		want       WorkspaceMode
	}{
		{"desktop defaults to a volume", "", "Docker Desktop", WorkspaceVolume},
		{"linux defaults to a bind", "", "Ubuntu 24.04", WorkspaceBind},
		{"unknown defaults to a bind", "", "", WorkspaceBind},
		{"bind overrides desktop", WorkspaceBind, "Docker Desktop", WorkspaceBind},
		{"volume overrides linux", WorkspaceVolume, "Ubuntu 24.04", WorkspaceVolume},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, reason := decideWorkspace(tt.configured, tt.os)
			assert.Equal(t, got, tt.want)
			assert.Assert(t, reason != "")
		})
	}
}

func TestParseWorkspaceMode(t *testing.T) {
	for _, ok := range []string{"", "volume", "bind"} {
		_, err := ParseWorkspaceMode(ok)
		assert.NilError(t, err)
	}
	_, err := ParseWorkspaceMode("nfs")
	assert.ErrorContains(t, err, `"nfs"`)
}

func TestParseSnapshotRef(t *testing.T) {
	tests := []struct {
		name    string
		ref     string
		mode    WorkspaceMode
		volume  string
		dir     string
		wantErr bool
	}{
		{"volume", snapshotScheme + "smelt-snap-a", WorkspaceVolume, "smelt-snap-a", "", false},
		{"bind", snapshotScheme + "/tmp/snaps/a", WorkspaceBind, "", "/tmp/snaps/a", false},
		{"directory ref in volume mode", snapshotScheme + "/tmp/snaps/a", WorkspaceVolume, "", "", true},
		{"volume ref in bind mode", snapshotScheme + "smelt-snap-a", WorkspaceBind, "", "", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := parseSnapshotRef(tt.ref, tt.mode)
			if tt.wantErr {
				assert.Assert(t, err != nil)
				return
			}
			assert.NilError(t, err)
			assert.Equal(t, got.volume, tt.volume)
			assert.Equal(t, got.dir, tt.dir)
		})
	}
}

func TestParseOwner(t *testing.T) {
	o, err := parseOwner("1001:1002\n")
	assert.NilError(t, err)
	assert.Equal(t, o.uid, 1001)
	assert.Equal(t, o.gid, 1002)
	for _, bad := range []string{"", "1001", "a:b", "-1:0"} {
		_, err := parseOwner(bad)
		assert.Assert(t, err != nil, bad)
	}
}

func TestNormalizeEntry(t *testing.T) {
	tests := []struct {
		in   string
		want string
		ok   bool
	}{
		{"a/b.go", "a/b.go", true},
		{"./a/b.go", "a/b.go", true},
		{"a//b", "a/b", true},
		{"", "", false},
		{".", "", false},
		{"/etc/passwd", "", false},
		{"../x", "", false},
		{"a/../../x", "", false},
	}
	for _, tt := range tests {
		got, ok := normalizeEntry(tt.in)
		assert.Equal(t, ok, tt.ok, tt.in)
		assert.Equal(t, got, tt.want, tt.in)
	}
}

func TestParseListingDropsDirectories(t *testing.T) {
	got := parseListing([]byte("a.go\x00nested/\x00dir/b.go\x00../escape\x00"))
	assert.DeepEqual(t, got, []string{"a.go", "dir/b.go"})
}

func TestStaleFilesAndPruneScript(t *testing.T) {
	stale := staleFiles([]string{"keep.go", "old.go", "dir/sub/gone.go", "dir/kept.go"}, []string{"keep.go", "dir/kept.go"})
	assert.DeepEqual(t, stale, []string{"old.go", "dir/sub/gone.go"})
	script := pruneScript("/repo", stale)
	assert.Assert(t, strings.HasPrefix(script, "cd '/repo' || exit 1\n"), script)
	assert.Assert(t, strings.Contains(script, "rm -f -- 'old.go'\n"), script)
	// Deepest directory first, so a parent is empty by the time it is tried.
	assert.Assert(t, strings.Index(script, "rmdir -- 'dir/sub'") < strings.Index(script, "rmdir -- 'dir'"), script)
	assert.Assert(t, strings.Contains(pruneScript("/repo", []string{"it's.go"}), `'it'\''s.go'`))
}

func TestOrphanedVolume(t *testing.T) {
	now := time.Date(2026, 10, 29, 12, 0, 0, 0, time.UTC)
	owned := map[string]string{label: "1"}
	old := now.Add(-25 * time.Hour).Format(time.RFC3339)
	fresh := now.Add(-time.Hour).Format(time.RFC3339)
	tests := []struct {
		name string
		vol  *volume.Volume
		want bool
	}{
		{"old workspace", &volume.Volume{Name: workVolumePrefix + "a", Labels: owned, CreatedAt: old}, true},
		{"old snapshot", &volume.Volume{Name: snapVolumePrefix + "a", Labels: owned, CreatedAt: old}, true},
		{"fresh workspace", &volume.Volume{Name: workVolumePrefix + "a", Labels: owned, CreatedAt: fresh}, false},
		{"unlabelled", &volume.Volume{Name: workVolumePrefix + "a", CreatedAt: old}, false},
		{"other name", &volume.Volume{Name: "data", Labels: owned, CreatedAt: old}, false},
		{"unreadable time", &volume.Volume{Name: workVolumePrefix + "a", Labels: owned, CreatedAt: "?"}, false},
		{"nil", nil, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, orphanedVolume(tt.vol, now), tt.want)
		})
	}
}

func gitOnPath(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not installed")
	}
}

func TestListLocalFilesHonorsGitignore(t *testing.T) {
	gitOnPath(t)
	dir := t.TempDir()
	write(t, filepath.Join(dir, ".gitignore"), "ignored.txt\nbuild/\n")
	write(t, filepath.Join(dir, "main.go"), "package main\n")
	write(t, filepath.Join(dir, "ignored.txt"), "x")
	write(t, filepath.Join(dir, "build", "out.bin"), "x")
	write(t, filepath.Join(dir, "pkg", "a.go"), "package pkg\n")
	write(t, filepath.Join(dir, ".git", "HEAD"), "ref: refs/heads/main\n")
	write(t, filepath.Join(dir, "nested", ".git", "HEAD"), "ref: refs/heads/main\n")
	write(t, filepath.Join(dir, "nested", "n.go"), "package nested\n")
	files, err := listLocalFiles(context.Background(), dir)
	assert.NilError(t, err)
	for _, name := range files {
		assert.Assert(t, !strings.HasPrefix(name, ".git/"), name)
	}
	assert.Assert(t, contains(files, ".gitignore", "main.go", "pkg/a.go", "nested/n.go"), files)
	assert.Assert(t, !contains(files, "ignored.txt") && !contains(files, "build/out.bin"), files)
}

func contains(have []string, want ...string) bool {
	for _, w := range want {
		found := false
		for _, h := range have {
			found = found || h == w
		}
		if !found {
			return false
		}
	}
	return true
}

func TestWorkspaceTarRoundTrip(t *testing.T) {
	src := t.TempDir()
	write(t, filepath.Join(src, "main.go"), "package main\n")
	write(t, filepath.Join(src, "pkg", "deep", "a.go"), "package deep\n")
	assert.NilError(t, os.Chmod(filepath.Join(src, "main.go"), 0o755))
	assert.NilError(t, os.Symlink("main.go", filepath.Join(src, "link")))
	files := []string{"link", "main.go", "pkg/deep/a.go", "vanished.go"}

	var buf bytes.Buffer
	assert.NilError(t, writeWorkspaceTar(&buf, src, files))
	// A packed stream hands every file to root, for Upload to reassign.
	tr := tar.NewReader(bytes.NewReader(buf.Bytes()))
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		assert.NilError(t, err)
		assert.Equal(t, hdr.Uid, 0)
		assert.Assert(t, hdr.Name != "vanished.go")
	}

	var chowned bytes.Buffer
	assert.NilError(t, chownTar(&chowned, bytes.NewReader(buf.Bytes()), 1001, 1002))
	tr = tar.NewReader(&chowned)
	hdr, err := tr.Next()
	assert.NilError(t, err)
	assert.Equal(t, hdr.Uid, 1001)
	assert.Equal(t, hdr.Gid, 1002)

	dst := t.TempDir()
	eligible := map[string]bool{"link": true, "main.go": true, "pkg/deep/a.go": true}
	written, err := extractWorkspace(bytes.NewReader(buf.Bytes()), dst, eligible)
	assert.NilError(t, err)
	assert.Equal(t, len(written), 3)
	assert.Equal(t, readFile(t, filepath.Join(dst, "pkg", "deep", "a.go")), "package deep\n")
	info, err := os.Stat(filepath.Join(dst, "main.go"))
	assert.NilError(t, err)
	assert.Equal(t, info.Mode().Perm(), os.FileMode(0o755))
	target, err := os.Readlink(filepath.Join(dst, "link"))
	assert.NilError(t, err)
	assert.Equal(t, target, "main.go")
}

func tarBytes(t *testing.T, entries ...tar.Header) []byte {
	t.Helper()
	data, err := io.ReadAll(tarOf(t, entries...))
	assert.NilError(t, err)
	return data
}

func tarOf(t *testing.T, entries ...tar.Header) io.Reader {
	t.Helper()
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	for _, hdr := range entries {
		hdr.Size = int64(len(hdr.Linkname))
		if hdr.Typeflag == tar.TypeReg {
			hdr.Size = 1
		}
		assert.NilError(t, tw.WriteHeader(&hdr))
		if hdr.Typeflag == tar.TypeReg {
			_, err := tw.Write([]byte("x"))
			assert.NilError(t, err)
		}
	}
	assert.NilError(t, tw.Close())
	return &buf
}

func TestExtractWorkspaceCannotEscape(t *testing.T) {
	parent := t.TempDir()
	dst := filepath.Join(parent, "dst")
	assert.NilError(t, os.Mkdir(dst, 0o755))
	// An entry named outside the root is skipped; one written through a symlink
	// that points outside is refused.
	stream := tarOf(t,
		tar.Header{Name: "../evil", Typeflag: tar.TypeReg, Mode: 0o644},
		tar.Header{Name: "esc", Typeflag: tar.TypeSymlink, Linkname: parent, Mode: 0o777},
		tar.Header{Name: "esc/pwned", Typeflag: tar.TypeReg, Mode: 0o644},
	)
	_, err := extractWorkspace(stream, dst, map[string]bool{"esc": true, "esc/pwned": true, "../evil": true})
	assert.Assert(t, err != nil, "writing through an outward symlink must fail")
	assert.Assert(t, !exists(filepath.Join(parent, "evil")))
	assert.Assert(t, !exists(filepath.Join(parent, "pwned")))
}

func TestRemoveMissingKeepsWhatWasWritten(t *testing.T) {
	dir := t.TempDir()
	write(t, filepath.Join(dir, "keep.go"), "k")
	write(t, filepath.Join(dir, "sub", "gone.go"), "g")
	assert.NilError(t, removeMissing(dir, []string{"keep.go", "sub/gone.go"}, map[string]bool{"keep.go": true}))
	assert.Assert(t, exists(filepath.Join(dir, "keep.go")))
	assert.Assert(t, !exists(filepath.Join(dir, "sub")), "the emptied directory goes too")
}

// volumeProvider is a provider on a fake engine that reports Docker Desktop, so
// the volume workspace is the default, and whose containers run as 1001:1001.
func volumeProvider(t *testing.T) (*Provider, *fakeDocker) {
	t.Helper()
	f := observedState()
	f.operatingSystem = "Docker Desktop"
	f.execOut = func(opts container.ExecOptions) string {
		if strings.Contains(strings.Join(opts.Cmd, " "), `id -u`) {
			return "1001:1001"
		}
		return ""
	}
	p := f.provider()
	p.workRoot = t.TempDir()
	return p, f
}

func TestCreateVolumeWorkspace(t *testing.T) {
	p, f := volumeProvider(t)
	ctx := context.Background()
	inst, err := p.Create(ctx, "seed", "", "/repo")
	assert.NilError(t, err)
	assert.Equal(t, inst.RepoPath, "/repo")

	name, opts := madeWith(t, f, workVolumePrefix+"seed")
	assert.Equal(t, opts.Labels[label], "1")

	host := f.hostConfigs[len(f.hostConfigs)-1]
	assert.Equal(t, len(host.Binds), 0, "no bind mount of the workspace")
	var found bool
	for _, m := range host.Mounts {
		if m.Target == "/repo" {
			found = true
			assert.Equal(t, m.Type, mount.TypeVolume)
			assert.Equal(t, m.Source, name)
			assert.Assert(t, m.VolumeOptions != nil && m.VolumeOptions.NoCopy)
		}
	}
	assert.Assert(t, found, "the volume is mounted at the repo path: %+v", host.Mounts)
	entries, err := os.ReadDir(p.workRoot)
	assert.NilError(t, err)
	for _, e := range entries {
		assert.Assert(t, e.Name() == "snapshots" || !strings.Contains(e.Name(), "seed"), "no workspace dir under workRoot: %s", e.Name())
	}

	var chowned bool
	for _, e := range f.execs {
		chowned = chowned || (e.User == "root" && strings.Contains(strings.Join(e.Cmd, " "), "chown 1001:1001"))
	}
	assert.Assert(t, chowned, "the mount point goes to the image's user: %+v", f.execs)

	assert.NilError(t, p.Delete(ctx, inst.ID))
	assert.DeepEqual(t, f.removedVolumes, []string{name})
}

func TestSnapshotAndCloneVolumeWorkspace(t *testing.T) {
	p, f := volumeProvider(t)
	ctx := context.Background()
	inst, err := p.Create(ctx, "seed", "", "/repo")
	assert.NilError(t, err)
	ref, err := p.Snapshot(ctx, inst.ID, "snap")
	assert.NilError(t, err)
	assert.Equal(t, ref, snapshotScheme+snapVolumePrefix+"snap")
	assert.Equal(t, f.made[snapVolumePrefix+"snap"].Labels[labelImage], observedID)

	_, err = p.Create(ctx, "clone", ref, "/repo")
	assert.NilError(t, err)
	assert.DeepEqual(t, f.created, []string{observedID, observedID})
	madeWith(t, f, workVolumePrefix+"clone")

	_, err = p.Create(ctx, "other", snapshotScheme+"/old/dir/snapshot", "/repo")
	assert.ErrorContains(t, err, "snapshot")
}

func TestSyncVolumeWorkspaceUploadsAndPrunes(t *testing.T) {
	gitOnPath(t)
	p, f := volumeProvider(t)
	ctx := context.Background()
	inst, err := p.Create(ctx, "seed", "", "/repo")
	assert.NilError(t, err)
	f.execOut = func(opts container.ExecOptions) string {
		script := strings.Join(opts.Cmd, " ")
		switch {
		case strings.Contains(script, "id -u"):
			return "1001:1001"
		case strings.Contains(script, "ls-files"):
			return "main.go\x00stale.go\x00"
		}
		return ""
	}
	src := t.TempDir()
	write(t, filepath.Join(src, "main.go"), "package main\n")
	assert.NilError(t, p.Sync(ctx, inst.ID, "/repo", src, false, noStatus))

	tr := tar.NewReader(bytes.NewReader(f.uploads["/repo"]))
	hdr, err := tr.Next()
	assert.NilError(t, err)
	assert.Equal(t, hdr.Name, "main.go")
	assert.Equal(t, hdr.Uid, 1001, "the upload is owned by the image's user")

	// The listing creates the repository, and the prune runs after the upload
	// and removes what was deleted locally.
	var inits bool
	for _, e := range f.execs {
		if script := strings.Join(e.Cmd, " "); strings.Contains(script, "ls-files") && strings.Contains(script, "git init") {
			inits = true
		}
	}
	assert.Assert(t, inits, "Sync runs git init: %+v", f.execs)
	var pruned bool
	for _, script := range f.sentScripts() {
		pruned = pruned || strings.Contains(script, "rm -f -- 'stale.go'")
	}
	assert.Assert(t, pruned, "Sync removes the file deleted locally: %q", f.sentScripts())
}

func TestPullVolumeWorkspace(t *testing.T) {
	p, f := volumeProvider(t)
	ctx := context.Background()
	inst, err := p.Create(ctx, "seed", "", "/repo")
	assert.NilError(t, err)
	f.execOut = func(opts container.ExecOptions) string {
		if strings.Contains(strings.Join(opts.Cmd, " "), "ls-files") {
			return "main.go\x00"
		}
		return ""
	}
	dest := t.TempDir()
	write(t, filepath.Join(dest, "precious.go"), "keep me")
	// Entries named unlike the listing say the archive is not laid out as
	// expected; deleting the local files on that evidence would be wrong.
	f.download = tarBytes(t, tar.Header{Name: "elsewhere/main.go", Typeflag: tar.TypeReg, Mode: 0o644})
	err = p.Pull(ctx, inst.ID, "/repo", dest, noStatus)
	assert.ErrorContains(t, err, "none of the workspace's files")
	assert.Equal(t, readFile(t, filepath.Join(dest, "precious.go")), "keep me")

	f.download = tarBytes(t, tar.Header{Name: "main.go", Typeflag: tar.TypeReg, Mode: 0o644})
	assert.NilError(t, p.Pull(ctx, inst.ID, "/repo", dest, noStatus))
	assert.Equal(t, readFile(t, filepath.Join(dest, "main.go")), "x")
}
