package factory

import (
	"strings"
	"testing"

	"github.com/smeltdev/smelt/internal/review"
	"gotest.tools/v3/assert"
)

func TestBuildHandoffContainsContextWithoutDiff(t *testing.T) {
	pack := &Pack{Entries: []PackEntry{{Kind: PackEdit, Path: "internal/a.go", Reason: "edit here"}}}
	finding := review.Finding{Prompt: "maintainability", File: "internal/a.go", Body: "handle the error"}
	handoff := BuildHandoff(HandoffOptions{
		Ticket: "ticket text", Plan: &Plan{Summary: "approved plan"}, Pack: pack,
		Change: "2 files changed", Summary: "implemented the feature", Findings: []review.Finding{finding},
	})
	for _, want := range []string{"## Ticket", "ticket text", "## Approved plan", "approved plan", "## Context pack", "internal/a.go", "2 files changed", "implemented the feature", "handle the error"} {
		assert.Assert(t, strings.Contains(handoff, want), "missing %q", want)
	}
	assert.Assert(t, !strings.Contains(handoff, "diff --git"))
}
