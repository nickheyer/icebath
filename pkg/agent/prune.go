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

// Estimated tokens of eligible results that pile up before a batch is stubbed, so the cached prefix changes rarely
const pruneBatch = 8000

// Longest argument excerpt a stub carries
const stubArgs = 80

// Hook that swaps tool results older than After assistant turns for one line stubs in the request
type Pruner struct {
	Hooks
	Session *session.Session
	After   int
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
	var pool []protocol.ToolResult
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
			pool = append(pool, r)
			pending += protocol.EstimateTokens(r.Content)
		}
	}
	if pending >= pruneBatch {
		for _, r := range pool {
			p.Session.Prune(r.CallID, pruneStub(calls[r.CallID], r))
		}
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
	sb.WriteString("[" + name)
	if args := excerptArgs(call.Args); args != "" {
		sb.WriteString(" " + args)
	}
	fmt.Fprintf(&sb, " result dropped from context, %d lines, about %s tokens", lines, roughTokens(protocol.EstimateTokens(r.Content)))
	if attachments > 0 {
		fmt.Fprintf(&sb, ", %d attachments", attachments)
	}
	sb.WriteString(", run the call again if it is needed]")
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
