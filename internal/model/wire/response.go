package wire

import (
	"encoding/json"
	"net/http"
	"strings"
	"time"
)

// Why a provider stopped generating, in the lowercase chat vocabulary
type FinishReason string

const (
	FinishStop          FinishReason = "stop"
	FinishToolCalls     FinishReason = "tool_calls"
	FinishLength        FinishReason = "length"
	FinishContentFilter FinishReason = "content_filter"
	FinishIncomplete    FinishReason = "incomplete"
)

// Folds a provider finish word to lowercase and its alias, empty staying empty
func NormalizeFinish(raw string) FinishReason {
	if raw == "" {
		return ""
	}
	lowered := strings.ToLower(raw)
	switch lowered {
	case "max_tokens":
		return FinishLength
	case "end":
		return FinishStop
	case "function_call":
		return FinishToolCalls
	}
	return FinishReason(lowered)
}

// Translates a provider stop word through a table, an absent or unlisted word meaning stop
func MapFinish(word string, table map[string]FinishReason) FinishReason {
	if mapped, ok := table[word]; ok {
		return mapped
	}
	return FinishStop
}

// Token counters of one response or a sum of responses
type Usage struct {
	InputTokens      int
	OutputTokens     int
	CacheReadTokens  int
	CacheWriteTokens int
	ReasoningTokens  int
	Requests         int
	FirstHopInput    int
	FirstHopCached   int
	ServedTier       string
	Speed            string
	ContextWindow    int
}

// Input the provider priced, uncached plus cache reads and writes
func (u Usage) PromptTokens() int {
	return u.InputTokens + u.CacheReadTokens + u.CacheWriteTokens
}

// Prompt and output tokens together
func (u Usage) TotalTokens() int {
	return u.PromptTokens() + u.OutputTokens
}

// Sums the counters and request counts, dropping the detail that belongs to a single response
func (u Usage) Add(o Usage) Usage {
	return Usage{
		InputTokens:      u.InputTokens + o.InputTokens,
		OutputTokens:     u.OutputTokens + o.OutputTokens,
		CacheReadTokens:  u.CacheReadTokens + o.CacheReadTokens,
		CacheWriteTokens: u.CacheWriteTokens + o.CacheWriteTokens,
		ReasoningTokens:  u.ReasoningTokens + o.ReasoningTokens,
		Requests:         u.Requests + o.Requests,
	}
}

// Assembled result of one provider call, Next naming a follow-up call that continues or resumes the same turn
type Response struct {
	Message        Message
	FinishReason   FinishReason
	RawFinish      string
	StopDetails    json.RawMessage
	Incomplete     string
	Usage          *Usage
	ID             string
	Model          string
	ServedModel    string
	Upstream       string
	Header         http.Header
	FirstByte      time.Time
	Streamed       bool
	Projected      []Message
	ToolIterations int
	IssuerKind     string
	IssuerModel    string
	Next           *Call
}

// What a stream event reports
type EventKind uint8

const (
	EventOpen EventKind = iota + 1
	EventText
	EventReasoning
	EventToolStart
	EventToolArgs
	EventRefusal
	EventUsage
	EventKeepalive
	EventProgress
	EventNotice
	EventItemStart
	EventItemDone
	EventMessage
	EventClose
)

// One streamed fact delivered in order to the sink
type Event struct {
	Kind     EventKind
	Text     string
	Inline   bool
	Tool     ToolDelta
	Usage    *Usage
	Progress Progress
	Notice   Notice
	Item     *Item
	Message  *Message
	End      StreamEnd
	At       time.Time
}

type StreamEnd struct {
	Finished bool
	Error    string
}

// Fragment of a tool call the model is generating
type ToolDelta struct {
	Index int
	ID    string
	Name  string
	Args  string
}

// Prompt or model-load progress of a local server
type Progress struct {
	Stage      string
	Done       int64
	Total      int64
	Percent    float64
	ServerTime time.Duration
	Timed      bool
}

// What a router notice announces
type NoticeKind uint8

const (
	NoticeWait NoticeKind = iota + 1
	NoticeDrop
	NoticeReconnect
	NoticeDegrade
)

// Wait, drop, reconnect or degrade announcement raised around a call
type Notice struct {
	Kind    NoticeKind
	Text    string
	Attempt int
	Elapsed time.Duration
}

// Tool item of an agent runtime with a stable id, Timed marking a known duration
type Item struct {
	ID       string
	Type     string
	Name     string
	Args     json.RawMessage
	Preview  string
	Result   string
	IsError  bool
	Duration time.Duration
	Timed    bool
}

// Receives each event in stream order on the goroutine that reads the stream
type Sink func(Event)
