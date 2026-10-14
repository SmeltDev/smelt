package factory

import (
	"fmt"

	"github.com/smeltdev/smelt/internal/config"
	"github.com/smeltdev/smelt/internal/envspec"
	"github.com/smeltdev/smelt/internal/sidecar"
	"github.com/smeltdev/smelt/internal/sidecar/docker"
)

// DockerBackend builds the local Docker backend for the project at root,
// configured from cfg. The daemon's factory runs and `smelt factory bootstrap`
// share it, so both set the same Dockerfile and cache.
func DockerBackend(root string, cfg *config.ProjectConfig) (sidecar.Backend, error) {
	b, err := docker.New()
	if err != nil {
		return nil, fmt.Errorf("docker backend unavailable: %w", err)
	}
	if cfg.Factory != nil {
		b.ManagedDockerfile = cfg.Factory.Dockerfile
	}
	if cfg.Factory.CacheEnabled() {
		b.CacheKey = config.ProjectHash(root)
		b.GoStack = cfg.Environment != nil && cfg.Environment.Stack == envspec.StackGo
	}
	return b, nil
}
