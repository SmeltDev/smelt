package quality

import "context"

// Assessment is the measurement of a working tree, and when it was compared
// with a revision, that revision's and the findings.
type Assessment struct {
	After Report
	// Before and Findings are set only when a base revision was given.
	Before   *Report
	Findings []Finding
}

// Assess measures the working tree at dir. With a non-empty base it also
// measures that revision and compares the two.
func Assess(ctx context.Context, dir, base string, cfg Config) (Assessment, error) {
	files, err := ReadTree(ctx, dir)
	if err != nil {
		return Assessment{}, err
	}
	a := Assessment{After: Measure(files, cfg)}
	if base == "" {
		return a, nil
	}
	baseFiles, err := ReadRev(ctx, dir, base)
	if err != nil {
		return Assessment{}, err
	}
	before := Measure(baseFiles, cfg)
	a.Before = &before
	a.Findings = Compare(before, a.After, ChangedFunctions(before, a.After), cfg)
	return a, nil
}
