package gatewayrun

import (
	"context"
	"fmt"
	"strings"

	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/i18n"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/tools"
)

// Approve argument words and the approval scope each one selects
var approveChoiceByArg = map[string]string{
	"always": "always", "permanent": "always", "permanently": "always",
	"session": "session", "ses": "session",
}

// Session key with no reply while an agent thread blocks on approval, else the stale or none reply to send
func (r *GatewayRunner) blockingApprovalOrStale(ctx context.Context, event *gateway.MessageEvent, staleKey, noneKey string) (string, string, error) {
	sessionKey, err := r.sessionKeyForSource(ctx, event.Source)
	if err != nil {
		return "", "", err
	}
	if tools.HasBlockingApproval(sessionKey) {
		return sessionKey, "", nil
	}
	if r.pendingApprovals().Contains(sessionKey) {
		r.pendingApprovals().Delete(sessionKey)
		return sessionKey, i18n.T(ctx, staleKey, "", nil), nil
	}
	return sessionKey, i18n.T(ctx, noneKey, "", nil), nil
}

// Unblocks the waiting agent threads once, for the session or always, every pending one with all
func (r *GatewayRunner) handleApproveCommand(ctx context.Context, event *gateway.MessageEvent) (*string, error) {
	sessionKey, stale, err := r.blockingApprovalOrStale(ctx, event, "gateway.approval_expired", "gateway.approve.no_pending")
	if err != nil {
		return nil, err
	}
	if stale != "" {
		return &stale, nil
	}
	args := strings.Fields(strings.ToLower(pystr.Strip(event.GetCommandArgs())))
	choices := map[string]bool{}
	resolveAll := false
	for _, a := range args {
		if choice, ok := approveChoiceByArg[a]; ok {
			choices[choice] = true
		}
		if a == "all" {
			resolveAll = true
		}
	}
	choice := "once"
	if choices["always"] {
		choice = "always"
	} else if choices["session"] {
		choice = "session"
	}
	count := tools.ResolveGatewayApproval(sessionKey, choice, resolveAll, "", "")
	if count == 0 {
		reply := i18n.T(ctx, "gateway.approve.no_pending", "", nil)
		return &reply, nil
	}
	form := "singular"
	if count > 1 {
		form = "plural"
	}
	confirmation := i18n.T(ctx, "gateway.approve."+choice+"_"+form, "", map[string]any{"count": count})
	runLog().Info(fmt.Sprintf("User approved %d dangerous command(s) via /approve (%s)", count, choice))
	return r.deliverApprovalConfirmation(ctx, event, confirmation, "approve")
}

// Denies the oldest pending dangerous command or all of them, relaying an optional one line reason
func (r *GatewayRunner) handleDenyCommand(ctx context.Context, event *gateway.MessageEvent) (*string, error) {
	sessionKey, stale, err := r.blockingApprovalOrStale(ctx, event, "gateway.deny.stale", "gateway.deny.no_pending")
	if err != nil {
		return nil, err
	}
	if stale != "" {
		return &stale, nil
	}
	rawArgs := pystr.Strip(event.GetCommandArgs())
	tokens := strings.Fields(rawArgs)
	resolveAll := len(tokens) > 0 && strings.ToLower(tokens[0]) == "all"
	reason := rawArgs
	if resolveAll {
		reason = pystr.Strip(rawArgs[len(tokens[0]):])
	}
	reason = pystr.Strip(headRunes(reason, 280))
	count := tools.ResolveGatewayApproval(sessionKey, "deny", resolveAll, reason, "")
	if count == 0 {
		reply := i18n.T(ctx, "gateway.deny.no_pending", "", nil)
		return &reply, nil
	}
	withReason := ""
	key := "gateway.deny.denied"
	if reason != "" {
		withReason = " (with reason)"
		key += "_reason"
	}
	if count > 1 {
		key += "_plural"
	} else {
		key += "_singular"
	}
	runLog().Info(fmt.Sprintf("User denied %d dangerous command(s) via /deny%s", count, withReason))
	confirmation := i18n.T(ctx, key, "", map[string]any{"count": count, "reason": reason})
	return r.deliverApprovalConfirmation(ctx, event, confirmation, "deny")
}

// Confirmation text for normal delivery, or sent directly with control lane metadata on a native streaming adapter
func (r *GatewayRunner) deliverApprovalConfirmation(ctx context.Context, event *gateway.MessageEvent, confirmation, verb string) (*string, error) {
	source := event.Source
	adapter, err := r.deliveryAdapterFor(ctx, source)
	if err != nil {
		return nil, err
	}
	if adapter != nil {
		adapter.Base().ResumeTypingForChat(source.ChatID)
	}
	if !gateway.AdapterSupportsNativeStreaming(adapter) {
		return &confirmation, nil
	}
	replyTo := ""
	if event.MessageID != nil {
		replyTo = *event.MessageID
	}
	if _, err := adapter.Send(ctx, source.ChatID, confirmation, replyTo, map[string]any{"is_approval_prompt": true, "force_proactive_send": true}); err != nil {
		runLog().Warn(fmt.Sprintf("Failed to send /%s confirmation to %s: %s", verb, source.ChatID, err))
	}
	return nil, nil
}
