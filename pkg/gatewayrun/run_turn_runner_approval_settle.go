package gatewayrun

import (
	"context"
	"fmt"
	"regexp"
	"strings"

	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/i18n"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/tools"
)

// Arms a settle hook posting the timed out notice for the approval request once its wait times out
func registerTimeoutNotice(t *TurnRunner, approvalData map[string]any, command string, cardMessageID *string) {
	requestID := ""
	if pyrepr.Truthy(approvalData["request_id"]) {
		requestID = pyrepr.Str(approvalData["request_id"])
	}
	sessionKey := t.ctx.sessionKey
	if requestID == "" || sessionKey == "" {
		return
	}
	timeoutS := gateway.ApprovalTimeoutSeconds(t.turnCtx)
	if value, present := approvalData["timeout_s"]; present && value != nil {
		if n, ok := pyIntValue(value); ok {
			timeoutS = n
		}
	}
	flagged := gateway.EADefaultReasonLocalized(t.turnCtx)
	if pyrepr.Truthy(approvalData["description"]) {
		flagged = pyrepr.Str(approvalData["description"])
	}
	tools.RegisterGatewaySettle(sessionKey, requestID, func(reason string) {
		if reason != "timeout" {
			return
		}
		if still := t.ctx.runStillCurrent; still != nil && !still() {
			return
		}
		t.schedule(func(ctx context.Context) (any, error) {
			postTimeoutNotice(ctx, t.ctx, command, cardMessageID, timeoutS, flagged)
			return nil, nil
		}, "Approval timeout notice scheduling error")
	})
}

// Edits the approval card to the timed out body or sends the notice as a new message
func postTimeoutNotice(ctx context.Context, tc *TurnContext, command string, cardMessageID *string, timeoutS int, reason string) {
	adapter := tc.statusAdapter
	notice := gateway.FormatApprovalTimedOutNotice(ctx, timeoutS)
	metadata := interimMetadata(tc.statusThreadMetadata)
	defer func() {
		if r := recover(); r != nil {
			runLog().Debug(fmt.Sprintf("Approval timeout notice failed: %v", r))
		}
	}()
	if cardMessageID != nil && *cardMessageID != "" && editTimedOutCard(ctx, adapter, tc.statusChatID, *cardMessageID, notice, command, reason) {
		return
	}
	if _, err := adapter.Send(ctx, tc.statusChatID, notice, "", metadata); err != nil {
		runLog().Debug(fmt.Sprintf("Approval timeout notice failed: %s", err))
	}
}

// Runs of three or more backticks that would close the card's code fence
var backtickRunRe = regexp.MustCompile("`{3,}")

// Timed out card body fitting one platform message with the reason and the fence safe command
func timedOutCardBody(ctx context.Context, adapter gateway.PlatformAdapter, chatID, notice, command, reason string) (string, error) {
	split := func(s string) string {
		return backtickRunRe.ReplaceAllStringFunc(s, func(run string) string {
			return strings.Join(strings.Split(run, ""), "​")
		})
	}
	command, reason = split(command), split(reason)
	measure, err := adapter.MessageLenFnForChat(chatID)
	if err != nil {
		return "", err
	}
	suffix := "..."
	if reason != "" {
		notice = notice + "\n" + gateway.EAReasonLabelLocalized(ctx) + ": " + gateway.FitPrefix(reason, 500, measure, suffix)
	}
	frame := measure(i18n.T(ctx, "gateway.exec_approval.timed_out_card_body", "", map[string]any{"notice": notice, "command": ""}))
	maxLen, err := adapter.MaxMessageLengthForChat(chatID)
	if err != nil {
		return "", err
	}
	budget := maxLen - frame - measure(suffix)
	return i18n.T(ctx, "gateway.exec_approval.timed_out_card_body", "", map[string]any{"notice": notice,
		"command": gateway.FitPrefix(command, max(0, budget), measure, suffix)}), nil
}

// Edits the card in place to the timed out body, false when the notice goes out as a new message
func editTimedOutCard(ctx context.Context, adapter gateway.PlatformAdapter, chatID, messageID, notice, command, reason string) bool {
	body, err := timedOutCardBody(ctx, adapter, chatID, notice, command, reason)
	if err != nil {
		runLog().Debug(fmt.Sprintf("Approval card edit failed; sending the notice as a new message: %s", err))
		return false
	}
	result, err := adapter.EditMessage(ctx, chatID, messageID, body, false)
	if err != nil {
		runLog().Debug(fmt.Sprintf("Approval card edit failed; sending the notice as a new message: %s", err))
		return false
	}
	return result.Success
}
