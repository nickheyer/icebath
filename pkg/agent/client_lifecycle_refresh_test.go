package agent

import (
	"encoding/base64"
	"encoding/json"
	"testing"

	"github.com/nickheyer/icebath/pkg/provider"
)

func nousJWT(exp float64, sub string) string {
	b64 := func(v any) string {
		raw, _ := json.Marshal(v)
		return base64.RawURLEncoding.EncodeToString(raw)
	}
	return b64(map[string]any{"alg": "none"}) + "." + b64(map[string]any{"exp": exp, "sub": sub}) + ".sig"
}

// Nous chat agent over a fake OpenAI constructor recording each rebuilt client
func nousAdoptionAgent(t *testing.T, key string) (*AIAgent, *[]*fakeOpenAI) {
	t.Helper()
	installFakeResponseHooks(t)
	constructed := installFakeOpenAI(t)
	t.Setenv("ICEBATH_HOME", t.TempDir())
	a := &AIAgent{provider: "nous", apiMode: "chat_completions", apiKey: key}
	a.setBaseURL("https://inference-api.nousresearch.com/v1")
	a.clientKwargs = map[string]any{}
	return a, constructed
}

// Replaces the Nous store resolver for one test
func installNousResolver(t *testing.T, fn func(env provider.PoolEnv, opts provider.NousResolveOptions) (map[string]any, error)) {
	t.Helper()
	prev := resolveNousCredentials
	resolveNousCredentials = fn
	t.Cleanup(func() { resolveNousCredentials = prev })
}

func mustNotHitStore(t *testing.T) func(provider.PoolEnv, provider.NousResolveOptions) (map[string]any, error) {
	return func(provider.PoolEnv, provider.NousResolveOptions) (map[string]any, error) {
		t.Fatal("must not hit the store")
		return nil, nil
	}
}

func TestKeyFarFromExpiryIsLeftAloneWithoutTouchingTheStore(t *testing.T) {
	a, constructed := nousAdoptionAgent(t, nousJWT(nowSeconds()+3000, "acct-A"))
	installNousResolver(t, mustNotHitStore(t))
	if a.adoptNousKeyBeforeExpiry(t.Context()) || len(*constructed) != 0 {
		t.Fatal("far key adopted")
	}
}

func TestKeyInsideTheSkewAdoptsTheStoresFreshKeyWithoutForcingARefresh(t *testing.T) {
	a, constructed := nousAdoptionAgent(t, nousJWT(nowSeconds()+60, "acct-A"))
	fresh := nousJWT(nowSeconds()+3600, "acct-A")
	var calls []provider.NousResolveOptions
	installNousResolver(t, func(_ provider.PoolEnv, opts provider.NousResolveOptions) (map[string]any, error) {
		calls = append(calls, opts)
		return map[string]any{"api_key": fresh, "base_url": a.baseURL}, nil
	})
	if !a.adoptNousKeyBeforeExpiry(t.Context()) {
		t.Fatal("fresh key not adopted")
	}
	if len(calls) != 1 || calls[0].ForceRefresh {
		t.Fatalf("resolver calls %+v", calls)
	}
	if a.apiKey != fresh || a.clientKwargs["api_key"] != fresh || len(*constructed) != 1 {
		t.Fatalf("adopted %q kwargs %v rebuilt %d", a.apiKey, a.clientKwargs, len(*constructed))
	}
}

func TestAFreshKeyForADifferentAccountIsNeverAdopted(t *testing.T) {
	a, constructed := nousAdoptionAgent(t, nousJWT(nowSeconds()+60, "acct-A"))
	other := nousJWT(nowSeconds()+3600, "acct-B")
	installNousResolver(t, func(provider.PoolEnv, provider.NousResolveOptions) (map[string]any, error) {
		return map[string]any{"api_key": other, "base_url": a.baseURL}, nil
	})
	if a.adoptNousKeyBeforeExpiry(t.Context()) || len(*constructed) != 0 || a.apiKey == other {
		t.Fatal("other account adopted")
	}
}

func TestAKeyWithoutAnAccountClaimIsLeftAloneProactively(t *testing.T) {
	a, _ := nousAdoptionAgent(t, nousJWT(nowSeconds()+60, ""))
	installNousResolver(t, mustNotHitStore(t))
	if a.adoptNousKeyBeforeExpiry(t.Context()) {
		t.Fatal("subless key adopted")
	}
}

func TestSameKeyBackFromTheStoreIsNotReadopted(t *testing.T) {
	key := nousJWT(nowSeconds()+60, "acct-A")
	a, constructed := nousAdoptionAgent(t, key)
	installNousResolver(t, func(provider.PoolEnv, provider.NousResolveOptions) (map[string]any, error) {
		return map[string]any{"api_key": key, "base_url": a.baseURL}, nil
	})
	if a.adoptNousKeyBeforeExpiry(t.Context()) || len(*constructed) != 0 {
		t.Fatal("same key readopted")
	}
}
