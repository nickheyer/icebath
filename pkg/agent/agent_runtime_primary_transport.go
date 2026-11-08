package agent

import (
	"context"
	"fmt"
	"maps"
	"time"

	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/provider"
)

// Python exception class names of transient transport failures the primary rebuild retries once
var transientTransportErrors = map[string]bool{
	"ReadTimeout": true, "ConnectTimeout": true, "PoolTimeout": true, "ConnectError": true, "ReadError": true,
	"RemoteProtocolError": true, "APIConnectionError": true, "APITimeoutError": true,
}

// Python class name of an API error, the SDK connection and timeout classes for a statusless provider error
func apiErrorClassName(err error) string {
	if apiErr, ok := err.(*provider.APIError); ok && apiErr.StatusCode == 0 {
		switch apiErr.Kind {
		case provider.ErrTimeout:
			return "APITimeoutError"
		case provider.ErrNetwork:
			return "APIConnectionError"
		}
	}
	return surfaceExceptionName(err)
}

// Snapshot field that must be present, a KeyError when the snapshot lacks it
func runtimeField(rt map[string]any, key string) (any, error) {
	v, ok := rt[key]
	if !ok {
		return nil, fmt.Errorf("KeyError: '%s'", key)
	}
	return v, nil
}

// Snapshot map field copied, empty for a missing or falsy value
func runtimeMapCopy(v any) map[string]any {
	m, _ := v.(map[string]any)
	if m == nil {
		return map[string]any{}
	}
	return maps.Clone(m)
}

// Copies the identity and transport fields of a primary runtime snapshot onto the agent in snapshot order
func applyPrimaryRuntimeFields(a *AIAgent, rt map[string]any) error {
	model, err := runtimeField(rt, "model")
	if err != nil {
		return err
	}
	a.model, _ = model.(string)
	providerName, err := runtimeField(rt, "provider")
	if err != nil {
		return err
	}
	a.provider, _ = providerName.(string)
	a.requestedProvider = a.provider
	if requested, ok := rt["requested_provider"]; ok {
		a.requestedProvider, _ = requested.(string)
	}
	baseURL, err := runtimeField(rt, "base_url")
	if err != nil {
		return err
	}
	text, _ := baseURL.(string)
	a.setBaseURL(text)
	if provider.IsActualRoute(a.provider, a.baseURL) {
		a.apiMode = "chat_completions"
	} else {
		mode, err := runtimeField(rt, "api_mode")
		if err != nil {
			return err
		}
		a.apiMode, _ = mode.(string)
	}
	clear(a.transportCache)
	apiKey, err := runtimeField(rt, "api_key")
	if err != nil {
		return err
	}
	a.apiKey, _ = apiKey.(string)
	a.reasoningEchoFlag = false
	if echo, ok := rt["reasoning_echo_flag"]; ok {
		a.reasoningEchoFlag = echo
	}
	a.requestOverrides = runtimeMapCopy(rt["request_overrides"])
	raw, err := runtimeField(rt, "client_kwargs")
	if err != nil {
		return err
	}
	kwargs, ok := raw.(map[string]any)
	if !ok {
		return fmt.Errorf("TypeError: 'client_kwargs' is not a dict")
	}
	a.clientKwargs = maps.Clone(kwargs)
	return nil
}

// Rebuilds the native Anthropic client from the snapshot credential, base URL and OAuth flag
func buildAnthropicClientFromRuntime(ctx context.Context, a *AIAgent, rt map[string]any) error {
	key, err := runtimeField(rt, "anthropic_api_key")
	if err != nil {
		return err
	}
	base, err := runtimeField(rt, "anthropic_base_url")
	if err != nil {
		return err
	}
	a.anthropicTokenSource, a.anthropicAPIKey = nil, ""
	switch k := key.(type) {
	case *provider.TaggedTokenSource:
		a.anthropicTokenSource = k
	case string:
		a.anthropicAPIKey = k
	}
	a.anthropicBaseURL, _ = base.(string)
	built, err := anthropicClientBuilder(ctx, key, a.anthropicBaseURL, a.providerRequestTimeout(ctx), false)
	if err != nil {
		return err
	}
	a.anthropicClient = built
	oauth, err := runtimeField(rt, "is_anthropic_oauth")
	if err != nil {
		return err
	}
	a.isAnthropicOAuth = pyTruthy(oauth)
	a.client = nil
	return nil
}

// Rebuilds the primary client from the snapshot, the MoA facade, Bedrock runtime, native Anthropic or OpenAI wire
func rebuildPrimaryClient(ctx context.Context, a *AIAgent, rt map[string]any, reason string) error {
	switch {
	case pystr.Lower(pystr.Strip(a.provider)) == "moa":
		a.client = buildMoaFacade(ctx, a, a.model)
		a.anthropicClient = nil
	case a.provider == "bedrock" && (a.apiMode == "anthropic_messages" || a.apiMode == "bedrock_converse"):
		return applyBedrockBinding(ctx, a, a.baseURL, provider.APIMode(a.apiMode))
	case a.apiMode == "anthropic_messages":
		return buildAnthropicClientFromRuntime(ctx, a, rt)
	default:
		client, err := a.createOpenAIClient(ctx, runtimeMapCopy(rt["client_kwargs"]), reason, true)
		if err != nil {
			return err
		}
		a.client = client
	}
	return nil
}

// Rebuilds the primary client once after retries exhaust on a transient transport error, never for aggregators
func (a *AIAgent) tryRecoverPrimaryTransport(ctx context.Context, apiError error, retryCount int) bool {
	errorType := apiErrorClassName(apiError)
	if a.fallbackActivated || !transientTransportErrors[errorType] || a.isOpenRouterURL() {
		return false
	}
	switch pystr.Lower(pystr.Strip(a.provider)) {
	case "nous", "nous-portal", "nousresearch":
		if a.apiMode != "anthropic_messages" {
			return false
		}
	}
	if err := a.rebuildPrimaryTransport(ctx, errorType, retryCount); err != nil {
		runAgentLog().Warn(fmt.Sprintf("Primary transport recovery failed: %s", err))
		return false
	}
	return true
}

// Retires the stale client, reinstalls the primary snapshot and waits before the last primary attempt
func (a *AIAgent) rebuildPrimaryTransport(ctx context.Context, errorType string, retryCount int) (err error) {
	defer func() {
		if r := recover(); r != nil {
			if isUserInterruptPanic(r) {
				panic(r)
			}
			err = fmt.Errorf("%v", r)
		}
	}()
	if a.client != nil {
		func() {
			defer func() { _ = recover() }()
			a.retireSharedOpenAIClient(a.client, "primary_recovery")
		}()
	}
	rt := a.primaryRuntime
	if rt == nil {
		return fmt.Errorf("'NoneType' object is not subscriptable")
	}
	if err := applyPrimaryRuntimeFields(a, rt); err != nil {
		return err
	}
	if err := rebuildPrimaryClient(ctx, a, rt, "primary_recovery"); err != nil {
		return err
	}
	waitTime := min(3+retryCount, 8)
	a.vprint(fmt.Sprintf("%s🔁 Transient %s on %s — rebuilt client, waiting %ds before one last primary attempt.", a.logPrefix, errorType, a.provider, waitTime), true, true)
	recoverySleep(time.Duration(waitTime) * time.Second)
	return nil
}
