package agent

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/nickheyer/icebath/pkg/provider"
	"github.com/nickheyer/icebath/pkg/tools"
)

func childPoolEntry(providerName, id, url string) provider.PooledCredential {
	return provider.PooledCredential{Provider: providerName, ID: id, Label: id, AuthType: "api_key", Priority: 0, Source: "env:" + id, AccessToken: "key-" + id, BaseURL: url}
}

// Records every pool load key and answers each with the scripted pool
func scriptLoadedPool(t *testing.T, pool *provider.Pool) *[]string {
	t.Helper()
	var keys []string
	prev := loadedPoolFn
	loadedPoolFn = func(_ context.Context, key string) *provider.Pool {
		keys = append(keys, key)
		return pool
	}
	t.Cleanup(func() { loadedPoolFn = prev })
	return &keys
}

// Records every credential swap with the lease counts at that moment
func recordChildSwaps(t *testing.T) *[]provider.PooledCredential {
	t.Helper()
	var swaps []provider.PooledCredential
	var mu sync.Mutex
	prev := swapChildCredentialFn
	swapChildCredentialFn = func(_ context.Context, _ *AIAgent, leased provider.PooledCredential) (bool, error) {
		mu.Lock()
		swaps = append(swaps, leased)
		mu.Unlock()
		return true, nil
	}
	t.Cleanup(func() { swapChildCredentialFn = prev })
	return &swaps
}

func TestChildCredentialPoolResolutionSameProviderSharesParentPool(t *testing.T) {
	parent := mockDelegateParent(t, 0)
	ctx := tools.WithToolEnv(context.Background(), parent.env)
	pool := provider.NewPool(agentPoolEnv(ctx), "openrouter", nil)
	parent.credentialPool = pool
	if got := resolveChildCredentialPool(ctx, "openrouter", parent, "", ""); got != pool {
		t.Fatalf("got %v", got)
	}
}

func TestChildCredentialPoolResolutionSameProviderPoolForAnotherEndpointIsNotShared(t *testing.T) {
	parent := mockDelegateParent(t, 0)
	ctx := tools.WithToolEnv(context.Background(), parent.env)
	azure := "https://res.cognitiveservices.azure.com/openai/v1"
	poolFor := func(url string) *provider.Pool {
		return provider.NewPool(agentPoolEnv(ctx), "openai", []provider.PooledCredential{childPoolEntry("openai", url, url)})
	}
	parent.provider, parent.baseURL = "openai", azure
	parent.credentialPool = poolFor("https://api.openai.com/v1")
	scriptLoadedPool(t, nil)
	if got := resolveChildCredentialPool(ctx, "openai", parent, azure, ""); got != nil {
		t.Fatalf("public pool shared with the azure child %v", got)
	}
	parent.credentialPool = poolFor(azure)
	if got := resolveChildCredentialPool(ctx, "openai", parent, azure, ""); got != parent.credentialPool {
		t.Fatalf("azure pool not shared %v", got)
	}
}

func TestChildCredentialPoolResolutionSameProviderWithoutParentPoolKeepsFixedCredential(t *testing.T) {
	parent := mockDelegateParent(t, 0)
	parent.provider, parent.credentialPool = "anthropic", nil
	loads := scriptLoadedPool(t, nil)
	if got := resolveChildCredentialPool(tools.WithToolEnv(context.Background(), parent.env), "anthropic", parent, "http://localhost:8080/anthropic", ""); got != nil {
		t.Fatalf("got %v", got)
	}
	if len(*loads) != 0 {
		t.Fatalf("pool loaded %v", *loads)
	}
}

func TestChildCredentialPoolResolutionFixedProxyChildDoesNotLeaseProviderDefaultPool(t *testing.T) {
	parent := mockDelegateParent(t, 0)
	ctx := tools.WithToolEnv(context.Background(), parent.env)
	parent.provider, parent.apiMode, parent.baseURL, parent.apiKey = "anthropic", "anthropic_messages", "https://api.anthropic.com", ""
	parent.clientKwargs = map[string]any{"api_key": "proxy-credential", "base_url": "http://localhost:8080/anthropic"}
	parent.credentialPool = nil
	providerPool := provider.NewPool(agentPoolEnv(ctx), "anthropic", []provider.PooledCredential{childPoolEntry("anthropic", "anthropic-default", "")})
	scriptLoadedPool(t, providerPool)
	swaps := recordChildSwaps(t)
	built := captureChildBuilds(t, parent.env, "done")
	spec := childSpec(parent, "Use the parent's fixed proxy route", nil, 10, "leaf")
	spec.Toolsets = []string{"terminal"}
	child, err := buildChildAgent(ctx, spec)
	if err != nil {
		t.Fatal(err)
	}
	result := runSingleChild(ctx, 0, "Use the parent's fixed proxy route", child, parent, "", nil, nil)
	if entryText(result, "status") != "completed" {
		t.Fatalf("result %v", result)
	}
	if o := (*built)[0]; o.BaseURL != "http://localhost:8080/anthropic" || o.APIKey != "proxy-credential" {
		t.Fatalf("options %+v", o)
	}
	if leases := providerPool.ActiveLeases(); len(leases) != 0 || len(*swaps) != 0 {
		t.Fatalf("leases %v swaps %v", leases, *swaps)
	}
}

