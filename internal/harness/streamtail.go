package harness

import (
	"bytes"
	"cmp"
)

// maxLastLine is the default bound on the one line a StreamTail keeps whatever
// its Limit: a result event bigger than this is cut, and LastLineCut says so.
const maxLastLine = 16 << 20

// StreamTail keeps the end of a stream: the last Limit bytes, starting on a
// line boundary, and always the whole last line. A streaming agent prints its
// result last, after every event of the turn, so keeping the start of the
// stream would lose the one line the caller reads. The zero value keeps
// everything.
type StreamTail struct {
	// Limit is about how many bytes to keep before the last line; 0 keeps
	// everything.
	Limit int
	// MaxLine bounds the last line; 0 means 16 MiB.
	MaxLine int

	buf     []byte
	cut     bool
	lastCut bool
}

// Write keeps p, dropping the oldest lines past Limit. It never fails.
func (t *StreamTail) Write(p []byte) (int, error) {
	t.buf = append(t.buf, p...)
	t.trim()
	return len(p), nil
}

// String is what is kept.
func (t *StreamTail) String() string { return string(t.buf) }

// Len is how many bytes are kept.
func (t *StreamTail) Len() int { return len(t.buf) }

// Cut reports whether any of the stream was dropped.
func (t *StreamTail) Cut() bool { return t.cut }

// LastLineCut reports whether the last line itself was cut, because it alone
// was over MaxLine: only then can a result printed last be incomplete.
func (t *StreamTail) LastLineCut() bool { return t.lastCut }

func (t *StreamTail) trim() {
	if t.Limit <= 0 || len(t.buf) <= t.Limit {
		return
	}
	start := len(t.buf) - t.Limit
	last := lastLineStart(t.buf)
	maxLine := cmp.Or(t.MaxLine, maxLastLine)
	switch {
	case len(t.buf)-last > maxLine:
		start = len(t.buf) - maxLine
		t.lastCut = true
	case start > last:
		start = last
	default:
		if i := bytes.IndexByte(t.buf[start:], '\n'); i >= 0 && start+i+1 <= last {
			start += i + 1
		}
	}
	if start == 0 {
		return
	}
	t.buf = append([]byte(nil), t.buf[start:]...)
	t.cut = true
}

// lastLineStart is the offset of the last line in b, ignoring one trailing
// newline: the line still being written, or the last complete one.
func lastLineStart(b []byte) int {
	end := len(b)
	if end > 0 && b[end-1] == '\n' {
		end--
	}
	return bytes.LastIndexByte(b[:end], '\n') + 1
}
