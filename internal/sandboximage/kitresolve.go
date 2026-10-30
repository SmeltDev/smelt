package sandboximage

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/docker/docker/api/types/filters"
	"github.com/docker/docker/api/types/image"

	"github.com/smeltdev/smelt/internal/version"
	"github.com/smeltdev/smelt/kit"
)

// kitStateFile is the file, in smelt's data dir, that remembers the last kit
// image a pull resolved to.
const kitStateFile = "kit-image.json"

const kitDockerfile = "Dockerfile"

// KitStatePath is where the last resolved kit is remembered, under dataDir.
func KitStatePath(dataDir string) string { return filepath.Join(dataDir, kitStateFile) }

// ErrKitUnavailable means no kit could be acquired or built, and no usable
// cached image remains.
var ErrKitUnavailable = errors.New("agent kit image unavailable")

// KitChoice is the kit image a build copies from.
type KitChoice struct {
	// Ref names the image by digest, so a new kit makes a new image tag.
	Ref string
	// Kit is the actual version selected, including an older offline fallback.
	Kit int
	// Offline is set when the pull failed and the last pulled kit stands in;
	// PullErr is why.
	Offline bool
	PullErr error
	// FoundOnDisk is set with Offline when no record named a usable image and
	// the kit was found by its version label among the local images.
	FoundOnDisk bool
	// Notice records source build provenance independently of terminal output.
	Notice string
	// Manifest, when set, is written to the sandbox image as LabelKitManifest.
	Manifest *KitManifest
}

// kitPuller is the part of a Builder ResolveKit uses.
type kitPuller interface {
	PullKit(ctx context.Context, tagRef string) (string, error)
	Exists(ctx context.Context, tag string) (bool, error)
}

// lastKit is what the state file holds.
type lastKit struct {
	Kit      int    `json:"kit"`
	Ref      string `json:"ref"`
	Selected string `json:"selected,omitempty"`
	Notice   string `json:"notice,omitempty"`
}

// ResolveKit checks local images, then the registry, then embedded sources.
// A refreshed selection takes precedence. The last usable image is a final
// fallback if acquisition fails, including across CLI upgrades.
func ResolveKit(ctx context.Context, p kitPuller, statePath string) (KitChoice, error) {
	if err := ctx.Err(); err != nil {
		return KitChoice{}, err
	}
	tag, version := KitTagRef(), KitVersion
	previous, _ := readLastKit(statePath)
	selected := ""
	// A refresh may select a kit newer than this CLI, but must not hold a
	// subsequent CLI upgrade back on an older kit.
	if previous.Selected != "" && previous.Kit >= KitVersion {
		// Refresh selects a particular manifest, not whatever its mutable tag
		// points to on the next run. Keep its recorded version and contents
		// together until another refresh or a CLI upgrade selects a newer kit.
		tag, version = previous.Ref, previous.Kit
		selected = previous.Selected
	}
	if selected == "" {
		if choice, ok := resolveLocalKit(ctx, p, statePath, previous); ok {
			return choice, nil
		}
	}
	ref, pullErr := p.PullKit(ctx, tag)
	if pullErr == nil {
		// Remembering is best effort: the build does not depend on it.
		_ = writeLastKit(statePath, lastKit{Kit: version, Ref: ref, Selected: selected})
		return KitChoice{Ref: ref, Kit: version}, nil
	}
	if ctx.Err() != nil {
		return KitChoice{}, ctx.Err()
	}
	if selected == "" {
		if builder, ok := p.(kitBuilder); ok {
			choice, err := BuildEmbeddedKit(ctx, builder, statePath, BuildOptions{}, pullErr)
			if err == nil {
				return choice, nil
			}
			pullErr = errors.Join(pullErr, err)
		}
	}
	if ctx.Err() != nil {
		return KitChoice{}, ctx.Err()
	}
	if choice, ok := resolveRememberedKit(ctx, p, statePath, pullErr); ok {
		return choice, nil
	}
	return KitChoice{}, fmt.Errorf("%w: %s: %w", ErrKitUnavailable, tag, pullErr)
}

