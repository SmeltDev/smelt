package lizard

import (
	"io/fs"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"gotest.tools/v3/assert"

	"github.com/smeltdev/smelt/internal/quality"
)

const realDir = "../testdata/real"

// realCapture is lizard's real output for a fixture project, as the script
// prints it, with the sources it was run over. The captures are written by
// scripts/capture-lizard-fixtures.sh and never by hand; a missing one skips
// the test rather than substituting made-up output.
func realCapture(t *testing.T, name string) (stdout string, sources []quality.File) {
	t.Helper()
	read := func(suffix string) string {
		data, err := os.ReadFile(filepath.Join(realDir, name+suffix))
		if os.IsNotExist(err) {
			t.Skipf("no real lizard capture %s%s: run scripts/capture-lizard-fixtures.sh", name, suffix)
		}
		assert.NilError(t, err)
		return string(data)
	}
	stdout = strings.Join([]string{markVersion, read(".version.txt"), markCSV, read(".csv"), markDup, read(".dup.txt")}, "\n")
	root := filepath.Join(realDir, "fixtures", name)
	assert.NilError(t, filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		content, err := os.ReadFile(p)
		rel, _ := filepath.Rel(root, p)
		sources = append(sources, quality.File{Path: filepath.ToSlash(rel), Content: content})
		return err
	}))
	return stdout, sources
}

func TestRealPythonCapture(t *testing.T) {
	stdout, sources := realCapture(t, "python")
	c, err := Parse(stdout)
	assert.NilError(t, err)
	assert.Equal(t, c.Provider, "lizard 1.24.1")
	assert.DeepEqual(t, c.Languages, []string{"python"})
	assert.Equal(t, len(c.NotMeasured), 0, "%v", c.NotMeasured)

	rep := quality.Measure(c, sources, quality.Config{MinCloneLines: 10})
	assert.Assert(t, rep.Measured)
	// generated.py and tests/ are left out, leaving four functions.
	assert.Equal(t, len(rep.Functions), 4)
	assert.Equal(t, rep.Limit, 15)
	assert.Equal(t, rep.OverThreshold, 1)
	assert.Equal(t, rep.Worst(1)[0].Name, "classify")
	assert.Equal(t, rep.Worst(1)[0].Complexity, 20)
	assert.Equal(t, rep.Worst(1)[0].Language, "python")
	assert.Assert(t, math.Abs(rep.Erosion-66.5) < 0.1, rep.Erosion)
	assert.Equal(t, len(rep.Clones), 1)
	assert.Equal(t, rep.Clones[0].Func, "total_for_quote")
	assert.Equal(t, rep.CloneLines, 14)

	sum := quality.Summarize(rep, rep, nil)
	assert.Equal(t, sum.Line(), "erosion 66.5% → 66.5% · functions over 15: 1 → 1 · clone lines 14 → 14")
	// The default minimum is 15 lines, which the 14-line block is under.
	assert.Equal(t, quality.Measure(c, sources, quality.Config{}).CloneLines, 0)
}

func TestRealTypeScriptCapture(t *testing.T) {
	stdout, sources := realCapture(t, "typescript")
	c, err := Parse(stdout)
	assert.NilError(t, err)
	assert.DeepEqual(t, c.Languages, []string{"typescript"})

	rep := quality.Measure(c, sources, quality.Config{})
	// gen.ts is generated and router.spec.ts is a test.
	assert.Equal(t, len(rep.Functions), 4)
	assert.Equal(t, rep.OverThreshold, 1)
	assert.Equal(t, rep.Worst(1)[0].Name, "route")
	assert.Equal(t, rep.Worst(1)[0].Complexity, 16)
	assert.Assert(t, math.Abs(rep.Erosion-58.6) < 0.1, rep.Erosion)
	assert.Equal(t, rep.CloneLines, 22, "the 22-line copy is over the default minimum")
	assert.Equal(t, rep.Clones[0].Func, "quoteTotal")
}

