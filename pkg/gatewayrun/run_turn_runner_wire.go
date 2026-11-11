package gatewayrun

import (
	"context"
	"strings"

	"github.com/nickheyer/icebath/pkg/agent"
	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
)

// Installs this message's callbacks, reasoning, tier, voice route and notes on the agent, never baked into the cache
func (t *TurnRunner) wireTurnAgentCallbacks(ctx context.Context, a *agent.AIAgent, route turnRoute, reasoningConfig map[string]any, streams turnStreamCallbacks) error {
	tc := t.ctx
	r := t.runner
	a.SetNotificationPresentation(string(tc.source.Platform), tc.userConfig)
	a.SetToolProgressCallback(t.progressCallback)
	var toolStart, toolComplete any
	if tc.voiceAckGuild != nil || tc.nativeSlackTaskCards {
		toolStart = tc.nativeToolStartCallback
		if toolStart == nil {
			toolStart = tc.voiceAckCallback
		}
	}
	if tc.nativeSlackTaskCards {
		toolComplete = tc.nativeToolCompleteCallback
	}
	if t.approvalOwner != nil {
		toolStart, toolComplete = t.combinedToolStartCallback, t.combinedToolCompleteCallback
	}
	a.SetToolStartCallback(toolStart)
	a.SetToolCompleteCallback(toolComplete)
	loaded, err := tc.hooksRef.LoadedHooks(ctx)
	if err != nil {
		return err
	}
	if len(loaded) > 0 || t.approvalOwner != nil {
		a.SetStepCallback(t.stepCallbackSync)
	} else {
		a.SetStepCallback(nil)
	}
	if streams.streamDelta != nil {
		a.SetStreamDeltaCallback(streams.streamDelta)
	} else {
		a.SetStreamDeltaCallback(nil)
	}
	if streams.wantInterimMsgs && streams.interim != nil {
		a.SetInterimAssistantCallback(streams.interim)
	} else {
		a.SetInterimAssistantCallback(nil)
	}
	local := localViewerTurn(t)
	a.SetStatusCallback(t.statusCallbackSync)
	if local {
		a.SetNoticeCallback(t.localNoticeCallbackSync)
	} else {
		a.SetNoticeCallback(t.noticeCallbackSync)
	}
	a.SetReasoningCallback(nil)
	if t.approvalOwner != nil && gateway.APIExecution(ctx) != nil {
		wireAPIObservers(ctx, a, t.approvalOwner, streams.wantInterimMsgs)
	}
	if local {
		a.SetNoticeClearCallback(t.localNoticeClearCallbackSync)
	} else {
		a.SetNoticeClearCallback(nil)
	}
	a.SetEventCallback(t.eventCallbackSync)
	a.SetReasoningConfig(reasoningConfig)
	a.SetServiceTier(r.serviceTier)
	a.SetDaybreak(r.loadDaybreak(ctx, tc.source))
	mergeTurnRequestOverrides(a, route)
	agent.ArmSurfaceTurn(ctx, a, r.consumePendingTurnSidecarNotes(tc.sessionKey), tc.voiceTurn)
	bgReview, bgRelease := t.makeBgReviewCallbacks()
	a.SetBackgroundReviewCallback(bgReview)
	if tc.statusAdapter != nil && tc.sessionKey != "" {
		if base := tc.statusAdapter.Base(); base != nil {
			base.RegisterPostDeliveryCallback(tc.sessionKey, bgRelease, tc.runGeneration)
		}
	}
	var memNotif any
	if display, ok := tc.userConfig["display"].(map[string]any); ok {
		memNotif = display["memory_notifications"]
	}
	if flag, isBool := memNotif.(bool); isBool {
		memNotif = "off"
		if flag {
			memNotif = "on"
		}
	}
	if pyrepr.Truthy(memNotif) {
		a.SetMemoryNotifications(strings.ToLower(pyrepr.Str(memNotif)))
	} else {
		a.SetMemoryNotifications("on")
	}
	a.SetClarifyCallback(t.clarifyCallbackSync)
	if tc.muteNotificationReply {
		a.SetToolProgressCallback(nil)
		a.SetToolStartCallback(nil)
		a.SetToolCompleteCallback(nil)
		a.SetStreamDeltaCallback(nil)
		a.SetInterimAssistantCallback(nil)
	}
	tc.setAgent(a)
	if owner := t.approvalOwner; owner != nil {
		owner.authority.AdoptAgent(owner.sessionID, owner.generation, a)
	}
	t.attachSessionTitleCallback(ctx, a)
	r.stampTurnProcess(a, tc.processTaskID, tc.processBaseline)
	tc.holderMu.Lock()
	tc.toolsHolder = a.ToolDefinitions()
	tc.holderMu.Unlock()
	return nil
}

// Wires the thread rename lane onto the agent as its session title callback before the turn prologue titles it
func (t *TurnRunner) attachSessionTitleCallback(ctx context.Context, a *agent.AIAgent) {
	tc := t.ctx
	defer func() {
		if rec := recover(); rec != nil {
			runLog().Debug("Failed to attach session title callback", "error", rec)
		}
	}()
	a.SetTitleFailureCallback(func(task string, err error) {
		runLog().Debug("Gateway auto-title failure suppressed (not user-visible): " + task + ": " + pyrepr.Str(err))
	})
	sessionID := a.SessionID()
	if localViewerTurn(t) {
		owner := t.approvalOwner
		authority, canonicalID := owner.authority, owner.sessionID
		a.SetOnSessionTitle(func(title, titleSource string) {
			ref := gateway.SessionRef{ProfileID: authority.ProfileID, SessionID: canonicalID}
			info, err := gateway.LocalSessionInfo(ctx, authority, ref)
			if err != nil {
				panic(err)
			}
			live, ok := authority.Live(canonicalID)
			if !ok || live == nil {
				panic(&pyRuntimeError{"KeyError: " + pyrepr.Repr(canonicalID)})
			}
			live.EventStream.Publish(canonicalID, info, "session.info", nil)
		})
		return
	}
	r := t.runner
	source := r.recoverDiscordAutoThreadSource(ctx, tc.source, tc.sessionKey)
	var lane func(ctx context.Context, source *gateway.SessionSource, sessionID, title string)
	switch {
	case r.isTelegramTopicLane(ctx, source):
		lane = r.scheduleTelegramTopicTitleRename
	case r.isDiscordAutoThreadLane(source) || r.isRelayDiscordChannelLane(source):
		lane = r.scheduleDiscordSemanticThreadRename
	default:
		return
	}
	a.SetOnSessionTitle(func(title, titleSource string) {
		if titleSource == "llm" {
			lane(ctx, source, sessionID, title)
		}
	})
}
