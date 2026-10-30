package vsc

import (
	"github.com/nickheyer/icebath/pkg/agent"
	"github.com/nickheyer/icebath/pkg/harness"
	"github.com/nickheyer/icebath/pkg/protocol"
	"github.com/nickheyer/icebath/pkg/tool/files"
)

// Feeds one harness message into the mirror, sending the client what changed
func (a *Agent) Observe(msg any) {
	switch m := msg.(type) {
	case harness.EventMsg:
		a.observeEvent(m)
	case harness.RunDoneMsg:
		if l := a.app.LiveOfRun(m.Run); l != nil {
			a.sendState(l)
		}
	case harness.ApprovalMsg:
		a.observeApproval(m)
	case harness.ApprovalDoneMsg:
		if l := a.app.LiveOfRun(m.Run); l != nil {
			a.sendState(l)
		}
	case harness.OpenedMsg:
		if m.Live != nil {
			a.sendState(m.Live)
		}
	}
	a.syncMode()
}

// Sends the run state when a run starts and the outcome of a file change when its tool call ends
func (a *Agent) observeEvent(m harness.EventMsg) {
	live := a.app.LiveOfRun(m.Run)
	if live == nil {
		return
	}
	switch m.Event.Type {
	case agent.EventRunStart:
		a.sendState(live)
	case agent.EventToolEnd:
		a.observeToolEnd(live.Session.ID(), m.Event)
	}
}

// Proposes the file change of an approval the TUI asks for, so its diff opens while the person decides there
func (a *Agent) observeApproval(m harness.ApprovalMsg) {
	p := m.Pending
	if p == nil {
		return
	}
	live := a.app.LiveOfRun(p.Run)
	if live == nil {
		return
	}
	a.sendState(live)
	a.propose(live.Session.ID(), p.Request.Call)
}

// Sends the proposed change of a file tool call and remembers the call until it ends
func (a *Agent) propose(sessionID string, call protocol.ToolCall) {
	fc, ok := FileChangeOf(call, a.app.Dir)
	if !ok {
		return
	}
	a.mu.Lock()
	a.proposed[call.ID] = true
	a.mu.Unlock()
	up := SessionUpdate{SessionUpdate: UpdateToolCall, ToolCallID: call.ID, Status: StatusPending, Content: []ToolCallContent{diffContent(fc)}}
	a.enqueue(MethodUpdate, SessionNotification{SessionID: sessionID, Update: up})
}

// Sends the change a file tool made, or fails the proposal of one that was refused or broke
func (a *Agent) observeToolEnd(sessionID string, ev agent.Event) {
	a.mu.Lock()
	proposed := a.proposed[ev.Call.ID]
	delete(a.proposed, ev.Call.ID)
	a.mu.Unlock()
	up := SessionUpdate{SessionUpdate: UpdateToolCallMod, ToolCallID: ev.Call.ID}
	switch change, ok := ev.Data.(files.Change); {
	case ok:
		up.Status = StatusCompleted
		up.Content = []ToolCallContent{changeContent(change)}
	case proposed:
		up.Status = StatusFailed
	default:
		return
	}
	a.enqueue(MethodUpdate, SessionNotification{SessionID: sessionID, Update: up})
}

func diffContent(fc FileChange) ToolCallContent {
	return ToolCallContent{Type: "diff", Path: fc.Path, OldText: fc.OldText, NewText: fc.NewText}
}

// Diff content from the record a files tool returns after it wrote
func changeContent(change files.Change) ToolCallContent {
	fc := ToolCallContent{Type: "diff", Path: change.Path, NewText: change.New}
	if !change.Created {
		old := change.Old
		fc.OldText = &old
	}
	return fc
}
