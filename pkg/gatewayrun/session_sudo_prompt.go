package gatewayrun

import (
	"context"

	"github.com/nickheyer/icebath/pkg/agent"
	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/tools"
)

// Seconds the masked sudo prompt waits for a viewer, the main sidecar deadline
const sudoTimeoutS = 120.0

// Sends one sudo request to the attached viewers and waits for its value, empty when unanswered
func askSudo(events *gateway.SessionEvents, sessionID, command string, cancelled func() bool, timeout float64) string {
	if cancelled == nil {
		cancelled = func() bool { return false }
	}
	return gateway.AskViewer(events, sessionID, "sudo", pyjson.ObjectOf("command", command), cancelled, &timeout, true)
}

// Installs the viewer sudo prompt on this thread for a local viewer turn and returns its restore
func sudoPromptScope(ctx context.Context, t *TurnRunner, a *agent.AIAgent) func() {
	if !localViewerTurn(t) {
		return func() {}
	}
	authority, sessionID := t.approvalOwner.authority, t.approvalOwner.sessionID
	callback := &tools.SudoPasswordCallback{Prompt: func(promptCtx context.Context) string {
		live, ok := authority.Live(sessionID)
		if !ok || live == nil {
			return ""
		}
		return askSudo(live.EventStream, sessionID, gateway.RedactApprovalCommand(promptCtx, tools.GetSudoPromptCommand(promptCtx)),
			a.InterruptRequested, sudoTimeoutS)
	}}
	previous := tools.GetSudoPasswordCallback(ctx)
	tools.SetSudoPasswordCallback(ctx, callback)
	return func() { tools.SetSudoPasswordCallback(ctx, previous) }
}
