package gatewayrun

import (
	"context"
	"fmt"
	"strconv"
	"sync/atomic"

	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/i18n"
	"github.com/nickheyer/icebath/pkg/settings"
	anyutil "github.com/nickheyer/icebath/pkg/utils/any"
)

// Acknowledges a slash command that falls through to the agent, onto the local command sink when one is bound
func (r *GatewayRunner) sendCommandAck(ctx context.Context, source *gateway.SessionSource, text, label string) {
	if sink := gateway.ActiveSink(ctx); sink != nil {
		sink.Notices = append(sink.Notices, text)
		return
	}
	adapter, err := r.deliveryAdapterFor(ctx, source)
	if err != nil {
		pkgLog().Debug(fmt.Sprintf("%s ack send failed", label), "error", err)
		return
	}
	if adapter == nil {
		return
	}
	if _, err := adapter.Send(ctx, source.ChatID, text, "", r.threadMetadataForSource(source, nil)); err != nil {
		pkgLog().Debug(fmt.Sprintf("%s ack send failed", label), "error", err)
	}
}

// Delivery origin of a shared command so its jobs reply to this chat and thread, nil without both
func commandOriginForSource(source *gateway.SessionSource) map[string]any {
	if source == nil || source.Platform == "" || source.ChatID == "" {
		return nil
	}
	var chatName, threadID any
	if source.ChatName != nil {
		chatName = *source.ChatName
	}
	if source.ThreadID != nil {
		threadID = *source.ThreadID
	}
	return map[string]any{"platform": string(source.Platform), "chat_id": source.ChatID, "chat_name": chatName, "thread_id": threadID}
}

// Gates a destructive session command behind a confirm prompt unless approvals.destructive_slash_confirm is off
func (r *GatewayRunner) maybeConfirmDestructiveSlash(ctx context.Context, event *gateway.MessageEvent, command, title, detail string, execute func(ctx context.Context) (any, error)) (any, error) {
	if !settings.ConfirmDestructive.Get(ctx) {
		return execute(ctx)
	}
	sessionKey, err := r.sessionKeyForSource(ctx, event.Source)
	if err != nil {
		return nil, err
	}
	onConfirm := func(ctx context.Context, choice string) (string, error) {
		result, err := runConfirmedDestructiveSlash(ctx, choice, command, execute, sessionKey)
		if err != nil {
			return "", err
		}
		text, _ := result.(string)
		return text, nil
	}
	prompt := i18n.T(ctx, "gateway.confirm.destructive_prompt", "", map[string]any{
		"command": command, "detail": detail, "prefix": r.typedCommandPrefixFor(event.Source.Platform)})
	reply, err := r.requestSlashConfirm(ctx, event, command, title, prompt, onConfirm)
	if err != nil || reply == nil {
		return nil, err
	}
	return *reply, nil
}

// Confirm callback body, cancel replies, always persists the opt out first, then the command runs
func runConfirmedDestructiveSlash(ctx context.Context, choice, command string, execute func(ctx context.Context) (any, error), sessionKey string) (any, error) {
	if choice == "cancel" {
		return i18n.T(ctx, "gateway.confirm.cancelled", "", map[string]any{"command": command}), nil
	}
	persisted := false
	if choice == "always" {
		persisted = SaveConfigValue(ctx, "approvals.destructive_slash_confirm", false)
		if persisted {
			pkgLog().Info(fmt.Sprintf("User opted out of destructive slash confirm (session=%s)", sessionKey))
		} else {
			pkgLog().Warn(fmt.Sprintf("Could not persist destructive_slash_confirm=false (session=%s); config.yaml is not writable", sessionKey))
		}
	}
	result, err := execute(ctx)
	if err != nil {
		return nil, err
	}
	if text, ok := result.(string); ok && choice == "always" {
		return text + destructiveOptoutNote(ctx, persisted), nil
	}
	return result, nil
}

// Asks to confirm a slash command, nil once buttons rendered or a decline suppressed them, else the prompt text
func (r *GatewayRunner) requestSlashConfirm(ctx context.Context, event *gateway.MessageEvent, command, title, message string, handler func(ctx context.Context, choice string) (string, error)) (*string, error) {
	source := event.Source
	sessionKey, err := r.sessionKeyForSource(ctx, source)
	if err != nil {
		return nil, err
	}
	if r.slashConfirmCounter == nil {
		r.slashConfirmCounter = &atomic.Int64{}
	}
	confirmID := strconv.FormatInt(r.slashConfirmCounter.Add(1), 10)
	SlashConfirmRegister(sessionKey, confirmID, command, handler)
	adapter, err := r.deliveryAdapterFor(ctx, source)
	if err != nil {
		return nil, err
	}
	metadata := r.threadMetadataForSource(source, gateway.ReplyAnchorForEvent(event))
	if adapter != nil {
		result, sendErr := adapter.SendSlashConfirm(ctx, source.ChatID, title, message, sessionKey, confirmID, metadata)
		if sendErr != nil {
			pkgLog().Debug(fmt.Sprintf("send_slash_confirm failed for %s on %s: %s", command, source.Platform, sendErr))
		} else {
			if result.Success {
				return nil, nil
			}
			if gateway.DeclinedSend(&result) {
				pkgLog().Warn(fmt.Sprintf("slash-confirm DECLINED by the connector's egress guard for %s on %s — suppressing the text fallback: %s",
					command, source.Platform, anyutil.String(result.Error)))
				SlashConfirmClear(sessionKey)
				return nil, nil
			}
		}
	}
	return &message, nil
}
