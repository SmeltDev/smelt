// Package docker implements sidecar.Backend on local Docker containers, so a
// factory run needs no CircleCI cloud sidecars.
//
// A member's workspace is a host directory bind-mounted into the container at
// its repoPath, so Sync and Pull are plain local rsync between host directories
// — no SSH tunnel. Because Docker's ContainerCommit does not capture bind-mount
// contents, Snapshot copies the seed's workspace directory aside and clones
// restore it, rather than committing an image.
package docker

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	cerrdefs "github.com/containerd/errdefs"
	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/client"
	"github.com/docker/docker/pkg/stdcopy"

	"github.com/smeltdev/smelt/internal/brand"
	"github.com/smeltdev/smelt/internal/iostream"
	"github.com/smeltdev/smelt/internal/sidecar"
)

// defaultBaseImage is the image a member runs when none is configured. The real
// factory image also carries git, the agent CLI, and the project's toolchain;
// this default at least provides a shell and git on most tags.
const defaultBaseImage = "debian:bookworm-slim"

// snapshotScheme marks a Snapshot ref as a smelt host-directory snapshot rather
// than a Docker image, so Create knows to restore from a directory.
const snapshotScheme = brand.Command + "fs:"

// EnvImage names the environment variable that overrides the provider's default
// image. `smelt factory build` reads it too, to know an image is configured.
const EnvImage = brand.EnvPrefix + "DOCKER_IMAGE"

// label marks every container smelt creates, so they can be told apart from the
// user's own and reaped.
const label = brand.LabelPrefix + "factory"

// labelName carries the name the pool gave a member, since the container's
// own name has a uniqueness suffix appended.
const labelName = brand.LabelPrefix + "name"

// defaultStopGrace is the SIGTERM-to-SIGKILL delay when Provider.StopGrace is
// unset.
const defaultStopGrace = 10 * time.Second

// killTimeout bounds each step of tearing down a cancelled exec, which runs on
// a context detached from the cancelled one.
const killTimeout = 10 * time.Second

// Provider runs sidecars as local Docker containers with bind-mounted workspaces.
type Provider struct {
	cli       client.APIClient
	baseImage string
	// workRoot is where per-member workspace directories and snapshots live.
	workRoot string
	// ManagedDockerfile is the project's factory.dockerfile, empty when the
	// project has none. It only changes the hint for a missing local image.
	ManagedDockerfile string
	// CacheKey, when set, mounts the project's cache volumes in every member:
	// Docker volumes named for the key that outlive containers and runs, so a
	// sidecar starts with the build cache earlier ones filled. It is a short
	// hash of the project (config.ProjectHash). Empty mounts none.
	CacheKey string
	// GoStack also mounts the Go module cache, for a Go project.
	GoStack bool
	// StopGrace is how long a cancelled command gets to exit on SIGTERM before
	// its process group is killed. Zero means defaultStopGrace.
	StopGrace time.Duration
	// ReadyTimeout bounds how long WaitEngine waits for a starting engine.
	// Zero means defaultReadyTimeout.
	ReadyTimeout time.Duration

	// now and sleep are test seams for the readiness wait; nil means real time.
	now   func() time.Time
	sleep func(context.Context, time.Duration) error

	mu      sync.Mutex
	members map[string]member // by container ID

	// uids caches the image user's uid by image ID; uidMu is held across the
	// probe, so concurrent Creates of one image probe it once.
	uidMu sync.Mutex
	uids  map[string]string
}

// member is a container the provider created: its bind-mounted workspace on
// the host, and the image it runs. A snapshot of a member records that image so
// clones restored from it run the same image, not the provider's default.
type member struct {
	hostDir string
	image   string
	// env is set in every command run in the container: the temporary cache
	// directories when the cache volumes could not be used.
	env map[string]string
}

// Layout of a snapshot directory: the workspace copy, and the image it ran on.
const (
	snapshotWorkspaceDir = "workspace"
	snapshotImageFile    = "image"
)

