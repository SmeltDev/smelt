// Package quality measures the code quality of a change, deterministically and
// without a model: cyclomatic complexity per function, the erosion that
// complexity concentrated in a few functions adds up to, and duplicated
// blocks. Go only; other languages are reported as not measured.
//
// Measure and Compare are pure over file contents. Reading those contents from
// a working tree or a git revision is source.go's job.
package quality

import (
	"cmp"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"math"
	"path"
	"slices"
	"strings"
)

// File is one source file to measure. Path is slash-separated and relative to
// the project root.
type File struct {
	Path    string
	Content []byte
}

// Function is one function or method with its measurements.
type Function struct {
	File     string
	Receiver string
	Name     string
	// Line is the line of the func keyword.
	Line       int
	Complexity int
	// Lines are the function's body lines: from the line of the body's opening
	// brace to the line of its closing brace, both included. The signature and
	// the doc comment are not part of it.
	Lines int
	// BodyHash identifies the body's structure, with identifiers and literals
	// replaced by placeholders, so a change to the logic changes it and a rename
	// does not.
	BodyHash string
	// key is Key, made unique within the report.
	key string
}

// Key identifies a function across two reports: its file, receiver and name.
// A rename or a move reads as a new function.
func (f Function) Key() string {
	if f.key != "" {
		return f.key
	}
	return f.baseKey()
}

func (f Function) baseKey() string { return f.File + "|" + f.Receiver + "|" + f.Name }

// Qualified is the function's name with its receiver, as a developer writes it.
func (f Function) Qualified() string {
	if f.Receiver == "" {
		return f.Name
	}
	return f.Receiver + "." + f.Name
}

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
	// Measured is false when the tree holds no Go to measure.
	Measured  bool
	Functions []Function
	// Erosion is the share, in percent, of the total weight (complexity times
	// the square root of body lines) held by functions whose complexity is
	// above 10.
	Erosion float64
	// Limit is the complexity above which a function counts toward
	// OverThreshold; OverThreshold is how many do.
	Limit         int
	OverThreshold int
	// CloneLines are the lines of duplicated blocks, counted once per extra copy.
	CloneLines int
	Clones     []Clone
	// Unparsed are the Go files that did not parse, which are left out. A tree
	// that does not compile is for the gates to report.
	Unparsed []string
}

// Worst is the n most complex functions, the longest first among equals.
func (r Report) Worst(n int) []Function {
	out := slices.Clone(r.Functions)
	slices.SortStableFunc(out, func(a, b Function) int {
		return cmp.Or(cmp.Compare(b.Complexity, a.Complexity), cmp.Compare(b.Lines, a.Lines), strings.Compare(a.Key(), b.Key()))
	})
	return out[:min(n, len(out))]
}

// Excluded reports whether a file is left out of measurement: tests, test data
// and vendored code. Generated files are recognised by their content.
func Excluded(p string) bool {
	if !strings.HasSuffix(p, ".go") || strings.HasSuffix(p, "_test.go") {
		return true
	}
	for _, seg := range strings.Split(path.Dir(p), "/") {
		if seg == "testdata" || seg == "vendor" {
			return true
		}
	}
	return false
}

// Measure measures files with cfg's limits.
func Measure(files []File, cfg Config) Report {
	cfg = cfg.withDefaults()
	rep := Report{Limit: cfg.MaxFunctionComplexity}
	sorted := slices.Clone(files)
	sortFiles(sorted)
	var seqs []sequence
	for _, f := range sorted {
		if !strings.HasSuffix(f.Path, ".go") {
			continue
		}
		rep.Measured = true
		if Excluded(f.Path) {
			continue
		}
		fns, fileSeqs, ok := measureFile(f)
		if !ok {
			rep.Unparsed = append(rep.Unparsed, f.Path)
			continue
		}
		rep.Functions = append(rep.Functions, fns...)
		seqs = append(seqs, fileSeqs...)
	}
	uniqueKeys(rep.Functions)
	rep.Erosion = erosion(rep.Functions)
	for _, fn := range rep.Functions {
		if fn.Complexity > cfg.MaxFunctionComplexity {
			rep.OverThreshold++
		}
	}
	rep.Clones, rep.CloneLines = findClones(seqs, cfg.MinCloneLines)
	return rep
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

// uniqueKeys makes every function's key distinct, as several init functions in
// one file would otherwise share one.
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

// measureFile parses one file and measures its functions. ok is false for a
// file that does not parse or is generated.
func measureFile(f File) (fns []Function, seqs []sequence, ok bool) {
	fset := token.NewFileSet()
	tree, err := parser.ParseFile(fset, f.Path, f.Content, parser.ParseComments|parser.SkipObjectResolution)
	if err != nil {
		return nil, nil, false
	}
	if ast.IsGenerated(tree) {
		return nil, nil, true
	}
	for _, d := range tree.Decls {
		decl, isFunc := d.(*ast.FuncDecl)
		if !isFunc || decl.Body == nil {
			continue
		}
		fn := Function{
			File: f.Path, Receiver: receiverName(decl), Name: decl.Name.Name,
			Line:       fset.Position(decl.Pos()).Line,
			Complexity: complexity(decl),
			Lines:      fset.Position(decl.Body.Rbrace).Line - fset.Position(decl.Body.Lbrace).Line + 1,
			BodyHash:   bodyHash(decl.Body),
		}
		fns = append(fns, fn)
		seqs = append(seqs, statements(fset, f.Path, fn.Qualified(), decl.Body))
	}
	return fns, seqs, true
}

// complexity is the function's cyclomatic complexity as gocyclo counts it: one,
// plus each if, for, range, non-default case, non-default select case, && and
// ||. Function literals count toward the function that holds them.
func complexity(decl *ast.FuncDecl) int {
	n := 1
	ast.Inspect(decl, func(node ast.Node) bool {
		switch v := node.(type) {
		case *ast.IfStmt, *ast.ForStmt, *ast.RangeStmt:
			n++
		case *ast.CaseClause:
			if v.List != nil {
				n++
			}
		case *ast.CommClause:
			if v.Comm != nil {
				n++
			}
		case *ast.BinaryExpr:
			if v.Op == token.LAND || v.Op == token.LOR {
				n++
			}
		}
		return true
	})
	return n
}

// receiverName is the type a method is declared on, without pointer or type
// arguments, "" for a plain function.
func receiverName(decl *ast.FuncDecl) string {
	if decl.Recv == nil || len(decl.Recv.List) == 0 {
		return ""
	}
	t := decl.Recv.List[0].Type
	for {
		switch v := t.(type) {
		case *ast.StarExpr:
			t = v.X
		case *ast.ParenExpr:
			t = v.X
		case *ast.IndexExpr:
			t = v.X
		case *ast.IndexListExpr:
			t = v.X
		case *ast.Ident:
			return v.Name
		default:
			return ""
		}
	}
}
