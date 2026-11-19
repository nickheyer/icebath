package agent

import (
	"context"
	"strings"
	"sync/atomic"

	"github.com/nickheyer/icebath/pkg/provider"
)

// the providers whose model names keep their vendor prefix at init
var modelNormalizeAggregators = map[string]bool{"openrouter": true, "ai-gateway": true, "kilocode": true}

var (
	openrouterPrewarmDone atomic.Bool
	openrouterPrewarmFn   = func(ctx context.Context) { provider.FetchModelMetadata(ctx, false) }
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
	finalizeRoutingPanics(func() {
		if !modelNormalizeAggregators[a.provider] {
			a.model = provider.NormalizeModelForProviderCtx(ctx, a.model, a.provider)
			a.modelNone = false
		}
	})
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
		a.startOpenRouterPrewarm(ctx)
	}
}

// Fetches the OpenRouter catalog in the background, a prewarm cut short by close leaving the next agent to run it
func (a *AIAgent) startOpenRouterPrewarm(ctx context.Context) {
	prewarmCtx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	done := make(chan struct{})
	go func() {
		defer close(done)
		openrouterPrewarmFn(prewarmCtx)
		if prewarmCtx.Err() != nil {
			openrouterPrewarmDone.Store(false)
		}
	}()
	previous := a.stopPrewarm
	a.stopPrewarm = func() {
		if previous != nil {
			previous()
		}
		cancel()
		<-done
	}
}

// Cancels the catalog prewarms the agent started and waits for them so none writes its cache after close
func (a *AIAgent) joinOpenRouterPrewarm() {
	if stop := a.stopPrewarm; stop != nil {
		a.stopPrewarm = nil
		stop()
	}
}