var _ sidecar.Backend = (*Provider)(nil)

// NewClient connects to the local Docker daemon using the standard environment
// (DOCKER_HOST and friends), negotiating the API version with the daemon. It is
// the one place a Docker client is constructed, shared with image builds.
func NewClient() (*client.Client, error) {
	cli, err := client.NewClientWithOpts(client.FromEnv, client.WithAPIVersionNegotiation())
	if err != nil {
		return nil, fmt.Errorf("docker: connect to daemon: %w", err)
	}
	return cli, nil
}

// New connects to the local Docker daemon using the standard environment
// (DOCKER_HOST and friends), negotiating the API version with the daemon.
func New() (*Provider, error) {
	cli, err := NewClient()
	if err != nil {
		return nil, err
	}
	b := &Provider{cli: cli, baseImage: defaultBaseImage, members: map[string]member{}}
	if img := os.Getenv(EnvImage); img != "" {
		b.baseImage = img
	}
	root := os.Getenv(brand.Env("DOCKER_WORKROOT"))
	if root == "" {
		cache, err := os.UserCacheDir()
		if err != nil {
			cache = os.TempDir()
		}
		root = filepath.Join(cache, brand.AppDir, "docker")
	}
	b.workRoot = root
	return b, nil
}

// NewWithClient builds a Provider around an existing Docker client, for tests.
func NewWithClient(cli client.APIClient, baseImage, workRoot string) *Provider {
	return &Provider{cli: cli, baseImage: baseImage, workRoot: workRoot, members: map[string]member{}}
}

// Close releases the Docker client's resources.
func (b *Provider) Close() error {
	if c, ok := b.cli.(interface{ Close() error }); ok {
		return c.Close()
	}
	return nil
}

func (b *Provider) remember(id string, m member) {
	b.mu.Lock()
	b.members[id] = m
	b.mu.Unlock()
}

func (b *Provider) member(id string) (member, bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	m, ok := b.members[id]
	return m, ok
}

func (b *Provider) hostDir(id string) (string, bool) {
	m, ok := b.member(id)
	return m.hostDir, ok
}

