// Package quality measures the code quality of a change, deterministically and
// without a model: cyclomatic complexity per function, the erosion that
// complexity concentrated in a few functions adds up to, and duplicated
// blocks.
//
// smelt never parses a language. A provider prints a Contract (functions and
// clones, see contract.go) and everything here works from that. Measure and
// Compare are pure over a contract and the source files it names; running a
// provider and fetching those files is the caller's job.
package quality

import (
	"cmp"
	"fmt"
	"math"
	"slices"
	"strings"
)

// Function is one function or method with its measurements.
type Function struct {
	File string
	// Name is the provider's name for it, qualified by its type when the
	// provider does so.
	Name     string
	Language string
	// Line and End are the function's first and last line.
	Line, End  int
	Complexity int
	// Lines are the function's non-blank, non-comment lines.
	Lines int
	// Hash identifies the function's source with its name left out, so a change
	// to the logic changes it and a rename does not.
	Hash string
	// key is Key, made unique within the report.
	key string
}

// Key identifies a function across two reports: its file and name. A rename or
// a move reads as a new function.
func (f Function) Key() string {
	if f.key != "" {
		return f.key
	}
	return f.baseKey()
}

func (f Function) baseKey() string { return f.File + "|" + f.Name }

// weight is the function's share of the erosion sum: complexity times the
// square root of its length, so that a long complex function counts for more
// than a short one without length dominating.
func (f Function) weight() float64 {
	return float64(f.Complexity) * math.Sqrt(float64(f.Lines))
}

// erosionFloor is the complexity above which a function counts as eroded.
const erosionFloor = 10

// Report is the measurement of one tree.
type Report struct {
	// Measured is false when the provider measured nothing, as when no language
	// of the tree is one it knows.
	Measured  bool
	Functions []Function
	// Erosion is the share, in percent, of the total weight (complexity times
	// the square root of lines) held by functions whose complexity is above 10.
	Erosion float64
	// Limit is the complexity above which a function counts toward
	// OverThreshold; OverThreshold is how many do.
	Limit         int
	OverThreshold int
	// CloneLines are the lines of duplicated blocks, counted once per extra copy.
	CloneLines int
	Clones     []Clone
	// NotMeasured are the files, or the parts, that were left unmeasured, with
	// the reason where there is one.
	NotMeasured []string
}

// Worst is the n most complex functions, the longest first among equals.
func (r Report) Worst(n int) []Function {
	out := slices.Clone(r.Functions)
	slices.SortStableFunc(out, func(a, b Function) int {
		return cmp.Or(cmp.Compare(b.Complexity, a.Complexity), cmp.Compare(b.Lines, a.Lines), strings.Compare(a.Key(), b.Key()))
	})
	return out[:min(n, len(out))]
}

// Measure builds the report of a contract. sources are the files it names;
// functions and clones in a file that is missing from them cannot be hashed,
// and are reported as not measured. Excluded and generated files are dropped.
func Measure(c Contract, sources []File, cfg Config) Report {
	cfg = cfg.withDefaults()
	rep := Report{Limit: cfg.MaxFunctionComplexity, Measured: c.Measured(), NotMeasured: slices.Clone(c.NotMeasured)}
	kept := newKeeper(cfg, sources)
	for _, f := range c.Functions {
		src, ok := kept.source(f.File)
		if !ok {
			rep.NotMeasured = appendUnique(rep.NotMeasured, kept.missing(f.File))
			continue
		}
		if src == nil {
			continue
		}
		rep.Functions = append(rep.Functions, Function{
			File: f.File, Name: f.Name, Language: f.Language, Line: f.Start, End: f.End,
			Complexity: f.Complexity, Lines: f.Lines, Hash: FunctionHash(src, f.Start, f.End, f.Name),
		})
	}
	slices.SortStableFunc(rep.Functions, func(a, b Function) int {
		return cmp.Or(strings.Compare(a.File, b.File), cmp.Compare(a.Line, b.Line))
	})
	uniqueKeys(rep.Functions)
	rep.Erosion = erosion(rep.Functions)
	for _, fn := range rep.Functions {
		if fn.Complexity > cfg.MaxFunctionComplexity {
			rep.OverThreshold++
		}
	}
	rep.Clones, rep.CloneLines = cloneReport(c.Clones, kept, rep.Functions, cfg.MinCloneLines)
	return rep
}

// keeper answers, for a path, whether it is measured and what its source is.
type keeper struct {
	ex      Excluder
	sources map[string][]byte
}

func newKeeper(cfg Config, sources []File) keeper {
	ex, err := NewExcluder(cfg.Exclude)
	if err != nil {
		// The config was validated when it was loaded; a glob that still fails
		// leaves nothing out rather than everything.
		ex = Excluder{}
	}
	k := keeper{ex: ex, sources: make(map[string][]byte, len(sources))}
	for _, f := range sources {
		k.sources[f.Path] = f.Content
	}
	return k
}

// source is the content of p. ok is false when p should be measured but its
// source is missing; a nil content with ok true means p is left out.
func (k keeper) source(p string) (content []byte, ok bool) {
	if k.ex.Path(p) {
		return nil, true
	}
	content, found := k.sources[p]
	if !found {
		return nil, false
	}
	if Generated(content) {
		return nil, true
	}
	return content, true
}

func (keeper) missing(p string) string { return p + " (source not available)" }

func appendUnique(list []string, s string) []string {
	if slices.Contains(list, s) {
		return list
	}
	return append(list, s)
}

// erosion is the percentage of the total weight held by functions above the
// erosion floor, 0 for a tree with no functions.
func erosion(fns []Function) float64 {
	var eroded, total float64
	for _, fn := range fns {
		w := fn.weight()
		total += w
		if fn.Complexity > erosionFloor {
			eroded += w
		}
	}
	if total == 0 {
		return 0
	}
	return 100 * eroded / total
}

// uniqueKeys makes every function's key distinct, as several functions of one
// name in one file would otherwise share one.
func uniqueKeys(fns []Function) {
	seen := map[string]int{}
	for i := range fns {
		base := fns[i].baseKey()
		seen[base]++
		fns[i].key = base
		if n := seen[base]; n > 1 {
			fns[i].key = fmt.Sprintf("%s#%d", base, n)
		}
	}
}
