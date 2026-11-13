package gateway

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"reflect"
	"sync"

	"github.com/google/uuid"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/plugins"
)

// Running gateway methods plugin message delivery resolves a session through, gateway/run.py GatewayRunner
type PluginMessageRunner interface {
	SessionStore() *SessionStore
	RestoredSource(ctx context.Context, entry *SessionEntry) (*SessionSource, error)
	SessionKeyForSource(ctx context.Context, source *SessionSource) (string, error)
	PluginLocalSession(ctx context.Context, source *SessionSource, sessionKey, pluginHome, pluginID string) (*LiveSession, error)
	PluginMessagingSessionOwned(ctx context.Context, sessionKey, pluginHome, pluginID string) bool
	DeliveryAdapterFor(ctx context.Context, source *SessionSource) (PlatformAdapter, error)
	ThreadMetadataForSource(source *SessionSource, replyToMessageID *string) map[string]any
}

// Running gateway loop state, the runner _running flag and its _gateway_loop
type GatewayLoopRunner interface {
	Running() bool
	GatewayLoop() EventLoop
	OnGatewayLoop(ctx context.Context) bool
}

// Resolved conversation, a messaging adapter and chat or a local session's live viewers
type PostTarget struct {
	Platform  string
	ChatID    string
	Adapter   PlatformAdapter
	Metadata  map[string]any
	Live      *LiveSession
	NoticeKey string
}

var (
	gatewayRunnerMu  sync.RWMutex
	gatewayRunnerVal any
)

// Records the gateway runner of this process, nil clearing it, the run.py weak reference
func SetGatewayRunnerRef(runner any) {
	gatewayRunnerMu.Lock()
	gatewayRunnerVal = runner
	gatewayRunnerMu.Unlock()
}

// Gateway runner of this process, nil outside a running gateway
func CurrentGatewayRunner() any {
	gatewayRunnerMu.RLock()
	defer gatewayRunnerMu.RUnlock()
	return gatewayRunnerVal
}

// Running runner with an open loop and whether ctx runs on that loop, nil without one
func GatewayLoop(ctx context.Context) (any, bool) {
	runner := CurrentGatewayRunner()
	r, ok := runner.(GatewayLoopRunner)
	if runner == nil || !ok || !r.Running() {
		return nil, false
	}
	if loop := r.GatewayLoop(); loop == nil || loop.IsClosed() {
		return nil, false
	}
	return runner, r.OnGatewayLoop(ctx)
}

// Runner as the platform actions facade drives it, nil without a runner
func GatewayRunnerRef() plugins.PlatformActionRunner {
	runner, _ := CurrentGatewayRunner().(plugins.PlatformActionRunner)
	return runner
}

type liveSourceKey struct{}

// Binds the live inbound source of a call dispatched before its session is stored
func WithLiveSource(ctx context.Context, source *SessionSource) context.Context {
	return context.WithValue(ctx, liveSourceKey{}, source)
}

// Live source bound in this context, nil when none
func BoundRoute(ctx context.Context) any {
	if source, ok := ctx.Value(liveSourceKey{}).(*SessionSource); ok && source != nil {
		return source
	}
	return nil
}

// Frozen launch policy a local session turn runs under, gateway/session_policy.py LocalSessionPolicy
type ActiveLaunchPolicy interface {
	PolicyPlatform() string
	PolicyCwd() string
}

type activePolicyKey struct{}

// Binds the launch policy of the turn executing in ctx
func WithActivePolicy(ctx context.Context, policy ActiveLaunchPolicy) context.Context {
	return context.WithValue(ctx, activePolicyKey{}, policy)
}

// Launch policy of the turn executing in ctx, nil for standalone serve and messaging turns
func ActivePolicy(ctx context.Context) ActiveLaunchPolicy {
	policy, _ := ctx.Value(activePolicyKey{}).(ActiveLaunchPolicy)
	return policy
}

// Platform and cwd of the active launch policy, ok false without one
func ActiveSessionPolicy(ctx context.Context) (platform, cwd string, ok bool) {
	policy := ActivePolicy(ctx)
	if policy == nil {
		return "", "", false
	}
	return policy.PolicyPlatform(), policy.PolicyCwd(), true
}

func pluginMessageErr(code, detail string) map[string]any {
	out := map[string]any{"ok": false, "error": code}
	if detail != "" {
		out["detail"] = detail
	}
	return out
}

func runePrefix(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n])
}

func pyLen(s string) int { return len([]rune(s)) }