// Create provisions a container whose workspace at repoPath is a bind-mounted
// host directory. image selects the source: empty or a Docker reference starts
// a fresh container from that image; a smeltfs snapshot ref restores the
// snapshot's workspace into the new host directory (and runs the base image).
func (b *Provider) Create(ctx context.Context, name, image, repoPath string) (sidecar.Instance, error) {
	if repoPath == "" {
		return sidecar.Instance{}, errors.New("docker: create needs a repoPath")
	}
	// The pool may reuse a name: it names the seed and the first clone made from
	// its snapshot alike, which CircleCI allows and Docker does not. A suffix
	// keeps container names and workspace directories unique per Create; the
	// pool's own name is kept as a label.
	suffix, err := randomSuffix()
	if err != nil {
		return sidecar.Instance{}, fmt.Errorf("docker: name container: %w", err)
	}
	containerName := name + "-" + suffix
	hostDir := filepath.Join(b.workRoot, containerName)
	if err := os.MkdirAll(hostDir, 0o777); err != nil {
		return sidecar.Instance{}, fmt.Errorf("docker: create workspace dir: %w", err)
	}

	containerImage := b.baseImage
	switch {
	case strings.HasPrefix(image, snapshotScheme):
		// Clone: run the image the snapshot's seed ran, and restore the
		// snapshot's workspace into this member's host dir.
		snapDir := strings.TrimPrefix(image, snapshotScheme)
		recorded, err := os.ReadFile(filepath.Join(snapDir, snapshotImageFile))
		if err != nil {
			_ = os.RemoveAll(hostDir)
			return sidecar.Instance{}, fmt.Errorf("docker: read snapshot image: %w", err)
		}
		containerImage = strings.TrimSpace(string(recorded))
		if err := copyTree(ctx, filepath.Join(snapDir, snapshotWorkspaceDir), hostDir); err != nil {
			_ = os.RemoveAll(hostDir)
			return sidecar.Instance{}, fmt.Errorf("docker: restore snapshot: %w", err)
		}
	case image != "":
		containerImage = image
	}

	// Containers are created from the resolved reference, which is the image's
	// ID when Docker cannot inspect it by name; snapshots record it too.
	containerImage, err = b.resolveImage(ctx, containerImage, nil)
	if err != nil {
		_ = os.RemoveAll(hostDir)
		return sidecar.Instance{}, err
	}

	caches := b.cacheLayout(ctx, containerImage)
	created, err := b.cli.ContainerCreate(ctx, &container.Config{
		Image:      containerImage,
		Cmd:        []string{"sleep", "infinity"},
		WorkingDir: repoPath,
		Labels:     map[string]string{label: "1", labelName: name},
	}, &container.HostConfig{
		Binds:  []string{hostDir + ":" + repoPath},
		Mounts: b.cacheMounts(caches),
	}, nil, nil, containerName)
	if err != nil {
		_ = os.RemoveAll(hostDir)
		return sidecar.Instance{}, fmt.Errorf("docker: create container: %w", err)
	}
	if err := b.cli.ContainerStart(ctx, created.ID, container.StartOptions{}); err != nil {
		_ = b.cli.ContainerRemove(context.WithoutCancel(ctx), created.ID, container.RemoveOptions{Force: true})
		_ = os.RemoveAll(hostDir)
		return sidecar.Instance{}, fmt.Errorf("docker: start container: %w", err)
	}
	cacheEnv, err := b.prepareCaches(ctx, created.ID, caches)
	if err != nil {
		_ = b.cli.ContainerRemove(context.WithoutCancel(ctx), created.ID, container.RemoveOptions{Force: true})
		_ = os.RemoveAll(hostDir)
		return sidecar.Instance{}, err
	}

	b.remember(created.ID, member{hostDir: hostDir, image: containerImage, env: cacheEnv})
	return sidecar.Instance{ID: created.ID, HostDir: hostDir, RepoPath: repoPath}, nil
}

// Delete removes the container and its host workspace directory. Removing one
// that is already gone is not an error.
func (b *Provider) Delete(ctx context.Context, id string) error {
	err := b.cli.ContainerRemove(ctx, id, container.RemoveOptions{Force: true})
	if err != nil && !cerrdefs.IsNotFound(err) {
		return fmt.Errorf("docker: remove container: %w", err)
	}
	if dir, ok := b.hostDir(id); ok {
		_ = os.RemoveAll(dir)
		b.mu.Lock()
		delete(b.members, id)
		b.mu.Unlock()
	}
	return nil
}

// Snapshot copies the member's workspace aside, records the image it runs, and
// returns a smeltfs ref to the result. It is independent of the seed
// container's lifecycle, so a later Delete of the seed does not disturb clones
// restoring from it.
func (b *Provider) Snapshot(ctx context.Context, id, name string) (string, error) {
	m, ok := b.member(id)
	if !ok {
		return "", fmt.Errorf("docker: snapshot: unknown container %s", id)
	}
	snapDir := filepath.Join(b.workRoot, "snapshots", name)
	if err := os.MkdirAll(snapDir, 0o777); err != nil {
		return "", fmt.Errorf("docker: snapshot dir: %w", err)
	}
	// The snapshot keeps .git so clones carry the baseline commit and history.
	if err := copyTree(ctx, m.hostDir, filepath.Join(snapDir, snapshotWorkspaceDir)); err != nil {
		return "", fmt.Errorf("docker: snapshot copy: %w", err)
	}
	if err := os.WriteFile(filepath.Join(snapDir, snapshotImageFile), []byte(m.image+"\n"), 0o644); err != nil {
		return "", fmt.Errorf("docker: snapshot image: %w", err)
	}
	return snapshotScheme + snapDir, nil
}

