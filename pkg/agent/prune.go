package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"

	"github.com/nickheyer/icebath/pkg/protocol"
	"github.com/nickheyer/icebath/pkg/session"
)

// Estimated tokens of eligible results that pile up before a batch is stubbed while the window is unknown
const pruneBatch = 8000

// Share of the window a request may fill before the oldest eligible results are stubbed
const pruneShare = 0.5

// Share of the window a prune frees below the pruning share, so the request prefix changes rarely
const pruneRelief = 0.15

// Longest argument excerpt a stub carries
const stubArgs = 80

// Hook that swaps old tool results for one line stubs once the request presses on the context window
type Pruner struct {
	Hooks
	Session *session.Session
	// Assistant turns a result stays whole before it may be stubbed
	After int
	// Context tokens one request may carry, zero while unknown
	Window int
}

// Tool result eligible for a stub with the tokens it holds, oldest first
type candidate struct {
	result protocol.ToolResult
	tokens int
}

func (p *Pruner) BeforeTurn(ctx context.Context, req *protocol.Request) error {
	if p.After <= 0 || p.Session == nil {
		return nil
	}
	calls := map[string]protocol.ToolCall{}
	for _, m := range req.Messages {
		for _, part := range m.Content {
			if c, ok := part.(protocol.ToolCall); ok {
				calls[c.ID] = c
			}
		}
	}
	var pool []candidate
	pending, turns := 0, 0
	for _, m := range slices.Backward(req.Messages) {
		if m.Role == protocol.RoleAssistant {
			turns++
			continue
		}
		if m.Role != protocol.RoleTool || turns < p.After {
			continue
		}
		for _, part := range m.Content {
			r, ok := part.(protocol.ToolResult)
			if !ok {
				continue
			}
			if _, done := p.Session.Pruned(r.CallID); done {
				continue
			}
			n := protocol.EstimateTokens(r.Content)
			pool = append(pool, candidate{result: r, tokens: n})
			pending += n
		}
	}
	slices.Reverse(pool)
	for _, c := range p.chosen(pool, pending, req.Messages) {
		p.Session.Prune(c.result.CallID, pruneStub(calls[c.result.CallID], c.result))
	}
	for i := range req.Messages {
		m := &req.Messages[i]
		if m.Role != protocol.RoleTool {
			continue
		}
		kept := make([]protocol.Part, 0, len(m.Content))
		for _, part := range m.Content {
			if r, ok := part.(protocol.ToolResult); ok {
				if stub, done := p.Session.Pruned(r.CallID); done {
					part = protocol.ToolResult{CallID: r.CallID, Name: r.Name, Content: []protocol.Part{protocol.Text{Text: stub}}, IsError: r.IsError}
				}
			}
			kept = append(kept, part)
		}
		m.Content = kept
	}
	return nil
}

// Candidates to stub, every one in a batch while the window is unknown, else the oldest until the request fits under the window share
func (p *Pruner) chosen(pool []candidate, pending int, messages []protocol.Message) []candidate {
	if p.Window <= 0 {
		if pending >= pruneBatch {
			return pool
		}
		return nil
	}
	have := p.Session.Context() + pendingTokens(messages)
	limit := int(float64(p.Window) * pruneShare)
	if have < limit {
		return nil
	}
	goal := int(float64(p.Window) * (pruneShare - pruneRelief))
	var out []candidate
	for _, c := range pool {
		if have <= goal {
			break
		}
		out = append(out, c)
		have -= c.tokens
	}
	return out
}

// One line stub naming the call and the size of the body it stands in for
func pruneStub(call protocol.ToolCall, r protocol.ToolResult) string {
	name := call.Name
	if name == "" {
		name = r.Name
	}
	text, attachments := resultText(r.Content)
	lines := 0
	if text != "" {
		lines = strings.Count(text, "\n") + 1
	}
	var sb strings.Builder
	sb.WriteString("[")
	sb.WriteString(name)
	if args := excerptArgs(call.Args); args != "" {
		sb.WriteString(" ")
		sb.WriteString(args)
	}
	fmt.Fprintf(&sb, " returned %d lines, about %s tokens, no longer in context", lines, roughTokens(protocol.EstimateTokens(r.Content)))
	if attachments > 0 {
		fmt.Fprintf(&sb, ", %d attachments", attachments)
	}
	sb.WriteString("]")
	return sb.String()
}

// Text of a result joined across its text parts, with the count of parts that are not text
func resultText(parts []protocol.Part) (string, int) {
	var sb strings.Builder
	attachments := 0
	for _, p := range parts {
		if t, ok := p.(protocol.Text); ok {
			sb.WriteString(t.Text)
		} else {
			attachments++
		}
	}
	return sb.String(), attachments
}

// Compact arguments cut to the stub excerpt length
func excerptArgs(args json.RawMessage) string {
	if len(args) == 0 {
		return ""
	}
	var buf bytes.Buffer
	if err := json.Compact(&buf, args); err != nil {
		return ""
	}
	s := buf.String()
	if s == "{}" || s == "null" {
		return ""
	}
	if runes := []rune(s); len(runes) > stubArgs {
		s = string(runes[:stubArgs]) + "..."
	}
	return s
}

// Token count as a plain number under a thousand and rounded to thousands above
func roughTokens(n int) string {
	if n < 1000 {
		return fmt.Sprint(n)
	}
	return fmt.Sprintf("%dk", (n+500)/1000)
}

var _ Hook = (*Pruner)(nil)
