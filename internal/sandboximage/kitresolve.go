package sandboximage

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/docker/docker/api/types/image"
)

// kitStateFile is the file, in smelt's data dir, that remembers the last kit
// image a pull resolved to.
const kitStateFile = "kit-image.json"

// KitStatePath is where the last resolved kit is remembered, under dataDir.
func KitStatePath(dataDir string) string { return filepath.Join(dataDir, kitStateFile) }

// ErrKitUnavailable means the kit image could not be pulled and none was
// pulled before.
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
}

// ResolveKit pulls the kit image for this smelt's KitVersion, resolves its tag
// to a digest and remembers it in statePath. When the pull fails it falls back
// to the digest pulled last time including across CLI upgrades, provided that image
// is still on the Docker host, and says so in the result.
func ResolveKit(ctx context.Context, p kitPuller, statePath string) (KitChoice, error) {
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
	ref, pullErr := p.PullKit(ctx, tag)
	if pullErr == nil {
		// Remembering is best effort: the build does not depend on it.
		_ = writeLastKit(statePath, lastKit{Kit: version, Ref: ref, Selected: selected})
		return KitChoice{Ref: ref, Kit: version}, nil
	}
	last, ok := readLastKit(statePath)
	if ok {
		if have, err := p.Exists(ctx, last.Ref); err == nil && have {
			return KitChoice{Ref: last.Ref, Kit: last.Kit, Offline: true, PullErr: pullErr}, nil
		}
	}
	return KitChoice{}, fmt.Errorf("%w: %s: %w", ErrKitUnavailable, tag, pullErr)
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
	c, err := ReadContext(kitDir, "Dockerfile")
	if err != nil {
		return "", err
	}
	args := map[string]*string{}
	for _, a := range KitPins() {
		args[a.Name] = &a.Value
	}
	opts.Context, opts.Slug, opts.Tag, opts.BuildArgs = c, "kit", KitLocalRef(), args
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
	return p.SandboxPlanWithKit(KitChoice{Ref: ref.Ref, Kit: ref.Kit}), true
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
		Context: &Context{Dockerfile: "Dockerfile", Content: sandboxDockerfile(p.Tag, choice)},
		Slug:    p.Slug, Tag: SandboxTag(p.Tag, choice.Ref), Toolchain: &p, Kit: choice.Kit,
	}
}
