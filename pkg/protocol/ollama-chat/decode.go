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

	"github.com/nickheyer/icebath/pkg/protocol"
)

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
		return protocol.Response{}, &protocol.Error{Status: 200, Message: wire.Error, Body: body}
	}
	if !wire.Done {
		return protocol.Response{}, fmt.Errorf("ollama-chat: response is not done")
	}
	out := protocol.Response{Model: wire.Model, Usage: usage(wire)}
	if wire.Message.Thinking != "" {
		out.Content = append(out.Content, protocol.Thinking{Text: wire.Message.Thinking})
	}
	if wire.Message.Content != "" {
		out.Content = append(out.Content, protocol.Text{Text: wire.Message.Content})
	}
	for _, tc := range wire.Message.ToolCalls {
		args, err := compactArgs(tc.Function.Arguments)
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
	return protocol.Usage{Input: wire.PromptEvalCount, Output: wire.EvalCount, CacheRead: wire.PromptEvalCachedCount}
}

func compactArgs(raw json.RawMessage) (json.RawMessage, error) {
	if len(raw) == 0 {
		return protocol.ParseArgs("")
	}
	var buf bytes.Buffer
	if err := json.Compact(&buf, raw); err != nil {
		return nil, err
	}
	return protocol.ParseArgs(buf.String())
}

// Tracks the block receiving fragments across NDJSON lines
type streamState struct {
	b        protocol.Builder
	open     string
	kind     string
	n        int
	hasCalls bool
}

// Yields neutral events from NDJSON lines until the line with done set
func (Protocol) Stream(r io.Reader) iter.Seq2[protocol.Event, error] {
	return func(yield func(protocol.Event, error) bool) {
		st := &streamState{}
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
		return false, &protocol.Error{Status: 200, Message: chunk.Error, Body: []byte(line)}
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
	stop, raw := stopReason(chunk.DoneReason, st.hasCalls)
	return true, st.b.Finish(stop, raw, usage(chunk))
}

// Appends to the open block of kind, closing a block of another kind first
func (st *streamState) fragment(kind, s string) error {
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

// Emits a whole tool call, which the server sends complete in one line
func (st *streamState) toolCall(tc toolCall) error {
	if err := st.closeOpen(); err != nil {
		return err
	}
	args, err := compactArgs(tc.Function.Arguments)
	if err != nil {
		return fmt.Errorf("ollama-chat: tool call %s: %w", tc.Function.Name, err)
	}
	st.hasCalls = true
	st.n++
	key := "call-" + strconv.Itoa(st.n)
	st.b.OpenToolCall(key, tc.ID, tc.Function.Name)
	if err := st.b.Delta(key, string(args)); err != nil {
		return err
	}
	return st.b.Close(key)
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
	var wire errorBody
	if err := json.Unmarshal(body, &wire); err == nil && wire.Error != "" {
		return &protocol.Error{Status: status, Message: wire.Error, Body: body}
	}
	return &protocol.Error{Status: status, Message: strings.TrimSpace(string(body)), Body: body}
}
