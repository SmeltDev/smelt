package factory

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gotest.tools/v3/assert"

	"github.com/smeltdev/smelt/internal/config"
	"github.com/smeltdev/smelt/internal/sidecar"
)

// seenReviews is how many reviews the fixture's fake reviewers have run.
func seenReviews(t *testing.T, home string) int {
	t.Helper()
	entries, err := os.ReadDir(filepath.Join(home, "seen"))
	if os.IsNotExist(err) {
		return 0
	}
	assert.NilError(t, err)
	return len(entries)
}

func TestCheckRunsGatesBeforeReviews(t *testing.T) {
	tests := []struct {
		name        string
		gatesFirst  bool
		gate        string
		wantReviews int
		wantStatus  map[string]Status
	}{
		{"failing gate skips the reviews", true, "echo lint broke; exit 1", 0,
			map[string]Status{"bugs": StatusSkipped, "style": StatusSkipped, "lint": StatusFailed}},
		{"passing gate runs the reviews", true, "true", 2,
			map[string]Status{"bugs": StatusPassed, "style": StatusPassed, "lint": StatusPassed}},
		{"gates first off restores the parallel run", false, "exit 1", 2,
			map[string]Status{"bugs": StatusPassed, "style": StatusPassed, "lint": StatusFailed}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx := context.Background()
			s, impl, _, home := newSidecarsFixture(t, nil)
			s.GatesFirst = tt.gatesFirst
			s.Commands = []config.Command{{Name: "lint", Run: tt.gate}}
			writeFile(t, impl.RepoPath, "main.go", "package main\n\n// v1\n")
			_, err := s.Collect(ctx)
			assert.NilError(t, err)

			checks, err := s.Check(ctx, 1)
			assert.NilError(t, err)
			assert.Equal(t, seenReviews(t, home), tt.wantReviews)
			got := map[string]Status{}
			for _, c := range checks {
				got[c.Name] = c.Status
				if c.Status == StatusSkipped {
					assert.Equal(t, c.Kind, KindReview)
					assert.Equal(t, c.Reason, "gates failed")
				}
			}
			assert.DeepEqual(t, got, tt.wantStatus)
			assert.Equal(t, len(checks), 3, "the checks keep their shape")
		})
	}
}

func TestSkippedReviewsAreNotFedBackOrRemembered(t *testing.T) {
	ctx := context.Background()
	s, impl, _, _ := newSidecarsFixture(t, nil)
	s.GatesFirst = true
	s.Commands = []config.Command{{Name: "lint", Run: "echo boom; exit 1"}}
	writeFile(t, impl.RepoPath, "main.go", "package main\n\n// v1\n")
	_, err := s.Collect(ctx)
	assert.NilError(t, err)
	checks, err := s.Check(ctx, 1)
	assert.NilError(t, err)

	assert.Assert(t, !Passed(checks))
	fb := Feedback(checks)
	assert.Assert(t, contains(fb, "validate: lint"), fb)
	assert.Assert(t, !contains(fb, "review:"), fb)
	assert.Equal(t, len(s.history), 0, "a review that did not run has nothing to remember")
}

func TestErroredGatesDoNotSkipTheReviews(t *testing.T) {
	assert.Assert(t, !gatesFailed([]Check{{Kind: KindValidate, Status: StatusErrored}, {Kind: KindValidate, Status: StatusPassed}}))
	assert.Assert(t, gatesFailed([]Check{{Kind: KindValidate, Status: StatusPassed}, {Kind: KindValidate, Status: StatusFailed}}))
	assert.Assert(t, !gatesFailed([]Check{{Kind: KindReview, Status: StatusFailed}}))
}

func TestFactoryConfigPlanAndGatesFirstDefaultOn(t *testing.T) {
	off := false
	var unset *config.FactoryConfig
	assert.Assert(t, unset.PlanEnabled() && unset.GatesFirstEnabled())
	assert.Assert(t, (&config.FactoryConfig{}).PlanEnabled() && (&config.FactoryConfig{}).GatesFirstEnabled())
	assert.Assert(t, !(&config.FactoryConfig{Plan: &off}).PlanEnabled())
	assert.Assert(t, !(&config.FactoryConfig{GatesFirst: &off}).GatesFirstEnabled())
}

