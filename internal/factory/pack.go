package factory

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path"
	"regexp"
	"slices"
	"sort"
	"strings"

	"github.com/smeltdev/smelt/internal/config"
	"github.com/smeltdev/smelt/internal/gitexec"
)

// PackKind says what a pack entry is for, and so whether its code is put in
// the prompt.
type PackKind string

// Pack entry kinds.
const (
	// PackEdit is code the change modifies. It is a pointer only: the agent's
	// edit tools need the file read in its own session, so inlining it would
	// cost its tokens twice.
	PackEdit PackKind = "edit"
	// PackReference is code to reuse or follow; its excerpt is inlined.
	PackReference PackKind = "reference"
	// PackTestPattern is the nearest existing test to copy; a reference.
	PackTestPattern PackKind = "test_pattern"
	// PackRule is a short quote from the project's docs that applies. Its
	// path is the source and its reason is the quote.
	PackRule PackKind = "rule"
	// PackNewFile is a file the change creates. Its reason names the existing
	// file it should follow, which is a reference entry of its own.
	PackNewFile PackKind = "new_file"
)

// charsPerToken is the estimate excerpts are counted with. It is rough, and
// undercounts dense or non-ASCII code.
const charsPerToken = 4

// PackEntry is one piece of code the agents need, pinned to the run's
// baseline commit.
type PackEntry struct {
	Kind PackKind `json:"kind"`
	Path string   `json:"path"`
	// Start and End are the 1-based first and last line; 0 means the whole
	// file, or no file lines at all for a rule or a new file.
	Start int `json:"start"`
	End   int `json:"end"`
	// Reason is why the agents need it.
	Reason string `json:"reason"`
	// Priority is 1 (must have) to 3 (nice to have).
	Priority int `json:"priority"`
	// Pointer keeps the entry from ever being inlined: set when the cap or a
	// re-anchoring turned a reference into a pointer.
	Pointer bool `json:"pointer,omitempty"`
	// Excerpt is the code of an inlined entry. smelt reads it from the
	// commit; it is never taken from the agent's answer.
	Excerpt string `json:"-"`
}

// DroppedEntry is an entry validation refused.
type DroppedEntry struct {
	Path   string `json:"path"`
	Start  int    `json:"start"`
	End    int    `json:"end"`
	Reason string `json:"reason"`
}

// RejectedHint is a hint of the request's author the planner chose not to
// follow, and why.
type RejectedHint struct {
	Hint   string `json:"hint"`
	Reason string `json:"reason"`
}

// Pack is the validated context pack of a run.
type Pack struct {
	Entries       []PackEntry    `json:"entries"`
	Dropped       []DroppedEntry `json:"dropped,omitempty"`
	RejectedHints []RejectedHint `json:"rejected_hints,omitempty"`
	// Tokens is the estimated size of the pack as rendered.
	Tokens int `json:"tokens"`
}

// Empty reports whether the pack has nothing to show the agents.
func (p Pack) Empty() bool { return len(p.Entries) == 0 }

// FileSource reads files as of one commit. A file that is not there is an
// error wrapping os.ErrNotExist.
type FileSource interface {
	ReadFile(ctx context.Context, path string) ([]byte, error)
}

// GitFiles reads files from the commit Rev of the repository Git runs in.
type GitFiles struct {
	Git gitexec.Runner
	Rev string
}

// ReadFile returns the blob at path in Rev.
func (g GitFiles) ReadFile(ctx context.Context, p string) ([]byte, error) {
	out, err := g.Git.Output(ctx, "cat-file", "blob", g.Rev+":"+p)
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, fmt.Errorf("%s at %s: %w", p, shortSHA(g.Rev), os.ErrNotExist)
	}
	return out, nil
}

// Label is the entry's location as path:start-end.
func (e PackEntry) Label() string { return rangeLabel(e.Path, e.Start, e.End) }

func rangeLabel(p string, start, end int) string {
	switch {
	case start <= 0:
		return p
	case end <= start:
		return fmt.Sprintf("%s:%d", p, start)
	}
	return fmt.Sprintf("%s:%d-%d", p, start, end)
}

// String is the dropped entry as the log shows it.
func (d DroppedEntry) String() string {
	return rangeLabel(d.Path, d.Start, d.End) + " — " + d.Reason
}

