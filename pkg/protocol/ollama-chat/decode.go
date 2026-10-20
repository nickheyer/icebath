package ollamachat

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"iter"
	"strconv"
	"strings"
	"time"

	"github.com/nickheyer/icebath/pkg/protocol"
)

var transientHints = []string{"timeout", "overloaded", "too many"}

// Reads the single ChatResponse of a non streaming call
func (Protocol) Decode(r io.Reader) (protocol.Response, error) {
	body, err := io.ReadAll(r)
	if err != nil {
		return protocol.Response{}, err
	}
	var wire response
	if err := json.Unmarshal(body, &wire); err != nil {
		return protocol.Response{}, fmt.Errorf("ollama-chat: response: %w", err)
	}
	if wire.Error != "" {
		return protocol.Response{}, &protocol.Error{Status: 200, Message: wire.Error, Body: body, Transient: transientMessage(wire.Error)}
	}
	if !wire.Done {
		return protocol.Response{}, fmt.Errorf("ollama-chat: response is not done")
	}
	out := protocol.Response{Model: wire.Model, Usage: usage(wire), Metrics: mapMetrics(wire)}
	if wire.Message.Thinking != "" {
		out.Content = append(out.Content, protocol.Thinking{Text: wire.Message.Thinking, Origin: protocol.OllamaChat})
	}
	if wire.Message.Content != "" {
		out.Content = append(out.Content, protocol.Text{Text: wire.Message.Content})
	}
	for _, tc := range wire.Message.ToolCalls {
		args, err := completeArgs(tc.Function.Arguments)
		if err != nil {
			return protocol.Response{}, fmt.Errorf("ollama-chat: tool call %s: %w", tc.Function.Name, err)
		}
		out.Content = append(out.Content, protocol.ToolCall{ID: tc.ID, Name: tc.Function.Name, Args: args})
	}
	out.StopReason, out.RawStopReason = stopReason(wire.DoneReason, len(wire.Message.ToolCalls) > 0)
	return out, nil
}

// Maps done_reason, which the server leaves empty on a natural stop, onto the neutral set
func stopReason(reason string, hasCalls bool) (protocol.StopReason, string) {
	switch reason {
	case "", "stop":
		if hasCalls {
			return protocol.StopToolUse, reason
		}
		return protocol.StopEnd, reason
	case "length":
		return protocol.StopLength, reason
	}
	return protocol.StopOther, reason
}

func usage(wire response) protocol.Usage {
	return protocol.Usage{Input: wire.PromptEvalCount, Output: wire.EvalCount}
}

// Maps the nanosecond duration fields onto the neutral timings
func mapMetrics(wire response) protocol.Metrics {
	return protocol.Metrics{
		Latency:  time.Duration(wire.TotalDuration),
		Load:     time.Duration(wire.LoadDuration),
		Prompt:   time.Duration(wire.PromptEvalDuration),
		Generate: time.Duration(wire.EvalDuration),
	}
}

// Reports whether an error message describes a timeout or an overloaded server
func transientMessage(msg string) bool {
	lower := strings.ToLower(msg)
	for _, hint := range transientHints {
		if strings.Contains(lower, hint) {
			return true
		}
	}
	return false
}

// Validates complete arguments arriving as an object or as a JSON encoded string
func completeArgs(raw json.RawMessage) (json.RawMessage, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return protocol.ParseArgs("")
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return protocol.ParseArgs(s)
	}
	var buf bytes.Buffer
	if err := json.Compact(&buf, raw); err != nil {
		return nil, err
	}
	return protocol.ParseArgs(buf.String())
}

// Tool call whose arguments may arrive as string fragments or object fragments across lines
type pendingCall struct {
	key     string
	name    string
	text    bool
	objects bool
	fields  map[string]json.RawMessage
}

// Tracks the block receiving fragments and the tool call accumulating by index across NDJSON lines
type streamState struct {
	b        protocol.Builder
	open     string
	kind     string
	n        int
	hasCalls bool
	calls    map[int]*pendingCall
	current  int
}

// Yields neutral events from NDJSON lines until the line with done set
func (Protocol) Stream(r io.Reader) iter.Seq2[protocol.Event, error] {
	return func(yield func(protocol.Event, error) bool) {
		st := &streamState{b: protocol.Builder{Origin: protocol.OllamaChat}, calls: map[int]*pendingCall{}, current: -1}
		br := bufio.NewReader(r)
		for {
			line, err := br.ReadString('\n')
			if err != nil && err != io.EOF {
				yield(protocol.Event{}, err)
				return
			}
			line = strings.TrimSpace(line)
			if line != "" {
				done, perr := st.apply(line)
				if perr != nil {
					yield(protocol.Event{}, perr)
					return
				}
				for _, ev := range st.b.Events() {
					if !yield(ev, nil) {
						return
					}
				}
				if done {
					return
				}
			}
			if err == io.EOF {
				yield(protocol.Event{}, fmt.Errorf("ollama-chat: stream ended before a done line"))
				return
			}
		}
	}
}