// Text clamped to the adapter's per chat limit with an ellipsis, never split
func fitPluginText(adapter PlatformAdapter, chatID, text string) string {
	limit, measure := 4096, pyLen
	l, err := adapter.MaxMessageLengthForChat(chatID)
	m, merr := adapter.MessageLenFnForChat(chatID)
	if err == nil && merr == nil && m != nil {
		limit, measure = l, m
	}
	if limit <= 0 || measure(text) <= limit {
		return text
	}
	cut := []rune(text)
	for len(cut) > 0 && measure(string(cut)+"…") > limit {
		cut = cut[:max(0, len(cut)-max(1, measure(string(cut)+"…")-limit))]
	}
	return string(cut) + "…"
}

// Whether the adapter overrides edit_message and does not refuse edits
func supportsPluginEdit(adapter PlatformAdapter) bool {
	if base := adapter.Base(); base != nil && !base.Traits.SupportsMessageEditing {
		return false
	}
	return AdapterOverridesBaseMethod(adapter, "EditMessage")
}

// Post target for the session key names, or the refusal, the live route serving an unstored session
func ResolvePluginMessageTarget(ctx context.Context, runner any, sessionKey string, route any, pluginHome, pluginID string) (any, map[string]any, error) {
	r, ok := runner.(PluginMessageRunner)
	if !ok {
		return nil, nil, fmt.Errorf("AttributeError: '%T' object has no attribute 'session_store'", runner)
	}
	store := r.SessionStore()
	if store == nil {
		return nil, nil, errors.New("AttributeError: 'NoneType' object has no attribute 'lookup_by_session_key'")
	}
	var source *SessionSource
	if entry := store.LookupBySessionKey(ctx, sessionKey); entry != nil && entry.Origin != nil {
		restored, err := r.RestoredSource(ctx, entry)
		if err != nil {
			return nil, nil, err
		}
		source = ReplaceSource(restored, nil)
	} else if live, isSource := route.(*SessionSource); isSource && live != nil {
		key, err := r.SessionKeyForSource(ctx, live)
		if err != nil {
			return nil, nil, err
		}
		if key == sessionKey {
			source = live
		}
	}
	if source == nil {
		return nil, pluginMessageErr("no_conversation", fmt.Sprintf("session %s is not known to this gateway", pyrepr.StrRepr(sessionKey))), nil
	}
	if source.Platform == PlatformLocal {
		live, err := r.PluginLocalSession(ctx, source, sessionKey, pluginHome, pluginID)
		if err != nil {
			return nil, nil, err
		}
		if live == nil {
			return nil, pluginMessageErr("no_conversation", "the local session is not live in this plugin's profile"), nil
		}
		key := fmt.Sprintf("plugin-message:%s:%s", pluginID, uuidHex12())
		return &PostTarget{Platform: "local", ChatID: source.ChatID, Live: live, NoticeKey: key}, nil, nil
	}
	if !r.PluginMessagingSessionOwned(ctx, sessionKey, pluginHome, pluginID) {
		return nil, pluginMessageErr("no_conversation", "the session belongs to another profile"), nil
	}
	adapter, err := r.DeliveryAdapterFor(ctx, source)
	if err != nil {
		return nil, nil, err
	}
	if adapter == nil {
		return nil, pluginMessageErr("adapter_unavailable", fmt.Sprintf("no connected %s adapter answers this chat", string(source.Platform))), nil
	}
	metadata := r.ThreadMetadataForSource(source, nil)
	return &PostTarget{Platform: string(source.Platform), ChatID: source.ChatID, Adapter: adapter, Metadata: metadata}, nil, nil
}

func uuidHex12() string {
	u := uuid.New()
	return fmt.Sprintf("%x", u[:])[:12]
}

// Keyed sticky notice to a local session's viewers
func publishLocal(target *PostTarget, text string) map[string]any {
	delivered := target.Live.EventStream.Publish(target.ChatID, pyjson.ObjectOf(
		"text", text, "level", "info", "kind", "sticky", "key", target.NoticeKey, "id", target.NoticeKey,
	), "notification.show", nil)
	return map[string]any{"ok": true, "message_id": target.NoticeKey, "delivered": delivered}
}

// Interim send through the target adapter, never sealing a relay stream open for the reply
func sendPluginMessage(ctx context.Context, target *PostTarget, text string) (map[string]any, error) {
	metadata := map[string]any{}
	for k, v := range target.Metadata {
		metadata[k] = v
	}
	metadata["_interim_send"] = true
	result, err := target.Adapter.Send(ctx, target.ChatID, fitPluginText(target.Adapter, target.ChatID, text), "", metadata)
	if err != nil {
		return nil, err
	}
	if !result.Success {
		detail := ""
		if pyrepr.Truthy(result.Error) {
			detail = pyrepr.Str(result.Error)
		}
		return pluginMessageErr("send_failed", runePrefix(detail, 512)), nil
	}
	var id any
	if result.MessageID != nil {
		id = pyrepr.Str(result.MessageID)
	}
	return map[string]any{"ok": true, "message_id": id}, nil
}

