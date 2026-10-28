// Context management hooks that summarize old turns and drop stale thinking before a request
package agent

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"hash"
	"io"
	"strings"

	"github.com/nickheyer/icebath/pkg/protocol"
	"github.com/nickheyer/icebath/pkg/session"
)

const summarySystem = `You write handoff summaries of agent sessions so the same agent can continue the work without the original transcript.
Write the summary in these sections, each as a short heading followed by concise bullet points:
Goal, Progress, Key facts, Decisions, Open items.
Keep every file path, host name, command, identifier and value that later work will need. State plainly what was verified and what was only attempted.`

const summaryCombine = `You merge partial handoff summaries of one agent session into a single summary with the same sections: Goal, Progress, Key facts, Decisions, Open items. Keep every path, host, command, identifier and value. Drop nothing that later work will need.`

const (
	// Characters of transcript summarized in one model call
	chunkChars = 200000
	// Output budget of one summary
	summaryMaxTokens = 2048
)

// Hook that replaces the oldest turns with a model written summary once the context grows past a threshold
type Summarizer struct {
	Hooks
	Model   Model
	ModelID string
	Session *session.Session
	// Context tokens, as measured by the last response plus what was added since, at which a summary is made
	Threshold int
	// Messages kept verbatim after the summary, none when zero
	Keep int
	// Extra guidance appended to the summary instructions
	Instructions string
	// Runs before a summary is made, such as external PreCompact hooks
	Before func(ctx context.Context) error
}

// Tokens the pending request carries beyond the last measured response, estimated from the text added since
func pendingTokens(messages []protocol.Message) int {
	last := -1
	for i, m := range messages {
		if m.Role == protocol.RoleAssistant {
			last = i
		}
	}
	tokens := 0
	for _, m := range messages[last+1:] {
		tokens += protocol.EstimateTokens(m.Content)
	}
	return tokens
}

// Summarizes and compacts the session when the pending request would grow past the threshold
func (s *Summarizer) BeforeTurn(ctx context.Context, req *protocol.Request) error {
	if s.Model == nil || s.ModelID == "" || s.Session == nil || s.Threshold <= 0 {
		return errors.New("compact: model, model id, session and threshold are required")
	}
	if s.Session.Context()+pendingTokens(req.Messages) < s.Threshold || s.Session.Len() <= s.Keep {
		return nil
	}
	if s.Before != nil {
		if err := s.Before(ctx); err != nil {
			return err
		}
	}
	before := s.Session.Len()
	Observe(ctx, Event{Type: EventCompactStart, Text: fmt.Sprintf("compacting %d messages", before)})
	summary, err := s.Summarize(ctx)
	if err != nil {
		return err
	}
	if err := s.Session.Compact(summary, s.Keep); err != nil {
		return err
	}
	req.Messages = s.Session.Snapshot()
	after := s.Session.Len()
	Observe(ctx, Event{Type: EventCompactEnd, Text: summary, Data: map[string]int{"before": before, "after": after}})
	Observe(ctx, Event{Type: EventNote, Text: fmt.Sprintf("compacted %d messages into a summary, %d remain", before, after)})
	s.Session.Log("", "compact", map[string]any{"before": before, "after": after, "summary": summary})
	return nil
}

// Asks the model for a summary of the turns compaction drops, in parts when they are too long for one call
func (s *Summarizer) Summarize(ctx context.Context) (string, error) {
	if s.Model == nil || s.ModelID == "" || s.Session == nil {
		return "", errors.New("compact: model, model id and session are required")
	}
	head := s.Session.Head(s.Keep)
	if len(head) == 0 {
		return "", errors.New("compact: nothing to summarize")
	}
	transcript := session.FromHeader(session.Header{ID: s.Session.ID()}, head, nil).Markdown()
	if len(transcript) <= chunkChars {
		text, err := s.ask(ctx, summarySystem, "<transcript>\n"+transcript+"\n</transcript>\n\nWrite the handoff summary of this transcript.")
		if err != nil {
			return "", err
		}
		return "Summary of the earlier conversation:\n\n" + text, nil
	}
	chunks := s.chunks(head)
	partials := make([]string, 0, len(chunks))
	for i, chunk := range chunks {
		text, err := s.ask(ctx, summarySystem, fmt.Sprintf("<transcript part=\"%d of %d\">\n%s\n</transcript>\n\nWrite the handoff summary of this part of the transcript.", i+1, len(chunks), chunk))
		if err != nil {
			return "", err
		}
		partials = append(partials, fmt.Sprintf("Part %d of %d:\n%s", i+1, len(chunks), text))
	}
	text, err := s.ask(ctx, summaryCombine, strings.Join(partials, "\n\n"))
	if err != nil {
		return "", err
	}
	return "Summary of the earlier conversation:\n\n" + text, nil
}

// Splits the entries into markdown chunks of at most the chunk budget, never splitting a message
func (s *Summarizer) chunks(entries []session.Entry) []string {
	var chunks []string
	var cur strings.Builder
	for _, e := range entries {
		one := session.FromHeader(session.Header{ID: s.Session.ID()}, []session.Entry{e}, nil).MarkdownWith(session.MarkdownOptions{Timestamps: true})
		body := one[strings.Index(one, "\n## "):]
		if cur.Len() > 0 && cur.Len()+len(body) > chunkChars {
			chunks = append(chunks, cur.String())
			cur.Reset()
		}
		cur.WriteString(body)
	}
	if cur.Len() > 0 {
		chunks = append(chunks, cur.String())
	}
	return chunks
}

