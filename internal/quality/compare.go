package quality

import (
	"fmt"
	"slices"
	"strings"
)

// Defaults of Config, the limits a project gets without setting any.
const (
	DefaultMaxFunctionComplexity = 15
	DefaultMaxErosionRise        = 1.0
	DefaultMinCloneLines         = 15
)

// worstShown is how many of the most complex functions a Summary lists.
const worstShown = 5

// Config is what counts as worse. A zero field means its default.
type Config struct {
	// MaxFunctionComplexity is the complexity above which a function is too
	// complex.
	MaxFunctionComplexity int
	// MaxErosionRise is how many percentage points erosion may rise by.
	MaxErosionRise float64
	// MinCloneLines is the shortest duplicated block, in statements.
	MinCloneLines int
}

func (c Config) withDefaults() Config {
	if c.MaxFunctionComplexity <= 0 {
		c.MaxFunctionComplexity = DefaultMaxFunctionComplexity
	}
	if c.MaxErosionRise <= 0 {
		c.MaxErosionRise = DefaultMaxErosionRise
	}
	if c.MinCloneLines <= 0 {
		c.MinCloneLines = DefaultMinCloneLines
	}
	return c
}

// Severity is how much a finding matters. Only medium sends work back.
type Severity string

// Finding severities.
const (
	SeverityMedium Severity = "medium"
	SeverityLow    Severity = "low"
)

// Finding is one way a change made quality worse.
type Finding struct {
	Severity Severity
	// File and Line locate it; File is "" for a finding about the whole tree.
	File string
	Line int
	// Func is the function concerned, with its receiver, "" for none.
	Func string
	// Message says what is wrong and, for a medium finding, what to do.
	Message string
}

// Location is File:Line, or "" for a finding about the whole tree.
func (f Finding) Location() string {
	switch {
	case f.File == "":
		return ""
	case f.Line <= 0:
		return f.File
	}
	return fmt.Sprintf("%s:%d", f.File, f.Line)
}

// splitAdvice is what a medium finding about a complex function tells the
// implementer to do.
const splitAdvice = "split it: a handler per case, or extract the decision into a pure function"

// ChangedFunctions is the functions of after that are new or whose body
// differs from the same function's in before. It is the one definition of
// changed that Compare and the factory share.
func ChangedFunctions(before, after Report) []Function {
	prior := make(map[string]string, len(before.Functions))
	for _, f := range before.Functions {
		prior[f.Key()] = f.BodyHash
	}
	var out []Function
	for _, f := range after.Functions {
		if hash, ok := prior[f.Key()]; !ok || hash != f.BodyHash {
			out = append(out, f)
		}
	}
	return out
}

// Compare lists how after is worse than before. changed is ChangedFunctions of
// the two. A function over the limit is a finding when the change added it or
// made it more complex; one that was already over and was touched without
// growing is not. Only the medium findings are meant to be fixed.
func Compare(before, after Report, changed []Function, cfg Config) []Finding {
	cfg = cfg.withDefaults()
	if !before.Measured || !after.Measured {
		return nil
	}
	var out []Finding
	out = append(out, complexFindings(before, changed, cfg)...)
	if rise := after.Erosion - before.Erosion; rise > cfg.MaxErosionRise {
		out = append(out, Finding{
			Severity: SeverityMedium,
			Message: fmt.Sprintf("erosion rose from %.1f%% to %.1f%% (+%.1f points, at most +%.1f allowed): too much of the code is in complex functions. Lower the complexity of the functions this change touched; %s. %s",
				before.Erosion, after.Erosion, rise, cfg.MaxErosionRise, splitAdvice, worstList(after)),
		})
	}
	return append(out, cloneFindings(before, after, changed)...)
}