func TestChildCredentialPoolResolutionNamedCustomChildPoolFollowsRequestedProviderNotEndpointOrder(t *testing.T) {
	parent := mockDelegateParent(t, 0)
	url := "https://gateway.invalid/v1"
	if err := os.WriteFile(filepath.Join(parent.env.Home, "config.yaml"), []byte("providers:\n  claude-ai:\n    api: "+url+"\n  open-ai:\n    api: "+url+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	ctx := tools.WithToolEnv(context.Background(), parent.env)
	parent.provider, parent.baseURL, parent.requestedProvider, parent.credentialPool = "custom", url, "custom:open-ai", nil
	loads := scriptLoadedPool(t, provider.NewPool(agentPoolEnv(ctx), "custom", nil))
	resolveChildCredentialPool(ctx, "custom", parent, url, "custom:open-ai")
	if len(*loads) != 1 || !strings.Contains((*loads)[0], "open-ai") || strings.Contains((*loads)[0], "claude") {
		t.Fatalf("loaded keys %v", *loads)
	}
}

func TestChildCredentialPoolResolutionBuildChildAgentStrictIntersectionWhenOptedOut(t *testing.T) {
	parent := mockDelegateParent(t, 0)
	writeDelegationBlock(t, parent.env.Home, "  inherit_mcp_toolsets: false\n")
	parent.enabledToolsets = []string{"web", "browser", "mcp-MiniMax"}
	built := captureChildBuilds(t, parent.env)
	spec := childSpec(parent, "Test narrowed toolsets", nil, 10, "leaf")
	spec.Toolsets = []string{"web", "browser"}
	if _, err := buildChildAgent(tools.WithToolEnv(context.Background(), parent.env), spec); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual((*built)[0].EnabledToolsets, []string{"web", "browser"}) {
		t.Fatalf("enabled %v", (*built)[0].EnabledToolsets)
	}
}

func TestChildCredentialLeasingRunSingleChildAcquiresAndReleasesLease(t *testing.T) {
	parent := mockDelegateParent(t, 0)
	ctx := tools.WithToolEnv(context.Background(), parent.env)
	child, _ := cannedDelegateChild(t, parent.env, "done")
	pool := provider.NewPool(agentPoolEnv(ctx), "openrouter", []provider.PooledCredential{childPoolEntry("openrouter", "cred-b", "")})
	child.credentialPool = pool
	var leasesAtSwap map[string]int
	prev := swapChildCredentialFn
	swapChildCredentialFn = func(_ context.Context, _ *AIAgent, leased provider.PooledCredential) (bool, error) {
		leasesAtSwap = pool.ActiveLeases()
		if leased.ID != "cred-b" {
			t.Errorf("swapped %v", leased)
		}
		return true, nil
	}
	t.Cleanup(func() { swapChildCredentialFn = prev })
	result := runSingleChild(ctx, 0, "Investigate rate limits", child, parent, "", nil, nil)
	if entryText(result, "status") != "completed" || leasesAtSwap["cred-b"] != 1 || len(pool.ActiveLeases()) != 0 {
		t.Fatalf("result %v leases at swap %v after %v", result, leasesAtSwap, pool.ActiveLeases())
	}
}

func TestChildCredentialLeasingRunSingleChildReleasesLeaseAfterFailure(t *testing.T) {
	parent := mockDelegateParent(t, 0)
	ctx := tools.WithToolEnv(context.Background(), parent.env)
	child := livenessChild(parent.env, "lease-failure", 0, 0)
	scriptChildConversation(t, child, func(context.Context) (map[string]any, error) { return nil, errors.New("boom") })
	pool := provider.NewPool(agentPoolEnv(ctx), "openrouter", []provider.PooledCredential{childPoolEntry("openrouter", "cred-a", "")})
	child.credentialPool = pool
	recordChildSwaps(t)
	result := runSingleChild(ctx, 1, "Trigger failure", child, parent, "", nil, nil)
	if entryText(result, "status") != "error" || len(pool.ActiveLeases()) != 0 {
		t.Fatalf("result %v leases %v", result, pool.ActiveLeases())
	}
}

func TestChildCredentialLeasingLeaseBindsOnlyAnEntryForTheChildEndpoint(t *testing.T) {
	env := delegationTestEnv(t, 0, false)
	ctx := tools.WithToolEnv(context.Background(), env)
	azure := "https://res.cognitiveservices.azure.com/openai/v1"
	pool := provider.NewPool(agentPoolEnv(ctx), "openai", []provider.PooledCredential{childPoolEntry("openai", "pub", "https://api.openai.com/v1"), childPoolEntry("openai", "az", azure)})
	pool.AcquireLease("az")
	swaps := recordChildSwaps(t)
	child := &AIAgent{provider: "openai", baseURL: azure, credentialPool: pool, env: env}
	_, leaseID := leaseChildCredential(ctx, child)
	if leaseID != "az" || len(*swaps) != 1 || (*swaps)[0].BaseURL != azure {
		t.Fatalf("lease %q swaps %v", leaseID, *swaps)
	}
	if !reflect.DeepEqual(pool.ActiveLeases(), map[string]int{"az": 2}) {
		t.Fatalf("leases %v", pool.ActiveLeases())
	}
}