// estimateTokens is the token count of s.
func estimateTokens(s string) int { return (len(s) + charsPerToken - 1) / charsPerToken }

// splitLines is the lines of a file; an empty file has none.
func splitLines(content string) []string {
	if content == "" {
		return nil
	}
	return strings.Split(strings.TrimSuffix(content, "\n"), "\n")
}

// ValidatePack checks entries against files and returns the pack that fits
// maxTokens (the default when it is not positive). Entries that do not hold
// are dropped with a reason. An excerpt the entry arrived with is ignored:
// smelt reads every one from files.
func ValidatePack(ctx context.Context, entries []PackEntry, files FileSource, maxTokens int) Pack {
	var p Pack
	for _, e := range entries {
		e.Excerpt = ""
		kept, why := validateEntry(ctx, e, files)
		if why != "" {
			p.Dropped = append(p.Dropped, DroppedEntry{Path: e.Path, Start: e.Start, End: e.End, Reason: why})
			continue
		}
		p.Entries = append(p.Entries, kept)
	}
	if maxTokens <= 0 {
		maxTokens = config.DefaultPackMaxTokens
	}
	p.Tokens = fitPack(p.Entries, maxTokens)
	return p
}

// validateEntry returns the entry with its excerpt, or why it is invalid.
func validateEntry(ctx context.Context, e PackEntry, files FileSource) (PackEntry, string) {
	e.Priority = min(max(e.Priority, 1), 3)
	switch e.Kind {
	case PackRule:
		if strings.TrimSpace(e.Reason) == "" {
			return e, "a rule with no text"
		}
		e.Start, e.End = 0, 0
		return e, ""
	case PackNewFile, PackEdit, PackReference, PackTestPattern:
	default:
		return e, fmt.Sprintf("unknown kind %q", e.Kind)
	}
	clean := path.Clean(strings.TrimSpace(e.Path))
	if clean == "." || path.IsAbs(clean) || clean == ".." || strings.HasPrefix(clean, "../") {
		return e, "not a path inside the repository"
	}
	e.Path = clean
	content, err := files.ReadFile(ctx, clean)
	if e.Kind == PackNewFile {
		return validateNewFile(e, err)
	}
	return validateExisting(e, content, err)
}

// validateNewFile accepts a new file when reading it failed: it must not exist.
func validateNewFile(e PackEntry, readErr error) (PackEntry, string) {
	if readErr == nil {
		return e, "the file already exists"
	}
	e.Start, e.End = 0, 0
	return e, ""
}

// validateExisting checks an entry about a file that must exist, and reads its
// excerpt when it is to be inlined.
func validateExisting(e PackEntry, content []byte, readErr error) (PackEntry, string) {
	if errors.Is(readErr, os.ErrNotExist) {
		return e, "no such file at the baseline commit"
	}
	if readErr != nil {
		return e, fmt.Sprintf("could not be read: %v", readErr)
	}
	lines := splitLines(string(content))
	if why := checkRange(&e, len(lines)); why != "" {
		return e, why
	}
	if e.Kind == PackEdit || e.Pointer {
		return e, ""
	}
	if e.Start == 0 {
		e.Excerpt = strings.Join(lines, "\n")
	} else {
		e.Excerpt = strings.Join(lines[e.Start-1:e.End], "\n")
	}
	return e, ""
}

// checkRange rejects a range that is not inside a file of n lines.
func checkRange(e *PackEntry, n int) string {
	if e.Start == 0 && e.End == 0 {
		return ""
	}
	if e.Start < 1 || e.End < e.Start {
		return fmt.Sprintf("invalid range %d-%d", e.Start, e.End)
	}
	if e.End > n {
		return fmt.Sprintf("beyond end of file (%d lines)", n)
	}
	return ""
}

// fitPack turns inlined entries into pointers, nice-to-have first and the
// largest first among equals, until the pack fits max tokens. It returns the
// pack's tokens.
func fitPack(entries []PackEntry, limit int) int {
	total := 0
	for _, e := range entries {
		total += e.tokens()
	}
	inlined := make([]int, 0, len(entries))
	for i, e := range entries {
		if e.Excerpt != "" {
			inlined = append(inlined, i)
		}
	}
	sort.SliceStable(inlined, func(a, b int) bool {
		ea, eb := entries[inlined[a]], entries[inlined[b]]
		if ea.Priority != eb.Priority {
			return ea.Priority > eb.Priority
		}
		return len(ea.Excerpt) > len(eb.Excerpt)
	})
	for _, i := range inlined {
		if total <= limit {
			break
		}
		total -= estimateTokens(entries[i].Excerpt)
		entries[i].Excerpt, entries[i].Pointer = "", true
	}
	return total
}