func resultField(result map[string]any, key string) string {
	v, ok := result[key]
	if !ok || v == nil {
		return "None"
	}
	if b, isBool := v.(bool); isBool {
		if b {
			return "True"
		}
		return "False"
	}
	return pyrepr.Str(v)
}

func errorSuffix(result map[string]any) string {
	if ok, _ := result["ok"].(bool); ok {
		return ""
	}
	return " error=" + resultField(result, "error")
}

func errText(err error) string {
	if s := err.Error(); s != "" {
		return s
	}
	return reflect.TypeOf(err).String()
}

// First message of a handle, an adapter failure returned as a send_failed result
func PostPluginMessage(ctx context.Context, target any, text, pluginID string) map[string]any {
	t := target.(*PostTarget)
	var result map[string]any
	var err error
	if t.Live != nil {
		result = publishLocal(t, text)
	} else {
		result, err = sendPluginMessage(ctx, t, text)
	}
	if err != nil {
		slog.Warn(fmt.Sprintf("plugin_message plugin=%s op=post send raised: %v", pluginID, err))
		result = pluginMessageErr("send_failed", runePrefix(err.Error(), 512))
	}
	slog.Info(fmt.Sprintf("plugin_message plugin=%s op=post platform=%s chat=%s ok=%s message_id=%s%s", pluginID,
		t.Platform, t.ChatID, resultField(result, "ok"), resultField(result, "message_id"), errorSuffix(result)))
	return result
}

// Edits message id in place, a surface that cannot edit getting a fresh message marked fallback
func EditPluginMessage(ctx context.Context, target, messageID any, text, pluginID string) map[string]any {
	t := target.(*PostTarget)
	id := ""
	if messageID != nil {
		id = pyrepr.Str(messageID)
	}
	result, err := editPluginMessage(ctx, t, id, text, pluginID)
	if err != nil {
		slog.Warn(fmt.Sprintf("plugin_message plugin=%s op=edit raised: %v", pluginID, err))
		result = pluginMessageErr("edit_failed", runePrefix(err.Error(), 512))
	}
	edited := "False"
	if _, has := result["edited"]; has {
		edited = resultField(result, "edited")
	}
	slog.Info(fmt.Sprintf("plugin_message plugin=%s op=edit platform=%s chat=%s ok=%s edited=%s message_id=%s%s", pluginID,
		t.Platform, t.ChatID, resultField(result, "ok"), edited, resultField(result, "message_id"), errorSuffix(result)))
	return result
}

func editPluginMessage(ctx context.Context, t *PostTarget, messageID, text, pluginID string) (map[string]any, error) {
	if t.Live != nil {
		result := publishLocal(t, text)
		result["edited"] = true
		return result, nil
	}
	if messageID != "" && supportsPluginEdit(t.Adapter) {
		sent, err := t.Adapter.EditMessage(ctx, t.ChatID, messageID, fitPluginText(t.Adapter, t.ChatID, text), false)
		edited, editError := false, ""
		if err != nil {
			slog.Debug(fmt.Sprintf("plugin_message plugin=%s edit_message raised: %v", pluginID, err))
			editError = errText(err)
		} else {
			edited = sent.Success
			if pyrepr.Truthy(sent.Error) {
				editError = pyrepr.Str(sent.Error)
			}
		}
		if edited {
			out := messageID
			if pyrepr.Truthy(sent.MessageID) {
				out = pyrepr.Str(sent.MessageID)
			}
			return map[string]any{"ok": true, "message_id": out, "edited": true}, nil
		}
		result, err := sendPluginMessage(ctx, t, text)
		if err != nil {
			return nil, err
		}
		result["edited"], result["fallback"], result["edit_error"] = false, true, runePrefix(editError, 256)
		return result, nil
	}
	result, err := sendPluginMessage(ctx, t, text)
	if err != nil {
		return nil, err
	}
	result["edited"], result["fallback"] = false, true
	return result, nil
}

// Fills the plugin services the gateway answers on the services struct the wiring installs
func InstallPluginServices(s *plugins.Services) {
	s.GatewayLoop = GatewayLoop
	s.BoundRoute = BoundRoute
	s.ResolvePluginMessageTarget = ResolvePluginMessageTarget
	s.PostPluginMessage = PostPluginMessage
	s.EditPluginMessage = EditPluginMessage
	s.ActiveSessionPolicy = ActiveSessionPolicy
	s.GatewayRunnerRef = GatewayRunnerRef
}
