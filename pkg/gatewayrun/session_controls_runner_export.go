package gatewayrun

import (
	"context"

	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/internal/pydict"
)

// Busy input mode of the routed profile startup snapshot for the session controls
func (r *GatewayRunner) EffectiveBusyInputMode(ctx context.Context, source *gateway.SessionSource) string {
	return r.effectiveBusyInputMode(ctx, source)
}

// Sets or clears the session scoped fast tier pick for the session controls
func (r *GatewayRunner) SetSessionServiceTierOverride(sessionKey string, serviceTier *string, clear bool) {
	r.setSessionServiceTierOverride(sessionKey, serviceTier, clear)
}

// Voice mode of the chat a source arrived on, off when none was stored
func (r *GatewayRunner) VoiceModeFor(source *gateway.SessionSource) string {
	if r.voiceMode == nil {
		return "off"
	}
	if mode, ok := r.voiceMode.Get(r.voiceKeyForSource(source)); ok {
		return mode
	}
	return "off"
}

// Stores and persists the voice mode of the chat a source arrived on
func (r *GatewayRunner) StoreVoiceMode(source *gateway.SessionSource, mode string) {
	if r.voiceMode == nil {
		r.voiceMode = pydict.New[string, string]()
	}
	r.voiceMode.Set(r.voiceKeyForSource(source), mode)
	r.saveVoiceModes()
}

// Service tier the session's next turn runs, its fast pick else the routed profile's
func (r *GatewayRunner) ResolveSessionServiceTier(ctx context.Context, source *gateway.SessionSource, sessionKey string) (*string, error) {
	return r.resolveSessionServiceTier(ctx, source, sessionKey)
}

// Applies a reasoning pick to a route for the session controls, the reply text returned
func (r *GatewayRunner) ApplyReasoningSelection(ctx context.Context, sessionKey, platformKey, value string, persistGlobal bool) string {
	return r.applyReasoningSelection(ctx, sessionKey, platformKey, value, persistGlobal)
}