// resolveRememberedKit falls back to the last recorded kit if it is still on
// disk, and otherwise to a local image labelled with this CLI's kit version.
func resolveRememberedKit(ctx context.Context, p kitPuller, statePath string, pullErr error) (KitChoice, bool) {
	if last, ok := readLastKit(statePath); ok {
		if have, err := p.Exists(ctx, last.Ref); err == nil && have {
			return KitChoice{Ref: last.Ref, Kit: last.Kit, Offline: true, PullErr: pullErr, Notice: last.Notice}, true
		}
	}
	return resolveLabeledKit(ctx, p, statePath, pullErr)
}

// resolveLabeledKit finds a local image labelled with this CLI's kit version,
// for when the record of the last kit is missing, unreadable or names an image
// that is gone. It rewrites the record so the next run finds the kit directly.
func resolveLabeledKit(ctx context.Context, p kitPuller, statePath string, pullErr error) (KitChoice, bool) {
	labeled, ok := p.(interface {
		LabeledKit(context.Context, int) (string, error)
	})
	if !ok {
		return KitChoice{}, false
	}
	ref, err := labeled.LabeledKit(ctx, KitVersion)
	if err != nil || ref == "" {
		return KitChoice{}, false
	}
	_ = writeLastKit(statePath, lastKit{Kit: KitVersion, Ref: ref})
	return KitChoice{Ref: ref, Kit: KitVersion, Offline: true, FoundOnDisk: true, PullErr: pullErr}, true
}

// resolveLocalKit checks revision-specific and imported version tags in order.
func resolveLocalKit(ctx context.Context, p kitPuller, statePath string, previous lastKit) (KitChoice, bool) {
	local, ok := p.(interface {
		LocalKit(context.Context, string) (string, error)
	})
	if !ok {
		return KitChoice{}, false
	}
	for _, tag := range []string{KitRevisionRef(), KitLocalRef()} {
		ref, err := local.LocalKit(ctx, tag)
		if err != nil || ref == "" {
			continue
		}
		notice := ""
		if previous.Ref == ref {
			notice = previous.Notice
		}
		if raw, err := os.ReadFile(kitProvenancePath(statePath, ref)); err == nil {
			notice = string(raw)
		}
		_ = writeLastKit(statePath, lastKit{Kit: KitVersion, Ref: ref, Notice: notice})
		return KitChoice{Ref: ref, Kit: KitVersion, Notice: notice}, true
	}
	return KitChoice{}, false
}

// RememberKit records a locally built kit for offline reuse.
func RememberKit(path, ref string) error {
	return writeLastKit(path, lastKit{Kit: KitVersion, Ref: ref})
}

func writeLastKit(path string, state lastKit) error {
	raw, err := json.Marshal(state)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	if state.Notice != "" {
		_ = os.WriteFile(kitProvenancePath(path, state.Ref), []byte(state.Notice), 0o600)
	}
	return os.WriteFile(path, raw, 0o644)
}

func readLastKit(path string) (lastKit, bool) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return lastKit{}, false
	}
	var k lastKit
	if json.Unmarshal(raw, &k) != nil || k.Kit <= 0 || k.Ref == "" {
		return lastKit{}, false
	}
	return k, true
}

// digestRef picks the entry of repoDigests ("repo@sha256:...") that belongs to
// KitRegistryRepo.
func digestRef(repoDigests []string) (string, bool) {
	for _, d := range repoDigests {
		if strings.HasPrefix(d, KitRegistryRepo+"@sha256:") {
			return d, true
		}
	}
	return "", false
}

// PullKit pulls tagRef and returns the reference of what it resolved to, by
// digest.
func (b *Builder) PullKit(ctx context.Context, tagRef string) (string, error) {
	rc, err := b.cli.ImagePull(ctx, tagRef, image.PullOptions{})
	if err != nil {
		return "", fmt.Errorf("pull %s: %w", tagRef, err)
	}
	// Docker can report registry failures in the JSON stream after returning
	// HTTP 200. Do not inspect a stale local tag as though the pull succeeded.
	err = readBuildStream(rc, func(string) {})
	_ = rc.Close()
	if err != nil {
		return "", fmt.Errorf("pull %s: %w", tagRef, err)
	}
	info, err := b.cli.ImageInspect(ctx, tagRef)
	if err != nil {
		return "", fmt.Errorf("look up %s: %w", tagRef, err)
	}
	ref, ok := digestRef(info.RepoDigests)
	if !ok {
		return "", fmt.Errorf("%s has no registry digest", tagRef)
	}
	return ref, nil
}