func (s *Summarizer) ask(ctx context.Context, prompt, text string) (string, error) {
	if s.Instructions != "" {
		prompt += "\n\n" + s.Instructions
	}
	req := protocol.Request{
		Model:     s.ModelID,
		System:    prompt,
		MaxTokens: summaryMaxTokens,
		Stream:    true,
		Messages:  []protocol.Message{{Role: protocol.RoleUser, Content: []protocol.Part{protocol.Text{Text: text}}}},
	}
	resp, err := protocol.Collect(s.Model.Stream(ctx, req))
	if err != nil {
		return "", fmt.Errorf("compact: %w", err)
	}
	out := resp.Text()
	if out == "" {
		return "", errors.New("compact: the model returned no summary")
	}
	return out, nil
}

// Hook that keeps thinking only on the newest turn of the run and only while the request before it is unchanged
type StripThinking struct {
	Hooks
	// Digest of the request the newest turn of the run answered, empty before the run has a turn
	bound string
	// Digest of the request last sent
	sent string
}

// Text left where an assistant turn held nothing but thinking
const Omitted = "(reasoning omitted)"

func (s *StripThinking) BeforeRun(ctx context.Context, input []protocol.Part) ([]protocol.Part, error) {
	s.bound, s.sent = "", ""
	return input, nil
}

func (s *StripThinking) AfterTurn(ctx context.Context, resp protocol.Response) error {
	s.bound = s.sent
	return nil
}

func (s *StripThinking) BeforeTurn(ctx context.Context, req *protocol.Request) error {
	last := -1
	if s.bound != "" {
		for i, m := range req.Messages {
			if m.Role == protocol.RoleAssistant {
				last = i
			}
		}
	}
	for i := range req.Messages {
		if i != last {
			dropThinking(&req.Messages[i])
		}
	}
	if last >= 0 && binding(req, req.Messages[:last]) != s.bound {
		dropThinking(&req.Messages[last])
	}
	s.sent = binding(req, req.Messages)
	return nil
}

// Takes the thinking out of an assistant turn, leaving the placeholder when nothing else was in it
func dropThinking(m *protocol.Message) {
	if m.Role != protocol.RoleAssistant {
		return
	}
	kept := make([]protocol.Part, 0, len(m.Content))
	for _, p := range m.Content {
		if _, thinking := p.(protocol.Thinking); !thinking {
			kept = append(kept, p)
		}
	}
	if len(kept) == 0 {
		kept = append(kept, protocol.Text{Text: Omitted})
	}
	m.Content = kept
}

// Digest of the model, system prompt, tools and messages with thinking left out, what a signature is bound to
func binding(req *protocol.Request, msgs []protocol.Message) string {
	d := digest{sha256.New()}
	d.str(req.Model)
	d.str(req.System)
	d.flag(req.KeepSystem)
	d.num(len(req.Tools))
	for _, t := range req.Tools {
		d.str(t.Name)
		d.str(t.Description)
		d.bytes(t.Parameters)
	}
	d.num(len(msgs))
	for _, m := range msgs {
		d.str(string(m.Role))
		d.parts(m.Content)
	}
	return string(d.Sum(nil))
}

// Hash writer that length prefixes every field so two different requests never write the same bytes
type digest struct{ hash.Hash }

func (d digest) num(n int) {
	d.Write(binary.AppendVarint(nil, int64(n)))
}

func (d digest) str(s string) {
	d.num(len(s))
	io.WriteString(d, s)
}

func (d digest) bytes(b []byte) {
	d.num(len(b))
	d.Write(b)
}

func (d digest) flag(b bool) {
	if b {
		d.num(1)
	} else {
		d.num(0)
	}
}

// Writes every part but thinking under its type, nested tool result parts included
func (d digest) parts(ps []protocol.Part) {
	for _, p := range ps {
		switch v := p.(type) {
		case protocol.Thinking:
		case protocol.Text:
			d.str("text")
			d.str(v.Text)
			d.num(len(v.Citations))
			for _, c := range v.Citations {
				d.str(c.Title)
				d.str(c.URL)
				d.str(c.Document)
				d.num(c.Start)
				d.num(c.End)
				d.str(c.Cited)
			}
		case protocol.Image:
			d.str("image")
			d.str(v.MediaType)
			d.bytes(v.Data)
		case protocol.Document:
			d.str("document")
			d.str(v.MediaType)
			d.bytes(v.Data)
			d.str(v.Name)
		case protocol.ToolCall:
			d.str("tool_call")
			d.str(v.ID)
			d.str(v.Name)
			d.bytes(v.Args)
			d.str(v.Signature)
			d.str(string(v.Origin))
		case protocol.ToolResult:
			d.str("tool_result")
			d.str(v.CallID)
			d.str(v.Name)
			d.flag(v.IsError)
			d.parts(v.Content)
		case protocol.ServerToolCall:
			d.str("server_tool_call")
			d.str(v.ID)
			d.str(v.Name)
			d.bytes(v.Args)
			d.str(string(v.Origin))
		case protocol.ServerToolResult:
			d.str("server_tool_result")
			d.str(v.CallID)
			d.str(v.Name)
			d.bytes(v.Content)
			d.str(string(v.Origin))
		default:
			d.str(fmt.Sprintf("%T%#v", v, v))
		}
	}
	d.str("end")
}

var (
	_ Hook = (*Summarizer)(nil)
	_ Hook = (*StripThinking)(nil)
)
