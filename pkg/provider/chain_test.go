package provider

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/config"
)

func ident(provider, model, baseURL string) BackendIdentity {
	return BuildBackendIdentity(provider, model, baseURL)
}

func withFirstClass(t *testing.T, names ...string) {
	t.Helper()
	old := providerIsFirstClass
	set := map[string]bool{}
	for _, n := range names {
		set[n] = true
	}
	providerIsFirstClass = func(p string) bool { return set[p] }
	t.Cleanup(func() { providerIsFirstClass = old })
}

func TestSameDeployment(t *testing.T) {
	failed := ident("custom", "zai-org/glm-5.2", "")
	sibling := ident("custom", "mindai/macaron-v1-venti", "")
	if SameDeployment(sibling, failed) || ShouldSkipCandidate(sibling, failed, ScopeModel) {
		t.Fatal("sibling models are independent deployments")
	}
	a := ident("custom", "qwen-3", "http://box1:1234/v1")
	b := ident("custom", "qwen-3", "http://box2:1234/v1")
	if SameDeployment(a, b) || ShouldSkipCandidate(a, b, ScopeModel) {
		t.Fatal("same model on two explicit urls is a pool")
	}
	a = ident("claude-cli", "claude-opus-4.7", "http://127.0.0.1:7891/v1")
	b = ident("claude-cli-alt", "claude-opus-4.7", "http://127.0.0.1:7891/v1")
	if !SameDeployment(a, b) {
		t.Fatal("shim aliases at one url with one model are the same deployment")
	}
	withFirstClass(t, "xai", "xai-oauth")
	a = ident("xai-oauth", "grok-4.5", "https://api.x.ai/v1")
	b = ident("xai", "grok-4.5", "https://api.x.ai/v1")
	if SameDeployment(a, b) || ShouldSkipCandidate(a, b, ScopeModel) {
		t.Fatal("first class pair on one host is not the same")
	}
}

func TestSameCredentialSurface(t *testing.T) {
	failed := ident("custom", "zai-org/glm-5.2", "")
	sibling := ident("custom", "mindai/macaron-v1-venti", "")
	if !SameCredentialSurface(sibling, failed) || !ShouldSkipCandidate(sibling, failed, ScopeCredential) {
		t.Fatal("same provider label shares the credential")
	}
	withFirstClass(t, "xai", "xai-oauth")
	a := ident("xai", "grok-4.5", "https://api.x.ai/v1")
	b := ident("xai-oauth", "grok-4.5", "https://api.x.ai/v1")
	if SameCredentialSurface(a, b) || ShouldSkipCandidate(a, b, ScopeCredential) {
		t.Fatal("distinct first class providers differ")
	}
	if SameCredentialSurface(ident("proxy-a", "m", "http://gw:9000/v1"), ident("proxy-b", "m", "http://gw:9000/v1")) {
		t.Fatal("distinct custom labels on one url are not assumed shared")
	}
}

func TestSameEndpoint(t *testing.T) {
	a := ident("a", "m1", "http://host:8000/v1/")
	b := ident("b", "m2", "http://HOST:8000/v1")
	if !SameEndpoint(a, b) || !ShouldSkipCandidate(a, b, ScopeEndpoint) {
		t.Fatal("trailing slash and host case are the same endpoint")
	}
	failed := ident("custom", "m", "https://Example.test/API/v1?token=AbC")
	if ShouldSkipCandidate(ident("custom", "m", "https://example.test/api/v1?token=AbC"), failed, ScopeEndpoint) {
		t.Fatal("path case distinguishes endpoints")
	}
	if ShouldSkipCandidate(ident("custom", "m", "https://example.test/API/v1?token=abc"), failed, ScopeEndpoint) {
		t.Fatal("query case distinguishes endpoints")
	}
	if SameEndpoint(ident("a", "m", "http://h1/v1"), ident("a2", "m", "http://h2/v1")) {
		t.Fatal("different urls differ")
	}
	if !SameEndpoint(ident("openrouter", "m1", ""), ident("openrouter", "m2", "")) || SameEndpoint(ident("openrouter", "m", ""), ident("nous", "m", "")) {
		t.Fatal("unknown url falls back to the provider default")
	}
}

func TestUnknownAxesNeverStrand(t *testing.T) {
	failed := ident("custom", "glm", "http://h/v1")
	for _, scope := range FailureScopes {
		if ShouldSkipCandidate(ident("", "", ""), failed, scope) {
			t.Fatalf("empty candidate skipped under %s", scope)
		}
	}
	if ShouldSkipCandidate(ident("custom", "some-model", ""), ident("custom", "", ""), ScopeModel) {
		t.Fatal("model scope requires model evidence")
	}
}

func writeConfig(t *testing.T, h string, doc map[string]any) {
	t.Helper()
	if err := config.SaveUser(context.Background(), h, doc); err != nil {
		t.Fatal(err)
	}
}

