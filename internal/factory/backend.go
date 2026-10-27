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
// share it, so both set the same Dockerfile and cache. machineRuns is the
// machine-wide factory.maxRuns (0 when unlimited or unknown); the default CPU
// share divides the engine's CPUs by the larger of it and the project's own
// concurrency.
func DockerBackend(root string, cfg *config.ProjectConfig, machineRuns int) (sidecar.Backend, error) {
	b, err := docker.New()
	if err != nil {
		return nil, fmt.Errorf("docker backend unavailable: %w", err)
	}
	b.ProjectPath = root
	b.MinFreeBytes = cfg.Factory.MinFreeBytes()
	b.CacheMaxBytes = cfg.Factory.CacheMaxBytes()
	b.StopGrace = cfg.Factory.StopGraceDuration()
	b.Runs = max(machineRuns, cfg.Factory.ConcurrentRuns())
	if cfg.Factory != nil {
		b.CPUs = cfg.Factory.CPUs
		b.ManagedDockerfile = cfg.Factory.Dockerfile
	}
	if cfg.Factory.CacheEnabled() {
		b.CacheKey = config.ProjectHash(root)
		b.GoStack = cfg.Environment != nil && cfg.Environment.Stack == envspec.StackGo
	}
	return b, nil
}