// BuildKit builds the kit image from kitDir (the repo's kit/) and tags it
// KitLocalRef. It returns the image's ID, which a Dockerfile can COPY from and
// which changes whenever the kit does.
func (b *Builder) BuildKit(ctx context.Context, kitDir string, opts BuildOptions) (string, error) {
	c, err := ReadContext(kitDir, kitDockerfile)
	if err != nil {
		return "", err
	}
	args := map[string]*string{}
	for _, a := range KitPins() {
		args[a.Name] = &a.Value
	}
	revision := KitRevision()
	args["SMELT_VERSION"] = &revision
	opts.Context, opts.Slug, opts.Tag, opts.BuildArgs = c, "kit", KitRevisionRef(), args
	opts.ExtraTags = []string{KitLocalRef()}
	if err := b.Build(ctx, opts); err != nil {
		return "", err
	}
	info, err := b.cli.ImageInspect(ctx, opts.Tag)
	if err != nil {
		return "", fmt.Errorf("look up %s: %w", opts.Tag, err)
	}
	return info.ID, nil
}

// SandboxPlan is the plan of the sandbox image: a generated layer that copies
// the kit kitRef into the toolchain image p. Its tag follows both, so a new kit
// rebuilds only this layer. It is pure.
func (p Plan) SandboxPlan(kitRef string) Plan {
	return p.SandboxPlanWithKit(KitChoice{Ref: kitRef, Kit: KitVersion})
}

// LastKitSandboxPlan is the sandbox plan as the last build made it, with the
// kit pulled last (see ResolveKit). It lets the tag of the factory image be
// known without Docker; ok is false when no kit was pulled yet.
func (p Plan) LastKitSandboxPlan(statePath string) (Plan, bool) {
	ref, ok := readLastKit(statePath)
	if !ok {
		return Plan{}, false
	}
	return p.SandboxPlanWithKit(KitChoice{Ref: ref.Ref, Kit: ref.Kit, Notice: ref.Notice}), true
}

// KitRefresher pulls and reads a published kit without running its agents.
type KitRefresher interface {
	PullKit(context.Context, string) (string, error)
	ReadKitManifest(context.Context, string) (KitManifest, error)
}

// RefreshKit selects a published kit for future builds, independently of the CLI release.
func RefreshKit(ctx context.Context, p KitRefresher, statePath, tag string) (KitChoice, error) {
	ref, err := p.PullKit(ctx, tag)
	if err != nil {
		return KitChoice{}, fmt.Errorf("refresh kit: %w", err)
	}
	manifest, err := p.ReadKitManifest(ctx, ref)
	if err != nil {
		return KitChoice{}, err
	}
	if manifest.Kit <= 0 {
		return KitChoice{}, fmt.Errorf("kit manifest has no valid version")
	}
	if err := writeLastKit(statePath, lastKit{Kit: manifest.Kit, Ref: ref, Selected: tag}); err != nil {
		return KitChoice{}, fmt.Errorf("save selected kit: %w", err)
	}
	return KitChoice{Ref: ref, Kit: manifest.Kit}, nil
}

// SandboxPlanWithKit plans an image with the selected kit's actual version.
func (p Plan) SandboxPlanWithKit(choice KitChoice) Plan {
	return Plan{
		Context:   &Context{Dockerfile: kitDockerfile, Content: sandboxDockerfile(p.Tag, choice)},
		KitNotice: choice.Notice,
		Slug:      p.Slug, Tag: SandboxTag(p.Tag, choice.Ref), Toolchain: &p, Kit: choice.Kit,
	}
}

// KitRevision identifies this binary, including builds without VCS metadata.
func KitRevision() string {
	if r := version.Current().Revision; r != "" {
		return strings.Map(func(c rune) rune {
			if c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' {
				return c
			}
			return '-'
		}, r)
	}
	h := sha256.New()
	for _, name := range []string{kitDockerfile, "runtime.sh"} {
		data, _ := kit.Files.ReadFile(name)
		_, _ = h.Write(data)
	}
	return fmt.Sprintf("%x", h.Sum(nil))[:12]
}

