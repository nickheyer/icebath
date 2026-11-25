package app

import (
	"fmt"
	"strings"

	agentv1 "github.com/nickheyer/icebath/pkg/gen/icebath/plugins/agent/v1"
	sessionsv1 "github.com/nickheyer/icebath/pkg/gen/icebath/sessions/v1"
)

const preview = 12

type entry struct {
	blocks  []string
	message bool
}

type transcript struct {
	entries   []entry
	text      strings.Builder
	reasoning strings.Builder
	approval  *agentv1.Approval
	busy      bool
}

func (t *transcript) apply(response *agentv1.WatchResponse) []string {
	switch event := response.GetEvent().(type) {
	case *agentv1.WatchResponse_Message:
		t.settle()
		t.entries = append(t.entries, entry{blocksOf(event.Message), true})
	case *agentv1.WatchResponse_Delta:
		t.text.WriteString(event.Delta.GetText())
		t.reasoning.WriteString(event.Delta.GetReasoning())
	case *agentv1.WatchResponse_Approval:
		t.approval = event.Approval
	case *agentv1.WatchResponse_Notice:
		t.entries = append(t.entries, entry{blocks: []string{event.Notice}})
	case *agentv1.WatchResponse_Busy:
		if t.busy = event.Busy; !t.busy {
			t.settle()
		}
	case *agentv1.WatchResponse_Rewind:
		kept := uint32(0)
		for i, e := range t.entries {
			if !e.message {
				continue
			}
			if kept == event.Rewind.GetMessages() {
				t.entries = t.entries[:i]
				break
			}
			kept++
		}
	}
	var blocks []string
	for _, e := range t.entries {
		blocks = append(blocks, e.blocks...)
	}
	if t.reasoning.Len() > 0 {
		blocks = append(blocks, thinking(t.reasoning.String()))
	}
	if t.text.Len() > 0 {
		blocks = append(blocks, t.text.String())
	}
	switch {
	case t.approval != nil:
		blocks = append(blocks, fmt.Sprintf("**Approve?** %s\n\n%s\n\nAnswer with `/approve`, `/approve session`, `/approve always` or `/deny`.", t.approval.GetReason(), fenced(t.approval.GetCommand())))
	case t.busy && t.text.Len() == 0:
		blocks = append(blocks, "_working…_")
	}
	return blocks
}

func (t *transcript) settle() {
	t.text.Reset()
	t.reasoning.Reset()
	t.approval = nil
}

func blocksOf(message *sessionsv1.Message) []string {
	switch message.GetRole() {
	case sessionsv1.Role_ROLE_USER:
		return []string{"> " + strings.ReplaceAll(message.GetText(), "\n", "\n> ")}
	case sessionsv1.Role_ROLE_TOOL:
		lines := strings.Split(message.GetText(), "\n")
		if len(lines) > preview {
			lines = append(lines[:preview], fmt.Sprintf("… %d more lines", len(lines)-preview))
		}
		return []string{fenced(strings.Join(lines, "\n"))}
	}
	var blocks []string
	if message.GetReasoning() != "" {
		blocks = append(blocks, thinking(message.GetReasoning()))
	}
	if message.GetText() != "" {
		blocks = append(blocks, message.GetText())
	}
	for _, call := range message.GetToolCalls() {
		blocks = append(blocks, "**"+call.GetName()+"**\n\n"+fenced(call.GetArguments()))
	}
	return blocks
}

func thinking(text string) string {
	return "_thinking_\n\n" + text
}

func fenced(text string) string {
	fence := "```"
	for strings.Contains(text, fence) {
		fence += "`"
	}
	return fence + "\n" + text + "\n" + fence
}
