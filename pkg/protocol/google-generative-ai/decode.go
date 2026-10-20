package googlegenerativeai

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"iter"
	"strconv"
	"strings"

	"github.com/nickheyer/icebath/pkg/protocol"
)

func (Protocol) Decode(r io.Reader) (protocol.Response, error) {
	raw, err := io.ReadAll(r)
	if err != nil {
		return protocol.Response{}, err
	}
	var resp response
	if err := json.Unmarshal(raw, &resp); err != nil {
		return protocol.Response{}, fmt.Errorf("google-generative-ai: response: %w", err)
	}
	if resp.Error != nil {
		return protocol.Response{}, wireError(resp.Error, raw)
	}
	var st state
	st.b.Start(resp.ResponseID, resp.ModelVersion)
	if err := st.apply(resp); err != nil {
		return protocol.Response{}, err
	}
	if len(resp.Candidates) == 0 {
		return protocol.Response{}, fmt.Errorf("google-generative-ai: response has no candidates")
	}
	if err := st.finish(); err != nil {
		return protocol.Response{}, err
	}
	return st.b.Response, nil
}

// Yields neutral events from alt=sse chunks until the chunk carrying finishReason
func (Protocol) Stream(r io.Reader) iter.Seq2[protocol.Event, error] {
	return func(yield func(protocol.Event, error) bool) {
		var st state
		for rec, err := range records(r) {
			if err != nil {
				yield(protocol.Event{}, err)
				return
			}
			data, isData := rec.data()
			if !isData {
				yield(protocol.Event{}, rawError(rec.lines))
				return
			}
			var chunk response
			if err := json.Unmarshal([]byte(data), &chunk); err != nil {
				yield(protocol.Event{}, fmt.Errorf("google-generative-ai: stream chunk: %w", err))
				return
			}
			if chunk.Error != nil {
				yield(protocol.Event{}, wireError(chunk.Error, []byte(data)))
				return
			}
			st.b.Start(chunk.ResponseID, chunk.ModelVersion)
			if err := st.apply(chunk); err != nil {
				yield(protocol.Event{}, err)
				return
			}
			for _, ev := range st.b.Events() {
				if !yield(ev, nil) {
					return
				}
			}
		}
		if st.finishReason == "" {
			yield(protocol.Event{}, fmt.Errorf("google-generative-ai: stream ended before finishReason"))
			return
		}
		if err := st.finish(); err != nil {
			yield(protocol.Event{}, err)
			return
		}
		for _, ev := range st.b.Events() {
			if !yield(ev, nil) {
				return
			}
		}
	}
}

func (Protocol) DecodeError(status int, body []byte) *protocol.Error {
	var env errorEnvelope
	if err := json.Unmarshal(body, &env); err == nil && env.Error != nil {
		return &protocol.Error{Status: status, Type: env.Error.Status, Message: env.Error.Message, Body: body}
	}
	return &protocol.Error{Status: status, Message: strings.TrimSpace(string(body)), Body: body}
}

func wireError(e *apiError, body []byte) *protocol.Error {
	return &protocol.Error{Status: e.Code, Type: e.Status, Message: e.Message, Body: body}
}

// Parses a non data record the way the official SDKs do, as a bare error document
func rawError(lines []string) error {
	body := []byte(strings.Join(lines, "\n"))
	var env errorEnvelope
	if err := json.Unmarshal(body, &env); err != nil || env.Error == nil {
		return fmt.Errorf("google-generative-ai: invalid stream chunk: %s", body)
	}
	return wireError(env.Error, body)
}

// Folds chunk parts into one Builder, keeping one block open across chunk boundaries
type state struct {
	b            protocol.Builder
	open         string
	kind         string
	n            int
	finishReason string
	hasCalls     bool
	usage        protocol.Usage
}

func (st *state) apply(resp response) error {
	if resp.UsageMetadata != nil {
		u := resp.UsageMetadata
		st.usage = protocol.Usage{Input: u.PromptTokenCount, Output: u.CandidatesTokenCount, CacheRead: u.CachedContentTokenCount, Reasoning: u.ThoughtsTokenCount}
	}
	if resp.PromptFeedback != nil && resp.PromptFeedback.BlockReason != "" {
		return fmt.Errorf("google-generative-ai: prompt blocked: %s", resp.PromptFeedback.BlockReason)
	}
	if len(resp.Candidates) == 0 {
		return nil
	}
	cand := resp.Candidates[0]
	for i, p := range cand.Content.Parts {
		if err := st.applyPart(p); err != nil {
			return fmt.Errorf("google-generative-ai: part %d: %w", i, err)
		}
	}
	if cand.FinishReason != "" {
		st.finishReason = cand.FinishReason
	}
	return nil
}

