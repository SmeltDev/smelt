package quality

import (
	"fmt"
	"go/ast"
	"go/token"
	"hash/fnv"
	"io"
)

// Clone is a block of statements that also appears elsewhere, as an extra
// copy: the first occurrence, in file and source order, is the original and is
// not reported.
type Clone struct {
	File string
	// Func is the function holding the copy.
	Func string
	// Line and EndLine are the copy's first and last line; Lines is how many.
	Line, EndLine, Lines int
	// Sig identifies the copied statements, so the same copy is recognised in
	// another report wherever it sits.
	Sig uint64
}

// stmt is one normalized statement of a function.
type stmt struct {
	hash       uint64
	start, end int
}

// sequence is a function's statements in source order, nested ones included.
type sequence struct {
	file, fn string
	stmts    []stmt
}

// statements flattens body into its statements, parents before children. Each
// is hashed on its own shape, without its nested statements, with identifiers
// and literals replaced by placeholders: a copy with renamed variables hashes
// the same.
func statements(fset *token.FileSet, file, fn string, body *ast.BlockStmt) sequence {
	seq := sequence{file: file, fn: fn}
	ast.Inspect(body, func(n ast.Node) bool {
		s, ok := n.(ast.Stmt)
		if !ok {
			return true
		}
		if _, isBlock := s.(*ast.BlockStmt); isBlock {
			return true
		}
		h := fnv.New64a()
		ast.Inspect(s, func(c ast.Node) bool {
			if c == nil {
				return false
			}
			if _, nested := c.(ast.Stmt); nested && c != n {
				return false
			}
			return writeShape(h, c)
		})
		seq.stmts = append(seq.stmts, stmt{hash: h.Sum64(), start: fset.Position(s.Pos()).Line, end: fset.Position(s.End()).Line})
		return true
	})
	return seq
}

// bodyHash identifies a body's whole structure, nested statements and
// function literals included.
func bodyHash(body *ast.BlockStmt) string {
	h := fnv.New64a()
	ast.Inspect(body, func(c ast.Node) bool {
		if c == nil {
			return false
		}
		return writeShape(h, c)
	})
	return fmt.Sprintf("%016x", h.Sum64())
}

// put writes formatted text to a hash, which never fails.
func put(w io.Writer, format string, a ...any) { _, _ = fmt.Fprintf(w, format, a...) }

// writeShape writes what makes node n what it is, leaving out names and
// values. It reports whether to look inside n: a function literal is written
// as one token, since its statements are measured on their own.
func writeShape(w io.Writer, n ast.Node) bool {
	switch v := n.(type) {
	case *ast.Ident:
		_, _ = io.WriteString(w, "I;")
	case *ast.BasicLit:
		_, _ = io.WriteString(w, "L;")
	case *ast.FuncLit:
		_, _ = io.WriteString(w, "F;")
		return false
	case *ast.BinaryExpr:
		put(w, "%T%s;", n, v.Op)
	case *ast.UnaryExpr:
		put(w, "%T%s;", n, v.Op)
	case *ast.AssignStmt:
		put(w, "%T%s;", n, v.Tok)
	case *ast.IncDecStmt:
		put(w, "%T%s;", n, v.Tok)
	case *ast.BranchStmt:
		put(w, "%T%s;", n, v.Tok)
	default:
		put(w, "%T;", n)
	}
	return true
}

// rollBase is the multiplier of the rolling hash; arithmetic wraps at 64 bits.
const rollBase = 1099511628211

// occurrence is a window of statements: the k statements of sequence seq
// starting at start.
type occurrence struct{ seq, start int }

// lineKey is one line of one file.
type lineKey struct {
	file string
	line int
}

// findClones finds the extra copies of blocks of at least minStmts statements.
// A window of minStmts statements that occurs again, without overlapping, is
// duplicated; windows that overlap merge into one block, so a 20-statement
// copy is one clone and not six. lines counts each line of the copies once.
func findClones(seqs []sequence, minStmts int) (clones []Clone, lines int) {
	minStmts = max(minStmts, 1)
	groups := map[uint64][]occurrence{}
	var order []uint64
	for si, s := range seqs {
		for start, h := range rollingHashes(s.stmts, minStmts) {
			if _, seen := groups[h]; !seen {
				order = append(order, h)
			}
			groups[h] = append(groups[h], occurrence{si, start})
		}
	}
	covered := make([][]bool, len(seqs))
	for i, s := range seqs {
		covered[i] = make([]bool, len(s.stmts))
	}
	for _, h := range order {
		for _, o := range copiesOf(groups[h], seqs, minStmts) {
			for i := o.start; i < o.start+minStmts; i++ {
				covered[o.seq][i] = true
			}
		}
	}
	counted := map[lineKey]bool{}
	for si, s := range seqs {
		for _, run := range runs(covered[si]) {
			c := cloneOf(s, run[0], run[1])
			clones = append(clones, c)
			for l := c.Line; l <= c.EndLine; l++ {
				counted[lineKey{s.file, l}] = true
			}
		}
	}
	return clones, len(counted)
}

// rollingHashes is the hash of every window of k statements, in order.
func rollingHashes(stmts []stmt, k int) []uint64 {
	if len(stmts) < k {
		return nil
	}
	var top uint64 = 1
	for range k - 1 {
		top *= rollBase
	}
	var h uint64
	for _, s := range stmts[:k] {
		h = h*rollBase + s.hash
	}
	out := []uint64{h}
	for i := k; i < len(stmts); i++ {
		h = (h-stmts[i-k].hash*top)*rollBase + stmts[i].hash
		out = append(out, h)
	}
	return out
}

// copiesOf is the occurrences in group that repeat an earlier one. Occurrences
// that overlap an accepted one in the same sequence are neither: repeated
// statements in a row would otherwise match themselves.
func copiesOf(group []occurrence, seqs []sequence, k int) []occurrence {
	var accepted, copies []occurrence
	for _, o := range group {
		if overlaps(accepted, o, k) || !sameWindow(seqs, group[0], o, k) {
			continue
		}
		if len(accepted) > 0 {
			copies = append(copies, o)
		}
		accepted = append(accepted, o)
	}
	return copies
}

func overlaps(accepted []occurrence, o occurrence, k int) bool {
	for _, a := range accepted {
		if a.seq == o.seq && o.start < a.start+k && a.start < o.start+k {
			return true
		}
	}
	return false
}

// sameWindow guards against a hash collision by comparing the statements.
func sameWindow(seqs []sequence, a, b occurrence, k int) bool {
	for i := range k {
		if seqs[a.seq].stmts[a.start+i].hash != seqs[b.seq].stmts[b.start+i].hash {
			return false
		}
	}
	return true
}

// runs is the [first, last] indices of each run of true values.
func runs(covered []bool) [][2]int {
	var out [][2]int
	for i := 0; i < len(covered); i++ {
		if !covered[i] {
			continue
		}
		j := i
		for j+1 < len(covered) && covered[j+1] {
			j++
		}
		out = append(out, [2]int{i, j})
		i = j
	}
	return out
}

// cloneOf describes statements first..last of s as a block.
func cloneOf(s sequence, first, last int) Clone {
	c := Clone{File: s.file, Func: s.fn, Line: s.stmts[first].start, EndLine: s.stmts[first].end}
	sig := fnv.New64a()
	for _, st := range s.stmts[first : last+1] {
		c.EndLine = max(c.EndLine, st.end)
		put(sig, "%x;", st.hash)
	}
	c.Lines = c.EndLine - c.Line + 1
	c.Sig = sig.Sum64()
	return c
}
