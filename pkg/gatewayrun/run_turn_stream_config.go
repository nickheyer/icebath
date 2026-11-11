package gatewayrun

import (
	"context"
	"errors"
	"os"
	"sort"
	"strings"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/provider"
	"github.com/nickheyer/icebath/pkg/tools"
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
	agentCfg, _ := userConfig["agent"].(map[string]any)
	disabled := tools.ParseConfigStringList(agentCfg["disabled_toolsets"])
	if len(disabled) == 0 {
		disabled = nil
	}
	return enabled, disabled
}

// Proxy URL of proxy mode, the profile scoped secret first and the gateway config second, empty when off
func (r *GatewayRunner) getProxyURL(ctx context.Context) string {
	url, _, err := provider.GetSecret(ctx, "GATEWAY_PROXY_URL")
	var unscoped *provider.UnscopedSecretError
	if errors.As(err, &unscoped) {
		url = os.Getenv("GATEWAY_PROXY_URL")
	}
	url = pystr.Strip(url)
	if url == "" {
		gatewayCfg, _ := gateway.LoadGatewayConfigMap(ctx, "")["gateway"].(map[string]any)
		url = pystr.Strip(pyrepr.Str(pyrepr.Or(gatewayCfg["proxy_url"], "")))
	}
	return strings.TrimRight(url, "/")
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
		cursor = pyrepr.Str(pyrepr.Or(scfg.Cursor, ""))
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
	platStreaming := config.ResolveDisplaySetting(ctx, gateway.LoadGatewayConfigMap(ctx, ""), platformConfigKey(source.Platform), "streaming", nil)
	if !scfg.EnabledFor(platStreaming) {
		return nil
	}
	adapter, err := r.deliveryAdapterFor(ctx, source)
	if err != nil {
		runLog().Debug("Proxy: could not set up stream consumer: " + pyrepr.ExceptionMessage(err))
		return nil
	}
	if adapter == nil {
		return nil
	}
	consumerCfg, pauseTyping, err := r.buildStreamConsumerConfig(source, scfg, adapter, "fallback")
	if err != nil {
		runLog().Debug("Proxy: could not set up stream consumer: " + pyrepr.ExceptionMessage(err))
		return nil
	}
	return gateway.NewGatewayStreamConsumer(adapter, source.ChatID, gateway.StreamConsumerOptions{Config: consumerCfg,
		Metadata: threadMetadata, OnBeforeFinalize: pauseTyping, InitialReplyToID: optText(eventMessageID), RunStillCurrent: runStillCurrent})
}
