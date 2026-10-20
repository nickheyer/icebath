package googlegenerativeai

import (
	"bufio"
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"iter"
	"strconv"
	"strings"

	"github.com/nickheyer/icebath/pkg/internal/sse"
	"github.com/nickheyer/icebath/pkg/protocol"
)

var transientStatuses = map[string]bool{
	"RESOURCE_EXHAUSTED": true,
	"UNAVAILABLE":        true,
	"DEADLINE_EXCEEDED":  true,
	"INTERNAL":           true,
}

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
		return protocol.Response{}, wireError(resp.Error, raw, false)
	}
	st := newState()
	st.b.Start(resp.ResponseID, resp.ModelVersion)
	if err := st.apply(resp); err != nil {
		return protocol.Response{}, err
	}
	if !st.blocked && len(resp.Candidates) == 0 {
		return protocol.Response{}, fmt.Errorf("google-generative-ai: response has no candidates")
	}
	if err := st.finish(); err != nil {
		return protocol.Response{}, err
	}
	return st.b.Response, nil
}

// Yields neutral events from alt=sse chunks until the chunk carrying finishReason or a block
func (Protocol) Stream(r io.Reader) iter.Seq2[protocol.Event, error] {
	return func(yield func(protocol.Event, error) bool) {
		br := bufio.NewReader(r)
		if startsObject(br) {
			body, err := io.ReadAll(br)
			if err != nil {
				yield(protocol.Event{}, err)
				return
			}
			yield(protocol.Event{}, rawError(body))
			return
		}
		st := newState()
		for ev, err := range sse.Read(br) {
			if err != nil {
				yield(protocol.Event{}, err)
				return
			}
			var chunk response
			if err := json.Unmarshal([]byte(ev.Data), &chunk); err != nil {
				yield(protocol.Event{}, fmt.Errorf("google-generative-ai: stream chunk: %w", err))
				return
			}
			if chunk.Error != nil {
				yield(protocol.Event{}, wireError(chunk.Error, []byte(ev.Data), true))
				return
			}
			st.b.Start(chunk.ResponseID, chunk.ModelVersion)
			if err := st.apply(chunk); err != nil {
				yield(protocol.Event{}, err)
				return
			}
			if st.blocked {
				break
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

// Reports whether the body opens with a JSON object rather than event stream fields
func startsObject(br *bufio.Reader) bool {
	head, _ := br.Peek(64)
	trimmed := bytes.TrimLeft(head, " \t\r\n")
	return len(trimmed) > 0 && trimmed[0] == '{'
}

func (Protocol) DecodeError(status int, body []byte) *protocol.Error {
	e := &protocol.Error{Status: status, Body: body, Transient: transientStatus(status)}
	var env errorEnvelope
	if err := json.Unmarshal(body, &env); err == nil && env.Error != nil {
		e.Type, e.Message = env.Error.Status, env.Error.Message
		e.Transient = e.Transient || transientStatuses[e.Type]
		return e
	}
	e.Message = strings.TrimSpace(string(body))
	return e
}

// Reports statuses that denote a timeout, conflict, throttle or server failure
func transientStatus(status int) bool {
	return status == 408 || status == 409 || status == 429 || status >= 500
}

func wireError(e *apiError, body []byte, inStream bool) *protocol.Error {
	transient := e.Code == 429 || e.Code == 500 || e.Code == 503 || e.Code == 504 || transientStatuses[e.Status]
	return &protocol.Error{Status: e.Code, Type: e.Status, Message: e.Message, Body: body, InStream: inStream, Transient: transient}
}

// Parses a bare JSON body in place of a stream the way the official SDKs do
func rawError(body []byte) error {
	var env errorEnvelope
	if err := json.Unmarshal(body, &env); err != nil || env.Error == nil {
		return fmt.Errorf("google-generative-ai: invalid stream body: %s", bytes.TrimSpace(body))
	}
	return wireError(env.Error, body, true)
}

// Folds chunk parts into one Builder, keeping one block open across chunk boundaries
type state struct {
	b            protocol.Builder
	open         string
	kind         string
	n            int
	finishReason string
	blocked      bool
	hasCalls     bool
	usage        protocol.Usage
	usageSeen    bool
}

func newState() *state {
	return &state{b: protocol.Builder{Origin: protocol.GoogleGenerativeAI}}
}

func (st *state) apply(resp response) error {
	if resp.UsageMetadata != nil {
		if u := mapUsage(*resp.UsageMetadata); !st.usageSeen || u != st.usage {
			st.usage, st.usageSeen = u, true
			st.b.Usage(u)
		}
	}
	if resp.PromptFeedback != nil && resp.PromptFeedback.BlockReason != "" {
		st.blocked = true
		st.finishReason = resp.PromptFeedback.BlockReason
		return nil
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
	if err := st.applyCitations(cand); err != nil {
		return fmt.Errorf("google-generative-ai: %w", err)
	}
	if cand.FinishReason != "" {
		st.finishReason = cand.FinishReason
	}
	return nil
}

// Takes cached tokens out of the prompt count and folds thought tokens into the output count
func mapUsage(u usageMetadata) protocol.Usage {
	return protocol.Usage{
		Input:     u.PromptTokenCount - u.CachedContentTokenCount,
		Output:    u.CandidatesTokenCount + u.ThoughtsTokenCount,
		CacheRead: u.CachedContentTokenCount,
		Reasoning: u.ThoughtsTokenCount,
	}
}

func (st *state) applyPart(p part) error {
	switch {
	case len(p.FileData) > 0:
		return fmt.Errorf("unsupported part kind fileData")
	case len(p.ExecutableCode) > 0:
		return st.serverToolCall("", nameCodeExecution, p.ExecutableCode)
	case len(p.CodeExecutionResult) > 0:
		return st.serverToolResult("", nameCodeExecution, p.CodeExecutionResult)
	case len(p.ToolCall) > 0:
		return st.serverToolCall(rawID(p.ToolCall), nameToolCall, p.ToolCall)
	case len(p.ToolResponse) > 0:
		return st.serverToolResult(rawID(p.ToolResponse), nameToolCall, p.ToolResponse)
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
		return st.image(*p.InlineData)
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

// Delivers a part the model executed itself as one complete server tool call
func (st *state) serverToolCall(id, name string, raw json.RawMessage) error {
	args, err := argsJSON(raw)
	if err != nil {
		return fmt.Errorf("%s: %w", name, err)
	}
	if err := st.closeOpen(); err != nil {
		return err
	}
	key := st.next("server")
	st.b.OpenServerToolCall(key, id, name)
	if err := st.b.Delta(key, string(args)); err != nil {
		return err
	}
	return st.b.Close(key)
}

func (st *state) serverToolResult(id, name string, raw json.RawMessage) error {
	content, err := argsJSON(raw)
	if err != nil {
		return fmt.Errorf("%s result: %w", name, err)
	}
	if err := st.closeOpen(); err != nil {
		return err
	}
	st.b.AddServerToolResult(protocol.ServerToolResult{CallID: id, Name: name, Content: content})
	return nil
}

func rawID(raw json.RawMessage) string {
	var v struct {
		ID string `json:"id"`
	}
	_ = json.Unmarshal(raw, &v)
	return v.ID
}

// Appends a generated image as a complete part after closing the block before it
func (st *state) image(b blob) error {
	data, err := base64.StdEncoding.DecodeString(b.Data)
	if err != nil {
		return fmt.Errorf("inlineData: %w", err)
	}
	if err := st.closeOpen(); err != nil {
		return err
	}
	st.b.Response.Content = append(st.b.Response.Content, protocol.Image{MediaType: b.MimeType, Data: data})
	return nil
}

// Attaches citation sources and grounding supports to the candidate text
func (st *state) applyCitations(cand candidate) error {
	var cites []protocol.Citation
	if cm := cand.CitationMetadata; cm != nil {
		for _, s := range append(cm.CitationSources, cm.Citations...) {
			cites = append(cites, protocol.Citation{Title: s.Title, URL: s.URI, Start: s.StartIndex, End: s.EndIndex})
		}
	}
	if gm := cand.GroundingMetadata; gm != nil {
		for _, sup := range gm.GroundingSupports {
			for _, idx := range sup.GroundingChunkIndices {
				if idx < 0 || idx >= len(gm.GroundingChunks) {
					return fmt.Errorf("grounding chunk index %d out of range", idx)
				}
				src := gm.GroundingChunks[idx].source()
				c := protocol.Citation{Title: src.Title, URL: src.URI}
				if sup.Segment != nil {
					c.Start, c.End, c.Cited = sup.Segment.StartIndex, sup.Segment.EndIndex, sup.Segment.Text
				}
				cites = append(cites, c)
			}
		}
	}
	if len(cites) == 0 {
		return nil
	}
	if st.open != "" && st.kind == "text" {
		for _, c := range cites {
			if err := st.b.Cite(st.open, c); err != nil {
				return err
			}
		}
		return nil
	}
	for i := len(st.b.Response.Content) - 1; i >= 0; i-- {
		if t, ok := st.b.Response.Content[i].(protocol.Text); ok {
			t.Citations = append(t.Citations, cites...)
			st.b.Response.Content[i] = t
			return nil
		}
	}
	if err := st.ensure("text"); err != nil {
		return err
	}
	for _, c := range cites {
		if err := st.b.Cite(st.open, c); err != nil {
			return err
		}
	}
	return st.closeOpen()
}

func (g groundingChunk) source() groundingSource {
	if g.Web != nil {
		return *g.Web
	}
	if g.RetrievedContext != nil {
		return *g.RetrievedContext
	}
	return groundingSource{}
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
	if st.blocked {
		return st.b.Finish(protocol.StopContentFilter, st.finishReason, st.usage)
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
