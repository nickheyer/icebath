package app

import (
	"fmt"
	"strings"

	agentv1 "github.com/nickheyer/icebath/pkg/gen/icebath/plugins/agent/v1"
	sessionsv1 "github.com/nickheyer/icebath/pkg/gen/icebath/sessions/v1"
)

const preview = 12

type entry struct {
	message *sessionsv1.Message
	notice  string
}

type transcript struct {
	entries   []entry
	text      strings.Builder
	reasoning strings.Builder
	approval  *agentv1.Approval
	state     *agentv1.State
	busy      bool
	expanded  bool
	quiet     bool
}

func (t *transcript) apply(response *agentv1.WatchResponse) {
	switch event := response.GetEvent().(type) {
	case *agentv1.WatchResponse_Message:
		t.settle()
		t.entries = append(t.entries, entry{message: event.Message})
	case *agentv1.WatchResponse_Delta:
		t.text.WriteString(event.Delta.GetText())
		t.reasoning.WriteString(event.Delta.GetReasoning())
	case *agentv1.WatchResponse_Approval:
		t.approval = event.Approval
	case *agentv1.WatchResponse_State:
		t.state = event.State
	case *agentv1.WatchResponse_Notice:
		t.entries = append(t.entries, entry{notice: event.Notice})
	case *agentv1.WatchResponse_Busy:
		if t.busy = event.Busy; !t.busy {
			t.settle()
		}
	case *agentv1.WatchResponse_Rewind:
		kept := uint32(0)
		for i, e := range t.entries {
			if e.message == nil {
				continue
			}
			if kept == event.Rewind.GetMessages() {
				t.entries = t.entries[:i]
				break
			}
			kept++
		}
	}
}

func (t *transcript) blocks() []string {
	var blocks []string
	for _, e := range t.entries {
		if e.message == nil {
			blocks = append(blocks, e.notice)
			continue
		}
		blocks = append(blocks, t.message(e.message)...)
	}
	if t.reasoning.Len() > 0 {
		blocks = append(blocks, t.thinking(t.reasoning.String()))
	}
	if t.text.Len() > 0 {
		blocks = append(blocks, t.text.String())
	}
	switch {
	case t.approval != nil:
		blocks = append(blocks, fmt.Sprintf("**Approve?** %s\n\n%s\n\nPress enter on an empty prompt to answer, or type `/approve`, `/approve session`, `/approve always` or `/deny`.", t.approval.GetReason(), fenced(t.approval.GetCommand())))
	case t.busy && t.text.Len() == 0:
		blocks = append(blocks, "_working…_")
	}
	return blocks
}

func (t *transcript) replies() []string {
	var replies []string
	for _, e := range t.entries {
		if e.message.GetRole() == sessionsv1.Role_ROLE_ASSISTANT && e.message.GetText() != "" {
			replies = append(replies, e.message.GetText())
		}
	}
	return replies
}

func (t *transcript) settle() {
	t.text.Reset()
	t.reasoning.Reset()
	t.approval = nil
}

func (t *transcript) message(message *sessionsv1.Message) []string {
	switch message.GetRole() {
	case sessionsv1.Role_ROLE_USER:
		return []string{"> " + strings.ReplaceAll(message.GetText(), "\n", "\n> ")}
	case sessionsv1.Role_ROLE_TOOL:
		lines := strings.Split(message.GetText(), "\n")
		if !t.expanded && len(lines) > preview {
			lines = append(lines[:preview:preview], fmt.Sprintf("… %d more lines", len(lines)-preview))
		}
		return []string{fenced(strings.Join(lines, "\n"))}
	}
	var blocks []string
	if message.GetReasoning() != "" {
		blocks = append(blocks, t.thinking(message.GetReasoning()))
	}
	if message.GetText() != "" {
		blocks = append(blocks, message.GetText())
	}
	for _, call := range message.GetToolCalls() {
		blocks = append(blocks, "**"+call.GetName()+"**\n\n"+fenced(call.GetArguments()))
	}
	return blocks
}

func (t *transcript) thinking(text string) string {
	if t.quiet {
		return "_thinking_"
	}
	return "_thinking_\n\n" + text
}

func fenced(text string) string {
	fence := "```"
	for strings.Contains(text, fence) {
		fence += "`"
	}
	return fence + "\n" + text + "\n" + fence
}