func TestChangedCommandPhases(t *testing.T) {
	for _, gatesFirst := range []bool{true, false} {
		for _, mode := range []string{"pass", "full failure", "review failure", "variant failure", "empty"} {
			t.Run(fmt.Sprintf("gatesFirst=%v/%s", gatesFirst, mode), func(t *testing.T) {
				ctx := context.Background()
				s, impl, _, home := newSidecarsFixture(t, nil)
				s.GatesFirst = gatesFirst
				marker := filepath.Join(home, "commands")
				changed := "echo changed >> " + sidecar.ShellEscape(marker)
				full := "test -d " + sidecar.ShellEscape(filepath.Join(home, "seen")) + " && echo full >> " + sidecar.ShellEscape(marker)
				if mode == "full failure" {
					full += "; echo full-broke; exit 1"
				}
				if mode == "variant failure" {
					changed += "; exit 1"
				}
				if mode == "review failure" {
					installFakeClaude(t, home, scriptedReviewer)
					writeFile(t, home, "findings.json", findingsJSON([3]any{1, "high", "broken"}))
				}
				s.Commands = []config.Command{{Name: "test", Run: full, Changed: changed}}
				if mode != "empty" {
					writeFile(t, impl.RepoPath, "main.go", "package main\n// changed\n")
				}
				_, err := s.Collect(ctx)
				assert.NilError(t, err)
				var recorded []Check
				s.OnCheck = func(c Check) { recorded = append(recorded, c) }
				checks, err := s.Check(ctx, 1)
				assert.NilError(t, err)
				data, err := os.ReadFile(marker)
				assert.NilError(t, err)
				want := "changed\nfull\n"
				if mode == "empty" {
					want = "full\n"
				}
				if mode == "review failure" || mode == "variant failure" {
					want = "changed\n"
				}
				assert.Equal(t, string(data), want)
				assert.Equal(t, Passed(checks), mode == "pass" || mode == "empty")
				if mode == "full failure" {
					assert.Assert(t, strings.Contains(Feedback(checks), "full-broke"))
				}
				if mode != "review failure" && mode != "variant failure" {
					assert.Equal(t, recorded[len(recorded)-1].Name, "test (full)")
				} else {
					assert.Equal(t, recorded[0].Name, "test (changed: 1 files)")
				}
			})
		}
	}
}

func TestChangedCommandsAcrossRounds(t *testing.T) {
	for _, gatesFirst := range []bool{false, true} {
		t.Run(fmt.Sprint(gatesFirst), func(t *testing.T) {
			ctx := context.Background()
			s, impl, _, home := newSidecarsFixture(t, nil)
			s.GatesFirst = gatesFirst
			installFakeClaude(t, home, scriptedReviewer)
			marker := sidecar.ShellEscape(filepath.Join(home, "commands"))
			s.Commands = []config.Command{
				{Name: "test", Run: "echo full >> " + marker, Changed: "echo changed >> " + marker + "; printf '%s\\n' {dirs}"},
				{Name: "legacy", Run: "echo legacy >> " + marker + "; echo legacy-output"},
			}
			for n := 1; n <= 3; n++ {
				findings := "[]"
				if n == 1 {
					findings = findingsJSON([3]any{1, "high", "broken"})
				}
				writeFile(t, home, "findings.json", findings)
				if n <= 2 {
					writeFile(t, impl.RepoPath, "big.txt", strings.Repeat("line\n", n))
				}
				_, err := s.Collect(ctx)
				assert.NilError(t, err)
				checks, err := s.Check(ctx, n)
				assert.NilError(t, err)
				assert.Equal(t, Passed(checks), n > 1)
				var names []string
				for _, c := range checks {
					if c.Kind == KindValidate {
						names = append(names, c.Name)
						if c.Name == "legacy" {
							assert.Equal(t, c.Output, "legacy-output")
						}
					}
				}
				want := []string{"test (changed: 1 dirs)", "legacy"}
				if n == 2 {
					want = append(want, "test (full)")
				}
				if n == 3 {
					want = []string{"legacy", "test (full)"}
				}
				assert.DeepEqual(t, names, want)
			}
			data, err := os.ReadFile(filepath.Join(home, "commands"))
			assert.NilError(t, err)
			assert.Equal(t, string(data), "changed\nlegacy\nchanged\nlegacy\nfull\nlegacy\nfull\n")
		})
	}
}

func TestChangedCommandsWaitForQuality(t *testing.T) {
	for _, gatesFirst := range []bool{false, true} {
		t.Run(fmt.Sprint(gatesFirst), func(t *testing.T) {
			f := &fakeSandbox{base: lizardTree(fnSpec{"a.go", "old", 2, 1, 4, "x"}), now: lizardTree(fnSpec{"a.go", "busy", 16, 1, 30, "y"})}
			s, impl, _, home := newSidecarsFixtureWith(t, nil, func(s *Sidecars) { s.Quality = f.quality() })
			f.edit()
			s.GatesFirst = gatesFirst
			marker := filepath.Join(home, "full")
			s.Commands = []config.Command{{Name: "test", Changed: "true", Run: "touch " + sidecar.ShellEscape(marker)}}
			writeFile(t, impl.RepoPath, "main.go", "package main\n// edited\n")
			_, err := s.Collect(context.Background())
			assert.NilError(t, err)
			checks, err := s.Check(context.Background(), 1)
			assert.NilError(t, err)
			assert.Assert(t, !Passed(checks))
			_, err = os.Stat(marker)
			assert.Assert(t, os.IsNotExist(err))
		})
	}
}

