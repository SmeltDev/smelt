package factory

import (
	"cmp"
	"regexp"
	"strings"
)

// skipLine matches the line `go test -v` ("--- SKIP: TestX (0.00s)") and
// gotestsum's summary ("=== SKIP: pkg TestX (0.00s)") print for a skipped test.
var skipLine = regexp.MustCompile(`^\s*(?:---|===) SKIP: (.+?)(?: \(\d+(?:\.\d+)?s\))?\s*$`)

// pytestSkipLine matches a pytest -rs summary line ("SKIPPED [1] tests/a.py:12:
// reason"), -rx's ("XFAIL tests/a.py::test_x - reason") and the -v form
// ("tests/a.py::test_x SKIPPED (reason) [ 50%]").
var pytestSkipLine = regexp.MustCompile(`^(?:SKIPPED \[\d+\] (\S+?):?(?: |$)|XFAIL (\S+)|(\S+::\S+) (?:SKIPPED|XFAIL)\b)`)

// pytestSummary matches pytest's closing line ("=== 3 passed, 1 skipped in
// 0.12s ===", or the same without the rule under -q).
var pytestSummary = regexp.MustCompile(`^(?:=+ )?(?:no tests ran|\d+ (?:passed|failed|skipped|xfailed|xpassed|errors?|deselected)\b.*) in \d+(?:\.\d+)?s`)

// goTestVerboseLine matches the per-test lines `go test -v` prints; without
// them a Go run cannot show its skips.
var goTestVerboseLine = regexp.MustCompile(`^\s*(?:---|===) (?:PASS|FAIL|SKIP)\b`)

// SkipScan is what a test runner's output says about skipped tests.
type SkipScan struct {
	// Tests are the skipped tests, once each, in the order they appear.
	Tests []string
	// Measured is whether the output came from a runner smelt reads skips
	// from. Without it an empty Tests means nothing was measured, not that
	// nothing was skipped.
	Measured bool
}

// ScanSkips reads the skipped tests out of go test, gotestsum and pytest output.
func ScanSkips(output string) SkipScan {
	var scan SkipScan
	seen := map[string]bool{}
	add := func(name string) {
		if name == "" || seen[name] {
			return
		}
		seen[name] = true
		scan.Tests = append(scan.Tests, name)
	}
	for _, line := range strings.Split(output, "\n") {
		line = strings.TrimRight(line, "\r")
		if m := skipLine.FindStringSubmatch(line); m != nil {
			add(m[1])
			scan.Measured = true
			continue
		}
		if m := pytestSkipLine.FindStringSubmatch(line); m != nil {
			add(cmp.Or(m[1], m[2], m[3]))
			scan.Measured = true
			continue
		}
		if goTestVerboseLine.MatchString(line) || pytestSummary.MatchString(line) {
			scan.Measured = true
		}
	}
	return scan
}

// SkippedTests lists the tests a test runner's output reports as skipped, once
// each, in the order they appear.
func SkippedTests(output string) []string {
	return ScanSkips(output).Tests
}

// Skipped is the tests the last round's validation commands skipped.
func (o Outcome) Skipped() []string {
	var out []string
	for _, c := range o.Checks {
		out = append(out, c.Skipped...)
	}
	return out
}

// SkipsNotMeasured reports whether the round ran validation commands and none
// of their output came from a runner whose skips smelt reads, so that "no
// skips" cannot be taken for a measurement.
func (o Outcome) SkipsNotMeasured() bool {
	ran := false
	for _, c := range o.Checks {
		if c.Kind != KindValidate || (c.Status != StatusPassed && c.Status != StatusFailed) {
			continue
		}
		if c.SkipsMeasured {
			return false
		}
		ran = true
	}
	return ran
}
