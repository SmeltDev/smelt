package grok

import (
	"encoding/json"
	"strings"

	"github.com/smeltdev/smelt/internal/harness"
)

// streamEvent is the part of one streaming-messages-json line the implementer
// reads. The format is Anthropic's Messages format, as Claude Code's
// stream-json is.
type streamEvent struct {
	Type      string `json:"type"`
	SessionID string `json:"session_id"`
	// ParentToolUseID is set on messages a subagent sent, which have a context
	// of their own.
	ParentToolUseID *string `json:"parent_tool_use_id"`
	Message         struct {
		ID    string `json:"id"`
		Usage struct {
			InputTokens         int `json:"input_tokens"`
			CacheCreationTokens int `json:"cache_creation_input_tokens"`
			CacheReadTokens     int `json:"cache_read_input_tokens"`
		} `json:"usage"`
		Content []struct {
			Type  string          `json:"type"`
			Text  string          `json:"text"`
			Name  string          `json:"name"`
			Input json.RawMessage `json:"input"`
		} `json:"content"`
	} `json:"message"`
	IsError      bool    `json:"is_error"`
	Result       string  `json:"result"`
	TotalCostUSD float64 `json:"total_cost_usd"`
	Usage        struct {
		InputTokens         int `json:"input_tokens"`
		CacheCreationTokens int `json:"cache_creation_input_tokens"`
		CacheReadTokens     int `json:"cache_read_input_tokens"`
		OutputTokens        int `json:"output_tokens"`
	} `json:"usage"`
}

// toolNames maps Grok's tools to the names the other harnesses report, so a
// run reads the same whichever agent ran it.
var toolNames = map[string]string{
	"read_file":            "Read",
	"search_replace":       "Edit",
	"run_terminal_command": "Bash",
	"grep":                 "Grep",
	"list_dir":             "LS",
}

// parser reports what the implementer does as grok's stream arrives.
type parser struct {
	harness.Lines
	onActivity func(harness.Activity)
	sessionID  string
	result     streamEvent
	sawResult  bool
	// peak and calls are the largest prompt and the number of model calls of
	// the turn. A call is counted once per message ID.
	peak, calls int
	seenMsgs    map[string]bool
}

// NewImplementParser implements harness.Harness.
func (Harness) NewImplementParser(onActivity func(harness.Activity)) harness.StreamParser {
	p := &parser{onActivity: onActivity}
	p.Handle = p.line
	return p
}

func (p *parser) line(b []byte) {
	var e streamEvent
	if json.Unmarshal(b, &e) != nil {
		return
	}
	if e.SessionID != "" {
		p.sessionID = e.SessionID
	}
	switch e.Type {
	case "result":
		p.result, p.sawResult = e, true
	case "assistant":
		p.context(e)
		p.activity(e)
	}
}

// context takes the size of the model call an assistant message was made by:
// everything sent, whether read from the cache, written to it or neither.
func (p *parser) context(e streamEvent) {
	u := e.Message.Usage
	size := u.InputTokens + u.CacheCreationTokens + u.CacheReadTokens
	if size == 0 || e.ParentToolUseID != nil {
		return
	}
	p.peak = max(p.peak, size)
	if e.Message.ID != "" {
		if p.seenMsgs[e.Message.ID] {
			return
		}
		if p.seenMsgs == nil {
			p.seenMsgs = map[string]bool{}
		}
		p.seenMsgs[e.Message.ID] = true
	}
	p.calls++
}

func (p *parser) activity(e streamEvent) {
	if p.onActivity == nil {
		return
	}
	for _, c := range e.Message.Content {
		switch c.Type {
		case "text":
			if t := strings.TrimSpace(c.Text); t != "" {
				p.onActivity(harness.Activity{Detail: t})
			}
		case "tool_use":
			tool, ok := toolNames[c.Name]
			if !ok {
				tool = c.Name
			}
			p.onActivity(harness.Activity{Tool: tool, Detail: toolDetail(c.Input)})
		}
	}
}

// Outcome implements harness.StreamParser.
func (p *parser) Outcome() harness.Outcome {
	u := p.result.Usage
	return harness.Outcome{
		Turn: harness.Turn{
			Summary:      p.result.Result,
			CostUSD:      p.result.TotalCostUSD,
			InputTokens:  u.InputTokens + u.CacheCreationTokens + u.CacheReadTokens,
			CachedTokens: u.CacheReadTokens,
			OutputTokens: u.OutputTokens,
			ContextPeak:  p.peak,
			ModelCalls:   p.calls,
			SessionID:    p.sessionID,
		},
		Done:    p.sawResult,
		Failed:  p.result.IsError,
		Message: p.result.Result,
	}
}

// toolDetail picks the field of a tool's input that says what it acted on.
func toolDetail(input json.RawMessage) string {
	var in map[string]any
	if json.Unmarshal(input, &in) != nil {
		return ""
	}
	for _, key := range []string{"target_file", "file_path", "command", "pattern", "path", "target_directory", "description"} {
		if v, ok := in[key].(string); ok && v != "" {
			return v
		}
	}
	return ""
}
