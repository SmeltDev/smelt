package factory

import (
	"fmt"
	"strings"

	"github.com/smeltdev/smelt/internal/review"
)

// HandoffOptions are the parts of a fresh round's handoff prompt.
type HandoffOptions struct {
	Ticket   string
	Plan     *Plan
	Pack     *Pack
	Change   string // the change's diff stat
	Summary  string // the implementer's last summary
	Findings []review.Finding
}

// BuildHandoff makes a bounded prompt for a fresh fix round. It intentionally
// describes the change instead of embedding its diff.
func BuildHandoff(o HandoffOptions) string {
	var b strings.Builder
	b.WriteString("## Ticket\n\n" + strings.TrimSpace(o.Ticket))
	if o.Plan != nil {
		b.WriteString("\n\n## Approved plan\n\n" + strings.TrimSpace(o.Plan.Markdown()))
	}
	if o.Pack != nil && !o.Pack.Empty() {
		b.WriteString("\n\n" + o.Pack.Markdown())
	}
	b.WriteString("\n\n## Change so far\n\n")
	b.WriteString(strings.TrimSpace(o.Change))
	if strings.TrimSpace(o.Summary) != "" {
		b.WriteString("\n\nImplementer's last summary: " + strings.TrimSpace(o.Summary))
	}
	b.WriteString("\n\n## Findings to fix\n")
	for _, f := range o.Findings {
		fmt.Fprintf(&b, "\n- [%s] %s: %s", f.Severity, f.Location(), f.Body)
	}
	return strings.TrimSpace(b.String())
}
