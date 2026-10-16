package quality

import "context"

// Measurement is what a provider measured and the sources it measured.
type Measurement struct {
	Contract Contract
	Sources  []File
}

// Measurer measures a tree: the working tree when rev is empty, otherwise that
// git revision.
type Measurer interface {
	Measure(ctx context.Context, rev string) (Measurement, error)
}

// Assessment is the measurement of a working tree, and when it was compared
// with a revision, that revision's and the findings.
type Assessment struct {
	After Report
	// Before and Findings are set only when a base revision was given.
	Before   *Report
	Findings []Finding
}

// Assess measures the working tree. With a non-empty base it also measures
// that revision and compares the two.
func Assess(ctx context.Context, m Measurer, base string, cfg Config) (Assessment, error) {
	now, err := m.Measure(ctx, "")
	if err != nil {
		return Assessment{}, err
	}
	a := Assessment{After: Measure(now.Contract, now.Sources, cfg)}
	if base == "" {
		return a, nil
	}
	then, err := m.Measure(ctx, base)
	if err != nil {
		return Assessment{}, err
	}
	before := Measure(then.Contract, then.Sources, cfg)
	a.Before = &before
	a.Findings = Compare(before, a.After, ChangedFunctions(before, a.After), cfg)
	return a, nil
}
