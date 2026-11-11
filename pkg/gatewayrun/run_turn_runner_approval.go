package gatewayrun

import (
	"context"
	"errors"
	"fmt"

	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/tools"
)

// Seconds a local viewer's approval or clarify prompt counts down, nil on a messaging turn or an unlimited clarify wait
func promptTimeoutS(t *TurnRunner, kind string) *float64 {
	if !localViewerTurn(t) {
		return nil
	}
	var seconds int
	if kind == "approval" {
		seconds = tools.GetApprovalTimeout(t.toolsEnv())
	} else {
		seconds = tools.GetClarifyTimeout(t.toolsEnv())
	}
	if seconds <= 0 {
		return nil
	}
	value := float64(seconds)
	return &value
}

// Notify callback of the dangerous command approval wait, rendering the prompt then registering it with the owner
func (t *TurnRunner) approvalNotifySync(approvalData map[string]any) error {
	owner := t.approvalOwner
	if owner != nil {
		if err := owner.authority.CheckApprovalGeneration(t.turnCtx, owner.sessionID, owner.generation); err != nil {
			return err
		}
	}
	if err := t.renderApprovalSync(approvalData); err != nil {
		return err
	}
	if owner != nil {
		return owner.authority.RegisterApproval(t.turnCtx, owner.sessionID, owner.generation, t.ctx.sessionKey, approvalData,
			promptTimeoutS(t, "approval"))
	}
	return nil
}

// Adapter that renders native exec approval buttons
type execApprovalCardSender interface {
	SendExecApproval(ctx context.Context, chatID, command, sessionKey string, description *string, metadata map[string]any, allowPermanent, allowSession, smartDenied bool, requestID *string) (gateway.SendResult, error)
}

// Boolean approval flag with its default when absent
func approvalFlag(data map[string]any, key string, def bool) bool {
	value, present := data[key]
	if !present {
		return def
	}
	return pyrepr.Truthy(value)
}

// Sends the approval request as native buttons when the adapter renders them, else as text with the typed approve steps
func (t *TurnRunner) renderApprovalSync(approvalData map[string]any) error {
	tc := t.ctx
	adapter := tc.statusAdapter
	adapter.Base().PauseTypingForChat(tc.statusChatID)
	t.closeNativeStreamBoundary("Approval", "", false)
	command, present := approvalData["command"]
	if !present {
		command = ""
	}
	cmd := gateway.RedactApprovalCommand(t.turnCtx, command)
	desc := gateway.RedactApprovalCommand(t.turnCtx, approvalData["description"])
	if desc == "" {
		desc = gateway.EADefaultReasonLocalized(t.turnCtx)
	}
	allowPermanent := approvalFlag(approvalData, "allow_permanent", true)
	allowSession := approvalFlag(approvalData, "allow_session", true)
	smartDenied := approvalFlag(approvalData, "smart_denied", false)
	if sender, ok := adapter.(execApprovalCardSender); ok && gateway.RendersExecApprovalButtons(adapter) {
		done, declined := t.sendApprovalCard(sender, approvalData, cmd, desc, allowPermanent, allowSession, smartDenied)
		if declined != nil {
			return declined
		}
		if done {
			return nil
		}
	}
	prefix := adapter.Base().Traits.TypedCommandPrefix
	msg := formatExecApprovalFallback(t.turnCtx, cmd, desc, prefix, allowPermanent, allowSession, smartDenied)
	metadata := map[string]any{}
	for k, v := range tc.statusThreadMetadata {
		metadata[k] = v
	}
	metadata["is_approval_prompt"] = true
	chatID := tc.statusChatID
	fut := t.schedule(func(ctx context.Context) (any, error) {
		return adapter.Send(ctx, chatID, msg, "", interimMetadata(metadata))
	}, "Approval text-send scheduling error")
	if fut == nil {
		return nil
	}
	if _, err := fut.resultWithin(15); err != nil {
		runLog().Error(fmt.Sprintf("Failed to send approval request: %s", err))
		return nil
	}
	registerTimeoutNotice(t, approvalData, cmd, nil)
	return nil
}

// Sends the native button card, true when no text fallback follows and the decline error when the connector refused it
func (t *TurnRunner) sendApprovalCard(sender execApprovalCardSender, approvalData map[string]any, cmd, desc string, allowPermanent, allowSession, smartDenied bool) (bool, error) {
	tc := t.ctx
	var requestID *string
	if value, ok := approvalData["request_id"]; ok && value != nil {
		id := pyrepr.Str(value)
		requestID = &id
	}
	chatID, sessionKey, metadata := tc.statusChatID, tc.sessionKey, tc.statusThreadMetadata
	fut := t.schedule(func(ctx context.Context) (any, error) {
		return sender.SendExecApproval(ctx, chatID, cmd, sessionKey, &desc, metadata, allowPermanent, allowSession, smartDenied, requestID)
	}, "send_exec_approval scheduling error")
	if fut == nil {
		runLog().Warn(fmt.Sprintf("Button-based approval failed, falling back to text: %s", errors.New("send_exec_approval: loop unavailable")))
		return false, nil
	}
	switch approvalSendOutcome(fut, 15) {
	case "sent":
		result, _ := fut.resultWithin(0)
		var cardID *string
		if sent, ok := result.(gateway.SendResult); ok && sent.MessageID != nil {
			id := pyrepr.Str(sent.MessageID)
			cardID = &id
		}
		registerTimeoutNotice(t, approvalData, cmd, cardID)
		return true, nil
	case "ambiguous":
		runLog().Warn("Button-based approval send timed out — treating as possibly-delivered (no re-send; the prompt stays armed for a late tap)")
		return true, nil
	case "declined":
		runLog().Warn("Button-based approval DECLINED by the connector's egress guard — not falling back to text (the destination is not approved for this connection)")
		return true, &execApprovalDeclinedError{"exec approval undeliverable: connector egress declined this destination"}
	}
	runLog().Warn("Button-based approval failed (send returned error), falling back to text")
	return false, nil
}
