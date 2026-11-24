package agent

import (
	"context"
	"fmt"
	"strings"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/plugins"
	"github.com/nickheyer/icebath/pkg/provider"
	"github.com/nickheyer/icebath/pkg/skills"
	"github.com/nickheyer/icebath/pkg/tools"
)

type lifecycleEnvKey struct{}

var (
	lifecyclePluginInvokeHook     = plugins.InvokeHook
	lifecyclePluginAInvokeHook    = plugins.AInvokeHook
	lifecyclePluginHasHook        = plugins.HasHook
	lifecycleRelayProfileKey      = tools.CurrentRelayProfileKey
	lifecycleFinalizeConversation = func(ctx context.Context, profileKey, sessionID string) {
		tools.RelayCoordinator.FinalizeConversation(ctx, profileKey, sessionID)
	}
)

// Auxiliary call hooks and skill lifecycle events reach the plugins
func init() {
	provider.SetAuxLifecycleHooks(LifecycleHasHook, LifecycleInvokeHook)
	skills.RegisterLifecycleDispatch(skills.LifecycleDispatch{HasHook: LifecycleHasHook, InvokeHook: LifecycleInvokeHook})
}

// Carries the agent env for lifecycle readers, and as the session env when the surface bound none
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

// Plugin hook results, none under a safe worker
func lifecycleInvokeHook(ctx context.Context, hookName string, kwargs map[string]any) []any {
	if config.SafeWorkerEnabled() {
		return []any{}
	}
	return lifecyclePluginInvokeHook(ctx, hookName, kwargs)
}

// Loop side form that awaits the plugin callbacks on the loop
func lifecycleAInvokeHook(ctx context.Context, hookName string, kwargs map[string]any) []any {
	return lifecyclePluginAInvokeHook(ctx, hookName, kwargs)
}

// Reports whether a plugin consumes a hook, false under a safe worker
func lifecycleHasHook(ctx context.Context, hookName string) bool {
	if config.SafeWorkerEnabled() {
		return false
	}
	return lifecyclePluginHasHook(ctx, hookName)
}

// Stripped non empty user facing texts from finalize results given as a string or a message map
func lifecycleSessionEndMessages(results []any) []string {
	messages := []string{}
	for _, result := range results {
		text := result
		if m, ok := result.(map[string]any); ok {
			text = m["message"]
		}
		if s, ok := text.(string); ok && strings.TrimSpace(s) != "" {
			messages = append(messages, strings.TrimSpace(s))
		}
	}
	return messages
}

// Hard closes the session Relay conversation, then returns the plugin finalize results
func lifecycleFinalizeSession(ctx context.Context, kwargs map[string]any) []any {
	sessionID := ""
	if v := kwargs["session_id"]; pyrepr.Truthy(v) {
		sessionID = pyrepr.Str(v)
	}
	if sessionID != "" {
		func() {
			defer func() {
				if r := recover(); r != nil {
					pkgLog().Warn("Core Relay session finalization failed", "error", fmt.Sprint(r))
				}
			}()
			lifecycleFinalizeConversation(ctx, lifecycleRelayProfileKey(ctx), sessionID)
		}()
	}
	return lifecyclePluginInvokeHook(ctx, "on_session_finalize", kwargs)
}

// The lifecycle invoke_hook the top level wiring installs as plugins Services.InvokeLifecycleHook
func LifecycleInvokeHook(ctx context.Context, hookName string, kwargs map[string]any) []any {
	return lifecycleInvokeHook(ctx, hookName, kwargs)
}

// LifecycleAInvokeHook is the event loop form of LifecycleInvokeHook
func LifecycleAInvokeHook(ctx context.Context, hookName string, kwargs map[string]any) []any {
	return lifecycleAInvokeHook(ctx, hookName, kwargs)
}

// LifecycleHasHook reports whether a plugin consumes a hook
func LifecycleHasHook(ctx context.Context, hookName string) bool {
	return lifecycleHasHook(ctx, hookName)
}

// SessionEndMessages returns the user facing texts of on_session_finalize results
func SessionEndMessages(results []any) []string { return lifecycleSessionEndMessages(results) }

// FinalizeSession runs on_session_finalize for the core Relay conversation and the plugins
func FinalizeSession(ctx context.Context, kwargs map[string]any) []any {
	return lifecycleFinalizeSession(ctx, kwargs)
}