func TestRealGoCapture(t *testing.T) {
	stdout, sources := realCapture(t, "go")
	c, err := Parse(stdout)
	assert.NilError(t, err)
	assert.DeepEqual(t, c.Languages, []string{"go"})
	assert.Equal(t, len(c.NotMeasured), 0, "an empty duplicate report is still a report: %v", c.NotMeasured)

	rep := quality.Measure(c, sources, quality.Config{})
	assert.Assert(t, rep.Measured)
	assert.Assert(t, len(rep.Functions) > 40)
	for _, f := range rep.Functions {
		assert.Equal(t, f.Language, "go")
		assert.Assert(t, !strings.HasSuffix(f.File, "_test.go"), f.File)
		assert.Assert(t, f.Name != "")
		assert.Assert(t, f.Hash != "")
	}
	assert.Assert(t, rep.Erosion >= 0 && rep.Erosion < 100)
	// Closures are reported by lizard without a name.
	var anonymous int
	for _, f := range rep.Functions {
		if f.Name == "(anonymous)" {
			anonymous++
		}
	}
	assert.Assert(t, anonymous > 0)
}

// The tests below use output written by hand in lizard's documented layout.
// They check the parser's rules and error paths, not that lizard prints this:
// the real captures above do that.

func synthetic(version, csv, dup string) string {
	return strings.Join([]string{markVersion, version, markCSV, csv, markDup, dup}, "\n")
}

func TestSyntheticCSVRowsBecomeContractFunctions(t *testing.T) {
	csv := `12,3,80,2,14,"f@5-18@./pkg/a.go","./pkg/a.go","f","f a int",5,18` + "\n" +
		`4,1,20,0,4,"@30-33@lib/b.rb","lib/b.rb","","",30,33` + "\n" +
		`3,2,20,0,3,"m@1-3@x/y.weird","x/y.weird","Type::m","Type::m",1,3` + "\n"
	c, err := Parse(synthetic("1.2.3", csv, "Total duplicate rate: 0%"))
	assert.NilError(t, err)
	assert.Equal(t, c.Provider, "lizard 1.2.3")
	assert.DeepEqual(t, c.Functions, []quality.ContractFunction{
		{File: "pkg/a.go", Name: "f", Language: "go", Start: 5, End: 18, Complexity: 3, Lines: 12},
		{File: "lib/b.rb", Name: "(anonymous)", Language: "ruby", Start: 30, End: 33, Complexity: 1, Lines: 4},
		{File: "x/y.weird", Name: "Type::m", Language: "weird", Start: 1, End: 3, Complexity: 2, Lines: 3},
	})
	assert.DeepEqual(t, c.Languages, []string{"go", "ruby", "weird"})
}

func TestSyntheticCSVWithAHeaderIsAccepted(t *testing.T) {
	csv := "NLOC,CCN,token,PARAM,length,location,file,function,long_name,start,end\n" +
		`2,1,9,1,2,"s@1-2@a.py","a.py","s","s( x )",1,2` + "\n"
	c, err := Parse(synthetic("1.0", csv, "Total duplicate rate: 0%"))
	assert.NilError(t, err)
	assert.Equal(t, len(c.Functions), 1)
}

func TestSyntheticBadOutputIsAnErrorNotAnEmptyContract(t *testing.T) {
	tests := []struct{ name, in, want string }{
		{"no sections", "lizard: command not found", "no function report"},
		{"short row", synthetic("1", "1,2,3\n", ""), "has 3 columns"},
		{"not a number", synthetic("1", `x,1,2,3,4,"l","a.go","f","f",1,2`+"\n"+`y,1,2,3,4,"l","a.go","f","f",1,2`+"\n", ""), "not a number"},
		{"unterminated quote", synthetic("1", "1,1,1,1,1,\"oops\n", ""), "CSV"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := Parse(tt.in)
			assert.ErrorContains(t, err, tt.want)
		})
	}
}

func TestSyntheticEmptyCSVIsAnEmptyContract(t *testing.T) {
	c, err := Parse(synthetic("1.0", "", "Total duplicate rate: 0%"))
	assert.NilError(t, err)
	assert.Assert(t, !c.Measured())
}

