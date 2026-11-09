package app

import (
	"fmt"
	"strconv"
	"sync"
	"time"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
)

const streamJSONToolOutputCap = 5000

// StreamJSONArgs is the chat argument namespace stream_json_requested inspects and forces quiet on
type StreamJSONArgs struct {
	OutputFormat, Query, QueryFile string
	TUI                            bool
	Quiet                          *bool
}

// Reports --format stream-json, exit code 2 without a query or with --tui, and forces quiet mode on
func StreamJSONRequested(a StreamJSONArgs) (bool, int) {
	if a.OutputFormat != "stream-json" {
		return false, 0
	}
	if a.Query == "" && a.QueryFile == "" {
		fmt.Fprintln(stderrWriter(), "Error: --format stream-json requires -q/--query.")
		return false, 2
	}
	if a.TUI {
		fmt.Fprintln(stderrWriter(), "Error: --format stream-json cannot be used with --tui.")
		return false, 2
	}
	if a.Quiet != nil {
		*a.Quiet = true
	}
	return true, 0
}

// Milliseconds since the Unix epoch
func nowMS() int64 { return time.Now().UnixMilli() }

// StreamJSONAgent is the agent surface Attach routes, its stream delta and tool progress callback slots
type StreamJSONAgent interface {
	SetStreamDeltaCallback(cb any)
	SetToolProgressCallback(cb any)
}

// StreamJSONEmitter is the agent callback sink writing one flushed JSONL event per stdout line
type StreamJSONEmitter struct {
	mu          sync.Mutex
	sessionID   string
	start       time.Time
	toolStarted map[string]time.Time
}

// Writes the system init event and returns the emitter holding the session id and start time
func NewStreamJSONEmitter(model, sessionID string) *StreamJSONEmitter {
	e := &StreamJSONEmitter{sessionID: sessionID, start: time.Now(), toolStarted: map[string]time.Time{}}
	init := pyjson.NewObject()
	init.Set("type", "system")
	init.Set("subtype", "init")
	init.Set("model", model)
	init.Set("session_id", sessionID)
	e.emit(init)
	return e
}

// Routes the agent streaming and tool callbacks into this emitter and returns it
func (e *StreamJSONEmitter) Attach(agent StreamJSONAgent) *StreamJSONEmitter {
	agent.SetStreamDeltaCallback(e.OnTextDelta)
	agent.SetToolProgressCallback(e.OnToolProgress)
	return e
}

// Records the gateway assigned session id so an interrupted or failed run still names it
func (e *StreamJSONEmitter) BindSession(sessionID string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if sessionID != "" {
		e.sessionID = sessionID
	}
}

// Emits a text event for every truthy delta, dropping only the None and empty turn end sentinels
func (e *StreamJSONEmitter) OnTextDelta(text any) {
	if !pyrepr.Truthy(text) {
		return
	}
	ev := pyjson.NewObject()
	ev.Set("type", "text")
	ev.Set("text", pyrepr.Str(text))
	e.emit(ev)
}

// Maps tool.started to tool_use and tool.completed to tool_result, every other progress event ignored
func (e *StreamJSONEmitter) OnToolProgress(eventType, toolName, preview string, args map[string]any, extra map[string]any) {
	name := toolName
	if name == "" {
		name = "unknown"
	}
	callID := ""
	if pyrepr.Truthy(extra["tool_call_id"]) {
		callID = pyrepr.Str(extra["tool_call_id"])
	}
	key := callID
	if key == "" {
		key = name
	}
	switch eventType {
	case "tool.started":
		e.mu.Lock()
		e.toolStarted[key] = time.Now()
		e.mu.Unlock()
		payload := pyjson.NewObject()
		payload.Set("type", "tool_use")
		payload.Set("name", name)
		if callID != "" {
			payload.Set("tool_call_id", callID)
		}
		if args != nil {
			payload.Set("input", args)
		}
		e.emit(payload)
	case "tool.completed":
		var duration float64
		if pyrepr.Truthy(extra["duration"]) {
			duration = pyFloat(extra["duration"])
		} else {
			e.mu.Lock()
			started, ok := e.toolStarted[key]
			delete(e.toolStarted, key)
			e.mu.Unlock()
			if ok {
				duration = time.Since(started).Seconds()
			}
		}
		output := ""
		if pyrepr.Truthy(extra["result"]) {
			output = pyrepr.Str(extra["result"])
		}
		if pystr.Len(output) > streamJSONToolOutputCap {
			output = pystr.Slice(output, 0, streamJSONToolOutputCap) + "..."
		}
		payload := pyjson.NewObject()
		payload.Set("type", "tool_result")
		payload.Set("name", name)
		if callID != "" {
			payload.Set("tool_call_id", callID)
		}
		payload.Set("output", output)
		payload.Set("duration_ms", int64(duration*1000))
		payload.Set("is_error", pyrepr.Truthy(extra["is_error"]))
		e.emit(payload)
	}
}

