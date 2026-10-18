package factory

import (
	"fmt"
	"strings"

	"github.com/smeltdev/smelt/internal/review"
)

// BuildHandoff makes a bounded prompt for a fresh fix round. It intentionally
// describes the change instead of embedding its diff.
func BuildHandoff(ticket string, plan *Plan, pack *Pack, change, summary string, findings []review.Finding) string {
	var b strings.Builder
	b.WriteString("## Ticket\n\n" + strings.TrimSpace(ticket))
	if plan != nil {
		b.WriteString("\n\n## Approved plan\n\n" + strings.TrimSpace(plan.Markdown()))
	}
	if pack != nil && !pack.Empty() {
		b.WriteString("\n\n" + pack.Markdown())
	}
	b.WriteString("\n\n## Change so far\n\n")
	b.WriteString(strings.TrimSpace(change))
	if strings.TrimSpace(summary) != "" {
		b.WriteString("\n\nImplementer's last summary: " + strings.TrimSpace(summary))
	}
	b.WriteString("\n\n## Findings to fix\n")
	for _, f := range findings {
		fmt.Fprintf(&b, "\n- [%s] %s: %s", f.Severity, f.Location(), f.Body)
	}
	return strings.TrimSpace(b.String())
}
