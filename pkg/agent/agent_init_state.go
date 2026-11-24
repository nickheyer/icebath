package agent

import (
	"context"
	"fmt"
	"sync"

	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/logging"
	"github.com/nickheyer/icebath/pkg/memory"
	"github.com/nickheyer/icebath/pkg/provider"
	"github.com/nickheyer/icebath/pkg/settings"
	"github.com/nickheyer/icebath/pkg/state"
	"github.com/nickheyer/icebath/pkg/tools"
	syncutil "github.com/nickheyer/icebath/pkg/utils/sync"
)

var (
	agentSetupLogging = func(ctx context.Context, h string) error {
		_, err := logging.SetupLogging(ctx, logging.Options{Home: h})
		return err
	}
	agentSetupVerboseLogging = logging.SetupVerboseLogging
)

// Fresh control flow state for interrupts, steer, redirect, delegation and background review
func setControlStateDefaults(a *AIAgent) {
	a.toolGuardrails = tools.NewToolCallGuardrailController(nil)
	a.hardInterruptRequested = &syncutil.Event{}
	a.clientLock = &syncutil.ReentrantMutex{}
	a.modelRequestActive = &syncutil.Event{}
	a.supportsActiveTurnRedirect = true
	a.pendingSteerLock = &sync.Mutex{}
	a.pendingRedirectLock = &sync.Mutex{}
	a.toolWorkerThreads = map[int64]bool{}
	a.toolWorkerThreadsLock = &sync.Mutex{}
	a.activeChildren = []*AIAgent{}
	a.activeChildrenLock = &sync.Mutex{}
	a.backgroundReviewLock = &sync.Mutex{}
}

// Activity clock, run budget and credits latch a new agent starts its first turn with
func initTurnState(a *AIAgent, runBudgetSeconds any) {
	a.lastActivityTS = activityWallNow()
	a.lastActivityDesc = "initializing"
	a.lastActivityProvenance = state.ActivityProvenanceUnknown
	a.runBudgetSeconds = 0
	if budget := normalizeRunBudgetSeconds(runBudgetSeconds); budget != nil {
		a.runBudgetSeconds = *budget
	}
	a.headerCapture = nil
}

// Fresh stream scrubbers, interim text set and image fallback cache for the agent's streams
func setStreamStateDefaults(a *AIAgent) {
	a.streamContextScrubber = memory.NewStreamingContextScrubber()
	a.streamThinkScrubber = provider.NewStreamingThinkScrubber()
	a.setCurrentStreamedAssistantText("")
	a.deliveredInterimTexts = map[string]bool{}
	a.streamWriterLock, a.streamWriterTLS = &sync.Mutex{}, &syncutil.GoroutineLocal[any]{}
	a.anthropicImageFallbackCache = map[string]string{}
}

// Installs the home log files and the verbose console handler when verbose logging is on
func setupAgentLogging(ctx context.Context, a *AIAgent) error {
	if err := agentSetupLogging(ctx, home.Dir(ctx)); err != nil {
		return err
	}
	if a.verboseLogging {
		if err := agentSetupVerboseLogging(); err != nil {
			return err
		}
		pkgLog().Info("Verbose logging enabled (third-party library logs suppressed)")
	}
	return nil
}

// Prints the credential source, the masked key over 12 characters, or the missing key warning
func printKeyBanner(key any, label string, warnMissing bool) {
	if source := provider.CredentialLabel(key); source != "" {
		_, _ = fmt.Fprintln(statusStdout, "🔑 Using credentials: "+source)
		return
	}
	if s, ok := key.(string); ok {
		if r := []rune(s); len(r) > 12 {
			_, _ = fmt.Fprintf(statusStdout, "🔑 Using %s: %s...%s\n", label, string(r[:8]), string(r[len(r)-4:]))
			return
		}
	}
	if warnMissing {
		_, _ = fmt.Fprintln(statusStdout, "⚠️  Warning: API key appears invalid or missing")
	}
}

// Should cache and native layout for the agent's own route under the operator disable
func (a *AIAgent) anthropicPromptCachePolicy(ctx context.Context) (bool, bool) {
	return provider.ResolveAnthropicPromptCachePolicy(ctx, &provider.CachePolicyAgent{
		Provider: a.provider, BaseURL: a.baseURL, APIMode: a.apiMode, Model: a.model,
		CacheDisabled: a.cacheDisabled, CustomProviders: a.customProviderEntries,
	}, provider.CacheDestination{})
}

// Resolves the prompt caching policy and the cache ttl tier in effect for ctx, auto by session pace, disabled ttls off
func initPromptCacheConfig(ctx context.Context, a *AIAgent) {
	a.usePromptCaching, a.useNativeCacheLayout = a.anthropicPromptCachePolicy(ctx)
	a.cacheDisabled = false
	a.cacheTTL = settings.PromptCachingCacheTtl.Default
	switch ttl := settings.PromptCachingCacheTtl.Get(ctx); ttl {
	case "5m", "1h":
		a.cacheTTL = ttl
	case "auto":
		a.cacheTTL = provider.AutoCacheTTLForSource(provider.SessionSourceFor(a.env, a.platform))
	case "off":
		a.usePromptCaching = false
		a.useNativeCacheLayout = false
		a.cacheTTL = ""
		a.cacheDisabled = true
	}
}
