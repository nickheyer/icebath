// Context management hooks that summarize old turns and drop stale thinking before a request
package compact

import (
	"context"
	"errors"
	"fmt"

	"github.com/nickheyer/icebath/pkg/agent"
	"github.com/nickheyer/icebath/pkg/protocol"
	"github.com/nickheyer/icebath/pkg/session"
)

const system = `You write handoff summaries of agent sessions so the same agent can continue the work without the original transcript.
Write the summary in these sections, each as a short heading followed by concise bullet points:
Goal, Progress, Key facts, Decisions, Open items.
Keep every file path, host name, command, identifier and value that later work will need. State plainly what was verified and what was only attempted.`

// Hook that replaces the oldest turns with a model written summary once the context grows past a threshold
type Summarizer struct {
	agent.Hooks
	Model   agent.Model
	ModelID string
	Session *session.Session
	// Context tokens, as measured by the last response, at which a summary is made
	Threshold int
	// Messages kept verbatim after the summary, six when zero
	Keep int
	// Output budget of the summary, 2048 when zero
	MaxTokens int
	// Extra guidance appended to the summary instructions
	Instructions string
}

func (s *Summarizer) keep() int {
	if s.Keep > 0 {
		return s.Keep
	}
	return 6
}

func (s *Summarizer) maxTokens() int {
	if s.MaxTokens > 0 {
		return s.MaxTokens
	}
	return 2048
}

// Summarizes and compacts the session when the context has grown past the threshold
func (s *Summarizer) BeforeTurn(ctx context.Context, req *protocol.Request) error {
	if s.Model == nil || s.ModelID == "" || s.Session == nil || s.Threshold <= 0 {
		return errors.New("compact: model, model id, session and threshold are required")
	}
	if s.Session.Context() < s.Threshold || s.Session.Len() <= s.keep() {
		return nil
	}
	before := s.Session.Len()
	summary, err := s.Summarize(ctx)
	if err != nil {
		return err
	}
	if err := s.Session.Compact(summary, s.keep()); err != nil {
		return err
	}
	req.Messages = s.Session.Snapshot()
	agent.Observe(ctx, agent.Event{Type: agent.EventNote, Text: fmt.Sprintf("compacted %d messages into a summary, %d remain", before, s.Session.Len())})
	return nil
}

// Asks the model for a summary of the whole transcript
func (s *Summarizer) Summarize(ctx context.Context) (string, error) {
	if s.Model == nil || s.ModelID == "" || s.Session == nil {
		return "", errors.New("compact: model, model id and session are required")
	}
	prompt := system
	if s.Instructions != "" {
		prompt += "\n\n" + s.Instructions
	}
	req := protocol.Request{
		Model:     s.ModelID,
		System:    prompt,
		MaxTokens: s.maxTokens(),
		Stream:    true,
		Messages:  []protocol.Message{{Role: protocol.RoleUser, Content: []protocol.Part{protocol.Text{Text: "<transcript>\n" + s.Session.Markdown() + "\n</transcript>\n\nWrite the handoff summary of this transcript."}}}},
	}
	resp, err := protocol.Collect(s.Model.Stream(ctx, req))
	if err != nil {
		return "", fmt.Errorf("compact: %w", err)
	}
	text := agent.Text(resp)
	if text == "" {
		return "", errors.New("compact: the model returned no summary")
	}
	return "Summary of the earlier conversation:\n\n" + text, nil
}

// Hook that drops thinking from every assistant turn but the last so stale reasoning is not resent
type StripThinking struct {
	agent.Hooks
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
	_ agent.Hook = (*Summarizer)(nil)
	_ agent.Hook = StripThinking{}
)
