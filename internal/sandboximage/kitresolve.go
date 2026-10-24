package sandboximage

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
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
	Kit int    `json:"kit"`
	Ref string `json:"ref"`
}

// ResolveKit pulls the kit image for this smelt's KitVersion, resolves its tag
// to a digest and remembers it in statePath. When the pull fails it falls back
// to the digest pulled last time for the same kit version, provided that image
// is still on the Docker host, and says so in the result.
func ResolveKit(ctx context.Context, p kitPuller, statePath string) (KitChoice, error) {
	ref, pullErr := p.PullKit(ctx, KitTagRef())
	if pullErr == nil {
		// Remembering is best effort: the build does not depend on it.
		_ = saveLastKit(statePath, ref)
		return KitChoice{Ref: ref}, nil
	}
	last, ok := readLastKit(statePath)
	if ok {
		if have, err := p.Exists(ctx, last); err == nil && have {
			return KitChoice{Ref: last, Offline: true, PullErr: pullErr}, nil
		}
	}
	return KitChoice{}, fmt.Errorf("%w: %s: %w", ErrKitUnavailable, KitTagRef(), pullErr)
}

func saveLastKit(path, ref string) error {
	raw, err := json.Marshal(lastKit{Kit: KitVersion, Ref: ref})
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	return os.WriteFile(path, raw, 0o644)
}

func readLastKit(path string) (string, bool) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return "", false
	}
	var k lastKit
	if json.Unmarshal(raw, &k) != nil || k.Kit != KitVersion || k.Ref == "" {
		return "", false
	}
	return k.Ref, true
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
	_, err = io.Copy(io.Discard, rc)
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

// WithKit is the plan with its Dockerfile's agent-kit section replaced by a
// copy from the kit image kitRef. The tag is computed from the composed
// Dockerfile, so a different kit is a different image. A Dockerfile without a
// kit section is returned unchanged.
func (p Plan) WithKit(kitRef string) (Plan, error) {
	if kitMarkerOffset(p.Context.Content) < 0 {
		return p, nil
	}
	c := *p.Context
	c.Content = ComposeWithKitImage(c.Content, kitRef)
	tag, err := c.Tag(p.Slug)
	if err != nil {
		return Plan{}, err
	}
	return Plan{Context: &c, Slug: p.Slug, Tag: tag}, nil
}

// WithLastKit is the plan as the last build composed it: with the kit pulled
// last (see ResolveKit) copied in. It lets the tag of the factory image be
// known without Docker. A Dockerfile without a kit section, or no kit pulled
// yet, leaves the plan unchanged.
func (p Plan) WithLastKit(statePath string) Plan {
	ref, ok := readLastKit(statePath)
	if !ok {
		return p
	}
	composed, err := p.WithKit(ref)
	if err != nil {
		return p
	}
	return composed
}
