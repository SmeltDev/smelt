package claude

import (
	"encoding/json"
	"strings"
	"time"

	"github.com/smeltdev/smelt/internal/harness"
)

// streamEvent is the part of one stream-json line the implementer reads.
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
	RateLimitInfo struct {
		Status        string `json:"status"`
		ResetsAt      int64  `json:"resetsAt"`
		RateLimitType string `json:"rateLimitType"`
		Utilization   *int   `json:"utilization"`
	} `json:"rate_limit_info"`
}

// rateLimitBuckets names claude's rate-limit types the way harness status does.
var rateLimitBuckets = map[string]string{"five_hour": "5h", "seven_day": "7d"}

// parser reports what the implementer does as claude's stream-json arrives.
type parser struct {
	harness.Lines
	onActivity func(harness.Activity)
	sessionID  string
	result     streamEvent
	sawResult  bool
	rateLimit  *harness.RateLimit
	// peak and calls are the largest prompt and the number of model calls of
	// the turn. claude can print one assistant event per content block of the
	// same message, so a call is counted once per message ID.
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
	case "rate_limit_event":
		p.rateLimit = rateLimit(e)
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

func rateLimit(e streamEvent) *harness.RateLimit {
	info := e.RateLimitInfo
	rl := &harness.RateLimit{Status: info.Status, Bucket: rateLimitBuckets[info.RateLimitType], Utilization: info.Utilization}
	if rl.Bucket == "" {
		rl.Bucket = info.RateLimitType
	}
	if info.ResetsAt > 0 {
		t := time.Unix(info.ResetsAt, 0).UTC()
		rl.ResetsAt = &t
	}
	return rl
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
			p.onActivity(harness.Activity{Tool: c.Name, Detail: toolDetail(c.Input)})
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
			RateLimit:    p.rateLimit,
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
	for _, key := range []string{"file_path", "command", "pattern", "path", "description"} {
		if v, ok := in[key].(string); ok && v != "" {
			return v
		}
	}
	return ""
}