// tokens is what the entry costs in the prompt: its line, and its excerpt.
func (e PackEntry) tokens() int {
	return estimateTokens(e.Label()+e.Reason) + estimateTokens(e.Excerpt)
}

// Reanchor makes a pack recorded against the commit old valid against cur: a
// range whose lines moved is found again by its first line, nearest to where
// it was, and one that cannot be found becomes a pointer to the whole file.
// Entries whose file is gone are dropped, as ValidatePack does.
func Reanchor(ctx context.Context, rec Pack, old, cur FileSource, maxTokens int) Pack {
	entries := make([]PackEntry, len(rec.Entries))
	for i, e := range rec.Entries {
		entries[i] = reanchorEntry(ctx, e, old, cur)
	}
	p := ValidatePack(ctx, entries, cur, maxTokens)
	p.RejectedHints = rec.RejectedHints
	return p
}

func reanchorEntry(ctx context.Context, e PackEntry, old, cur FileSource) PackEntry {
	if e.Start <= 0 || e.Kind == PackRule || e.Kind == PackNewFile {
		return e
	}
	oldLines, curLines := packLines(ctx, old, e.Path), packLines(ctx, cur, e.Path)
	if curLines == nil || e.End > len(oldLines) {
		// Nothing to compare with: validation keeps or drops the range as it is.
		return e
	}
	first := oldLines[e.Start-1]
	if e.End <= len(curLines) && curLines[e.Start-1] == first {
		return e
	}
	at := nearestLine(curLines, first, e.Start-1)
	if at < 0 || at+(e.End-e.Start) >= len(curLines) {
		e.Start, e.End, e.Pointer = 0, 0, true
		return e
	}
	e.End = at + (e.End - e.Start) + 1
	e.Start = at + 1
	return e
}

// packLines is the lines of the file at p in files, nil when it cannot be read.
func packLines(ctx context.Context, files FileSource, p string) []string {
	b, err := files.ReadFile(ctx, p)
	if err != nil {
		return nil
	}
	return splitLines(string(b))
}

// nearestLine is the index of the line equal to want closest to from, or -1.
// A blank want matches nothing: it would match anywhere.
func nearestLine(lines []string, want string, from int) int {
	if strings.TrimSpace(want) == "" {
		return -1
	}
	best := -1
	for i, l := range lines {
		if l != want {
			continue
		}
		if best < 0 || abs(i-from) < abs(best-from) {
			best = i
		}
	}
	return best
}

func abs(n int) int {
	if n < 0 {
		return -n
	}
	return n
}

// contextHeading matches the request's optional "Context" section heading.
var contextHeading = regexp.MustCompile(`(?i)^#{1,6}\s*context\s*$`)

// bulletPrefix is the list marker, and checkbox, in front of a hint.
var bulletPrefix = regexp.MustCompile(`^(?:[-*+]|\d+[.)])\s+(?:\[[ xX]\]\s+)?`)

// ContextHints are the hints in the request's `## Context` section: its
// bullets, or its lines, up to the next heading.
func ContextHints(request string) []string {
	var hints []string
	in := false
	for _, line := range strings.Split(request, "\n") {
		trimmed := strings.TrimSpace(line)
		switch {
		case contextHeading.MatchString(trimmed):
			in = true
		case strings.HasPrefix(trimmed, "#"):
			in = false
		case in && trimmed != "":
			hints = append(hints, bulletPrefix.ReplaceAllString(trimmed, ""))
		}
	}
	return hints
}

// MissingHints are the hints that appear neither in the pack nor among the
// rejected hints. A hint appears in the pack when it names an entry's path or
// is quoted by its reason.
func MissingHints(hints []string, p Pack) []string {
	var missing []string
	for _, h := range hints {
		if !slices.ContainsFunc(p.Entries, func(e PackEntry) bool { return mentions(h, e) }) &&
			!slices.ContainsFunc(p.RejectedHints, func(r RejectedHint) bool { return sameHint(h, r.Hint) }) {
			missing = append(missing, h)
		}
	}
	return missing
}

