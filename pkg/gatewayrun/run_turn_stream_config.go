package gatewayrun

import (
	"context"
	"errors"
	"sort"
	"strings"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/settings"
	"github.com/nickheyer/icebath/pkg/tools"
	anyutil "github.com/nickheyer/icebath/pkg/utils/any"
)

// Enabled toolsets of a run on source, an adapter override validated through the platform tools resolver
func (r *GatewayRunner) resolveEnabledToolsetsForSource(ctx context.Context, userConfig map[string]any, source *gateway.SessionSource, platformKey string) []string {
	var override []string
	var hasOverride bool
	func() {
		defer func() {
			if recover() != nil {
				override, hasOverride = nil, false
			}
		}()
		adapter, err := r.deliveryAdapterFor(ctx, source)
		if err != nil || adapter == nil {
			return
		}
		override, hasOverride = adapter.ToolsetsForSource(source)
	}()
	if hasOverride && len(override) > 0 {
		toolsets := map[string]any{}
		if existing, ok := userConfig["platform_toolsets"].(map[string]any); ok {
			for k, v := range existing {
				toolsets[k] = v
			}
		}
		names := make([]any, len(override))
		for i, name := range override {
			names[i] = name
		}
		toolsets[platformKey] = names
		merged := map[string]any{}
		for k, v := range userConfig {
			merged[k] = v
		}
		merged["platform_toolsets"] = toolsets
		userConfig = merged
	}
	enabled := tools.Default.PlatformTools(ctx, "", config.Tree(userConfig), platformKey, tools.PlatformToolsOptions{IncludeDefaultMCPServers: true})
	out := make([]string, 0, len(enabled))
	for name := range enabled {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

// Enabled and disabled toolsets of a run on source, disabled nil when the config lists none
func (r *GatewayRunner) resolveTurnToolsets(ctx context.Context, userConfig map[string]any, source *gateway.SessionSource, platformKey string) ([]string, []string) {
	enabled := r.resolveEnabledToolsetsForSource(ctx, userConfig, source, platformKey)
	disabled := settings.AgentDisabledToolsets.Strings(settings.WithFrozen(ctx, userConfig))
	if len(disabled) == 0 {
		disabled = nil
	}
	return enabled, disabled
}

// Proxy URL of proxy mode for the profile ctx serves, empty when off
func (r *GatewayRunner) getProxyURL(ctx context.Context) string {
	return strings.TrimRight(strings.TrimSpace(settings.GatewayProxyUrl.Get(gateway.SettingsContext(ctx))), "/")
}

// Errors the stream consumer config raises for a platform that cannot stream
var errSkipStreaming = errors.New("skip streaming for non-editable platform")

// Stream consumer config of a source and the telegram pause typing hook, an error for an uneditable platform when asked
func (r *GatewayRunner) buildStreamConsumerConfig(source *gateway.SessionSource, scfg gateway.StreamingConfig, adapter gateway.PlatformAdapter, onMissingCursor string) (*gateway.StreamConsumerConfig, func(context.Context), error) {
	var pauseTyping func(context.Context)
	if source.Platform == gateway.PlatformTelegram && adapter.Base() != nil {
		chatID := source.ChatID
		pauseTyping = func(context.Context) { adapter.Base().PauseTypingForChat(chatID) }
	}
	supportsEdit := adapter.Base() == nil || adapter.Base().Traits.SupportsMessageEditing
	_, supportsNative := adapter.(gateway.NativeStreamAdapter)
	if !supportsEdit && !supportsNative && onMissingCursor == "raise" {
		return nil, nil, errSkipStreaming
	}
	cursor := ""
	if supportsEdit {
		cursor = anyutil.String(anyutil.Or(scfg.Cursor, ""))
	}
	if source.Platform == gateway.PlatformMatrix {
		cursor = ""
	}
	freshFinal := 0.0
	if source.Platform == gateway.PlatformTelegram {
		freshFinal = scfg.FreshFinalAfterSeconds
	}
	threshold := gateway.DefaultStreamingBufferThreshold
	if scfg.BufferThreshold != nil {
		threshold = int(scfg.BufferThreshold.Int64())
	}
	transport := scfg.Transport
	if transport == "" {
		transport = "edit"
	}
	return &gateway.StreamConsumerConfig{EditInterval: scfg.EditInterval, BufferThreshold: threshold, Cursor: cursor,
		FreshFinalAfterSeconds: freshFinal, Transport: transport, ChatType: source.ChatType}, pauseTyping, nil
}

// Predicate of whether this run's generation still owns the session, always true when untracked
func (r *GatewayRunner) runStillCurrentFn(sessionKey string, runGeneration *int) func() bool {
	return func() bool {
		if runGeneration == nil || sessionKey == "" {
			return true
		}
		return r.isSessionRunCurrent(sessionKey, *runGeneration)
	}
}

// Agent result of a proxy failure carrying only the reply text
func proxyErrorResult(text string) map[string]any {
	return map[string]any{"final_response": text, "messages": []any{}, "api_calls": 0, "tools": []any{}}
}

// Platform stream consumer of the proxy path when streaming is on for the platform, nil otherwise
func (r *GatewayRunner) proxyStreamConsumer(ctx context.Context, source *gateway.SessionSource, eventMessageID *string, threadMetadata map[string]any, runStillCurrent func() bool) *gateway.GatewayStreamConsumer {
	scfg := gateway.DefaultStreamingConfig()
	if r.config != nil {
		scfg = r.config.Streaming
	}
	if !scfg.GloballyEnabled() {
		return nil
	}
	if !scfg.EnabledFor(settings.DisplayStreaming.Get(settings.WithPlatform(ctx, platformConfigKey(source.Platform)))) {
		return nil
	}
	adapter, err := r.deliveryAdapterFor(ctx, source)
	if err != nil {
		pkgLog().Debug("Proxy: could not set up stream consumer: " + err.Error())
		return nil
	}
	if adapter == nil {
		return nil
	}
	consumerCfg, pauseTyping, err := r.buildStreamConsumerConfig(source, scfg, adapter, "fallback")
	if err != nil {
		pkgLog().Debug("Proxy: could not set up stream consumer: " + err.Error())
		return nil
	}
	return gateway.NewGatewayStreamConsumer(adapter, source.ChatID, gateway.StreamConsumerOptions{Config: consumerCfg,
		Metadata: threadMetadata, OnBeforeFinalize: pauseTyping, InitialReplyToID: optText(eventMessageID), RunStillCurrent: runStillCurrent})
}