func readConfig(t *testing.T, h string) map[string]any {
	t.Helper()
	doc, err := config.ReadUser(h)
	if err != nil {
		t.Fatal(err)
	}
	return doc
}

func TestChainSkipsSameBackendAndUnusableEntries(t *testing.T) {
	h := t.TempDir()
	primary := Route{Provider: "openrouter", Model: "gpt-5.4", BaseURL: "https://openrouter.ai/api/v1"}
	store := NewStore(h)
	pools := map[string]PoolProbe{"zai": &fakePool{provider: "zai", entries: 1, available: false, nextAt: time.Now().Add(time.Hour), hasNext: true}}
	chain := NewChain(ChainOptions{Primary: primary, Store: store, LoadPool: func(ctx context.Context, provider string) PoolProbe { return pools[provider] },
		Fallbacks: []Route{
			{Provider: "openrouter", Model: "gpt-5.4", BaseURL: "https://openrouter.ai/api/v1/"},
			{Provider: "nous", Model: "Hermes-4"},
			{Provider: "zai", Model: "glm-5.2"},
			{Provider: "", Model: "x"},
			{Provider: "anthropic", Model: "claude-sonnet-4-6"},
		}})
	if !chain.TryActivateFallback(context.Background(), ReasonServerError, nil) {
		t.Fatal("expected the anthropic entry to activate")
	}
	if cur := chain.Current(); cur.Provider != "anthropic" || chain.Index() != 5 {
		t.Fatalf("current %+v index %d", cur, chain.Index())
	}
	if chain.TryActivateFallback(context.Background(), ReasonServerError, nil) {
		t.Fatal("chain exhausted")
	}
	if !chain.Cooldown().Active() {
		t.Fatal("non rate limit exhaustion arms the short cooldown")
	}
}

func TestChainRotateImplementsRotator(t *testing.T) {
	primary := Route{Provider: "openrouter", Model: "gpt-5.4"}
	chain := NewChain(ChainOptions{Primary: primary, Fallbacks: []Route{{Provider: "openai", Model: "gpt-4o"}}})
	var rotator Rotator = chain
	next, ok := rotator.Rotate(context.Background(), primary, &APIError{StatusCode: 429, Message: "rate limited", Kind: ErrRateLimit, Provider: "openrouter", Model: "gpt-5.4"})
	if !ok || next.Provider != "openai" {
		t.Fatalf("rotate %+v %v", next, ok)
	}
	if _, ok := rotator.Rotate(context.Background(), next, &APIError{StatusCode: 500}); ok {
		t.Fatal("exhausted chain must not rotate")
	}
	notices := chain.TakePendingNotices()
	if len(notices) != 1 || !strings.Contains(notices[0], "⚠️ Model fallback: gpt-5.4 via openrouter unavailable (rate limit); using "+NormalizeModelForProvider("gpt-4o", "openai")+" via openai.") {
		t.Fatalf("notices %q", notices)
	}
	if len(chain.TakePendingNotices()) != 0 {
		t.Fatal("notices must be cleared once taken")
	}
}

func TestRestorePrimaryGatesOnPoolReset(t *testing.T) {
	now := time.Unix(5000, 0)
	primary := Route{Provider: "openai-codex", Model: "gpt-5.5"}
	pool := &fakePool{provider: "openai-codex", entries: 1, available: false, nextAt: now.Add(time.Hour), hasNext: true}
	restores := 0
	chain := NewChain(ChainOptions{Primary: primary, Fallbacks: []Route{{Provider: "zai", Model: "glm-5.2"}}, Pool: pool, Now: func() time.Time { return now },
		LoadPool: func(ctx context.Context, provider string) PoolProbe {
			if provider == "openai-codex" {
				return pool
			}
			return nil
		},
		OnRestore: func(ctx context.Context, r Route, p PoolProbe) error { restores++; return nil }})
	chain.Cooldown().now = func() time.Time { return now }
	if !chain.TryActivateFallback(context.Background(), ReasonRateLimit, nil) {
		t.Fatal("activation")
	}
	chain.Cooldown().Clear()
	if _, restored := chain.RestorePrimary(context.Background()); restored || restores != 0 {
		t.Fatal("a future pool reset must block the restore")
	}
	pool.lifted = true
	chain.codexReopenChecked = false
	if _, restored := chain.RestorePrimary(context.Background()); restored {
		t.Fatal("the lifted cooldown clears the primary cooldown but the pool reset still gates")
	}
	pool.hasNext = false
	chain.Cooldown().Clear()
	if _, restored := chain.RestorePrimary(context.Background()); !restored || restores != 1 || chain.Activated() {
		t.Fatalf("restore expected: %v %d", restored, restores)
	}
}

func TestFallbackReasonTextNamesTheFailure(t *testing.T) {
	if FallbackReasonText(ReasonBilling) != "billing or quota exhausted" || FallbackReasonText("custom_thing") != "custom thing" || FallbackReasonText("") != "provider failure" {
		t.Fatal("reason text")
	}
}