func mentions(hint string, e PackEntry) bool {
	h := strings.ToLower(hint)
	return (e.Path != "" && e.Kind != PackRule && strings.Contains(h, strings.ToLower(e.Path))) ||
		strings.Contains(strings.ToLower(e.Reason), h)
}

func sameHint(a, b string) bool {
	a, b = strings.ToLower(strings.TrimSpace(a)), strings.ToLower(strings.TrimSpace(b))
	return a != "" && b != "" && (strings.Contains(a, b) || strings.Contains(b, a))
}

// packIntro is the line before the pack in the implementer's prompt.
const packIntro = "This is where the work is; read the edit targets before changing them. You may read anything else you need."

// reviewPackIntro is the line before the pack in a reviewer's prompt.
const reviewPackIntro = "The implementer was given this context. Report a change that ignores an entry it should have followed, such as a reference to reuse or a rule that applies, as you would any other problem."

// Markdown is the pack as the implementer's prompt shows it, "" when empty.
func (p Pack) Markdown() string { return p.section(packIntro) }

// ReviewMarkdown is the pack as a reviewer's prompt shows it, "" when empty.
func (p Pack) ReviewMarkdown() string { return p.section(reviewPackIntro) }

func (p Pack) section(intro string) string {
	if p.Empty() {
		return ""
	}
	var b strings.Builder
	b.WriteString("## Context pack\n\n" + intro + "\n")
	groups := []struct {
		heading string
		kinds   []PackKind
	}{
		{"Edit targets", []PackKind{PackEdit, PackNewFile}},
		{"Reuse and follow", []PackKind{PackReference, PackTestPattern}},
		{"Rules", []PackKind{PackRule}},
	}
	for _, g := range groups {
		var in []PackEntry
		for _, e := range p.Entries {
			if slices.Contains(g.kinds, e.Kind) {
				in = append(in, e)
			}
		}
		if len(in) == 0 {
			continue
		}
		fmt.Fprintf(&b, "\n%s:\n", g.heading)
		for _, e := range in {
			writePackEntry(&b, e)
		}
	}
	return strings.TrimRight(b.String(), "\n")
}

func writePackEntry(b *strings.Builder, e PackEntry) {
	switch e.Kind {
	case PackRule:
		fmt.Fprintf(b, "- %s (%s)\n", e.Reason, e.Path)
	case PackNewFile:
		fmt.Fprintf(b, "- `%s` (new file) — %s\n", e.Path, e.Reason)
	case PackEdit, PackReference, PackTestPattern:
		fmt.Fprintf(b, "- `%s` — %s\n", e.Label(), e.Reason)
	default:
		fmt.Fprintf(b, "- `%s` — %s\n", e.Label(), e.Reason)
	}
	if e.Excerpt == "" {
		return
	}
	fence := fenceFor(e.Excerpt)
	fmt.Fprintf(b, "%s\n%s\n%s\n", fence, e.Excerpt, fence)
}

// fenceFor is a code fence longer than any run of backticks in s.
func fenceFor(s string) string {
	longest, run := 0, 0
	for _, r := range s {
		if r == '`' {
			run++
			longest = max(longest, run)
			continue
		}
		run = 0
	}
	return strings.Repeat("`", max(3, longest+1))
}

// LogLines are the lines the run's log shows for the pack.
func (p Pack) LogLines() []string {
	lines := make([]string, 0, 1+len(p.Entries)+len(p.Dropped)+len(p.RejectedHints))
	lines = append(lines, fmt.Sprintf("pack: %d entries, ~%d tokens", len(p.Entries), p.Tokens))
	for _, e := range p.Entries {
		how := "pointer"
		if e.Excerpt != "" {
			how = "inlined"
		}
		lines = append(lines, fmt.Sprintf("pack: %s %s (priority %d, %s) — %s", e.Kind, e.Label(), e.Priority, how, e.Reason))
	}
	for _, d := range p.Dropped {
		lines = append(lines, "pack: dropped "+d.String())
	}
	for _, r := range p.RejectedHints {
		lines = append(lines, fmt.Sprintf("pack: hint not followed: %s — %s", r.Hint, r.Reason))
	}
	return lines
}
