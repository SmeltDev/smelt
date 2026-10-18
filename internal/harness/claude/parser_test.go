package claude

import (
	"os"
	"regexp"
	"strings"
	"testing"
	"time"

	"gotest.tools/v3/assert"

	"github.com/smeltdev/smelt/internal/harness"
)

// TestParserReassemblesSplitLines covers output arriving in chunks that cut
// lines anywhere, as the exec stream delivers it.
func TestParserReassemblesSplitLines(t *testing.T) {
	var acts []harness.Activity
	p := Harness{}.NewImplementParser(func(a harness.Activity) { acts = append(acts, a) })
	stream := `{"type":"assistant","message":{"content":[{"type":"tool_use","name":"Read","input":{"file_path":"a.go"}}]}}` + "\n" +
		`{"type":"result","result":"ok"}`
	for i := 0; i < len(stream); i += 7 {
		p.Write([]byte(stream[i:min(i+7, len(stream))]))
	}
	p.Flush()
	assert.DeepEqual(t, acts, []harness.Activity{{Tool: "Read", Detail: "a.go"}})
	assert.Assert(t, p.Outcome().Done)
	assert.Equal(t, p.Outcome().Turn.Summary, "ok")
}

func TestParserDropsOversizedLines(t *testing.T) {
	p := Harness{}.NewImplementParser(nil)
	p.Write([]byte(`{"type":"user","x":"` + strings.Repeat("a", harness.MaxLineBytes) + `"}` + "\n"))
	p.Write([]byte(`{"type":"result","result":"ok"}` + "\n"))
	assert.Assert(t, p.Outcome().Done)
}

func TestParserReadsTheRealStream(t *testing.T) {
	b, err := os.ReadFile("../testdata/real/claude-stream.jsonl")
	assert.NilError(t, err)
	p := Harness{}.NewImplementParser(nil)
	p.Write(b)
	p.Flush()
	out := p.Outcome()
	assert.Assert(t, out.Done && !out.Failed)
	assert.Equal(t, out.Turn.Summary, "ok")
	assert.Equal(t, out.Turn.OutputTokens, 4)
	assert.Equal(t, out.Turn.CachedTokens, 15333)
	assert.Equal(t, out.Turn.InputTokens, 2796+2074+15333)
	assert.Assert(t, out.Turn.CostUSD > 0)
	assert.Equal(t, out.Turn.SessionID, "00000000-0000-4000-8000-000000000004")
	rl := out.Turn.RateLimit
	assert.Assert(t, rl != nil)
	assert.Equal(t, rl.Status, "allowed")
	assert.Equal(t, rl.Bucket, "5h")
	assert.Assert(t, rl.ResetsAt.Equal(time.Unix(1791764400, 0)))
}

func TestParserKeepsARejectedRateLimit(t *testing.T) {
	p := Harness{}.NewImplementParser(nil)
	p.Write([]byte(`{"type":"rate_limit_event","rate_limit_info":{"status":"rejected","rateLimitType":"seven_day"}}` + "\n"))
	rl := p.Outcome().Turn.RateLimit
	assert.Assert(t, rl != nil)
	assert.Equal(t, rl.Status, "rejected")
	assert.Equal(t, rl.Bucket, "7d")
	assert.Assert(t, rl.ResetsAt == nil)
}

// realStreamLines is the real stream fixture, one entry per line.
func realStreamLines(t *testing.T) []string {
	t.Helper()
	b, err := os.ReadFile("../testdata/real/claude-stream.jsonl")
	assert.NilError(t, err)
	return strings.Split(strings.TrimSpace(string(b)), "\n")
}

func TestParserMeasuresTheRealStreamsContext(t *testing.T) {
	p := Harness{}.NewImplementParser(nil)
	p.Write([]byte(strings.Join(realStreamLines(t), "\n") + "\n"))
	turn := p.Outcome().Turn
	assert.Equal(t, turn.ContextPeak, 2796+2074+15333)
	assert.Equal(t, turn.ModelCalls, 1)
}

func TestParserCountsAMessageOnceAndSkipsSubagents(t *testing.T) {
	var assistant string
	for _, l := range realStreamLines(t) {
		if strings.Contains(l, `"type":"assistant"`) {
			assistant = l
		}
	}
	assert.Assert(t, assistant != "")
	// The same message again, as claude prints one event per content block.
	repeated := assistant
	// A subagent's message has a context of its own and is left out.
	subagent := strings.Replace(assistant, `"parent_tool_use_id":null`, `"parent_tool_use_id":"toolu_1"`, 1)
	subagent = strings.Replace(subagent, `msg_011CfwJVzX6bvw5XsdfyBf4J`, `msg_other`, 1)
	subagent = strings.Replace(subagent, `"cache_read_input_tokens":15333`, `"cache_read_input_tokens":99999`, 1)
	assert.Assert(t, subagent != assistant)

	p := Harness{}.NewImplementParser(nil)
	p.Write([]byte(assistant + "\n" + repeated + "\n" + subagent + "\n"))
	turn := p.Outcome().Turn
	assert.Equal(t, turn.ContextPeak, 2796+2074+15333)
	assert.Equal(t, turn.ModelCalls, 1)
}

func TestParserWithoutUsageHasNoContext(t *testing.T) {
	usage := regexp.MustCompile(`"usage":\{[^{}]*(\{[^{}]*\}[^{}]*)*\}`)
	var stream []string
	for _, l := range realStreamLines(t) {
		stream = append(stream, usage.ReplaceAllString(l, `"usage":{}`))
	}
	p := Harness{}.NewImplementParser(nil)
	p.Write([]byte(strings.Join(stream, "\n") + "\n"))
	turn := p.Outcome().Turn
	assert.Equal(t, turn.ContextPeak, 0)
	assert.Equal(t, turn.ModelCalls, 0)
}