func (st *streamState) apply(line string) (bool, error) {
	var chunk response
	if err := json.Unmarshal([]byte(line), &chunk); err != nil {
		return false, fmt.Errorf("ollama-chat: stream line: %w", err)
	}
	if chunk.Error != "" {
		return false, &protocol.Error{Status: 200, Message: chunk.Error, Body: []byte(line), InStream: true, Transient: transientMessage(chunk.Error)}
	}
	st.b.Start("", chunk.Model)
	if chunk.Message.Thinking != "" {
		if err := st.fragment("thinking", chunk.Message.Thinking); err != nil {
			return false, err
		}
	}
	if chunk.Message.Content != "" {
		if err := st.fragment("text", chunk.Message.Content); err != nil {
			return false, err
		}
	}
	for _, tc := range chunk.Message.ToolCalls {
		if err := st.toolCall(tc); err != nil {
			return false, err
		}
	}
	if !chunk.Done {
		return false, nil
	}
	if err := st.closeOpen(); err != nil {
		return false, err
	}
	if err := st.closeCall(); err != nil {
		return false, err
	}
	stop, raw := stopReason(chunk.DoneReason, st.hasCalls)
	st.b.Metrics(mapMetrics(chunk))
	return true, st.b.Finish(stop, raw, usage(chunk))
}

// Appends to the open block of kind, closing a block of another kind or a pending call first
func (st *streamState) fragment(kind, s string) error {
	if err := st.closeCall(); err != nil {
		return err
	}
	if st.open == "" || st.kind != kind {
		if err := st.closeOpen(); err != nil {
			return err
		}
		st.n++
		st.open, st.kind = kind+"-"+strconv.Itoa(st.n), kind
		if kind == "thinking" {
			st.b.OpenThinking(st.open, false)
		} else {
			st.b.OpenText(st.open)
		}
	}
	return st.b.Delta(st.open, s)
}

// Accumulates a tool call by index, closing the previous index or a renamed call at the same index
func (st *streamState) toolCall(tc toolCall) error {
	if err := st.closeOpen(); err != nil {
		return err
	}
	idx := tc.Function.Index
	if idx == 0 {
		idx = tc.Index
	}
	pc := st.calls[idx]
	if pc != nil && tc.Function.Name != "" && pc.name != tc.Function.Name {
		if err := st.closeCall(); err != nil {
			return err
		}
		pc = nil
	}
	if pc == nil {
		if err := st.closeCall(); err != nil {
			return err
		}
		st.n++
		pc = &pendingCall{key: "call-" + strconv.Itoa(st.n), name: tc.Function.Name, fields: map[string]json.RawMessage{}}
		st.calls[idx] = pc
		st.current = idx
		st.hasCalls = true
		st.b.OpenToolCall(pc.key, tc.ID, tc.Function.Name)
	}
	return st.arguments(pc, tc.Function.Arguments)
}

// Appends string fragments as deltas and merges object fragments for delivery at close
func (st *streamState) arguments(pc *pendingCall, raw json.RawMessage) error {
	if len(raw) == 0 || string(raw) == "null" {
		return nil
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		if pc.objects {
			return fmt.Errorf("ollama-chat: tool call %s mixes string and object arguments", pc.name)
		}
		pc.text = true
		return st.b.Delta(pc.key, s)
	}
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(raw, &obj); err != nil || obj == nil {
		return fmt.Errorf("ollama-chat: tool call %s: arguments are not a JSON object", pc.name)
	}
	if pc.text {
		return fmt.Errorf("ollama-chat: tool call %s mixes string and object arguments", pc.name)
	}
	pc.objects = true
	for k, v := range obj {
		pc.fields[k] = v
	}
	return nil
}

// Finalizes the call at the current index, delivering merged object arguments as one delta
func (st *streamState) closeCall() error {
	pc, ok := st.calls[st.current]
	if !ok {
		return nil
	}
	delete(st.calls, st.current)
	st.current = -1
	if pc.objects {
		raw, err := json.Marshal(pc.fields)
		if err != nil {
			return fmt.Errorf("ollama-chat: tool call %s: %w", pc.name, err)
		}
		if err := st.b.Delta(pc.key, string(raw)); err != nil {
			return err
		}
	}
	return st.b.Close(pc.key)
}

func (st *streamState) closeOpen() error {
	if st.open == "" {
		return nil
	}
	key := st.open
	st.open, st.kind = "", ""
	return st.b.Close(key)
}

// Parses the error string envelope, otherwise reporting the trimmed body text
func (Protocol) DecodeError(status int, body []byte) *protocol.Error {
	e := &protocol.Error{Status: status, Body: body, Transient: transientStatus(status)}
	var wire errorBody
	if err := json.Unmarshal(body, &wire); err == nil && wire.Error != "" {
		e.Message = wire.Error
	} else {
		e.Message = strings.TrimSpace(string(body))
	}
	e.Transient = e.Transient || transientMessage(e.Message)
	return e
}

// Reports statuses that denote a timeout, conflict, throttle or server failure
func transientStatus(status int) bool {
	return status == 408 || status == 409 || status == 429 || status >= 500
}
