package docker

import (
	"context"
	"encoding/base64"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"gotest.tools/v3/assert"

	"github.com/smeltdev/smelt/internal/iostream"
)

// testImage is small and ships a POSIX sh, ls and cat, which is all these
// tests exec inside the container. The transport (rsync, git init) runs on the
// host, so the image needs no git or rsync of its own.
const testImage = "alpine:3.20"

const repoPath = "/home/user/repo"

func noStatus(iostream.Level, string) {}

// newLiveBackend connects to the local Docker daemon, skipping the test when
// one is not reachable so the suite stays green on machines without Docker. Its
// workspaces live under a temp dir and it runs the small test image.
func newLiveBackend(t *testing.T) *Provider {
	t.Helper()
	b, err := New()
	if err != nil {
		t.Skipf("no docker client: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := b.cli.Ping(ctx); err != nil {
		t.Skipf("docker daemon not reachable: %v", err)
	}
	b.baseImage = testImage
	b.workRoot = t.TempDir()
	return b
}

func TestExecStreamsOutputAndExitCode(t *testing.T) {
	b := newLiveBackend(t)
	inst, err := b.Create(context.Background(), "smelt-test-exec", "", repoPath)
	assert.NilError(t, err)
	defer func() { _ = b.Delete(context.Background(), inst.ID) }()

	var stdout, stderr strings.Builder
	onOutput := func(stream string, data []byte) {
		switch stream {
		case iostream.StreamStdout:
			stdout.Write(data)
		case iostream.StreamStderr:
			stderr.Write(data)
		}
	}
	var submitted string
	code, err := b.Exec(context.Background(), inst.ID,
		"echo out-line; echo err-line 1>&2; exit 3",
		nil, onOutput, func(s string) { submitted = s })
	assert.NilError(t, err)
	assert.Equal(t, code, 3)
	assert.Equal(t, strings.TrimSpace(stdout.String()), "out-line")
	assert.Equal(t, strings.TrimSpace(stderr.String()), "err-line")
	assert.Assert(t, submitted != "")
}

func TestExecPassesEnv(t *testing.T) {
	b := newLiveBackend(t)
	inst, err := b.Create(context.Background(), "smelt-test-env", "", repoPath)
	assert.NilError(t, err)
	defer func() { _ = b.Delete(context.Background(), inst.ID) }()

	var stdout strings.Builder
	code, err := b.Exec(context.Background(), inst.ID, `printf '%s' "$SMELT_TEST_VAR"`,
		map[string]string{"SMELT_TEST_VAR": "hello-env"},
		func(stream string, data []byte) {
			if stream == iostream.StreamStdout {
				stdout.Write(data)
			}
		}, nil)
	assert.NilError(t, err)
	assert.Equal(t, code, 0)
	assert.Equal(t, stdout.String(), "hello-env")
}

// liveModes are the workspace modes the live tests run in. The bind mode needs
// rsync on the host; the volume mode needs nothing but the daemon.
var liveModes = []WorkspaceMode{WorkspaceBind, WorkspaceVolume}

// forEachLiveMode runs fn against a live backend in each workspace mode.
func forEachLiveMode(t *testing.T, fn func(t *testing.T, b *Provider)) {
	for _, mode := range liveModes {
		t.Run(string(mode), func(t *testing.T) {
			b := newLiveBackend(t)
			if mode == WorkspaceBind {
				if _, err := exec.LookPath("rsync"); err != nil {
					t.Skip("rsync is not installed")
				}
			}
			b.Workspace = mode
			fn(t, b)
		})
	}
}

// execOutput runs script in the container and returns its stdout.
func execOutput(t *testing.T, b *Provider, id, script string) string {
	t.Helper()
	var out strings.Builder
	code, err := b.Exec(context.Background(), id, script, nil, func(stream string, data []byte) {
		if stream == iostream.StreamStdout {
			out.Write(data)
		}
	}, nil)
	assert.NilError(t, err)
	assert.Equal(t, code, 0, "%s: %s", script, out.String())
	return out.String()
}

// TestSyncSnapshotCloneAndPull exercises the whole provisioning + transport
// path the pool drives: sync a tree in, snapshot it, create a clone from the
// snapshot, confirm the clone has the files, then pull a container's workspace
// back out.
func TestSyncSnapshotCloneAndPull(t *testing.T) {
	forEachLiveMode(t, func(t *testing.T, b *Provider) {
		ctx := context.Background()

		// A local source tree with a tracked file and a gitignored one.
		srcTree := t.TempDir()
		write(t, filepath.Join(srcTree, "main.go"), "package main\n")
		write(t, filepath.Join(srcTree, ".gitignore"), "ignored.txt\n")
		write(t, filepath.Join(srcTree, "ignored.txt"), "do not sync\n")

		// The seed runs an explicit image while the provider's default is bogus:
		// a clone must run the seed's image (recorded in the snapshot), and would
		// fail to pull the default if it fell back to it.
		b.baseImage = "smelt-test/does-not-exist:never"
		seed, err := b.Create(ctx, "smelt-test-seed", testImage, repoPath)
		assert.NilError(t, err)
		defer func() { _ = b.Delete(ctx, seed.ID) }()

		assert.NilError(t, b.Sync(ctx, seed.ID, repoPath, srcTree, true, noStatus))

		// The container sees the tracked file at repoPath; the ignored one did not
		// sync.
		assert.Equal(t, execOutput(t, b, seed.ID, "cat "+repoPath+"/main.go"), "package main\n")
		assert.Equal(t, execOutput(t, b, seed.ID, "ls -A "+repoPath+" | grep -c '^ignored.txt$' || true"), "0\n")

		// Snapshot the seed and clone from it: the clone's workspace has the files.
		snap, err := b.Snapshot(ctx, seed.ID, "smelt-test-snap")
		assert.NilError(t, err)
		assert.Assert(t, strings.HasPrefix(snap, snapshotScheme))
		defer func() { removeSnapshot(t, b, snap) }()

		clone, err := b.Create(ctx, "smelt-test-clone", snap, repoPath)
		assert.NilError(t, err)
		defer func() { _ = b.Delete(ctx, clone.ID) }()
		assert.Equal(t, execOutput(t, b, clone.ID, "cat "+repoPath+"/main.go"), "package main\n")

		// Pull the seed's workspace back out: the tracked file comes, .git does not.
		dest := t.TempDir()
		assert.NilError(t, b.Pull(ctx, seed.ID, repoPath, dest, noStatus))
		assert.Equal(t, readFile(t, filepath.Join(dest, "main.go")), "package main\n")
		assert.Assert(t, !exists(filepath.Join(dest, ".git")), "pull must not bring back .git")
	})
}

// removeSnapshot deletes what a snapshot ref names: a directory or a volume.
func removeSnapshot(t *testing.T, b *Provider, ref string) {
	t.Helper()
	src, err := parseSnapshotRef(ref, b.Workspace)
	if err != nil {
		t.Errorf("snapshot ref: %v", err)
		return
	}
	if src.volume != "" {
		b.removeVolume(context.Background(), src.volume)
		return
	}
	_ = os.RemoveAll(src.dir)
}

// TestCreateSameNameTwice mirrors the pool, which names the seed and the first
// clone from its snapshot alike: both must come up, with their own workspaces.
func TestCreateSameNameTwice(t *testing.T) {
	forEachLiveMode(t, func(t *testing.T, b *Provider) {
		ctx := context.Background()
		first, err := b.Create(ctx, "smelt-test-dup", "", repoPath)
		assert.NilError(t, err)
		defer func() { _ = b.Delete(ctx, first.ID) }()
		second, err := b.Create(ctx, "smelt-test-dup", "", repoPath)
		assert.NilError(t, err)
		defer func() { _ = b.Delete(ctx, second.ID) }()
		assert.Assert(t, first.ID != second.ID)
		execOutput(t, b, first.ID, "touch "+repoPath+"/only-first")
		assert.Equal(t, execOutput(t, b, second.ID, "ls "+repoPath), "", "each member needs its own workspace")
	})
}

func TestDeleteRemovesWorkspace(t *testing.T) {
	forEachLiveMode(t, func(t *testing.T, b *Provider) {
		ctx := context.Background()
		inst, err := b.Create(ctx, "smelt-test-del", "", repoPath)
		assert.NilError(t, err)
		m, _ := b.member(inst.ID)
		assert.NilError(t, b.Delete(ctx, inst.ID))
		if m.volume != "" {
			_, err := b.cli.VolumeInspect(ctx, m.volume)
			assert.Assert(t, err != nil, "Delete should remove the workspace volume")
			return
		}
		assert.Assert(t, !exists(m.hostDir), "Delete should remove the host workspace")
	})
}

func write(t *testing.T, path, content string) {
	t.Helper()
	assert.NilError(t, os.MkdirAll(filepath.Dir(path), 0o777))
	assert.NilError(t, os.WriteFile(path, []byte(content), 0o644))
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	assert.NilError(t, err)
	return string(data)
}

func exists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// execCancelled runs script in a new container, cancels it once the script has
// recorded its process group, and returns how long Exec took to return and what
// `ps` shows afterwards.
func execCancelled(t *testing.T, b *Provider, script string) (time.Duration, string) {
	t.Helper()
	inst, err := b.Create(context.Background(), "smelt-test-cancel", "", repoPath)
	assert.NilError(t, err)
	t.Cleanup(func() { _ = b.Delete(context.Background(), inst.ID) })

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	type result struct{ err error }
	done := make(chan result, 1)
	go func() {
		_, err := b.Exec(ctx, inst.ID, script, nil, nil, nil)
		done <- result{err}
	}()
	time.Sleep(2 * time.Second) // let the command start and record its group
	stopped := time.Now()
	cancel()
	select {
	case r := <-done:
		assert.Assert(t, errors.Is(r.err, context.Canceled), "%v", r.err)
		took := time.Since(stopped)
		var ps strings.Builder
		_, err := b.Exec(context.Background(), inst.ID, "ps", nil, func(_ string, d []byte) { ps.Write(d) }, nil)
		assert.NilError(t, err)
		return took, ps.String()
	case <-time.After(60 * time.Second):
		t.Fatal("Exec did not return after the cancel")
	}
	return 0, ""
}

func TestExecCancelKillsTheProcessGroup(t *testing.T) {
	b := newLiveBackend(t)
	b.StopGrace = 5 * time.Second
	took, ps := execCancelled(t, b, "sleep 600 & wait")
	assert.Assert(t, took < b.StopGrace+2*time.Second, "took %s", took)
	assert.Assert(t, !strings.Contains(ps, "sleep 600"), ps)
}

func TestExecCancelKillsAfterTheGraceWhenTermIsIgnored(t *testing.T) {
	b := newLiveBackend(t)
	b.StopGrace = time.Second
	took, ps := execCancelled(t, b, "trap '' TERM; sleep 600 & wait")
	assert.Assert(t, took >= b.StopGrace, "took %s", took)
	assert.Assert(t, took < b.StopGrace+2*time.Second, "took %s", took)
	assert.Assert(t, !strings.Contains(ps, "sleep 600"), ps)
}

func TestGroupedCommandKeepsTheScript(t *testing.T) {
	cmd := groupedCommand(filepath.Join(t.TempDir(), "pgid"))
	if _, err := exec.LookPath("setsid"); err != nil {
		t.Skip("no setsid on the host")
	}
	c := exec.Command(cmd[0], cmd[1:]...)
	c.Stdin = strings.NewReader("exit 3")
	err := c.Run()
	var ee *exec.ExitError
	assert.Assert(t, errors.As(err, &ee) && ee.ExitCode() == 3, "%v", err)
}

func TestGroupedCommandRunsALargeScriptWithoutStdin(t *testing.T) {
	cmd := groupedCommand(filepath.Join(t.TempDir(), "pgid"))
	script := "cat | wc -c; echo '" + strings.Repeat("x", 1<<20) + "' | wc -c"
	c := exec.Command(cmd[0], cmd[1:]...)
	c.Stdin = strings.NewReader(script)
	out, err := c.Output()
	assert.NilError(t, err)
	// The script's own stdin is /dev/null, so it never sees its own text.
	assert.Equal(t, strings.Fields(string(out))[0], "0")
}

func TestExecArgvIsIndependentOfTheScript(t *testing.T) {
	big := strings.Repeat("x", 1<<20)
	f := &fakeDocker{}
	b := &Provider{cli: f}
	_, err := b.Exec(context.Background(), "c", big, nil, func(string, []byte) {}, nil)
	assert.NilError(t, err)
	assert.Equal(t, len(f.execs), 1)
	total := 0
	for _, a := range f.execs[0].Cmd {
		total += len(a)
	}
	assert.Assert(t, total < 4096, "argv is %d bytes", total)
	assert.Assert(t, f.execs[0].AttachStdin)
}

func TestRunRefusesAScriptInArgv(t *testing.T) {
	b := &Provider{cli: &fakeDocker{}}
	_, err := b.execAs(context.Background(), "c", "", strings.Repeat("x", 200*1024), nil)
	assert.ErrorContains(t, err, "internal: script passed as an argument (204800 bytes)")
}

func TestExecPipesALargeScriptToTheContainer(t *testing.T) {
	b := newLiveBackend(t)
	inst, err := b.Create(context.Background(), "smelt-test-bigscript", "", repoPath)
	assert.NilError(t, err)
	defer func() { _ = b.Delete(context.Background(), inst.ID) }()

	prompt := base64.StdEncoding.EncodeToString([]byte(strings.Repeat("p", 1<<20)))
	var stdout strings.Builder
	code, err := b.Exec(context.Background(), inst.ID,
		"echo "+prompt+" | base64 -d | wc -c", nil,
		func(stream string, data []byte) {
			if stream == iostream.StreamStdout {
				stdout.Write(data)
			}
		}, nil)
	assert.NilError(t, err)
	assert.Equal(t, code, 0)
	assert.Equal(t, strings.TrimSpace(stdout.String()), "1048576")
}

// TestLiveVolumeWorkspaceRunsGoTest syncs this repository into a volume
// workspace and runs its tests there. It needs a daemon that can run a Go image
// (golang:1.26) and asserts no mount of the
// container is a bind mount.
func TestLiveVolumeWorkspaceRunsGoTest(t *testing.T) {
	if testing.Short() {
		t.Skip("slow")
	}
	b := newLiveBackend(t)
	b.Workspace = WorkspaceVolume
	const image = "golang:1.26"
	ctx := context.Background()
	if _, err := b.cli.ImageInspect(ctx, image); err != nil {
		t.Skipf("image %s not present: %v", image, err)
	}
	root, err := filepath.Abs("../../..")
	assert.NilError(t, err)

	inst, err := b.Create(ctx, "smelt-test-livego", image, repoPath)
	assert.NilError(t, err)
	defer func() { _ = b.Delete(ctx, inst.ID) }()
	assert.NilError(t, b.Sync(ctx, inst.ID, repoPath, root, true, noStatus))

	insp, err := b.cli.ContainerInspect(ctx, inst.ID)
	assert.NilError(t, err)
	for _, m := range insp.Mounts {
		assert.Assert(t, m.Type != "bind", "unexpected bind mount %s -> %s", m.Source, m.Destination)
	}
	execOutput(t, b, inst.ID, "cd "+repoPath+" && go test ./internal/brand/...")
}
