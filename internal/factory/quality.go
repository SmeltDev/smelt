package factory

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/smeltdev/smelt/internal/config"
	"github.com/smeltdev/smelt/internal/quality"
)

// QualityConfig is the quality limits of a project's factory config, nil when
// the check is turned off.
func QualityConfig(f *config.FactoryConfig) *quality.Config {
	if !f.QualityEnabled() {
		return nil
	}
	cfg := QualityLimits(f)
	return &cfg
}

// QualityLimits is the limits in factory.quality, whether or not the check is
// enabled; a limit that is not set is left zero, which means its default.
func QualityLimits(f *config.FactoryConfig) quality.Config {
	if f == nil || f.Quality == nil {
		return quality.Config{}
	}
	return quality.Config{
		MaxFunctionComplexity: f.Quality.MaxFunctionComplexity,
		MaxErosionRise:        f.Quality.MaxErosionRise,
		MinCloneLines:         f.Quality.MinCloneLines,
	}
}

// Quality measures a run's change on the host, in the run's worktree: the
// tree as the round left it against the tree the run started from. It needs
// no sandbox and no toolchain, so it works on every backend.
type Quality struct {
	Config quality.Config
	// Dir is the run's worktree, which holds the implementer's work once a
	// round has pulled it.
	Dir string
	// Baseline is the commit the change is measured against.
	Baseline string
	// Warn, when set, hears of a measurement that failed.
	Warn func(msg string)

	// read are the seams to the files; nil uses git. Tests replace them.
	readTree func(ctx context.Context, dir string) ([]quality.File, error)
	readRev  func(ctx context.Context, dir, rev string) ([]quality.File, error)

	mu   sync.Mutex
	base *quality.Report
}

// Check measures the worktree and compares it with the baseline. A
// measurement that fails is an errored check, which is not fed back, and a
// warning.
func (q *Quality) Check(ctx context.Context) Check {
	start := time.Now()
	sum, err := q.measure(ctx)
	c := FromQuality(sum, time.Since(start), err)
	if err != nil && q.Warn != nil {
		q.Warn(fmt.Sprintf("quality was not measured: %v", err))
	}
	return c
}

func (q *Quality) measure(ctx context.Context) (quality.Summary, error) {
	before, err := q.baseline(ctx)
	if err != nil {
		return quality.Summary{}, err
	}
	readTree := q.readTree
	if readTree == nil {
		readTree = quality.ReadTree
	}
	files, err := readTree(ctx, q.Dir)
	if err != nil {
		return quality.Summary{}, err
	}
	after := quality.Measure(files, q.Config)
	findings := quality.Compare(before, after, quality.ChangedFunctions(before, after), q.Config)
	return quality.Summarize(before, after, findings), nil
}

// baseline is the measurement of the commit the run started from, made once.
// A failure is not kept, so the next round tries again.
func (q *Quality) baseline(ctx context.Context) (quality.Report, error) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.base != nil {
		return *q.base, nil
	}
	readRev := q.readRev
	if readRev == nil {
		readRev = quality.ReadRev
	}
	files, err := readRev(ctx, q.Dir, q.Baseline)
	if err != nil {
		return quality.Report{}, err
	}
	rep := quality.Measure(files, q.Config)
	q.base = &rep
	return rep, nil
}
