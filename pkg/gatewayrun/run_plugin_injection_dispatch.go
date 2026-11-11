package gatewayrun

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/google/uuid"
	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/state"
)

// Routes a plugin triggered turn into a stored session through its live adapter, false when it cannot run
func (r *GatewayRunner) dispatchPluginMessageInjection(ctx context.Context, sessionKey *string, content, pluginID, pluginHome string) (bool, error) {
	if !r.pluginInjectionAccepting() {
		return false, nil
	}
	key := strOrEmpty(sessionKey)
	entry := r.asyncSessionStore.Store().LookupBySessionKey(ctx, key)
	if entry == nil || entry.Origin == nil || !r.pluginInjectionAccepting() {
		return false, nil
	}
	restored, err := r.restoredSource(ctx, entry)
	if err != nil {
		return false, err
	}
	source := gateway.ReplaceSource(restored, nil)
	if source.Platform == gateway.PlatformLocal {
		return r.dispatchPluginLocalInjection(ctx, source, entry, content, pluginID, pluginHome)
	}
	if !r.pluginMessagingSessionOwned(ctx, key, pluginHome, pluginID) {
		return false, nil
	}
	if !r.pluginInjectionAuthorized(ctx, source, pluginID, "session="+strOrNone(sessionKey)) {
		return false, nil
	}
	adapter, err := r.deliveryAdapterFor(ctx, source)
	if err != nil || adapter == nil {
		return false, err
	}
	scoped, err := r.pluginInjectionScope(ctx, source, "")
	if err != nil {
		return false, err
	}
	admitted, err := r.admitPluginInjection(scoped, adapter, source, key, entry.SessionID, content, pluginID)
	if err != nil {
		return false, err
	}
	if admitted != nil {
		return *admitted, nil
	}
	event := gateway.NewMessageEvent(content)
	event.Source, event.Internal, event.AllowGatewayControl = source, true, false
	event.Metadata = map[string]any{
		"hermes_plugin_id": pluginID, "hermes_plugin_injection": true,
		"gateway_session_key": key, "gateway_session_id": entry.SessionID,
		"gateway_session_strict": true,
	}
	if err := adapter.HandleMessage(scoped, event); err != nil {
		return false, err
	}
	runLog().Info(fmt.Sprintf("Plugin message injection dispatched: plugin=%s session=%s session_id=%s", pluginID, key, entry.SessionID))
	return true, nil
}

// Commits a plugin turn into a local session's FIFO when the plugin profile's own authority holds the session
func (r *GatewayRunner) dispatchPluginLocalInjection(ctx context.Context, source *gateway.SessionSource, entry *gateway.SessionEntry, content, pluginID, pluginHome string) (bool, error) {
	live, err := r.pluginLocalSession(ctx, source, entry.SessionKey, pluginHome, pluginID)
	if err != nil || live == nil {
		return false, err
	}
	scoped, err := r.pluginInjectionScope(ctx, source, pluginHome)
	if err != nil {
		return false, err
	}
	adapter, err := r.deliveryAdapterFor(scoped, live.Source)
	if err != nil {
		return false, err
	}
	admitted, err := r.admitPluginInjection(scoped, adapter, live.Source, entry.SessionKey, entry.SessionID, content, pluginID)
	return admitted != nil && *admitted, err
}

// Starts or continues the session at a plugin supplied origin through the adapter's normal message path
func (r *GatewayRunner) dispatchPluginOriginInjection(ctx context.Context, source *gateway.SessionSource, content, pluginID string) (bool, error) {
	target := fmt.Sprintf("origin=%s/%s", source.Platform, source.ChatID)
	if !r.pluginInjectionAccepting() {
		return false, nil
	}
	adapter, err := r.deliveryAdapterFor(ctx, source)
	if err != nil {
		return false, err
	}
	if adapter == nil {
		runLog().Warn(fmt.Sprintf("Plugin message injection: adapter for %s went away before dispatch", target))
		return false, nil
	}
	if !r.pluginInjectionAuthorized(ctx, source, pluginID, target) {
		return false, nil
	}
	sessionKey, err := r.sessionKeyForSource(ctx, source)
	if err != nil {
		return false, err
	}
	scoped, err := r.pluginInjectionScope(ctx, source, "")
	if err != nil {
		return false, err
	}
	authority, err := gateway.ActiveAuthority(scoped, r)
	if err != nil {
		return false, err
	}
	if authority != nil {
		entry := r.asyncSessionStore.Store().LookupBySessionKey(scoped, sessionKey)
		if entry == nil {
			runLog().Warn(fmt.Sprintf("Plugin message injection refused: plugin=%s %s has no session under the session authority yet", pluginID, target))
			return false, nil
		}
		admitted, err := r.admitPluginInjection(scoped, adapter, source, sessionKey, entry.SessionID, content, pluginID)
		return admitted != nil && *admitted, err
	}
	event := gateway.NewMessageEvent(content)
	event.Source, event.Internal, event.AllowGatewayControl = source, true, false
	event.Metadata = map[string]any{
		"hermes_plugin_id": pluginID, "hermes_plugin_injection": true,
		"gateway_session_key": sessionKey,
	}
	if err := adapter.HandleMessage(scoped, event); err != nil {
		return false, err
	}
	runLog().Info(fmt.Sprintf("Plugin message injection dispatched: plugin=%s %s session=%s", pluginID, target, sessionKey))
	return true, nil
}

// Commits a plugin turn into the session's durable FIFO as a producer admission, nil when no authority serves the scope
func (r *GatewayRunner) admitPluginInjection(ctx context.Context, adapter gateway.PlatformAdapter, source *gateway.SessionSource, sessionKey, sessionID, content, pluginID string) (*bool, error) {
	authority, err := gateway.ActiveAuthority(ctx, r)
	if err != nil || authority == nil {
		return nil, err
	}
	identity, err := pyjson.Dumps([]any{"plugin", pluginID, sessionKey, strings.ReplaceAll(uuid.NewString(), "-", "")}, pyjson.Separators(",", ":"))
	if err != nil {
		return nil, err
	}
	event := gateway.NewMessageEvent(content)
	event.Source, event.Internal, event.AllowGatewayControl, event.MessageID = source, true, false, &identity
	event.Metadata = map[string]any{"gateway_session_key": sessionKey, "gateway_session_id": sessionID}
	if _, err := authority.AdmitAutomation(ctx, adapter, event, identity, nil, nil); err != nil {
		var refusal *state.RuntimeStoreError
		if errors.As(err, &refusal) {
			runLog().Warn(fmt.Sprintf("Plugin message injection refused by the session authority: plugin=%s session=%s (%s)", pluginID, sessionKey, refusal.Reason))
			return boolp(false), nil
		}
		return nil, err
	}
	runLog().Info(fmt.Sprintf("Plugin message injection admitted: plugin=%s session=%s session_id=%s", pluginID, sessionKey, sessionID))
	return boolp(true), nil
}
