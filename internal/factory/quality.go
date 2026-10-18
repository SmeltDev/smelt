package factory

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/smeltdev/smelt/internal/config"
	"github.com/smeltdev/smelt/internal/quality"
	"github.com/smeltdev/smelt/internal/review"
	"github.com/smeltdev/smelt/internal/sidecar"
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
	q := f.Quality
	return quality.Config{
		MaxFunctionComplexity: q.MaxFunctionComplexity,
		MaxErosionRise:        q.MaxErosionRise,
		MinCloneLines:         q.MinCloneLines,
		Command:               f.QualityCommand(),
		Exclude: quality.ExcludeConfig{
			Exclude: q.Exclude, Include: q.Include,
			NoDefaults: q.DefaultExcludes != nil && !*q.DefaultExcludes,
		},
	}
}

// Quality measures a run's change in the implementer's sandbox, like the gates:
// the tree as the round left it against the tree the run started from. The
// provider (lizard, or the project's command) runs there, so it sees the
// toolchain and files the implementer worked with.
type Quality struct {
	Config quality.Config
	Exec   review.Execer
	// Entry is the implementer's sidecar, which holds its work.
	Entry *sidecar.PoolEntry
	// Baseline is the commit on Entry the change is measured against. Prepare
	// sets it when it measures the baseline.
	Baseline string
	// Warn, when set, hears of a measurement that failed.
	Warn func(msg string)

	mu   sync.Mutex
	base *quality.Report
}

func (q *Quality) measurer() SandboxMeasurer {
	return SandboxMeasurer{Exec: q.Exec, Entry: q.Entry, Provider: ProviderFor(q.Config), Config: q.Config}
}

// MeasureBaseline measures the sandbox as it is now, which is the baseline
// commit, and keeps the result for every round. rev is that commit. A failure
// is warned and not kept: the first round that needs the baseline tries again.
func (q *Quality) MeasureBaseline(ctx context.Context, rev string) {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.Baseline = rev
	m, err := q.measurer().Measure(ctx, "")
	if err != nil {
		q.warn(fmt.Sprintf("the baseline quality was not measured: %v (it is tried again after the first round)", err))
		return
	}
	rep := quality.Measure(m.Contract, m.Sources, q.Config)
	q.base = &rep
}

func (q *Quality) warn(msg string) {
	if q.Warn != nil {
		q.Warn(msg)
	}
}

// Check measures the sandbox and compares it with the baseline. A measurement
// that cannot be made is a not_measured check, which never blocks a round, and
// a warning.
func (q *Quality) Check(ctx context.Context) Check {
	start := time.Now()
	sum, err := q.measure(ctx)
	c := FromQuality(sum, time.Since(start), err)
	if err != nil {
		q.warn(fmt.Sprintf("quality was not measured: %v", err))
	}
	return c
}

func (q *Quality) measure(ctx context.Context) (quality.Summary, error) {
	before, err := q.baseline(ctx)
	if err != nil {
		return quality.Summary{}, err
	}
	m, err := q.measurer().Measure(ctx, "")
	if err != nil {
		return quality.Summary{}, err
	}
	after := quality.Measure(m.Contract, m.Sources, q.Config)
	findings := quality.Compare(before, after, quality.ChangedFunctions(before, after), q.Config)
	return quality.Summarize(before, after, findings), nil
}

// baseline is the measurement of the commit the run started from. It is made
// once, normally by MeasureBaseline; one that failed is made here, from the
// commit in a temporary worktree, since the tree has moved on.
func (q *Quality) baseline(ctx context.Context) (quality.Report, error) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.base != nil {
		return *q.base, nil
	}
	if q.Baseline == "" {
		return quality.Report{}, errors.New("no baseline commit to measure")
	}
	m, err := q.measurer().Measure(ctx, q.Baseline)
	if err != nil {
		return quality.Report{}, err
	}
	rep := quality.Measure(m.Contract, m.Sources, q.Config)
	q.base = &rep
	return rep, nil
}