// Python float() of a number, duration or numeric string
func pyFloat(v any) float64 {
	switch n := v.(type) {
	case float64:
		return n
	case float32:
		return float64(n)
	case int:
		return float64(n)
	case int64:
		return float64(n)
	case time.Duration:
		return n.Seconds()
	case bool:
		if n {
			return 1
		}
		return 0
	case string:
		f, err := strconv.ParseFloat(n, 64)
		if err != nil {
			panic(fmt.Sprintf("ValueError: could not convert string to float: %s", pyrepr.Repr(n)))
		}
		return f
	}
	panic(fmt.Sprintf("TypeError: float() argument must be a string or a real number, not '%s'", pyrepr.TypeName(v)))
}

// Writes the terminal result record once and returns the process exit code it reports
func (e *StreamJSONEmitter) EmitResult(result any, sessionID string, exitCode int) int {
	data, ok := result.(map[string]any)
	if !ok {
		final := ""
		if result != nil {
			final = pyrepr.Str(result)
		}
		data = map[string]any{"final_response": final}
	}
	if exitCode == 0 && pyrepr.Truthy(data["failed"]) {
		exitCode = 1
	}
	e.mu.Lock()
	if sessionID == "" {
		sessionID = e.sessionID
	}
	start := e.start
	e.mu.Unlock()
	tokens := pyjson.NewObject()
	tokens.Set("input", orZero(data["input_tokens"]))
	tokens.Set("output", orZero(data["output_tokens"]))
	tokens.Set("total", orZero(data["total_tokens"]))
	tokens.Set("cache_read", orZero(data["cache_read_tokens"]))
	tokens.Set("cache_write", orZero(data["cache_write_tokens"]))
	payload := pyjson.NewObject()
	payload.Set("type", "result")
	payload.Set("session_id", sessionID)
	payload.Set("exit_code", exitCode)
	text := ""
	if pyrepr.Truthy(data["final_response"]) {
		text = pyrepr.Str(data["final_response"])
	}
	payload.Set("text", text)
	payload.Set("tokens", tokens)
	payload.Set("duration_ms", int64(time.Since(start).Seconds()*1000))
	if pyrepr.Truthy(data["error"]) {
		payload.Set("error", pyrepr.Str(data["error"]))
	}
	if pyrepr.Truthy(data["replay_gap"]) {
		payload.Set("replay_gap", true)
	}
	e.emit(payload)
	fmt.Fprintf(stderrWriter(), "\nsession_id: %s\n", sessionID)
	return exitCode
}

// The value when truthy, otherwise the integer zero
func orZero(v any) any {
	if pyrepr.Truthy(v) {
		return v
	}
	return 0
}

// Writes one JSON line with a timestamp and flushes, a closed consumer pipe ending the write silently
func (e *StreamJSONEmitter) emit(obj *pyjson.Object) {
	obj.Set("timestamp", nowMS())
	line, err := pyjson.Dumps(obj, pyjson.EnsureASCII(false))
	if err != nil {
		panic(err)
	}
	out := console().Out
	e.mu.Lock()
	defer e.mu.Unlock()
	_, _ = fmt.Fprint(out, line+"\n")
}