// IsStale is always false: local containers do not go stale the way remote
// sidecars do.
func (b *Provider) IsStale(context.Context, string) bool { return false }

// Sync mirrors the local tree at cwd into the member's bind-mounted workspace
// with rsync (honoring .gitignore, deleting extraneous files), then ensures a
// git repo exists there so the baseline commit and `git diff HEAD` work. retry
// is unused: a local container is reachable as soon as it is created.
func (b *Provider) Sync(ctx context.Context, id, repoPath, cwd string, _ bool, status iostream.StatusFunc) error {
	dir, ok := b.hostDir(id)
	if !ok {
		return fmt.Errorf("docker: sync: unknown container %s", id)
	}
	status(iostream.LevelInfo, fmt.Sprintf("Syncing workspace %s...", repoPath))
	// Mirror cwd -> hostDir. The worktree's own .git is excluded; a fresh git
	// repo is initialised below, matching the CircleCI worktree sync.
	if err := rsyncMirror(ctx, cwd, dir, "--exclude=/.git", "--filter=:- .gitignore"); err != nil {
		return fmt.Errorf("docker: sync: %w", err)
	}
	if _, err := os.Stat(filepath.Join(dir, ".git")); errors.Is(err, os.ErrNotExist) {
		if out, err := exec.CommandContext(ctx, "git", "-C", dir, "init", "-q").CombinedOutput(); err != nil {
			return fmt.Errorf("docker: sync: git init: %w: %s", err, strings.TrimSpace(string(out)))
		}
	}
	status(iostream.LevelDone, "Synced")
	return nil
}

// Pull mirrors the member's workspace back into localDir with rsync, excluding
// every .git and files git ignores, matching the CircleCI pull's intent.
func (b *Provider) Pull(ctx context.Context, id, repoPath, localDir string, status iostream.StatusFunc) error {
	dir, ok := b.hostDir(id)
	if !ok {
		return fmt.Errorf("docker: pull: unknown container %s", id)
	}
	status(iostream.LevelInfo, fmt.Sprintf("Pulling workspace %s...", repoPath))
	if err := rsyncMirror(ctx, dir, localDir, "--exclude=.git", "--filter=:- .gitignore"); err != nil {
		return fmt.Errorf("docker: pull: %w", err)
	}
	status(iostream.LevelDone, "Pulled")
	return nil
}

// Exec runs script with `sh` inside container id and streams its stdout and
// stderr to onOutput (the script is sent on the exec's stdin, never in argv), returning the command's exit code. onSubmitted, when
// non-nil, receives the Docker exec ID between creation and streaming.
//
// The script runs in its own process group. When ctx is done the group gets
// SIGTERM, then SIGKILL after StopGrace, because Docker has no API to kill an
// exec and the stream would otherwise stay open until the command ends.
func (b *Provider) Exec(ctx context.Context, id, script string, env map[string]string, onOutput iostream.OutputFn, onSubmitted func(string)) (int, error) {
	if m, ok := b.member(id); ok && len(m.env) > 0 {
		merged := make(map[string]string, len(m.env)+len(env))
		maps.Copy(merged, m.env)
		maps.Copy(merged, env)
		env = merged
	}
	pgidFile := pgidPath()
	return b.run(ctx, id, execSpec{cmd: groupedCommand(pgidFile), stdin: script, pgidFile: pgidFile}, env, onOutput, onSubmitted)
}

// execAs is Exec as user, or as the image's own user when user is empty. It is
// for the short helper scripts of the cache setup, which are not killed on
// cancel.
func (b *Provider) execAs(ctx context.Context, id, user, script string, onOutput iostream.OutputFn) (int, error) {
	return b.run(ctx, id, execSpec{user: user, cmd: []string{"sh", "-c", script}}, nil, onOutput, nil)
}

