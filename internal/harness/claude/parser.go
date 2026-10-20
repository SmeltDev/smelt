package claude

import (
	"encoding/json"
	"errors"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/smeltdev/smelt/internal/harness"
)

// streamEvent is the part of one stream-json line the implementer reads.
type streamEvent struct {
	Type      string `json:"type"`
	Subtype   string `json:"subtype"`
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
			Type      string          `json:"type"`
			Text      string          `json:"text"`
			Name      string          `json:"name"`
			Input     json.RawMessage `json:"input"`
			ID        string          `json:"id"`
			ToolUseID string          `json:"tool_use_id"`
			IsError   bool            `json:"is_error"`
			Content   json.RawMessage `json:"content"`
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
		Status         string `json:"status"`
		ResetsAt       int64  `json:"resetsAt"`
		RateLimitType  string `json:"rateLimitType"`
		Utilization    *int   `json:"utilization"`
		UnifiedWindows map[string]struct {
			Utilization float64 `json:"utilization"`
			ResetsAt    int64   `json:"resetsAt"`
		} `json:"unifiedWindows"`
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
	compactions int
	seenMsgs    map[string]bool
	// called are the IDs of the tool calls reported, and early the results that
	// arrived before their call.
	called map[string]bool
	early  map[string]harness.Activity
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
	case "system":
		if e.Subtype == "compact_boundary" {
			p.compactions++
		}
	case "assistant":
		p.context(e)
		p.activity(e)
	case "user":
		p.results(e)
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
	highest := -1.0
	if rl.Utilization != nil {
		highest = float64(*rl.Utilization) / 100
	}
	for name, w := range info.UnifiedWindows {
		if rl.Windows == nil {
			rl.Windows = map[string]harness.UsageWindow{}
		}
		var reset *time.Time
		if w.ResetsAt > 0 {
			t := time.Unix(w.ResetsAt, 0).UTC()
			reset = &t
		}
		bucket := rateLimitBuckets[name]
		if bucket == "" {
			bucket = name
		}
		rl.Windows[bucket] = harness.UsageWindow{Utilization: w.Utilization, ResetsAt: reset}
		if w.Utilization > highest {
			highest = w.Utilization
			pct := int(w.Utilization * 100)
			rl.Utilization = &pct
			rl.Bucket = bucket
			rl.ResetsAt = reset
		}
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
			p.toolUse(c.ID, c.Name, toolDetail(c.Input))
		}
	}
}

// toolUse reports a call, with its result if that came first.
func (p *parser) toolUse(id, name, detail string) {
	a := harness.Activity{Tool: name, Detail: detail, ID: id}
	if early, ok := p.early[id]; ok && id != "" {
		delete(p.early, id)
		a.Outcome, a.ExitCode, a.Error = early.Outcome, early.ExitCode, early.Error
	} else if id != "" {
		if p.called == nil {
			p.called = map[string]bool{}
		}
		p.called[id] = true
	}
	p.onActivity(a)
}

// results reports how the calls in a user event ended, matched by tool_use_id.
// A result whose call has not been seen is kept until it is.
func (p *parser) results(e streamEvent) {
	if p.onActivity == nil {
		return
	}
	for _, c := range e.Message.Content {
		if c.Type != "tool_result" || c.ToolUseID == "" {
			continue
		}
		a := toolResult(c.ToolUseID, c.IsError, resultText(c.Content))
		if p.called[c.ToolUseID] {
			delete(p.called, c.ToolUseID)
			p.onActivity(a)
			continue
		}
		if p.early == nil {
			p.early = map[string]harness.Activity{}
		}
		p.early[c.ToolUseID] = a
	}
}

// exitCodeRe finds the exit code claude prints first in a failed Bash result.
var exitCodeRe = regexp.MustCompile(`^Exit code (\d+)`)

// toolResult builds the Update for a call's result.
func toolResult(id string, isError bool, text string) harness.Activity {
	a := harness.Activity{ID: id, Update: true, Outcome: harness.CallOK}
	if !isError {
		return a
	}
	a.Outcome = harness.CallFailed
	text = strings.TrimSpace(text)
	if m := exitCodeRe.FindStringSubmatch(text); m != nil {
		if n, err := strconv.Atoi(m[1]); err == nil {
			a.ExitCode = &n
		}
		_, text, _ = strings.Cut(text, "\n")
	}
	a.Error = harness.FirstLine(text)
	return a
}

// resultText reads a tool_result's content, a string or a list of text blocks.
func resultText(raw json.RawMessage) string {
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	var blocks []struct {
		Text string `json:"text"`
	}
	if json.Unmarshal(raw, &blocks) != nil {
		return ""
	}
	parts := make([]string, 0, len(blocks))
	for _, b := range blocks {
		parts = append(parts, b.Text)
	}
	return strings.Join(parts, "\n")
}

// Outcome implements harness.StreamParser.
func (p *parser) Outcome() harness.Outcome {
	u := p.result.Usage
	var err error
	if p.result.IsError && p.rateLimit != nil && p.rateLimit.Status != "allowed" && p.rateLimit.Status != "allowed_warning" {
		err = harness.Classified(harness.RateLimited, errors.New(p.result.Result), p.rateLimit.ResetsAt)
	} else if p.result.IsError && CredentialRejected(p.result.Result, "") {
		err = harness.Classified(harness.Auth, ErrCredentialRejected, nil)
	}
	return harness.Outcome{Err: err,
		Turn: harness.Turn{
			Summary:      p.result.Result,
			CostUSD:      p.result.TotalCostUSD,
			InputTokens:  u.InputTokens + u.CacheCreationTokens + u.CacheReadTokens,
			CachedTokens: u.CacheReadTokens,
			OutputTokens: u.OutputTokens,
			ContextPeak:  p.peak,
			ModelCalls:   p.calls,
			Compactions:  p.compactions,
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
