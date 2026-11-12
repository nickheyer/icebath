package agent

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/observability"
	"github.com/nickheyer/icebath/pkg/plugins"
	"github.com/nickheyer/icebath/pkg/tools"
)

type lifecycleEnvKey struct{}

var (
	lifecycleObserveLifecycle     = observability.ObserveLifecycle
	lifecycleHandlesHook          = observability.HandlesHook
	lifecyclePluginInvokeHook     = plugins.InvokeHook
	lifecyclePluginAInvokeHook    = plugins.AInvokeHook
	lifecyclePluginHasHook        = plugins.HasHook
	lifecycleRelayProfileKey      = tools.CurrentRelayProfileKey
	lifecycleFinalizeConversation = func(ctx context.Context, profileKey, sessionID string) {
		tools.RelayCoordinator.FinalizeConversation(ctx, profileKey, sessionID)
	}
)

// Carries the agent env for lifecycle observers, and as the session env when the surface bound none
func withLifecycleEnv(ctx context.Context, env *tools.Env) context.Context {
	if env == nil {
		return ctx
	}
	if tools.ToolEnvFrom(ctx) == nil {
		ctx = tools.WithToolEnv(ctx, env)
	}
	return context.WithValue(ctx, lifecycleEnvKey{}, env)
}

// Agent env the context carries, nil outside an agent turn
func lifecycleEnv(ctx context.Context) *tools.Env {
	env, _ := ctx.Value(lifecycleEnvKey{}).(*tools.Env)
	return env
}

// Dispatches a hook to the built in observers unless a safe worker is bound, logging a failure
func lifecycleObserve(ctx context.Context, hookName string, kwargs map[string]any) {
	if config.SafeWorkerEnabled() {
		return
	}
	defer func() {
		if r := recover(); r != nil {
			slog.Warn("Built-in observability hook failed", "error", fmt.Sprint(r))
		}
	}()
	lifecycleObserveLifecycle(ctx, lifecycleEnv(ctx), hookName, kwargs)
}

// Notifies the built in observers then returns the plugin hook results, none under a safe worker
func lifecycleInvokeHook(ctx context.Context, hookName string, kwargs map[string]any) []any {
	if config.SafeWorkerEnabled() {
		return []any{}
	}
	lifecycleObserve(ctx, hookName, kwargs)
	return lifecyclePluginInvokeHook(ctx, hookName, kwargs)
}

// Loop side form that notifies the observers then awaits the plugin callbacks on the loop
func lifecycleAInvokeHook(ctx context.Context, hookName string, kwargs map[string]any) []any {
	lifecycleObserve(ctx, hookName, kwargs)
	return lifecyclePluginAInvokeHook(ctx, hookName, kwargs)
}

// Reports whether a built in observer or a plugin consumes a hook, false under a safe worker
func lifecycleHasHook(ctx context.Context, hookName string) bool {
	if config.SafeWorkerEnabled() {
		return false
	}
	if lifecycleObserverHandles(ctx, hookName) {
		return true
	}
	return lifecyclePluginHasHook(ctx, hookName)
}

// Asks the built in observers about a hook and logs a failed inspection as unhandled
func lifecycleObserverHandles(ctx context.Context, hookName string) (handled bool) {
	defer func() {
		if r := recover(); r != nil {
			slog.Warn("Unable to inspect built-in observability hooks", "error", fmt.Sprint(r))
			handled = false
		}
	}()
	return lifecycleHandlesHook(ctx, hookName)
}

// Stripped non empty user facing texts from finalize results given as a string or a message map
func lifecycleSessionEndMessages(results []any) []string {
	messages := []string{}
	for _, result := range results {
		text := result
		if m, ok := result.(map[string]any); ok {
			text = m["message"]
		}
		if s, ok := text.(string); ok && pystr.Strip(s) != "" {
			messages = append(messages, pystr.Strip(s))
		}
	}
	return messages
}

// Notifies observers, hard closes the session Relay conversation, then returns the plugin finalize results
func lifecycleFinalizeSession(ctx context.Context, kwargs map[string]any) []any {
	lifecycleObserve(ctx, "on_session_finalize", kwargs)
	sessionID := ""
	if v := kwargs["session_id"]; pyrepr.Truthy(v) {
		sessionID = pyrepr.Str(v)
	}
	if sessionID != "" {
		func() {
			defer func() {
				if r := recover(); r != nil {
					slog.Warn("Core Relay session finalization failed", "error", fmt.Sprint(r))
				}
			}()
			lifecycleFinalizeConversation(ctx, lifecycleRelayProfileKey(ctx), sessionID)
		}()
	}
	return lifecyclePluginInvokeHook(ctx, "on_session_finalize", kwargs)
}

// LifecycleInvokeHook is the hermes lifecycle invoke_hook the top level wiring installs as plugins Services.InvokeLifecycleHook
func LifecycleInvokeHook(ctx context.Context, hookName string, kwargs map[string]any) []any {
	return lifecycleInvokeHook(ctx, hookName, kwargs)
}

// LifecycleAInvokeHook is the event loop form of LifecycleInvokeHook
func LifecycleAInvokeHook(ctx context.Context, hookName string, kwargs map[string]any) []any {
	return lifecycleAInvokeHook(ctx, hookName, kwargs)
}

// LifecycleHasHook reports whether a built in observer or a plugin consumes a hook
func LifecycleHasHook(ctx context.Context, hookName string) bool {
	return lifecycleHasHook(ctx, hookName)
}

// SessionEndMessages returns the user facing texts of on_session_finalize results
func SessionEndMessages(results []any) []string { return lifecycleSessionEndMessages(results) }

// FinalizeSession runs on_session_finalize for observers, the core Relay conversation and the plugins
func FinalizeSession(ctx context.Context, kwargs map[string]any) []any {
	return lifecycleFinalizeSession(ctx, kwargs)
}