func (st *state) applyPart(p part) error {
	for _, u := range []struct {
		name string
		raw  json.RawMessage
	}{{"fileData", p.FileData}, {"executableCode", p.ExecutableCode}, {"codeExecutionResult", p.CodeExecutionResult}, {"toolCall", p.ToolCall}, {"toolResponse", p.ToolResponse}} {
		if len(u.raw) > 0 {
			return fmt.Errorf("unsupported part kind %s", u.name)
		}
	}
	switch {
	case p.FunctionCall != nil:
		st.hasCalls = true
		args, err := argsJSON(p.FunctionCall.Args)
		if err != nil {
			return err
		}
		if err := st.closeOpen(); err != nil {
			return err
		}
		key := st.next("call")
		st.b.OpenToolCall(key, p.FunctionCall.ID, p.FunctionCall.Name)
		if err := st.b.Delta(key, string(args)); err != nil {
			return err
		}
		if p.ThoughtSignature != "" {
			if err := st.b.Signature(key, p.ThoughtSignature); err != nil {
				return err
			}
		}
		return st.b.Close(key)
	case p.InlineData != nil:
		return fmt.Errorf("unsupported part kind inlineData")
	case p.Thought:
		if err := st.ensure("thinking"); err != nil {
			return err
		}
		if err := st.delta(text(p)); err != nil {
			return err
		}
		if p.ThoughtSignature != "" {
			return st.b.Signature(st.open, p.ThoughtSignature)
		}
		return nil
	}
	if err := st.ensure("text"); err != nil {
		return err
	}
	if err := st.delta(text(p)); err != nil {
		return err
	}
	if p.ThoughtSignature == "" {
		return nil
	}
	if err := st.closeOpen(); err != nil {
		return err
	}
	key := st.next("signature")
	st.b.OpenThinking(key, false)
	if err := st.b.Signature(key, p.ThoughtSignature); err != nil {
		return err
	}
	return st.b.Close(key)
}

func text(p part) string {
	if p.Text == nil {
		return ""
	}
	return *p.Text
}

// Opens a block of kind unless one is already receiving fragments
func (st *state) ensure(kind string) error {
	if st.open != "" && st.kind == kind {
		return nil
	}
	if err := st.closeOpen(); err != nil {
		return err
	}
	key := st.next(kind)
	if kind == "thinking" {
		st.b.OpenThinking(key, false)
	} else {
		st.b.OpenText(key)
	}
	st.open, st.kind = key, kind
	return nil
}

func (st *state) delta(s string) error {
	if s == "" {
		return nil
	}
	return st.b.Delta(st.open, s)
}

func (st *state) closeOpen() error {
	if st.open == "" {
		return nil
	}
	key := st.open
	st.open, st.kind = "", ""
	return st.b.Close(key)
}

func (st *state) next(kind string) string {
	st.n++
	return kind + "-" + strconv.Itoa(st.n)
}

func (st *state) finish() error {
	if err := st.closeOpen(); err != nil {
		return err
	}
	return st.b.Finish(stopReason(st.finishReason, st.hasCalls), st.finishReason, st.usage)
}

// Maps a FinishReason value onto the neutral set, with tool use detected from parts
func stopReason(reason string, hasCalls bool) protocol.StopReason {
	switch reason {
	case "STOP":
		if hasCalls {
			return protocol.StopToolUse
		}
		return protocol.StopEnd
	case "MAX_TOKENS", "CONTINUATION":
		return protocol.StopLength
	case "SAFETY", "RECITATION", "LANGUAGE", "BLOCKLIST", "PROHIBITED_CONTENT", "SPII", "IMAGE_SAFETY", "IMAGE_PROHIBITED_CONTENT", "IMAGE_RECITATION", "ESCALATION", "PUP_LIMITED_DISABLED":
		return protocol.StopContentFilter
	}
	return protocol.StopOther
}

// Blank line delimited block of lines, as split by the official Go SDK scanner
type record struct {
	lines []string
}

// Joins the data lines of a record, reporting false when it carries none
func (r record) data() (string, bool) {
	if !strings.HasPrefix(r.lines[0], "data:") {
		return "", false
	}
	var out []string
	for _, l := range r.lines {
		out = append(out, strings.TrimPrefix(strings.TrimPrefix(l, "data:"), " "))
	}
	return strings.Join(out, "\n"), true
}

func records(r io.Reader) iter.Seq2[record, error] {
	return func(yield func(record, error) bool) {
		br := bufio.NewReader(r)
		var cur record
		for {
			line, err := br.ReadString('\n')
			if err != nil && err != io.EOF {
				yield(record{}, err)
				return
			}
			line = strings.TrimSuffix(strings.TrimSuffix(line, "\n"), "\r")
			if line != "" {
				cur.lines = append(cur.lines, line)
			} else if len(cur.lines) > 0 {
				if !yield(cur, nil) {
					return
				}
				cur = record{}
			}
			if err == io.EOF {
				if len(cur.lines) > 0 {
					yield(cur, nil)
				}
				return
			}
		}
	}
}
