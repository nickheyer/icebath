// Context management hooks that summarize old turns and drop stale thinking before a request
package agent

import (
	"context"
	"errors"
	"fmt"
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
	// Messages kept verbatim after the summary, six when zero
	Keep int
	// Extra guidance appended to the summary instructions
	Instructions string
	// Runs before a summary is made, such as external PreCompact hooks
	Before func(ctx context.Context) error
}

func (s *Summarizer) keep() int {
	if s.Keep > 0 {
		return s.Keep
	}
	return 6
}

// Tokens the pending request carries beyond the last measured response, estimated from the text added since
func pendingTokens(messages []protocol.Message) int {
	last := -1
	for i, m := range messages {
		if m.Role == protocol.RoleAssistant {
			last = i
		}
	}
	chars := 0
	for _, m := range messages[last+1:] {
		for _, p := range m.Content {
			switch v := p.(type) {
			case protocol.Text:
				chars += len(v.Text)
			case protocol.ToolResult:
				chars += len(protocol.Response{Content: v.Content}.Text())
			case protocol.ToolCall:
				chars += len(v.Args)
			case protocol.Image, protocol.Document:
				chars += 4000
			}
		}
	}
	return chars / 4
}

// Summarizes and compacts the session when the pending request would grow past the threshold
func (s *Summarizer) BeforeTurn(ctx context.Context, req *protocol.Request) error {
	if s.Model == nil || s.ModelID == "" || s.Session == nil || s.Threshold <= 0 {
		return errors.New("compact: model, model id, session and threshold are required")
	}
	if s.Session.Context()+pendingTokens(req.Messages) < s.Threshold || s.Session.Len() <= s.keep() {
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
	if err := s.Session.Compact(summary, s.keep()); err != nil {
		return err
	}
	req.Messages = s.Session.Snapshot()
	after := s.Session.Len()
	Observe(ctx, Event{Type: EventCompactEnd, Text: summary, Data: map[string]int{"before": before, "after": after}})
	Observe(ctx, Event{Type: EventNote, Text: fmt.Sprintf("compacted %d messages into a summary, %d remain", before, after)})
	s.Session.Log("", "compact", map[string]any{"before": before, "after": after, "summary": summary})
	return nil
}

// Asks the model for a summary of the whole transcript, in parts when it is too long for one call
func (s *Summarizer) Summarize(ctx context.Context) (string, error) {
	if s.Model == nil || s.ModelID == "" || s.Session == nil {
		return "", errors.New("compact: model, model id and session are required")
	}
	transcript := s.Session.Markdown()
	if len(transcript) <= chunkChars {
		text, err := s.ask(ctx, summarySystem, "<transcript>\n"+transcript+"\n</transcript>\n\nWrite the handoff summary of this transcript.")
		if err != nil {
			return "", err
		}
		return "Summary of the earlier conversation:\n\n" + text, nil
	}
	chunks := s.chunks()
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

// Splits the transcript into markdown chunks of at most the chunk budget, never splitting a message
func (s *Summarizer) chunks() []string {
	var chunks []string
	var cur strings.Builder
	for _, e := range s.Session.Entries() {
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

// Hook that drops thinking from every assistant turn but the last so stale reasoning is not resent
type StripThinking struct {
	Hooks
}

// Text left where an assistant turn held nothing but thinking
const Omitted = "(reasoning omitted)"

func (StripThinking) BeforeTurn(ctx context.Context, req *protocol.Request) error {
	last := -1
	for i, m := range req.Messages {
		if m.Role == protocol.RoleAssistant {
			last = i
		}
	}
	for i := range req.Messages {
		m := &req.Messages[i]
		if m.Role != protocol.RoleAssistant || i == last {
			continue
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
	return nil
}

var (
	_ Hook = (*Summarizer)(nil)
	_ Hook = StripThinking{}
)
