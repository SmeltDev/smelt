package quality

import (
	"strings"
	"testing"

	"gotest.tools/v3/assert"
)

func fn(file, name string, cc int, hash string) Function {
	return Function{File: file, Name: name, Line: 10, Complexity: cc, Lines: 20, Hash: hash}
}

func report(erosion float64, fns ...Function) Report {
	return Report{Measured: true, Erosion: erosion, Functions: fns, Limit: 15}
}

func TestCompareFunctionRules(t *testing.T) {
	tests := []struct {
		name         string
		before       []Function
		after        []Function
		wantSeverity Severity // "" for no finding
		wantContains string
	}{
		{"new function over the limit", nil, []Function{fn("a.go", "f", 16, "h")}, SeverityMedium, "complexity 16 (is new"},
		{"new function at the limit", nil, []Function{fn("a.go", "f", 15, "h")}, "", ""},
		{"changed function crosses the limit", []Function{fn("a.go", "f", 10, "h1")}, []Function{fn("a.go", "f", 16, "h2")}, SeverityMedium, "grew from 10"},
		{"already over and grew", []Function{fn("a.go", "f", 20, "h1")}, []Function{fn("a.go", "f", 21, "h2")}, SeverityMedium, "grew from 20"},
		{"already over, touched, not grown", []Function{fn("a.go", "f", 20, "h1")}, []Function{fn("a.go", "f", 20, "h2")}, "", ""},
		{"already over, touched, simplified", []Function{fn("a.go", "f", 20, "h1")}, []Function{fn("a.go", "f", 18, "h2")}, "", ""},
		{"already over and untouched", []Function{fn("a.go", "f", 30, "h")}, []Function{fn("a.go", "f", 30, "h")}, "", ""},
		{"moved function reads as new", []Function{fn("a.go", "f", 20, "h")}, []Function{fn("b.go", "f", 20, "h")}, SeverityMedium, "is new"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			before, after := report(10, tt.before...), report(10, tt.after...)
			got := Compare(before, after, ChangedFunctions(before, after), Config{})
			if tt.wantSeverity == "" {
				assert.Equal(t, len(got), 0, "%+v", got)
				return
			}
			assert.Equal(t, len(got), 1, "%+v", got)
			assert.Equal(t, got[0].Severity, tt.wantSeverity)
			assert.Assert(t, strings.Contains(got[0].Message, tt.wantContains), got[0].Message)
			assert.Assert(t, strings.Contains(got[0].Message, "split it: a handler per case, or extract the decision into a pure function"))
			assert.Equal(t, got[0].Line, 10)
		})
	}
}

func TestCompareHonoursTheConfiguredLimit(t *testing.T) {
	before, after := report(0), report(0, fn("a.go", "f", 8, "h"))
	changed := ChangedFunctions(before, after)
	assert.Equal(t, len(Compare(before, after, changed, Config{})), 0)
	assert.Equal(t, len(Compare(before, after, changed, Config{MaxFunctionComplexity: 7})), 1)
}

func TestCompareErosionRise(t *testing.T) {
	tests := []struct {
		name          string
		before, after float64
		cfg           Config
		want          int
	}{
		{"rise over the default", 20, 21.5, Config{}, 1},
		{"rise within the default", 20, 20.9, Config{}, 0},
		{"rise equal to the limit", 20, 21, Config{}, 0},
		{"erosion falls", 20, 10, Config{}, 0},
		{"stricter limit", 20, 20.9, Config{MaxErosionRise: 0.5}, 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := Compare(report(tt.before), report(tt.after), nil, tt.cfg)
			assert.Equal(t, len(got), tt.want, "%+v", got)
			for _, f := range got {
				assert.Equal(t, f.Severity, SeverityMedium)
				assert.Equal(t, f.File, "")
			}
		})
	}
}

func TestCompareCloneRules(t *testing.T) {
	clone := Clone{File: "a.go", Func: "f", Line: 12, EndLine: 40, Lines: 29, Sig: 7}
	changed := []Function{fn("a.go", "f", 3, "h2")}
	tests := []struct {
		name          string
		before, after []Clone
		changed       []Function
		want          int
	}{
		{"new block in a changed function", nil, []Clone{clone}, changed, 1},
		{"block the baseline had", []Clone{{Sig: 7}}, []Clone{clone}, changed, 0},
		{"new block in an untouched function", nil, []Clone{clone}, nil, 0},
		{"second copy of a known block", []Clone{{Sig: 7}}, []Clone{clone, clone}, changed, 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			before, after := report(0), report(0)
			before.Clones, after.Clones = tt.before, tt.after
			got := Compare(before, after, tt.changed, Config{})
			assert.Equal(t, len(got), tt.want, "%+v", got)
			for _, f := range got {
				assert.Equal(t, f.Severity, SeverityLow)
				assert.Equal(t, f.Location(), "a.go:12")
			}
		})
	}
}

func TestCompareSaysNothingWhenNotMeasured(t *testing.T) {
	after := report(90, fn("a.go", "f", 40, "h"))
	assert.Equal(t, len(Compare(Report{}, after, after.Functions, Config{})), 0)
	assert.Equal(t, len(Compare(after, Report{}, nil, Config{})), 0)
}

func TestChangedFunctions(t *testing.T) {
	before := report(0, fn("a.go", "same", 1, "x"), fn("a.go", "edited", 1, "x"), fn("a.go", "gone", 1, "x"))
	after := report(0, fn("a.go", "same", 1, "x"), fn("a.go", "edited", 2, "y"), fn("a.go", "added", 1, "z"), fn("b.go", "same", 1, "x"))
	var names []string
	for _, f := range ChangedFunctions(before, after) {
		names = append(names, f.File+":"+f.Name)
	}
	assert.DeepEqual(t, names, []string{"a.go:edited", "a.go:added", "b.go:same"})
}

func TestSummaryLineAndFailing(t *testing.T) {
	before, after := report(27.3), report(27.1)
	before.OverThreshold, after.OverThreshold = 14, 14
	before.CloneLines, after.CloneLines = 120, 120
	findings := []Finding{{Severity: SeverityLow, Message: "l"}, {Severity: SeverityMedium, Message: "m"}}
	sum := Summarize(before, after, findings)
	assert.Equal(t, sum.Line(), "erosion 27.3% → 27.1% · functions over 15: 14 → 14 · clone lines 120 → 120")
	assert.Equal(t, len(sum.Failing()), 1)
	assert.Equal(t, sum.Failing()[0].Message, "m")
	assert.Equal(t, Summarize(Report{}, Report{}, nil).Line(), "not measured (no supported code found)")
}

func TestConfigDefaults(t *testing.T) {
	c := Config{}.withDefaults()
	assert.Equal(t, c.MaxFunctionComplexity, 15)
	assert.Equal(t, c.MaxErosionRise, 1.0)
	assert.Equal(t, c.MinCloneLines, 15)
	assert.Equal(t, Config{MaxFunctionComplexity: 9}.withDefaults().MaxFunctionComplexity, 9)
}