// execSpec is what to run in a container. A pgidFile means the command records
// its process group there and is killed through it when the context ends.
type execSpec struct {
	user     string
	cmd      []string
	stdin    string // when set, attached to the exec's stdin
	pgidFile string
}

func (b *Provider) run(ctx context.Context, id string, spec execSpec, env map[string]string, onOutput iostream.OutputFn, onSubmitted func(string)) (int, error) {
	if err := sidecar.CheckArgv(spec.cmd...); err != nil {
		return 0, err
	}
	created, err := b.cli.ContainerExecCreate(ctx, id, container.ExecOptions{
		User:         spec.user,
		Cmd:          spec.cmd,
		Env:          envSlice(env),
		AttachStdin:  spec.stdin != "",
		AttachStdout: true,
		AttachStderr: true,
	})
	if err != nil {
		return 0, fmt.Errorf("docker exec create: %w", err)
	}
	if onSubmitted != nil {
		onSubmitted(created.ID)
	}

	att, err := b.cli.ContainerExecAttach(ctx, created.ID, container.ExecAttachOptions{})
	if err != nil {
		return 0, fmt.Errorf("docker exec attach: %w", err)
	}
	defer att.Close()

	if spec.stdin != "" {
		// Written off the reading path so a large script and the output stream
		// cannot deadlock; the deferred att.Close fails a stuck write.
		go func() {
			if _, err := io.WriteString(att.Conn, spec.stdin); err == nil {
				_ = att.CloseWrite()
			}
		}()
	}

	if spec.pgidFile != "" {
		finished := make(chan struct{})
		defer close(finished)
		go b.killOnCancel(ctx, finished, id, spec.pgidFile, att.Close)
	}

	out := streamWriter{stream: iostream.StreamStdout, fn: onOutput}
	errW := streamWriter{stream: iostream.StreamStderr, fn: onOutput}
	if _, err := stdcopy.StdCopy(out, errW, att.Reader); err != nil && ctx.Err() == nil {
		return 0, fmt.Errorf("docker exec stream: %w", err)
	}
	if err := ctx.Err(); err != nil {
		return 0, err
	}

	insp, err := b.cli.ContainerExecInspect(ctx, created.ID)
	if err != nil {
		return 0, fmt.Errorf("docker exec inspect: %w", err)
	}
	return insp.ExitCode, nil
}

// killOnCancel waits for ctx to end or the exec to finish. On cancel it sends
// SIGTERM to the exec's process group, SIGKILL after the grace, and closes the
// stream so the copy returns even when nothing could be signalled.
func (b *Provider) killOnCancel(ctx context.Context, finished <-chan struct{}, id, pgidFile string, closeStream func()) {
	select {
	case <-finished:
		return
	case <-ctx.Done():
	}
	defer closeStream()
	b.signalGroup(ctx, id, pgidFile, "TERM")
	grace := b.StopGrace
	if grace <= 0 {
		grace = defaultStopGrace
	}
	timer := time.NewTimer(grace)
	defer timer.Stop()
	select {
	case <-finished:
		return
	case <-timer.C:
	}
	b.signalGroup(ctx, id, pgidFile, "KILL")
}