func TestChangedCommandsFullSARIFRatchet(t *testing.T) {
	ctx := context.Background()
	empty := `{"version":"2.1.0","runs":[{"results":[]}]}`
	finding := `{"version":"2.1.0","runs":[{"results":[{"ruleId":"R","message":{"text":"new bug"},"locations":[{"physicalLocation":{"artifactLocation":{"uri":"main.go"},"region":{"startLine":2}}}]}]}]}`
	s, impl, _, _ := newSidecarsFixtureWith(t, nil, func(s *Sidecars) {
		s.Prompts = nil
		writeFile(t, s.Implementer.Entry.RepoPath, "lint.sarif", empty)
		c := lint("cat lint.sarif")
		c.Changed = "printf '%s' " + sidecar.ShellEscape(empty)
		s.Commands = []config.Command{c}
	})
	writeFile(t, impl.RepoPath, "main.go", "package main\n// edited\n")
	writeFile(t, impl.RepoPath, "lint.sarif", finding)
	_, err := s.Collect(ctx)
	assert.NilError(t, err)
	checks, err := s.Check(ctx, 1)
	assert.NilError(t, err)
	assert.Equal(t, len(checks), 2)
	assert.Equal(t, checks[0].Name, "lint (changed: 2 files)")
	assert.Equal(t, checks[0].Status, StatusPassed)
	assert.Equal(t, checks[1].Name, "lint (full)")
	assert.Equal(t, checks[1].Status, StatusFailed)
	assert.Equal(t, checks[1].Ratchet.New, 1)
	assert.Assert(t, strings.Contains(Feedback(checks), "new bug"))
}

func TestChangedCommandSnapshotIncludesGateEdits(t *testing.T) {
	ctx := context.Background()
	s, impl, _, _ := newSidecarsFixture(t, nil)
	s.Prompts = nil
	s.Commands = []config.Command{{Name: "test", Changed: "echo variant >> generated", Run: "echo full >> generated"}}
	writeFile(t, impl.RepoPath, "main.go", "package main\n// edit\n")
	_, err := s.Collect(ctx)
	assert.NilError(t, err)
	first, err := s.Check(ctx, 1)
	assert.NilError(t, err)
	assert.Equal(t, len(first), 2)
	second, err := s.Check(ctx, 2)
	assert.NilError(t, err)
	assert.Equal(t, len(second), 1)
	assert.Equal(t, second[0].Name, "test (full)")
	data, err := os.ReadFile(filepath.Join(impl.RepoPath, "generated"))
	assert.NilError(t, err)
	assert.Equal(t, string(data), "variant\nfull\nfull\n")
}

// A transient Prepare failure must not let a partial variant become the shared
// baseline: the final full check still needs findings from unchanged files.
func TestChangedCommandsRecoverFullSARIFBaseline(t *testing.T) {
	ctx := context.Background()
	empty := `{"version":"2.1.0","runs":[{"results":[]}]}`
	finding := `{"version":"2.1.0","runs":[{"results":[{"ruleId":"R","message":{"text":"existing bug"},"locations":[{"physicalLocation":{"artifactLocation":{"uri":"b.py"},"region":{"startLine":1}}}]}]}]}`
	s, impl, _, _ := newSidecarsFixtureWith(t, nil, func(s *Sidecars) {
		s.Prompts = nil
		repo := s.Implementer.Entry.RepoPath
		writeFile(t, repo, "b.py", "existing bug\n")
		writeFile(t, repo, "lint.sarif", finding)
		marker := sidecar.ShellEscape(filepath.Join(t.TempDir(), "baseline-attempted"))
		c := lint("if test -f " + marker + "; then cat lint.sarif; else touch " + marker + "; echo transient; fi")
		c.Changed = "printf '%s' " + sidecar.ShellEscape(empty)
		s.Commands = []config.Command{c}
	})
	writeFile(t, impl.RepoPath, "a.py", "changed\n")
	_, err := s.Collect(ctx)
	assert.NilError(t, err)
	for round := 1; round <= 2; round++ {
		checks, err := s.Check(ctx, round)
		assert.NilError(t, err)
		assert.Assert(t, Passed(checks), "%+v", checks)
		full := checks[len(checks)-1]
		assert.Equal(t, full.Name, "lint (full)")
		assert.Equal(t, full.Ratchet.Existing, 1)
		assert.Equal(t, full.Ratchet.New, 0)
	}
}