// KitRevisionRef is the local tag specific to this binary's source revision.
func KitRevisionRef() string { return KitLocalRef() + "-" + KitRevision() }

type kitBuilder interface {
	BuildKit(context.Context, string, BuildOptions) (string, error)
}

// EmbeddedKitContext materializes the canonical kit inputs. The caller removes dir.
func EmbeddedKitContext() (dir string, err error) {
	dir, err = os.MkdirTemp("", "smelt-kit-")
	if err != nil {
		return "", err
	}
	for _, name := range []string{kitDockerfile, "runtime.sh"} {
		data, readErr := kit.Files.ReadFile(name)
		if readErr == nil {
			readErr = os.WriteFile(filepath.Join(dir, name), data, 0o644)
		}
		if readErr != nil {
			_ = os.RemoveAll(dir)
			return "", readErr
		}
	}
	return dir, nil
}

// BuildEmbeddedKit rebuilds this binary's kit without requiring a source checkout.
func BuildEmbeddedKit(ctx context.Context, b kitBuilder, state string, opts BuildOptions, cause error) (KitChoice, error) {
	if err := ctx.Err(); err != nil {
		return KitChoice{}, err
	}
	dir, err := EmbeddedKitContext()
	if err != nil {
		return KitChoice{}, err
	}
	defer func() { _ = os.RemoveAll(dir) }()
	ref, err := b.BuildKit(ctx, dir, opts)
	if err != nil {
		return KitChoice{}, err
	}
	notice := fmt.Sprintf("using a locally built agent kit (kit %d, smelt %s)", KitVersion, KitRevision())
	if cause != nil {
		notice += ": the published kit isn't available (" + kitFailureReason(cause) + ")"
	}
	_ = writeLastKit(state, lastKit{Kit: KitVersion, Ref: ref, Notice: notice})
	return KitChoice{Ref: ref, Kit: KitVersion, Notice: notice}, nil
}

func kitFailureReason(err error) string {
	s := strings.ToLower(err.Error())
	for _, reason := range []string{"denied", "not found", "unauthorized"} {
		if strings.Contains(s, reason) {
			return reason
		}
	}
	return "offline or registry unavailable"
}

// ClaimKitNotice atomically claims the automatic notice for an immutable image.
// Failure to persist a stamp must never prevent use of the image.
func ClaimKitNotice(state, ref string) bool {
	sum := sha256.Sum256([]byte(ref))
	path := filepath.Join(filepath.Dir(state), fmt.Sprintf("kit-notice-%x", sum))
	if os.MkdirAll(filepath.Dir(path), 0o700) != nil {
		return true
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return !errors.Is(err, os.ErrExist)
	}
	_ = f.Close()
	return true
}

// LocalKit resolves a local tag to an immutable image ID without pulling.
func (b *Builder) LocalKit(ctx context.Context, tag string) (string, error) {
	info, err := b.cli.ImageInspect(ctx, tag)
	return info.ID, err
}

// LabeledKit returns the newest local image labelled with the given kit
// version, or "" when there is none.
func (b *Builder) LabeledKit(ctx context.Context, version int) (string, error) {
	want := LabelKit + "=" + strconv.Itoa(version)
	images, err := b.cli.ImageList(ctx, image.ListOptions{Filters: filters.NewArgs(filters.Arg("label", want))})
	if err != nil {
		return "", fmt.Errorf("list kit images: %w", err)
	}
	var best image.Summary
	for _, img := range images {
		// The daemon's filter also matches a bare key; insist on the value.
		if img.Labels[LabelKit] != strconv.Itoa(version) {
			continue
		}
		if best.ID == "" || img.Created > best.Created {
			best = img
		}
	}
	return best.ID, nil
}

func kitProvenancePath(state, ref string) string {
	sum := sha256.Sum256([]byte(ref))
	return filepath.Join(filepath.Dir(state), fmt.Sprintf("kit-source-%x", sum))
}
