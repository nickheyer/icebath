package agent

import (
	"context"
	"strings"
	"sync/atomic"

	"github.com/nickheyer/icebath/pkg/provider"
)

// hermes_cli.model_normalize _AGGREGATOR_PROVIDERS, the providers whose model names keep their vendor prefix at init
var modelNormalizeAggregators = map[string]bool{"openrouter": true, "nous": true, "ai-gateway": true, "kilocode": true}

var (
	openrouterPrewarmDone atomic.Bool
	openrouterPrewarmFn   = func(ctx context.Context) { provider.FetchModelMetadata(ctx, false) }
	nousKeepaliveStart    = provider.StartNousAuthKeepalive
	finalizeRoutingPanics = func(fn func()) {
		defer func() { _ = recover() }()
		fn()
	}
)

// Validates the pool, warms the transport, normalizes and pins the model and upgrades GPT 5 routes to Responses
func (a *AIAgent) finalizeRouting(ctx context.Context, apiMode string) {
	if pool := a.credentialPool; pool != nil {
		matches := false
		finalizeRoutingPanics(func() {
			matches = provider.CredentialPoolMatchesProvider(ctx, pool.Provider(), a.provider, a.baseURL)
		})
		if !matches {
			a.credentialPool = nil
		}
	}
	finalizeRoutingPanics(func() { a.getTransport("") })
	if a.provider == "nous" {
		finalizeRoutingPanics(func() { nousKeepaliveStart(agentPoolEnv(ctx), provider.NousKeepaliveStartOptions{}) })
	}
	finalizeRoutingPanics(func() {
		if !modelNormalizeAggregators[a.provider] {
			a.model = provider.NormalizeModelForProvider(a.model, a.provider)
			a.modelNone = false
		}
	})
	a.model = provider.PinModelForRoute(a.provider, a.baseURL, a.model)
	a.modelNone = a.modelNone && a.model == ""
	baseLower := strings.ToLower(a.baseURL)
	if apiMode == "" && a.apiMode == "chat_completions" &&
		!provider.IsActualRoute(a.provider, a.baseURL) &&
		!strings.HasPrefix(baseLower, "acp://") && !strings.HasPrefix(baseLower, "acp+tcp://") &&
		!provider.IsExternalProcessProvider(ctx, a.provider) &&
		!a.isAzureOpenAIURL(nil) &&
		(a.isDirectOpenAIURL(nil) || a.ProviderModelRequiresResponsesAPI(a.model, a.provider)) {
		a.apiMode = "codex_responses"
		clear(a.transportCache)
	}
	if (a.provider == "openrouter" || a.isOpenRouterURL()) && openrouterPrewarmDone.CompareAndSwap(false, true) {
		go openrouterPrewarmFn(context.WithoutCancel(ctx))
	}
}
