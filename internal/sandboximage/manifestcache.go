package sandboximage

import (
	"context"
	"encoding/json"
	"maps"
	"os"
	"path/filepath"

	"github.com/docker/docker/api/types/image"
)

// CachedKitManifest is ReadKitManifest for the image with the given ID, kept
// in the JSON file at path so an image already read starts no container. An
// image's files never change, so the ID is the whole key. Entries of images
// the host no longer lists are dropped. The cache is best effort: an
// unreadable or unwritable file only costs a read, and an empty id or path
// skips it.
func (b *Builder) CachedKitManifest(ctx context.Context, path, id string) (KitManifest, error) {
	if id == "" || path == "" {
		return b.ReadKitManifest(ctx, id)
	}
	entries := readManifestCache(path)
	if pruned := b.pruneManifests(ctx, entries); pruned {
		defer func() { writeManifestCache(path, entries) }()
	}
	if m, ok := entries[id]; ok {
		return m, nil
	}
	m, err := b.ReadKitManifest(ctx, id)
	if err != nil {
		return KitManifest{}, err
	}
	entries[id] = m
	writeManifestCache(path, entries)
	return m, nil
}

// pruneManifests drops the entries of images the host does not list, and says
// whether it dropped any. When the list is unavailable it keeps them all.
func (b *Builder) pruneManifests(ctx context.Context, entries map[string]KitManifest) bool {
	if len(entries) == 0 {
		return false
	}
	images, err := b.cli.ImageList(ctx, image.ListOptions{All: true})
	if err != nil {
		return false
	}
	present := make(map[string]bool, len(images))
	for _, img := range images {
		present[img.ID] = true
	}
	dropped := false
	for id := range maps.Clone(entries) {
		if !present[id] {
			delete(entries, id)
			dropped = true
		}
	}
	return dropped
}

func readManifestCache(path string) map[string]KitManifest {
	var m map[string]KitManifest
	if raw, err := os.ReadFile(path); err == nil {
		_ = json.Unmarshal(raw, &m)
	}
	if m == nil {
		m = map[string]KitManifest{}
	}
	return m
}

// writeManifestCache replaces the file atomically, so concurrent status calls
// never read half a write.
func writeManifestCache(path string, entries map[string]KitManifest) {
	raw, err := json.Marshal(entries)
	if err != nil || os.MkdirAll(filepath.Dir(path), 0o700) != nil {
		return
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".kit-manifests-*")
	if err != nil {
		return
	}
	_, werr := tmp.Write(raw)
	cerr := tmp.Close()
	if werr != nil || cerr != nil || os.Rename(tmp.Name(), path) != nil {
		_ = os.Remove(tmp.Name())
	}
}
