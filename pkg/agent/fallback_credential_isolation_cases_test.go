package agent

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/provider"
)

type isolationClient struct{ apiKey, baseURL string }

func (c isolationClient) BaseURL() string { return c.baseURL }
func (c isolationClient) APIKey() string  { return c.apiKey }
func (c isolationClient) Close() error    { return nil }
func (c isolationClient) CreateChatCompletion(context.Context, map[string]any) (any, error) {
	return nil, errors.New("isolation test client has no completion endpoint")
}

func isolationPool(a *AIAgent, providerName string, entries ...provider.PooledCredential) *provider.Pool {
	if len(entries) == 0 {
		entries = []provider.PooledCredential{{Provider: providerName, ID: providerName + "-entry-0", AccessToken: "key-" + providerName,
			BaseURL: "https://" + providerName + ".example.com/v1"}}
	}
	return provider.NewPool(agentPoolEnv(a.homeContext()), providerName, entries)
}

func benchedIsolationPool(a *AIAgent, providerName string, availableIn time.Duration) *provider.Pool {
	now := float64(time.Now().UnixNano()) / 1e9
	return isolationPool(a, providerName, provider.PooledCredential{Provider: providerName, ID: providerName + "-entry-0", AccessToken: "key-" + providerName,
		BaseURL: "https://" + providerName + ".example.com/v1", LastStatus: provider.StatusExhausted, LastStatusAt: now, LastErrorResetAt: now + availableIn.Seconds()})
}

type isolationResolves struct {
	mu        sync.Mutex
	providers []string
}

func (r *isolationResolves) calls() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.providers...)
}

func recordIsolationResolves(t *testing.T, client provider.AuxClient, model string) *isolationResolves {
	t.Helper()
	r := &isolationResolves{}
	prev := agentResolveProviderClient
	agentResolveProviderClient = func(_ context.Context, providerName, _ string, _ provider.ProviderClientOptions) (provider.AuxClient, string, error) {
		r.mu.Lock()
		r.providers = append(r.providers, providerName)
		r.mu.Unlock()
		return client, model, nil
	}
	t.Cleanup(func() { agentResolveProviderClient = prev })
	return r
}

func recordIsolationPoolLoads(t *testing.T, pools map[string]*provider.Pool) *[]string {
	t.Helper()
	keys := &[]string{}
	var mu sync.Mutex
	prev := agentLoadPool
	agentLoadPool = func(_ provider.PoolEnv, key string) *provider.Pool {
		mu.Lock()
		*keys = append(*keys, key)
		mu.Unlock()
		return pools[key]
	}
	t.Cleanup(func() { agentLoadPool = prev })
	return keys
}

func isolationAgent(t *testing.T, providerName, model, baseURL, apiMode string, chain []map[string]any) *AIAgent {
	t.Helper()
	a := newRestoreAgent(t, "", nil, "custom", baseURL, nil)
	a.provider, a.requestedProvider, a.model, a.apiMode = providerName, providerName, model, apiMode
	a.fallbackChain, a.fallbackIndex, a.fallbackActivated = chain, 0, false
	a.credentialPool = isolationPool(a, providerName)
	a.transportCache = map[string]provider.Transport{}
	return a
}

func TestFallbackCredentialIsolationFallbackAttachesMatchingPoolAfterClear(t *testing.T) {
	a := isolationAgent(t, "ollama-cloud", "glm-5.2", "https://ollama.com/v1", "chat_completions",
		[]map[string]any{{"provider": "openai-codex", "model": "gpt-5.5"}})
	fallbackPool := isolationPool(a, "openai-codex")
	resolves := recordIsolationResolves(t, isolationClient{apiKey: "codex-key", baseURL: "https://chatgpt.com/backend-api/codex"}, "gpt-5.5")
	loads := recordIsolationPoolLoads(t, map[string]*provider.Pool{"openai-codex": fallbackPool})
	if !a.tryActivateFallback(a.homeContext(), "") {
		t.Fatal("fallback did not activate")
	}
	if got := resolves.calls(); len(got) != 1 {
		t.Fatalf("provider client resolved %d times: %v", len(got), got)
	}
	if len(*loads) == 0 {
		t.Fatal("fallback pool was never loaded")
	}
	for _, key := range *loads {
		if key != "openai-codex" {
			t.Fatalf("pool loads reached %v, want only the fallback provider", *loads)
		}
	}
	if a.provider != "openai-codex" || a.model != "gpt-5.5" || a.baseURL != "https://chatgpt.com/backend-api/codex" || a.apiMode != "codex_responses" {
		t.Fatalf("route %s %s %s %s", a.provider, a.model, a.baseURL, a.apiMode)
	}
	if a.credentialPool != fallbackPool || a.credentialPool.Provider() != "openai-codex" {
		t.Fatalf("credential pool %v provider %q", a.credentialPool, a.credentialPool.Provider())
	}
	if len(a.transportCache) != 0 {
		t.Fatalf("transport cache kept %d entries across the switch", len(a.transportCache))
	}
}

func TestFallbackCredentialIsolationFallbackSkipsACandidateWhosePoolIsExhaustedForHours(t *testing.T) {
	a := isolationAgent(t, "anthropic", "claude", "https://api.anthropic.com", "chat_completions",
		[]map[string]any{{"provider": "openai-codex", "model": "gpt-5.5"}, {"provider": "openrouter", "model": "m"}})
	benched := benchedIsolationPool(a, "openai-codex", 30995*time.Second)
	throttled := benchedIsolationPool(a, "openrouter", 30*time.Second)
	if benched.HasAvailable("gpt-5.5") || throttled.HasAvailable("m") {
		t.Fatal("benched pools report an available credential")
	}
	resolves := recordIsolationResolves(t, isolationClient{apiKey: "k", baseURL: "https://openrouter.ai/api/v1"}, "m")
	recordIsolationPoolLoads(t, map[string]*provider.Pool{"openai-codex": benched, "openrouter": throttled})
	if !a.tryActivateFallback(a.homeContext(), "") {
		t.Fatal("fallback did not activate")
	}
	if got := resolves.calls(); len(got) != 1 || got[0] != "openrouter" {
		t.Fatalf("provider client resolves %v want one for openrouter", got)
	}
	if a.provider != "openrouter" || a.credentialPool != throttled {
		t.Fatalf("provider %s pool %v want openrouter with the throttled pool", a.provider, a.credentialPool)
	}
}
