package gatewayrun

import (
	"context"
	"fmt"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/provider"
)

// Whether a status adapter is present and this run is still the current generation
func (t *TurnRunner) statusLive() bool {
	return t.ctx.statusAdapter != nil && t.ctx.runStillCurrent()
}

// Schedules a status text send on the turn's status adapter
func (t *TurnRunner) sendStatusText(text string, metadata map[string]any, logMessage string) {
	tc := t.ctx
	adapter, chatID := tc.statusAdapter, tc.statusChatID
	t.schedule(func(ctx context.Context) (any, error) {
		return adapter.Send(ctx, chatID, text, "", metadata)
	}, logMessage)
}

// Emits an agent lifecycle event to the turn's gateway hooks on the loop
func (t *TurnRunner) eventCallbackSync(eventType string, hookContext map[string]any) {
	tc := t.ctx
	if tc.loopForStep == nil || tc.hooksRef == nil {
		runLog().Debug("event_callback hook error: loop is None")
		return
	}
	hooks := tc.hooksRef
	taskCtx := context.WithoutCancel(t.turnCtx)
	if err := tc.loopForStep.CallSoonThreadsafe(func() {
		spawnGatewayTask(taskCtx, "event-callback-hook", func(ctx context.Context) (any, error) {
			return nil, hooks.emit(ctx, eventType, hookContext)
		})
	}); err != nil {
		runLog().Debug(fmt.Sprintf("event_callback hook error: %s", err))
	}
}

// Publishes an agent notice to local viewers as notification.show
func (t *TurnRunner) localNoticeCallbackSync(notice provider.AgentNotice) {
	tc := t.ctx
	if tc.muteNotificationReply || !t.statusLive() || pystr.Strip(notice.Text) == "" {
		return
	}
	_, err := config.RenderNotification(t.turnCtx, func() error {
		t.publishExecution("notification.show", map[string]any{"text": pystr.Strip(notice.Text), "level": notice.Level,
			"kind": notice.Kind, "ttl_ms": notice.TTLMs, "key": notice.Key, "id": notice.ID})
		return nil
	}, string(tc.source.Platform), config.IsDiagnosticNotice(notice.Level, notice.Key), tc.userConfig)
	if err != nil {
		runLog().Debug(fmt.Sprintf("local notice render failed: %s", err))
	}
}

// Withdraws a keyed notice from local viewers as notification.clear
func (t *TurnRunner) localNoticeClearCallbackSync(key string) {
	if key != "" && t.statusLive() {
		t.publishExecution("notification.clear", map[string]any{"key": key})
	}
}

// Delivers an agent status line to the chat or to local viewers after the noise filter and notification gate
func (t *TurnRunner) statusCallbackSync(eventType string, message any) {
	tc := t.ctx
	if tc.muteNotificationReply || !t.statusLive() {
		return
	}
	prepared := prepareGatewayStatusMessage(t.turnCtx, tc.source.Platform, eventType, message)
	if prepared == nil {
		platform := "unknown"
		if tc.source.Platform != "" {
			platform = string(tc.source.Platform)
		}
		text := ""
		if pyrepr.Truthy(message) {
			text = pyrepr.Str(message)
		}
		runLog().Debug(fmt.Sprintf("status_callback suppressed for %s/%s: %s", platform, eventType,
			pystr.Slice(redactGatewayUserFacingSecrets(t.turnCtx, text), 0, 160)))
		return
	}
	text := *prepared
	present := func() error {
		if localViewerTurn(t) {
			t.publishExecution("status.update", map[string]any{"kind": viewerStatusKind(eventType, text), "text": text})
			return nil
		}
		adapter, chatID, metadata := tc.statusAdapter, tc.statusChatID, tc.statusThreadMetadata
		fut := t.schedule(func(ctx context.Context) (any, error) {
			return sendOrUpdateStatus(ctx, adapter, chatID, eventType, text, metadata)
		}, fmt.Sprintf("status_callback (%s) scheduling error", eventType))
		if fut != nil && tc.cleanupProgress {
			onDone(fut, t.trackFutureCleanupID)
		}
		return nil
	}
	if _, err := config.RenderNotification(t.turnCtx, present, string(tc.source.Platform), config.IsWarningStatus(eventType, message), tc.userConfig); err != nil {
		runLog().Debug(fmt.Sprintf("status render failed: %s", err))
	}
}