// signalGroup sends sig to the process group recorded in pgidFile. It runs on a
// context detached from the cancelled one; a failure leaves the group alone and
// the stream close ends the wait.
func (b *Provider) signalGroup(ctx context.Context, id, pgidFile, sig string) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), killTimeout)
	defer cancel()
	created, err := b.cli.ContainerExecCreate(ctx, id, container.ExecOptions{
		User: "root",
		Cmd:  []string{"sh", "-c", signalScript, "sh", pgidFile, sig},
	})
	if err != nil {
		log.Printf("docker: signal %s: %v", id, err)
		return
	}
	if err := b.cli.ContainerExecStart(ctx, created.ID, container.ExecStartOptions{Detach: true}); err != nil {
		log.Printf("docker: signal %s: %v", id, err)
		return
	}
	// ExecStart returns before the command ends; wait so KILL follows TERM.
	for ctx.Err() == nil {
		insp, err := b.cli.ContainerExecInspect(ctx, created.ID)
		if err != nil || !insp.Running {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// signalScript sends signal $2 to the process group whose id is in file $1. The
// command may not have written the file yet when the stop arrives, so it waits
// a moment for it.
const signalScript = `for i in 1 2 3 4 5 6 7 8 9 10; do [ -s "$1" ] && break; sleep 0.2; done
p=$(cat "$1" 2>/dev/null) && [ -n "$p" ] && kill -"$2" -- "-$p"`

// pgidPath is a fresh file name in the container for a command's process group.
func pgidPath() string {
	var r [8]byte
	_, _ = rand.Read(r[:])
	return "/tmp/" + brand.Command + "-pgid-" + hex.EncodeToString(r[:])
}

// groupedCommand runs the script it reads from stdin under `sh` in its own
// session, whose id is the process group the exec is killed through, written to
// pgidFile. The script is spooled to a file and run with stdin from /dev/null,
// so the command cannot swallow the rest of it. setsid runs in the background
// and is waited for: a foreground setsid forks when its caller leads a group and
// would lose the exit code. Without setsid the script runs ungrouped and cannot
// be signalled.
func groupedCommand(pgidFile string) []string {
	const wrapper = `s="$1.sh"
cat > "$s"
if command -v setsid >/dev/null 2>&1; then
  setsid sh -c 'echo $$ > "$0"; exec sh "$1" </dev/null' "$1" "$s" &
  wait $!
else
  sh "$s" </dev/null
fi
code=$?
rm -f "$1" "$s"
exit $code`
	return []string{"sh", "-c", wrapper, "sh", pgidFile}
}

// rsyncMirror makes dst an exact copy of src (archive mode, deleting extraneous
// files) plus the given extra filters. Trailing slashes make rsync copy src's
// contents into dst rather than nesting a directory.
func rsyncMirror(ctx context.Context, src, dst string, extra ...string) error {
	if err := os.MkdirAll(dst, 0o777); err != nil {
		return fmt.Errorf("mkdir %s: %w", dst, err)
	}
	args := append([]string{"--archive", "--delete"}, extra...)
	args = append(args, strings.TrimRight(src, "/")+"/", strings.TrimRight(dst, "/")+"/")
	cmd := exec.CommandContext(ctx, "rsync", args...)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("rsync: %w: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

// copyTree copies src into dst exactly, including .git, with rsync.
func copyTree(ctx context.Context, src, dst string) error {
	if err := os.MkdirAll(dst, 0o777); err != nil {
		return fmt.Errorf("mkdir %s: %w", dst, err)
	}
	cmd := exec.CommandContext(ctx, "rsync", "--archive", "--delete", strings.TrimRight(src, "/")+"/", strings.TrimRight(dst, "/")+"/")
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("rsync copy: %w: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

// envSlice turns an environment map into the "KEY=VALUE" slice Docker wants.
func envSlice(env map[string]string) []string {
	if len(env) == 0 {
		return nil
	}
	out := make([]string, 0, len(env))
	for k, v := range env {
		out = append(out, k+"="+v)
	}
	return out
}

// streamWriter forwards each write to an OutputFn under a fixed stream label.
// It copies the buffer because stdcopy reuses its scratch space between writes.
type streamWriter struct {
	stream string
	fn     iostream.OutputFn
}

func (w streamWriter) Write(p []byte) (int, error) {
	if w.fn != nil {
		w.fn(w.stream, append([]byte(nil), p...))
	}
	return len(p), nil
}

// randomSuffix returns a short random hex string for unique container names.
func randomSuffix() (string, error) {
	var b [4]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(b[:]), nil
}
