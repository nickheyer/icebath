package gateway

import "sort"

// StreamEvent is the closed union of transport events the agent worker emits to the stream consumer
type StreamEvent interface {
	isStreamEvent()
}

// MessageChunk is one text delta of the assistant reply
type MessageChunk struct {
	Text string
}

// MessageStop ends a text segment, an intermediate stop starts a fresh segment below tool chrome
type MessageStop struct {
	Final bool
}

// Commentary is interim assistant text shown between tool calls
type Commentary struct {
	Text string
}

// ToolCallChunk announces a tool call with its preview, arguments and per turn index
type ToolCallChunk struct {
	ToolName string
	Preview  *string
	// Values as Python holds them, int64 for an int and float64 for a float
	Args    map[string]any
	ArgKeys []string
	Index   int
}

// ToolCallFinished closes a tool call with its wall clock duration and whether it returned
type ToolCallFinished struct {
	ToolName string
	Duration float64
	OK       bool
	Index    int
}

// LongToolHint flags a long running tool for the verbose capability hint
type LongToolHint struct {
	ToolName string
	Duration float64
}

// GatewayNotice is a gateway owned notice such as restart or online with its default rendering
type GatewayNotice struct {
	Kind  string
	Text  string
	Extra map[string]any
}

func (MessageChunk) isStreamEvent()     {}
func (MessageStop) isStreamEvent()      {}
func (Commentary) isStreamEvent()       {}
func (ToolCallChunk) isStreamEvent()    {}
func (ToolCallFinished) isStreamEvent() {}
func (LongToolHint) isStreamEvent()     {}
func (GatewayNotice) isStreamEvent()    {}

// Argument keys in insertion order, the recorded order first then any key it misses in sorted order
func (c ToolCallChunk) OrderedArgKeys() []string {
	seen := map[string]bool{}
	out := make([]string, 0, len(c.Args))
	for _, k := range c.ArgKeys {
		if _, ok := c.Args[k]; ok && !seen[k] {
			seen[k] = true
			out = append(out, k)
		}
	}
	rest := make([]string, 0)
	for k := range c.Args {
		if !seen[k] {
			rest = append(rest, k)
		}
	}
	sort.Strings(rest)
	return append(out, rest...)
}
