package quality

import (
	"fmt"
	"strings"
	"testing"

	"gotest.tools/v3/assert"
)

// blockTemplates are statements that differ in shape, so that no two windows
// of the block are alike. {a}, {b} and {c} are variable names.
var blockTemplates = []string{
	"{a} = {b} + {c}", "{a} = {b} - {c}", "{a} = {b} * {c}", "{a} = {b} / {c}",
	"{a} = {b} % {c}", "{a} = {b} & {c}", "{a} = {b} | {c}", "{a} = {b} ^ {c}",
	"{a} = {b} << {c}", "{a} = {b} >> {c}", "{a} += {b}", "{a} -= {b}",
	"{a} *= {b}", "{a} /= {b}", "{a}++", "{a}--", "{a} = []int{{{b}}}",
	"{a} = {b}[{c}]", "{a} = -{b}", "{a} = f({b})",
}

// block is n statements of blockTemplates over the given variable names, one
// per line.
func block(n int, a, b, c string) string {
	var sb strings.Builder
	for _, tpl := range blockTemplates[:n] {
		r := strings.NewReplacer("{{{b}}}", "{"+b+"}", "{a}", a, "{b}", b, "{c}", c)
		sb.WriteString("\t" + r.Replace(tpl) + "\n")
	}
	return sb.String()
}

// twoFunctions is two files, each with a function of the same n statements,
// the second with its variables renamed.
func twoFunctions(n int) []File {
	first := "package p\n\nfunc one(x, y, z int) {\n" + block(n, "x", "y", "z") + "}\n"
	second := "package p\n\nfunc two(p, q, r int) {\n" + block(n, "p", "q", "r") + "}\n"
	return []File{{Path: "a.go", Content: []byte(first)}, {Path: "b.go", Content: []byte(second)}}
}

func TestCopiedBlockWithRenamedVariablesIsOneClone(t *testing.T) {
	rep := Measure(twoFunctions(20), Config{})
	assert.Equal(t, len(rep.Clones), 1, "%+v", rep.Clones)
	c := rep.Clones[0]
	// The first occurrence is the original; the later one is the copy.
	assert.Equal(t, c.File, "b.go")
	assert.Equal(t, c.Func, "two")
	assert.Equal(t, c.Lines, 20)
	assert.Equal(t, rep.CloneLines, 20)
}

func TestTenLineCopiesAreNotClones(t *testing.T) {
	rep := Measure(twoFunctions(10), Config{})
	assert.Equal(t, len(rep.Clones), 0)
	assert.Equal(t, rep.CloneLines, 0)
}

func TestMinCloneLinesIsConfigurable(t *testing.T) {
	rep := Measure(twoFunctions(10), Config{MinCloneLines: 8})
	assert.Equal(t, len(rep.Clones), 1)
	assert.Equal(t, rep.CloneLines, 10)
}

func TestLinesAreCountedOncePerExtraCopy(t *testing.T) {
	files := twoFunctions(20)
	third := "package p\n\nfunc three(i, j, k int) {\n" + block(20, "i", "j", "k") + "}\n"
	files = append(files, File{Path: "c.go", Content: []byte(third)})
	rep := Measure(files, Config{})
	assert.Equal(t, len(rep.Clones), 2)
	assert.Equal(t, rep.CloneLines, 40)
}

func TestDifferentStatementInTheMiddleBreaksTheClone(t *testing.T) {
	files := twoFunctions(20)
	files[1].Content = []byte(strings.Replace(string(files[1].Content), "\tp += q\n", "\tp -= q\n", 1))
	assert.Equal(t, len(Measure(files, Config{}).Clones), 0)
}

func TestRepeatedStatementsDoNotMatchThemselves(t *testing.T) {
	src := "package p\n\nfunc f(x int) {\n" + strings.Repeat("\tx++\n", 40) + "}\n"
	rep := Measure([]File{{Path: "p.go", Content: []byte(src)}}, Config{})
	// Non-overlapping windows of 15 still repeat in 40 identical statements.
	assert.Assert(t, len(rep.Clones) <= 1, "%+v", rep.Clones)
	assert.Assert(t, rep.CloneLines <= 25, "%d", rep.CloneLines)
}

func TestLiteralsDoNotMakeBlocksDiffer(t *testing.T) {
	a := "package p\n\nfunc one(x int) {\n" + strings.Repeat("\tx = f(1)\n\tx++\n", 8) + "}\n"
	b := strings.NewReplacer("one", "two", "f(1)", `f("s")`).Replace(a)
	rep := Measure([]File{{Path: "a.go", Content: []byte(a)}, {Path: "b.go", Content: []byte(b)}}, Config{})
	assert.Assert(t, len(rep.Clones) >= 1, fmt.Sprint(rep.Clones))
}

func TestRollingHashesMatchDirectHashes(t *testing.T) {
	stmts := []stmt{{hash: 3}, {hash: 5}, {hash: 7}, {hash: 11}, {hash: 13}}
	got := rollingHashes(stmts, 3)
	assert.Equal(t, len(got), 3)
	for i, h := range got {
		want := rollingHashes(stmts[i:i+3], 3)[0]
		assert.Equal(t, h, want)
	}
	assert.Assert(t, rollingHashes(stmts, 6) == nil)
}
