package app

import (
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/nickheyer/icebath/pkg/errchain"
	anyutil "github.com/nickheyer/icebath/pkg/utils/any"
	jsonutil "github.com/nickheyer/icebath/pkg/utils/json"
	textutil "github.com/nickheyer/icebath/pkg/utils/text"
)

const streamJSONToolOutputCap = 5000

// StreamJSONArgs is the chat argument namespace stream_json_requested inspects and forces quiet on
type StreamJSONArgs struct {
	OutputFormat, Query, QueryFile string
	Quiet                          *bool
}

// Usage error text of a chat launch that carries no query, printed with exit 2
const QuerylessChatUsage = "no query given, pass one with icebath chat -q/--query or pipe it on stdin"

// Text on stdin with its trailing newlines trimmed, read to EOF once per process
var StdinQuery = sync.OnceValues(func() (string, error) {
	raw, err := io.ReadAll(os.Stdin)
	return strings.TrimRight(string(raw), "\r\n"), err
})

// Error text of a stdin query read that failed
func stdinQueryError(err error) string {
	return fmt.Sprintf("cannot read the query from stdin (%v), pass one with icebath chat -q/--query", err)
}

// Sets an empty query to the stdin text, exit 2 with the usage error when stdin is blank and no image rides along
func QueryFromStdin(query *string, image string) int {
	text, err := StdinQuery()
	if err != nil {
		fmt.Fprintln(stderrWriter(), "Error: "+stdinQueryError(err))
		return 2
	}
	if strings.TrimSpace(text) != "" {
		*query = text
		return 0
	}
	if image != "" {
		return 0
	}
	fmt.Fprintln(stderrWriter(), "Error: "+QuerylessChatUsage)
	return 2
}

// Reports --format stream-json, exit code 2 without a query, and forces quiet mode on
func StreamJSONRequested(a StreamJSONArgs) (bool, int) {
	if a.OutputFormat != "stream-json" {
		return false, 0
	}
	if a.Query == "" && a.QueryFile == "" {
		fmt.Fprintln(stderrWriter(), "Error: --format stream-json requires -q/--query.")
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
	init := jsonutil.NewObject()
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
	if !anyutil.IsTrue(text) {
		return
	}
	ev := jsonutil.NewObject()
	ev.Set("type", "text")
	ev.Set("text", anyutil.String(text))
	e.emit(ev)
}

// Maps tool.started to tool_use and tool.completed to tool_result, every other progress event ignored
func (e *StreamJSONEmitter) OnToolProgress(eventType, toolName, preview string, args map[string]any, extra map[string]any) {
	name := toolName
	if name == "" {
		name = "unknown"
	}
	callID := ""
	if anyutil.IsTrue(extra["tool_call_id"]) {
		callID = anyutil.String(extra["tool_call_id"])
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
		payload := jsonutil.NewObject()
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
		if anyutil.IsTrue(extra["duration"]) {
			duration = floatNumber(extra["duration"])
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
		if anyutil.IsTrue(extra["result"]) {
			output = anyutil.String(extra["result"])
		}
		if utf8.RuneCountInString(output) > streamJSONToolOutputCap {
			output = textutil.First(output, streamJSONToolOutputCap) + "..."
		}
		payload := jsonutil.NewObject()
		payload.Set("type", "tool_result")
		payload.Set("name", name)
		if callID != "" {
			payload.Set("tool_call_id", callID)
		}
		payload.Set("output", output)
		payload.Set("duration_ms", int64(duration*1000))
		payload.Set("is_error", anyutil.IsTrue(extra["is_error"]))
		e.emit(payload)
	}
}

// Python float() of a number, duration or numeric string
func floatNumber(v any) float64 {
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
			panic(fmt.Sprintf("%s is not a number", n))
		}
		return f
	}
	panic(fmt.Sprintf("expected a number, got %s", errchain.Kind(v)))
}

// Writes the terminal result record once and returns the process exit code it reports
func (e *StreamJSONEmitter) EmitResult(result any, sessionID string, exitCode int) int {
	data, ok := result.(map[string]any)
	if !ok {
		final := ""
		if result != nil {
			final = anyutil.String(result)
		}
		data = map[string]any{"final_response": final}
	}
	if exitCode == 0 && anyutil.IsTrue(data["failed"]) {
		exitCode = 1
	}
	e.mu.Lock()
	if sessionID == "" {
		sessionID = e.sessionID
	}
	start := e.start
	e.mu.Unlock()
	tokens := jsonutil.NewObject()
	tokens.Set("input", orZero(data["input_tokens"]))
	tokens.Set("output", orZero(data["output_tokens"]))
	tokens.Set("total", orZero(data["total_tokens"]))
	tokens.Set("cache_read", orZero(data["cache_read_tokens"]))
	tokens.Set("cache_write", orZero(data["cache_write_tokens"]))
	payload := jsonutil.NewObject()
	payload.Set("type", "result")
	payload.Set("session_id", sessionID)
	payload.Set("exit_code", exitCode)
	text := ""
	if anyutil.IsTrue(data["final_response"]) {
		text = anyutil.String(data["final_response"])
	}
	payload.Set("text", text)
	payload.Set("tokens", tokens)
	payload.Set("duration_ms", int64(time.Since(start).Seconds()*1000))
	if anyutil.IsTrue(data["error"]) {
		payload.Set("error", anyutil.String(data["error"]))
	}
	if anyutil.IsTrue(data["replay_gap"]) {
		payload.Set("replay_gap", true)
	}
	e.emit(payload)
	fmt.Fprintf(stderrWriter(), "\nsession_id: %s\n", sessionID)
	return exitCode
}

// The value when truthy, otherwise the integer zero
func orZero(v any) any {
	if anyutil.IsTrue(v) {
		return v
	}
	return 0
}

// Writes one JSON line with a timestamp and flushes, a closed consumer pipe ending the write silently
func (e *StreamJSONEmitter) emit(obj *jsonutil.Object) {
	obj.Set("timestamp", nowMS())
	line, err := jsonutil.Encode(obj, jsonutil.Spaced())
	if err != nil {
		panic(err)
	}
	out := console().Out
	e.mu.Lock()
	defer e.mu.Unlock()
	_, _ = fmt.Fprint(out, line+"\n")
}
