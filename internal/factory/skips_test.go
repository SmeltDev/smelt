package factory

import (
	"os"
	"testing"

	"gotest.tools/v3/assert"

	"github.com/smeltdev/smelt/internal/config"
	"github.com/smeltdev/smelt/internal/sidecar"
)

func TestSkippedTests(t *testing.T) {
	tests := []struct {
		name   string
		output string
		want   []string
	}{
		{"none", "ok  \tgithub.com/x/y\t0.01s\n--- PASS: TestA (0.00s)\n", nil},
		{"go test -v", "=== RUN   TestA\n--- SKIP: TestA (0.00s)\n    a_test.go:9: needs docker\n--- PASS: TestB (0.00s)\n    --- SKIP: TestB/sub (0.00s)\n",
			[]string{"TestA", "TestB/sub"}},
		{"gotestsum", "=== SKIP: internal/x TestDocker (0.00s)\n    x_test.go:4: no docker\n\n=== SKIP: internal/y TestOther (0.12s)\n",
			[]string{"internal/x TestDocker", "internal/y TestOther"}},
		{"both formats list a test once", "--- SKIP: TestA (0.00s)\n=== SKIP: TestA (0.00s)\n", []string{"TestA"}},
		{"a log line is not a skip", "level=info msg=\"--- SKIP: nothing\"\nSKIP this\n", nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.DeepEqual(t, SkippedTests(tt.output), tt.want)
		})
	}
}

func TestScanSkips(t *testing.T) {
	pytest, err := os.ReadFile("testdata/pytest_skips.txt")
	assert.NilError(t, err)
	tests := []struct {
		name         string
		output       string
		want         []string
		wantMeasured bool
	}{
		{"pytest -rsx", string(pytest), []string{"tests/test_library.py:21", "tests/test_player.py:40", "tests/test_player.py::test_seek_past_end"}, true},
		{"pytest -v", "tests/test_a.py::test_x SKIPPED (no docker) [ 50%]\ntests/test_a.py::test_y PASSED [100%]\n", []string{"tests/test_a.py::test_x"}, true},
		{"pytest -q with no skips", "...\n3 passed in 0.02s\n", nil, true},
		{"pytest without tests", "no tests ran in 0.01s\n", nil, true},
		{"go test -v with no skips", "--- PASS: TestA (0.00s)\nok  \tx\t0.01s\n", nil, true},
		{"go test without -v cannot show skips", "ok  \tgithub.com/x/y\t0.01s\n", nil, false},
		{"other runner", "Ran 3 tests\nOK\n", nil, false},
		{"empty", "", nil, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := ScanSkips(tt.output)
			assert.DeepEqual(t, got.Tests, tt.want)
			assert.Equal(t, got.Measured, tt.wantMeasured)
		})
	}
}

func TestSkipsNotMeasured(t *testing.T) {
	validate := func(measured bool) Check {
		return Check{Name: "test", Kind: KindValidate, Status: StatusPassed, SkipsMeasured: measured}
	}
	assert.Assert(t, !(Outcome{}).SkipsNotMeasured(), "no validation ran")
	assert.Assert(t, (Outcome{Checks: []Check{validate(false)}}).SkipsNotMeasured())
	assert.Assert(t, !(Outcome{Checks: []Check{validate(false), validate(true)}}).SkipsNotMeasured())
	assert.Assert(t, !(Outcome{Checks: []Check{{Kind: KindReview, Status: StatusPassed}}}).SkipsNotMeasured())
}

func TestSkipsNotMeasuredIsInTheNotes(t *testing.T) {
	n := Notes{Title: "T", What: "- w", Why: "- y", How: "- h", SkipsNotMeasured: true}
	assert.Assert(t, contains(n.PRBody("r", Outcome{}, ""), "Skips not measured"))
	assert.Assert(t, !contains(Notes{Title: "T", What: "w", Why: "y", How: "h"}.PRBody("r", Outcome{}, ""), "Skips not measured"))
}

func TestOutcomeSkippedCollectsFromChecks(t *testing.T) {
	o := Outcome{Checks: []Check{{Name: "test", Skipped: []string{"TestA"}}, {Name: "lint"}, {Name: "e2e", Skipped: []string{"TestB"}}}}
	assert.DeepEqual(t, o.Skipped(), []string{"TestA", "TestB"})
	assert.Equal(t, len((Outcome{}).Skipped()), 0)
}

func TestSkippedTestsAreInTheNotes(t *testing.T) {
	n := Notes{Title: "T", What: "- w", Why: "- y", How: "- h", Skipped: []string{"TestA", "TestB"}}
	assert.Assert(t, contains(n.PRBody("r", Outcome{}, ""), "Skipped in the sandbox: TestA, TestB"))
	assert.Assert(t, contains(n.CommitMessage("r", Outcome{}, ""), "Skipped in the sandbox: TestA, TestB"))

	fb := fallbackNotes("Title", "timed out")
	fb.Skipped = []string{"TestA"}
	assert.Assert(t, contains(fb.PRBody("r", Outcome{}, ""), "Skipped in the sandbox: TestA"))
	assert.Assert(t, !contains(Notes{Title: "T", What: "w", Why: "y", How: "h"}.PRBody("r", Outcome{}, ""), "Skipped"))
}

func TestRunCommandListsSkippedTests(t *testing.T) {
	exec := localExec(t.TempDir())
	entry := &sidecar.PoolEntry{ID: "impl", RepoPath: t.TempDir()}
	c := runCommand(t.Context(), exec, entry, config.Command{Name: "test", Run: "printf -- '--- SKIP: TestDocker (0.00s)\\nok\\n'"})
	assert.Equal(t, c.Status, StatusPassed)
	assert.DeepEqual(t, c.Skipped, []string{"TestDocker"})
}

func TestRunCommandFlagsOutputWithoutARunnerSummary(t *testing.T) {
	exec := localExec(t.TempDir())
	entry := &sidecar.PoolEntry{ID: "impl", RepoPath: t.TempDir()}
	c := runCommand(t.Context(), exec, entry, config.Command{Name: "test", Run: "echo OK"})
	assert.Assert(t, !c.SkipsMeasured)
	c = runCommand(t.Context(), exec, entry, config.Command{Name: "test", Run: "echo '3 passed in 0.1s'"})
	assert.Assert(t, c.SkipsMeasured)
}
