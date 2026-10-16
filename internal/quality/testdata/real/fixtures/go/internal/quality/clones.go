package quality

import (
	"cmp"
	"hash/fnv"
	"slices"
	"strings"
)

// Clone is a block that also appears elsewhere, as an extra copy: the first
// occurrence, in file and line order, is the original and is not reported.
type Clone struct {
	File string
	// Func is the function holding the copy, "" when it is outside any.
	Func string
	// Line and EndLine are the copy's first and last line; Lines is how many.
	Line, EndLine, Lines int
	// Sig identifies the copied text, so the same copy is recognised in another
	// report wherever it sits.
	Sig uint64
}

// lineKey is one line of one file.
type lineKey struct {
	file string
	line int
}

// cloneReport turns the contract's clones into one Clone per extra copy, and
// counts the lines of the copies, each once. A block shorter than minLines is
// left out, as is a location in a file that is excluded or has no source.
func cloneReport(clones []ContractClone, k keeper, fns []Function, minLines int) (out []Clone, lines int) {
	counted := map[lineKey]bool{}
	for _, cl := range clones {
		if cl.Lines < minLines {
			continue
		}
		locs := keptLocations(cl.Locations, k)
		for _, loc := range locs[min(1, len(locs)):] {
			out = append(out, cloneAt(loc, k.sources[loc.File], fns))
			for l := loc.Start; l <= loc.End; l++ {
				counted[lineKey{loc.File, l}] = true
			}
		}
	}
	slices.SortStableFunc(out, func(a, b Clone) int {
		return cmp.Or(strings.Compare(a.File, b.File), cmp.Compare(a.Line, b.Line))
	})
	return out, len(counted)
}

// keptLocations are the locations that are measured, in file and line order.
// A block left with fewer than two has no copy.
func keptLocations(locs []Location, k keeper) []Location {
	var out []Location
	for _, l := range locs {
		if src, ok := k.source(l.File); ok && src != nil {
			out = append(out, l)
		}
	}
	if len(out) < 2 {
		return nil
	}
	slices.SortFunc(out, func(a, b Location) int {
		return cmp.Or(strings.Compare(a.File, b.File), cmp.Compare(a.Start, b.Start))
	})
	return out
}

// cloneAt describes the copy at loc, whose file holds src.
func cloneAt(loc Location, src []byte, fns []Function) Clone {
	sig := fnv.New64a()
	put(sig, "%s", strings.Join(strings.Fields(strings.Join(lineRange(src, loc.Start, loc.End), "\n")), " "))
	return Clone{
		File: loc.File, Func: enclosing(fns, loc.File, loc.Start),
		Line: loc.Start, EndLine: loc.End, Lines: loc.End - loc.Start + 1,
		Sig: sig.Sum64(),
	}
}

// enclosing is the name of the function of file that holds line, "" for none.
func enclosing(fns []Function, file string, line int) string {
	for _, f := range fns {
		if f.File == file && f.Line <= line && line <= f.End {
			return f.Name
		}
	}
	return ""
}