// complexFindings is the findings about changed functions over the limit.
func complexFindings(before Report, changed []Function, cfg Config) []Finding {
	prior := make(map[string]int, len(before.Functions))
	for _, f := range before.Functions {
		prior[f.Key()] = f.Complexity
	}
	var out []Finding
	for _, f := range changed {
		if f.Complexity <= cfg.MaxFunctionComplexity {
			continue
		}
		was, existed := prior[f.Key()]
		if existed && f.Complexity <= was {
			continue
		}
		origin := "is new"
		if existed {
			origin = fmt.Sprintf("grew from %d", was)
		}
		out = append(out, Finding{
			Severity: SeverityMedium, File: f.File, Line: f.Line, Func: f.Qualified(),
			Message: fmt.Sprintf("%s has cyclomatic complexity %d (%s; limit %d): %s",
				f.Qualified(), f.Complexity, origin, cfg.MaxFunctionComplexity, splitAdvice),
		})
	}
	return out
}

// cloneFindings is a low finding for each copied block in a changed function
// that the baseline did not have.
func cloneFindings(before, after Report, changed []Function) []Finding {
	known := map[uint64]int{}
	for _, c := range before.Clones {
		known[c.Sig]++
	}
	touched := map[string]bool{}
	for _, f := range changed {
		touched[f.File+"|"+f.Qualified()] = true
	}
	var out []Finding
	for _, c := range after.Clones {
		if known[c.Sig] > 0 {
			known[c.Sig]--
			continue
		}
		if !touched[c.File+"|"+c.Func] {
			continue
		}
		out = append(out, Finding{
			Severity: SeverityLow, File: c.File, Line: c.Line, Func: c.Func,
			Message: fmt.Sprintf("%d lines here repeat a block that is already elsewhere; consider sharing the code", c.Lines),
		})
	}
	return out
}

// worstList names the most complex functions of r for a message.
func worstList(r Report) string {
	worst := r.Worst(3)
	parts := make([]string, 0, len(worst))
	for _, f := range worst {
		parts = append(parts, fmt.Sprintf("%s (%s, complexity %d)", f.Qualified(), locationOf(f), f.Complexity))
	}
	if len(parts) == 0 {
		return ""
	}
	return "Most complex now: " + strings.Join(parts, ", ") + "."
}

func locationOf(f Function) string { return fmt.Sprintf("%s:%d", f.File, f.Line) }

// Summary is a change's before and after, for display and for the status
// JSON.
type Summary struct {
	// Measured is false when the project has no Go, so there is nothing to say.
	Measured                                bool
	ErosionBefore, ErosionAfter             float64
	Limit                                   int
	OverThresholdBefore, OverThresholdAfter int
	CloneLinesBefore, CloneLinesAfter       int
	// Worst are the most complex functions after the change.
	Worst    []Function
	Findings []Finding
}

// Summarize puts two reports and the findings of comparing them together.
func Summarize(before, after Report, findings []Finding) Summary {
	return Summary{
		Measured:      before.Measured || after.Measured,
		ErosionBefore: before.Erosion, ErosionAfter: after.Erosion,
		Limit:               after.Limit,
		OverThresholdBefore: before.OverThreshold, OverThresholdAfter: after.OverThreshold,
		CloneLinesBefore: before.CloneLines, CloneLinesAfter: after.CloneLines,
		Worst:    after.Worst(worstShown),
		Findings: slices.Clone(findings),
	}
}

// Line is the one-line account shown with every run.
func (s Summary) Line() string {
	if !s.Measured {
		return "not measured (no Go code)"
	}
	return fmt.Sprintf("erosion %.1f%% → %.1f%% · functions over %d: %d → %d · clone lines %d → %d",
		s.ErosionBefore, s.ErosionAfter, s.Limit, s.OverThresholdBefore, s.OverThresholdAfter, s.CloneLinesBefore, s.CloneLinesAfter)
}

// Failing are the medium findings, the ones to fix.
func (s Summary) Failing() []Finding {
	return slices.DeleteFunc(slices.Clone(s.Findings), func(f Finding) bool { return f.Severity != SeverityMedium })
}
