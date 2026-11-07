package agent

import (
	"context"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
)

type mutedSurfaceKey struct{}

var freeformPresentationEvents = map[string]bool{
	"message.start": true, "message.delta": true, "message.interim": true, "message.complete": true,
	"reasoning.delta": true, "thinking.delta": true, "status.update": true, "notification.show": true,
	"tool.start": true, "tool.complete": true, "tool.generating": true, "error": true, "reaction": true,
}

// Text and media UI events of the session whose diagnostic only turn is muted in this context
func EventPresentationMuted(ctx context.Context, event, sessionID string) bool {
	muted, ok := ctx.Value(mutedSurfaceKey{}).(string)
	return ok && muted == sessionID && freeformPresentationEvents[event]
}

// Early failure and watch monitor process events are diagnostics, never the requested final result
func DiagnosticProcessEvent(event map[string]any) bool {
	if pyrepr.Truthy(event["task_failure_notice"]) {
		return true
	}
	switch event["type"] {
	case "watch_disabled", "watch_overflow_tripped", "watch_overflow_released":
		return true
	}
	return false
}

// Owning profile effective config read once per turn as a fresh copy
func NotificationConfigSnapshot(ctx context.Context) map[string]any {
	return config.EffectiveUserConfig(ctx)
}

// Binds one foreground warning policy for callbacks and returns the restore of the previous binding
func (a *AIAgent) NotificationPolicySnapshot(platform string, cfg map[string]any) (restore func()) {
	savedPlatform, savedConfig := a.notificationPlatform, a.notificationConfig
	a.notificationConfig, a.notificationPlatform = cfg, &platform
	return func() { a.notificationPlatform, a.notificationConfig = savedPlatform, savedConfig }
}

type presentationSnapshot struct {
	streamDelta, interimAssistant, reasoning, toolProgress, toolStart, toolComplete, toolGen any
	reaction                                                                                 func(string)
	suppressStatusOutput, muteNotificationReply                                              bool
}

// Freezes a muted turn presentation without touching prompts or tool schemas, restored by the returned func
func (a *AIAgent) NotificationTurn(ctx context.Context, muted bool, sessionID string) (context.Context, func()) {
	if !muted {
		return ctx, func() {}
	}
	saved := presentationSnapshot{
		a.streamDeltaCallback, a.interimAssistantCallback, a.reasoningCallback, a.toolProgressCallback,
		a.toolStartCallback, a.toolCompleteCallback, a.toolGenCallback, a.reactionCallback,
		a.suppressStatusOutput, a.muteNotificationReply,
	}
	a.streamDeltaCallback, a.interimAssistantCallback, a.reasoningCallback, a.toolProgressCallback = nil, nil, nil, nil
	a.toolStartCallback, a.toolCompleteCallback, a.toolGenCallback, a.reactionCallback = nil, nil, nil, nil
	a.suppressStatusOutput, a.muteNotificationReply = true, true
	return context.WithValue(ctx, mutedSurfaceKey{}, sessionID), func() {
		a.streamDeltaCallback, a.interimAssistantCallback, a.reasoningCallback, a.toolProgressCallback = saved.streamDelta, saved.interimAssistant, saved.reasoning, saved.toolProgress
		a.toolStartCallback, a.toolCompleteCallback, a.toolGenCallback, a.reactionCallback = saved.toolStart, saved.toolComplete, saved.toolGen, saved.reaction
		a.suppressStatusOutput, a.muteNotificationReply = saved.suppressStatusOutput, saved.muteNotificationReply
	}
}