func TestSyntheticDuplicateBlocks(t *testing.T) {
	dup := `Duplicates
===================================
Duplicate block:
--------------------------
./a.py:5 ~ 27
b.py:90 ~ 112
^^^^^^^^^^^^^^^^^^^^^^^^^^

Duplicate block:
--------------------------
a.py:40 ~ 44
b.py:50 ~ 59
c.py:1 ~ 5
^^^^^^^^^^^^^^^^^^^^^^^^^^

Total duplicate rate: 3.00%
`
	c, err := Parse(synthetic("1", `2,1,9,1,2,"s@1-2@a.py","a.py","s","s( x )",1,2`+"\n", dup))
	assert.NilError(t, err)
	assert.Equal(t, len(c.NotMeasured), 0)
	assert.DeepEqual(t, c.Clones, []quality.ContractClone{
		{Lines: 23, Locations: []quality.Location{{File: "a.py", Start: 5, End: 27}, {File: "b.py", Start: 90, End: 112}}},
		{Lines: 5, Locations: []quality.Location{{File: "a.py", Start: 40, End: 44}, {File: "b.py", Start: 50, End: 59}, {File: "c.py", Start: 1, End: 5}}},
	})
}

func TestSyntheticUnknownDuplicateFormatMeansClonesAreNotMeasured(t *testing.T) {
	csv := `2,1,9,1,2,"s@1-2@a.py","a.py","s","s( x )",1,2` + "\n"
	for name, dup := range map[string]string{"empty": "", "other format": "found 3 clones somewhere\n"} {
		t.Run(name, func(t *testing.T) {
			c, err := Parse(synthetic("1", csv, dup))
			assert.NilError(t, err, "the functions are still measured")
			assert.Equal(t, len(c.Functions), 1)
			assert.Equal(t, len(c.Clones), 0)
			assert.Equal(t, len(c.NotMeasured), 1)
			assert.Assert(t, strings.HasPrefix(c.NotMeasured[0], "clones: "), c.NotMeasured)
		})
	}
}

func TestScriptSkipsTheDefaultExclusionsAndRunsLizardThreeTimes(t *testing.T) {
	script := Script(quality.Excluder{})
	for _, want := range []string{
		"git -c core.quotepath=off ls-files -co --exclude-standard",
		"vendor/*|*/vendor/*", "*_test.go|*/*_test.go",
		"*.go|", "*.py|", ") ;; *) continue;; esac",
		"lizard --version", "lizard --csv", "lizard -Eduplicate", "exit 127", NotInstalledHint,
	} {
		assert.Assert(t, strings.Contains(script, want), "%q missing from:\n%s", want, script)
	}
	assert.Assert(t, !strings.Contains(script, "*.md"), "text formats lizard happens to read are not measured")
	off, err := quality.NewExcluder(quality.ExcludeConfig{NoDefaults: true})
	assert.NilError(t, err)
	assert.Assert(t, !strings.Contains(Script(off), "vendor/*"), "no path prefilter when the defaults are off")
}

// TestScriptEndToEnd runs the script itself over a git repository, with
// whatever lizard is on PATH; it skips on a machine without lizard.
func TestScriptEndToEnd(t *testing.T) {
	for _, tool := range []string{"lizard", "git", "sh", "xargs"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skipf("%s is not installed", tool)
		}
	}
	dir := t.TempDir()
	run := func(script string) string {
		cmd := exec.Command("sh", "-c", script)
		cmd.Dir = dir
		out, err := cmd.CombinedOutput()
		assert.NilError(t, err, string(out))
		return string(out)
	}
	_, sources := realCapture(t, "python")
	for _, f := range sources {
		p := filepath.Join(dir, filepath.FromSlash(f.Path))
		assert.NilError(t, os.MkdirAll(filepath.Dir(p), 0o755))
		assert.NilError(t, os.WriteFile(p, f.Content, 0o644))
	}
	assert.NilError(t, os.WriteFile(filepath.Join(dir, ".gitignore"), []byte("ignored.py\n"), 0o644))
	assert.NilError(t, os.WriteFile(filepath.Join(dir, "ignored.py"), []byte("def i(x):\n    return x\n"), 0o644))
	run("git init -q && git add -A")

	c, err := Parse(run(Script(quality.Excluder{})))
	assert.NilError(t, err)
	var files []string
	for _, f := range c.Functions {
		files = append(files, f.File+":"+f.Name)
	}
	slices.Sort(files)
	// tests/ is skipped by the script, ignored.py by git; generated.py is left
	// for Measure, which reads its content.
	assert.DeepEqual(t, files, []string{"app/generated.py:g", "app/orders.py:classify", "app/orders.py:short", "app/orders.py:total_for_cart", "app/orders.py:total_for_quote"})
	assert.Assert(t, strings.HasPrefix(c.Provider, "lizard "))
}
