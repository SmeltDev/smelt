package docker

import (
	"context"
	"fmt"
	"log"
	"math"
	"strconv"

	"github.com/smeltdev/smelt/internal/brand"
)

// minDefaultCPUs is the floor of the computed default share: a run needs room
// for a build and its tests, however many runs share the machine.
const minDefaultCPUs = 2

// envCPUs names the variable that tells a container's commands its CPU share.
func envCPUs() string { return brand.Env("CPUS") }

// CPUShare is the CPUs each run container may use. The zero value is no limit.
type CPUShare struct {
	// CPUs is the limit; zero means none.
	CPUs float64
	// EngineCPUs and Runs explain a computed default; Configured marks a
	// factory.cpus the user set, which they do not explain.
	EngineCPUs int
	Runs       int
	Configured bool
}

// resolveCPUShare decides the share. configured is factory.cpus (nil when
// unset, 0 for no limit); an unset value splits the engine's CPUs among runs,
// never below the floor and never above the engine's count. An engine that
// reports no CPUs gets no limit.
func resolveCPUShare(configured *float64, engineCPUs, runs int) CPUShare {
	runs = max(runs, 1)
	if configured != nil {
		return CPUShare{CPUs: max(*configured, 0), Configured: true}
	}
	if engineCPUs <= 0 {
		return CPUShare{}
	}
	share := math.Min(float64(engineCPUs), math.Max(float64(engineCPUs)/float64(runs), minDefaultCPUs))
	return CPUShare{CPUs: share, EngineCPUs: engineCPUs, Runs: runs}
}

// Limited reports whether the share limits the container.
func (s CPUShare) Limited() bool { return s.CPUs > 0 }

// NanoCPUs is the share as Docker's HostConfig.NanoCPUs; 0 means no limit.
func (s CPUShare) NanoCPUs() int64 {
	if !s.Limited() {
		return 0
	}
	return int64(math.Round(s.CPUs * 1e9))
}

// Env is the container environment entry for the share, nil when unlimited.
func (s CPUShare) Env() []string {
	if !s.Limited() {
		return nil
	}
	return []string{envCPUs() + "=" + strconv.FormatFloat(s.CPUs, 'f', -1, 64)}
}

// String describes the share for the run log and status, "" when unlimited.
func (s CPUShare) String() string {
	if !s.Limited() {
		return ""
	}
	if s.Configured {
		return fmt.Sprintf("cpus %.1f (factory.cpus)", s.CPUs)
	}
	return fmt.Sprintf("cpus %.1f (%d engine CPUs / %d runs)", s.CPUs, s.EngineCPUs, s.Runs)
}

// cpuShare resolves the provider's share. The engine's CPU count is read once;
// an engine that cannot say gets no limit, and is asked again next time.
func (b *Provider) cpuShare(ctx context.Context) CPUShare {
	if b.CPUs != nil {
		return resolveCPUShare(b.CPUs, 0, b.Runs)
	}
	b.cpuMu.Lock()
	defer b.cpuMu.Unlock()
	if b.engineCPUs == 0 {
		info, err := b.cli.Info(ctx)
		if err != nil {
			log.Printf("docker: read engine CPUs, running without a CPU limit: %v", err)
			return CPUShare{}
		}
		if info.NCPU <= 0 {
			log.Print("docker: engine reports no CPU count, running without a CPU limit")
			return CPUShare{}
		}
		b.engineCPUs = info.NCPU
	}
	return resolveCPUShare(nil, b.engineCPUs, b.Runs)
}

// CPUsDescription describes the CPU share run containers get, "" for none.
func (b *Provider) CPUsDescription(ctx context.Context) string {
	return b.cpuShare(ctx).String()
}
