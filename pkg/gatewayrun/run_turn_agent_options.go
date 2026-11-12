package gatewayrun

import (
	"github.com/nickheyer/icebath/pkg/agent"
	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/internal/pyint"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/provider"
)

// Spreads resolved runtime keyword arguments onto agent constructor options, the AIAgent of a runtime splat
func applyRuntimeKwargs(o *agent.AIAgentOptions, runtime map[string]any) {
	o.APIKey = runtime["api_key"]
	if o.APIKey == nil {
		o.APIKey = ""
	}
	o.BaseURL = noneText(runtime["base_url"])
	o.Provider = noneText(runtime["provider"])
	o.RequestedProvider = noneText(runtime["requested_provider"])
	o.APIMode = noneText(runtime["api_mode"])
	o.Command = noneText(runtime["command"])
	o.Args = nil
	if args, ok := runtime["args"].([]string); ok {
		o.Args = append([]string(nil), args...)
	} else if args, ok := runtime["args"].([]any); ok {
		for _, a := range args {
			o.Args = append(o.Args, pyrepr.Str(a))
		}
	}
	o.CredentialPool, _ = runtime["credential_pool"].(*provider.Pool)
	o.RequestOverrides, _ = runtime["request_overrides"].(map[string]any)
	if raw, ok := runtime["max_tokens"]; ok && raw != nil {
		if n, err := pyint.Int64(raw); err == nil {
			o.MaxTokens = int(n)
		}
	}
	if raw, ok := runtime["capabilities"].(map[string]any); ok {
		o.Capabilities = make(map[string]bool, len(raw))
		for k, v := range raw {
			o.Capabilities[k] = pyrepr.Truthy(v)
		}
	}
}

// Thread metadata for a send that replies to the event on its source
func (r *GatewayRunner) eventThreadMetadata(event *gateway.MessageEvent, source *gateway.SessionSource) map[string]any {
	return r.threadMetadataForSource(source, gateway.ReplyAnchorForEvent(event))
}
