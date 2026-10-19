package harness

import (
	"strings"
	"testing"

	"gotest.tools/v3/assert"
)

func writeAll(t *testing.T, st *StreamTail, chunks ...string) {
	t.Helper()
	for _, c := range chunks {
		_, err := st.Write([]byte(c))
		assert.NilError(t, err)
	}
}

func TestStreamTailKeepsEverythingUnderTheLimit(t *testing.T) {
	st := StreamTail{Limit: 100}
	writeAll(t, &st, "a\n", "b\n")
	assert.Equal(t, st.String(), "a\nb\n")
	assert.Assert(t, !st.Cut())
}

func TestStreamTailKeepsTheResultAfterAMegabyteOfEvents(t *testing.T) {
	st := StreamTail{Limit: 128 << 10}
	event := `{"type":"assistant","message":"` + strings.Repeat("x", 1000) + `"}` + "\n"
	for range 1100 { // about 1.1 MB, written in event-sized chunks
		writeAll(t, &st, event)
	}
	result := `{"type":"result","structured_output":{"summary":"ok"}}` + "\n"
	writeAll(t, &st, result[:10], result[10:]) // the result arrives split
	assert.Assert(t, st.Cut())
	assert.Assert(t, !st.LastLineCut())
	assert.Assert(t, st.Len() <= 128<<10+len(result))
	assert.Assert(t, strings.HasSuffix(st.String(), result))
	assert.Assert(t, strings.HasPrefix(st.String(), `{"type":"assistant"`), "kept text starts on a line boundary")
	assert.Equal(t, ResultLine(st.String()), strings.TrimSpace(result))
}

func TestStreamTailKeepsAWholeLastLineOverTheLimit(t *testing.T) {
	st := StreamTail{Limit: 10}
	big := `{"type":"result","result":"` + strings.Repeat("y", 100) + `"}`
	writeAll(t, &st, "event one\n", "event two\n", big+"\n")
	assert.Equal(t, st.String(), big+"\n")
	assert.Assert(t, !st.LastLineCut())
}

func TestStreamTailCutsALastLineOverTheHardCap(t *testing.T) {
	st := StreamTail{Limit: 10}
	writeAll(t, &st, strings.Repeat("z", maxLastLine+5))
	assert.Equal(t, st.Len(), maxLastLine)
	assert.Assert(t, st.LastLineCut())
}

func TestStreamTailZeroValueKeepsEverything(t *testing.T) {
	var st StreamTail
	writeAll(t, &st, strings.Repeat("q", 1<<20))
	assert.Equal(t, st.Len(), 1<<20)
	assert.Assert(t, !st.Cut())
}
