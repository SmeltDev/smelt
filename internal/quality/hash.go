package quality

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"regexp"
	"strings"
)

// FunctionHash identifies a function's body in any language: its source lines
// start..end (1-based, both included) with the function's own name removed and
// whitespace collapsed. Re-indenting or renaming the function keeps it; any
// other edit changes it. A recursive call counts as the name, so renaming a
// recursive function keeps it too.
func FunctionHash(content []byte, start, end int, name string) string {
	return textHash(strings.Join(lineRange(content, start, end), "\n"), shortName(name))
}

// lineRange is lines start..end of content, clamped to the file.
func lineRange(content []byte, start, end int) []string {
	lines := strings.Split(string(content), "\n")
	start = max(start, 1)
	end = min(end, len(lines))
	if start > end {
		return nil
	}
	return lines[start-1 : end]
}

// textHash is the hash of text with word removed as a whole word and
// whitespace collapsed; an empty word removes nothing.
func textHash(text, word string) string {
	if word != "" {
		text = regexp.MustCompile(`\b`+regexp.QuoteMeta(word)+`\b`).ReplaceAllString(text, " ")
	}
	sum := sha256.Sum256([]byte(strings.Join(strings.Fields(text), " ")))
	return hex.EncodeToString(sum[:8])
}

var identifier = regexp.MustCompile(`[A-Za-z_$][\w$]*$`)

// shortName is the last identifier of a provider's function name, which may
// be qualified (Type::method, Type.method) or decorated; "" when it has none.
func shortName(name string) string {
	return identifier.FindString(strings.TrimSpace(name))
}

// put writes formatted text to a hash, which never fails.
func put(w io.Writer, format string, a ...any) { _, _ = fmt.Fprintf(w, format, a...) }
