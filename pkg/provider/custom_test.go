package provider

import (
	"context"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestFindCustomProviderIdentity(t *testing.T) {
	cfg := map[string]any{"providers": map[string]any{"mimo-v2.5-pro": map[string]any{"name": "MiMo v2.5 Pro", "base_url": "https://api.mimo.example/v1"}}}
	if got := FindCustomProviderIdentity("https://api.mimo.example/v1", cfg["providers"].(map[string]any)); got != "custom:mimo-v2.5-pro" {
		t.Fatalf("by display name: %q", got)
	}
	cfg = map[string]any{"providers": map[string]any{"local": map[string]any{"base_url": "http://127.0.0.1:8000/v1"}}}
	if got := FindCustomProviderIdentity("http://127.0.0.1:8000/v1", cfg["providers"].(map[string]any)); got != "custom:local" {
		t.Fatalf("providers by key: %q", got)
	}
	cfg = map[string]any{"providers": map[string]any{"local-127.0.0.1:8000": map[string]any{"name": "Local Ollama", "base_url": "http://127.0.0.1:8000/v1"}}}
	slug := FindCustomProviderIdentity("http://127.0.0.1:8000/v1", cfg["providers"].(map[string]any))
	if slug != "custom:local-127.0.0.1:8000" {
		t.Fatalf("stable key not display name: %q", slug)
	}
	r := testResolver(t, cfg, &fakeCreds{}, "")
	if entry := r.getNamedCustomProvider(slug, r.providers(), true); entry == nil || entry["name"] != "Local Ollama" {
		t.Fatalf("slug resolves back: %v", entry)
	}
	cfg = map[string]any{"providers": map[string]any{"local": map[string]any{"name": "local", "base_url": "http://Localhost:8000/v1/"}}}
	if got := FindCustomProviderIdentity("http://localhost:8000/v1", cfg["providers"].(map[string]any)); got != "custom:local" {
		t.Fatalf("trailing slash and case: %q", got)
	}
	cfg = map[string]any{"providers": map[string]any{"other": map[string]any{"name": "other", "base_url": "https://elsewhere.example/v1"}}}
	if got := FindCustomProviderIdentity("https://api.mimo.example/v1", cfg["providers"].(map[string]any)); got != "" {
		t.Fatalf("no match: %q", got)
	}
	if got := FindCustomProviderIdentity("", map[string]any{"x": map[string]any{"name": "x"}}); got != "" {
		t.Fatalf("empty url: %q", got)
	}
	cfg = map[string]any{"providers": map[string]any{"mimo-v2.5-pro": map[string]any{"name": "mimo-v2.5-pro", "base_url": "https://api.mimo.example/v1", "api_key": "sk-entry"}}}
	r = testResolver(t, cfg, &fakeCreds{}, "")
	slug = FindCustomProviderIdentity("https://api.mimo.example/v1", cfg["providers"].(map[string]any))
	if entry := r.getNamedCustomProvider(slug, r.providers(), true); slug != "custom:mimo-v2.5-pro" || entry == nil || entry["api_key"] != "sk-entry" {
		t.Fatalf("round trip: %q %v", slug, entry)
	}
	cfg = map[string]any{"providers": map[string]any{
		"alpha": map[string]any{"base_url": "https://alpha.invalid/v1", "key_env": "ICEBATH_CUSTOM_ALPHA_API_KEY"},
		"beta":  map[string]any{"base_url": "https://beta.invalid/v1", "api_key": "beta-inline-key"},
	}}
	r = testResolver(t, cfg, &fakeCreds{}, "")
	if entry := r.getNamedCustomProvider("beta", r.providers(), true); entry == nil || entry["name"] != "beta" || entry["api_key"] != "beta-inline-key" {
		t.Fatalf("beta: %v", entry)
	}
}

func TestCanonicalCustomIdentity(t *testing.T) {
	const providerKey, displayName, baseURL, model = "my-endpoint", "My Endpoint Display", "https://example.invalid/v1", "cool-model-1"
	cfg := map[string]any{"providers": map[string]any{providerKey: map[string]any{"name": displayName, "base_url": baseURL, "api_key": "sk-test", "default_model": model, "models": []any{model}}}}
	testResolver(t, cfg, &fakeCreds{}, "")
	ctx := WithFrozenRuntimeConfig(context.Background(), cfg)
	canonical := "custom:" + providerKey
	if got := CanonicalCustomIdentity(ctx, "", displayName, "", ""); got != canonical {
		t.Fatalf("display name heals: %q", got)
	}
	cfgWithModel := map[string]any{"providers": cfg["providers"], "model": map[string]any{"provider": displayName}}
	if got := CanonicalCustomIdentity(WithFrozenRuntimeConfig(context.Background(), cfgWithModel), "", "", "", ""); got != canonical {
		t.Fatalf("config model provider heals: %q", got)
	}
	if got := CanonicalCustomIdentity(ctx, "", providerKey, "", ""); got != canonical {
		t.Fatalf("config key spelling: %q", got)
	}
	byURL := CanonicalCustomIdentity(ctx, baseURL, "", "", "")
	byModel := CanonicalCustomIdentity(ctx, "", "", model, "")
	if byURL != canonical || byModel != canonical {
		t.Fatalf("sources disagree: %q %q", byURL, byModel)
	}
	if got := CanonicalCustomIdentity(ctx, "", "not-a-configured-entry", "", ""); got != "" {
		t.Fatalf("unconfigured: %q", got)
	}
	legacy := map[string]any{"providers": map[string]any{"legacy-endpoint": map[string]any{"name": "Legacy Endpoint", "base_url": "https://legacy.invalid/v1", "api_key": "sk-legacy", "models": []any{"legacy-model"}}}}
	if got := CanonicalCustomIdentity(WithFrozenRuntimeConfig(context.Background(), legacy), "", "Legacy Endpoint", "", ""); got != "custom:legacy-endpoint" {
		t.Fatalf("display name of another entry: %q", got)
	}
}

func TestIsRoutableProvider(t *testing.T) {
	cfg := map[string]any{"providers": map[string]any{"my-endpoint": map[string]any{"name": "My Endpoint Display", "base_url": "https://example.invalid/v1", "api_key": "sk-test"}}}
	r := testResolver(t, cfg, &fakeCreds{}, "")
	for _, name := range []string{"", "auto", "openrouter", "my-endpoint", "custom:my-endpoint"} {
		if !r.IsRoutableProvider(name) {
			t.Errorf("%q should be routable", name)
		}
	}
	for _, name := range []string{"custom", "stale-endpoint", "custom:stale-endpoint"} {
		if r.IsRoutableProvider(name) {
			t.Errorf("%q should not be routable", name)
		}
	}
	legacy := map[string]any{"providers": map[string]any{"legacy-endpoint": map[string]any{"name": "Legacy Endpoint", "base_url": "https://legacy.invalid/v1", "api_key": "sk-legacy"}}}
	r = testResolver(t, legacy, &fakeCreds{}, "")
	for _, name := range []string{"legacy-endpoint", "custom:legacy-endpoint", "Legacy Endpoint"} {
		if !r.IsRoutableProvider(name) {
			t.Errorf("%q should be routable by key and display name", name)
		}
	}
}

func TestSessionProviderIdentityAndRoutedIdentity(t *testing.T) {
	cfg := map[string]any{"providers": map[string]any{
		"a": map[string]any{"base_url": "https://gw.example/v1", "models": []any{"m1"}},
		"b": map[string]any{"base_url": "https://gw.example/v1", "models": []any{"m2"}},
	}}
	testResolver(t, cfg, &fakeCreds{}, "")
	ctx := WithFrozenRuntimeConfig(context.Background(), cfg)
	if got := SessionProviderIdentity(ctx, "custom", "custom:b", "https://gw.example/v1", ""); got != "custom:b" {
		t.Fatalf("requested entry still serving the url: %q", got)
	}
	if got := SessionProviderIdentity(ctx, "custom", "custom:zzz", "https://gw.example/v1", ""); got != "custom:a" {
		t.Fatalf("endpoint owner: %q", got)
	}
	if got := SessionProviderIdentity(ctx, "deepseek", "", "", ""); got != "deepseek" {
		t.Fatalf("builtin adapter: %q", got)
	}
	if got := RoutedCustomIdentity(ctx, "https://gw.example/v1", "m2", ""); got != "custom:b" {
		t.Fatalf("model on url: %q", got)
	}
	if got := RoutedCustomIdentity(ctx, "", "m1", ""); got != "custom:a" {
		t.Fatalf("model alone: %q", got)
	}
	if got := RoutedCustomIdentity(ctx, "https://gw.example/v1", "", ""); got != "custom:a" {
		t.Fatalf("first on url: %q", got)
	}
	if got := FindCustomProviderIdentityByModel("m2", cfg["providers"].(map[string]any)); got != "custom:b" {
		t.Fatalf("by model: %q", got)
	}
}

func TestCustomProviderSlugAndAliases(t *testing.T) {
	if CustomProviderSlug("My Endpoint", "") != "custom:my-endpoint" || CustomProviderSlug("display", "Key Name") != "custom:key-name" || CustomProviderSlug("custom:x", "") != "custom:x" {
		t.Fatal("slug")
	}
	aliases := CustomProviderAliases("My Endpoint", "custom:mykey")
	for _, a := range []string{"my endpoint", "my-endpoint", "custom:my-endpoint", "custom:mykey", "mykey", "custom:custom:mykey"} {
		if !aliases[a] {
			t.Errorf("alias %q missing from %v", a, aliases)
		}
	}
	userProviders := map[string]any{"gw": map[string]any{"base_url": "https://gw.example/v1", "key_env": "K"}, "bedrock": map[string]any{"stale_timeout_seconds": 600}}
	if p := resolveUserProvider("gw", userProviders); p == nil || p.ID != "gw" || p.BaseURL != "https://gw.example/v1" || !reflect.DeepEqual(p.KeyEnv, []string{"K"}) || p.Transport != "openai_chat" {
		t.Fatalf("user provider: %+v", p)
	}
	if resolveUserProvider("bedrock", userProviders) != nil {
		t.Fatal("tuning block is not a custom endpoint")
	}
	list := []any{map[string]any{"name": "First", "base_url": "https://f/v1"}, map[string]any{"name": "Second", "base_url": "https://s/v1", "provider_key": "sec"}}
	if p := resolveCustomProvider("custom", list); p == nil || p.Name != "First" {
		t.Fatalf("bare custom self heals: %+v", p)
	}
	if p := resolveCustomProvider("sec", list); p == nil || p.ID != "custom:sec" {
		t.Fatalf("provider key alias: %+v", p)
	}
	if p := ResolveCustomProvider("Second", list); p == nil || p.BaseURL != "https://s/v1" {
		t.Fatalf("exported resolver: %+v", p)
	}
}

func TestCustomEntryOwnership(t *testing.T) {
	cfg := map[string]any{"providers": map[string]any{"deepseek": map[string]any{"base_url": "http://127.0.0.1:18080/v1", "key_env": "ENTRY_KEY_VAR"}, "plain": map[string]any{"base_url": "http://127.0.0.1:1/v1"}}}
	if !EntryDeclaresCredential(map[string]any{"key_cmd": "op read x"}) || EntryDeclaresCredential(map[string]any{"base_url": "u"}) || !EntryDeclaresCredential(map[string]any{"declares_credential": true}) {
		t.Fatal("declares credential")
	}
	match := customEntryForEndpoint("http://127.0.0.1:18080/v1", "custom:deepseek", cfg["providers"].(map[string]any))
	if match == nil || match.name != "deepseek" {
		t.Fatalf("by name: %+v", match)
	}
	if customEntryForEndpoint("http://other/v1", "custom:deepseek", cfg["providers"].(map[string]any)) != nil {
		t.Fatal("named entry never serves another url")
	}
	if match := customEntryForEndpoint("http://127.0.0.1:1/v1", "", cfg["providers"].(map[string]any)); match == nil || match.name != "plain" {
		t.Fatalf("by url: %+v", match)
	}
	if keys := customProviderPoolKeyCandidates("http://127.0.0.1:18080/v1", "", cfg["providers"].(map[string]any)); !reflect.DeepEqual(keys, []string{"deepseek", "custom:deepseek"}) {
		t.Fatalf("pool keys: %v", keys)
	}
}

func TestCommandTokenSource(t *testing.T) {
	calls := 0
	src := BuildCommandTokenSource("echo tok", "lbl", "", nil)
	now := time.Now()
	src.now = func() time.Time { return now }
	src.run = func(ctx context.Context, command string, env []string) ([]byte, int, error) {
		calls++
		return []byte(`{"access_token":"jwt-1","expires_in":120}`), 0, nil
	}
	tok, err := src.Token(context.Background())
	if err != nil || tok != "jwt-1" {
		t.Fatalf("got %q %v", tok, err)
	}
	if tok, _ := src.Token(context.Background()); tok != "jwt-1" || calls != 1 {
		t.Fatalf("cached: %q %d", tok, calls)
	}
	now = now.Add(61 * time.Second)
	if _, _ = src.Token(context.Background()); calls != 2 {
		t.Fatalf("re-mint after leeway: %d", calls)
	}
	src.run = func(ctx context.Context, command string, env []string) ([]byte, int, error) {
		return []byte("a\nb\n"), 0, nil
	}
	now = now.Add(time.Hour)
	if _, err := src.Token(context.Background()); err == nil || !strings.Contains(err.Error(), "printed multiple lines") {
		t.Fatalf("multi line: %v", err)
	}
	src.run = func(ctx context.Context, command string, env []string) ([]byte, int, error) {
		return nil, 3, context.Canceled
	}
	if _, err := src.Token(context.Background()); err == nil || !strings.Contains(err.Error(), "exited 3") || strings.Contains(err.Error(), "echo tok") {
		t.Fatalf("exit code without command text: %v", err)
	}
	src.run = func(ctx context.Context, command string, env []string) ([]byte, int, error) {
		return []byte("   "), 0, nil
	}
	if _, err := src.Token(context.Background()); err == nil || !strings.Contains(err.Error(), "produced no output") {
		t.Fatalf("no output: %v", err)
	}
	src.run = func(ctx context.Context, command string, env []string) ([]byte, int, error) {
		return []byte(`{"token":"x"}`), 0, nil
	}
	if _, err := src.Token(context.Background()); err == nil || !strings.Contains(err.Error(), "without an 'access_token'") {
		t.Fatalf("json without access_token: %v", err)
	}
	if BuildCommandTokenSource("  ", "x", "", nil) != nil || src.CacheIdentity() != "cmd:echo tok" {
		t.Fatal("empty command and identity")
	}
	real := BuildCommandTokenSource(printTokenCmd("bare-token"), "x", "", nil)
	if tok, err := real.Token(context.Background()); err != nil || tok != "bare-token" {
		t.Fatalf("real shell: %q %v", tok, err)
	}
}

func TestNamedEntryKeyCmdMintsPerRequest(t *testing.T) {
	cfg := map[string]any{"providers": map[string]any{"gw": map[string]any{"base_url": "https://gw.example/v1", "key_cmd": printTokenCmd("minted-token"), "api_key": "static-ignored"}}}
	r := testResolver(t, cfg, &fakeCreds{}, "")
	rt := mustResolve(t, r, "custom:gw", "", "", "")
	if rt.TokenSource == nil || rt.APIKey != "" || rt.KeyCmd != printTokenCmd("minted-token") {
		t.Fatalf("got %+v", rt)
	}
	if tok, err := rt.TokenSource(context.Background()); err != nil || tok != "minted-token" {
		t.Fatalf("mint: %q %v", tok, err)
	}
	if err := raiseForCredentiallessBareCustom("custom", rt); err != nil {
		t.Fatalf("token source counts as a credential: %v", err)
	}
}

func TestCustomOpencodeFamilyRuntime(t *testing.T) {
	mk := func(provider, extra string, def string) map[string]any {
		entry := map[string]any{"base_url": "https://opencode.ai/zen/go/v1", "key_env": "OPENCODE_GO_BRIDGE_API_KEY"}
		if extra != "" {
			entry["api_mode"] = extra
		}
		return map[string]any{"model": map[string]any{"provider": provider, "default": def}, "providers": map[string]any{provider: entry}}
	}
	dotenv := "OPENCODE_GO_BRIDGE_API_KEY=sk-test-bridge\n"
	r := testResolver(t, mk("opencode-go-bridge", "", "grok-4.5"), &fakeCreds{}, dotenv)
	rt := mustResolve(t, r, "opencode-go-bridge", "", "", "grok-4.5")
	if rt.APIMode != ModeCodexResponses || !strings.HasSuffix(strings.TrimRight(rt.BaseURL, "/"), "/zen/go/v1") {
		t.Fatalf("grok: %+v", rt)
	}
	rt = mustResolve(t, r, "opencode-go-bridge", "", "", "minimax-m2.7")
	if rt.APIMode != ModeAnthropicMessages || !strings.HasSuffix(strings.TrimRight(rt.BaseURL, "/"), "/zen/go") {
		t.Fatalf("minimax: %+v", rt)
	}
	rt = mustResolve(t, r, "opencode-go-bridge", "", "", "deepseek-v4-flash")
	if rt.APIMode != ModeChatCompletions || !strings.HasSuffix(strings.TrimRight(rt.BaseURL, "/"), "/zen/go/v1") {
		t.Fatalf("chat: %+v", rt)
	}
	r = testResolver(t, mk("opencode-go-bridge", "chat_completions", "grok-4.5"), &fakeCreds{}, dotenv)
	if rt := mustResolve(t, r, "opencode-go-bridge", "", "", "grok-4.5"); rt.APIMode != ModeChatCompletions {
		t.Fatalf("explicit transport wins: %+v", rt)
	}
	r = testResolver(t, mk("my-oc-proxy", "", "gpt-5.6-luna"), &fakeCreds{}, dotenv)
	if rt := mustResolve(t, r, "my-oc-proxy", "", "", "gpt-5.6-luna"); rt.APIMode != ModeCodexResponses {
		t.Fatalf("host derived family: %+v", rt)
	}
}

func TestConfigPathsAndCodexModelProviderID(t *testing.T) {
	cfg := map[string]any{
		"providers": map[string]any{"gw": map[string]any{"base_url": "https://gw.example/v1", "name": "Gateway"}},
	}
	r := testResolver(t, cfg, &fakeCreds{}, "")
	if p := r.NamedCustomProviderConfigPath("custom:gw", cfg["providers"].(map[string]any)); p == nil || p.Section != "providers" || p.Key != "gw" {
		t.Fatalf("providers path: %+v", p)
	}
	if p := CustomProviderConfigPathForURL(WithFrozenRuntimeConfig(context.Background(), cfg), "https://gw.example/v1/"); p == nil || p.Section != "providers" || p.Key != "gw" {
		t.Fatalf("url path: %+v", p)
	}
	if got := r.CodexModelProviderID("custom:gw"); got != "gw" {
		t.Fatalf("codex id: %q", got)
	}
	if got := r.CodexModelProviderID("custom"); got != "" {
		t.Fatalf("bare custom has no id: %q", got)
	}
	if !r.HasNamedCustomProvider("Gateway") || r.HasNamedCustomProvider("nobody") {
		t.Fatal("has named custom provider")
	}
	if p := r.NamedCustomProviderConfigPath("DeepSeek", map[string]any{"deepseek": map[string]any{"base_url": "https://x/v1"}}); p == nil || p.Section != "providers" || p.Key != "deepseek" {
		t.Fatalf("a builtin name means its same keyed entry that declares an endpoint: %+v", p)
	}
	if p := r.NamedCustomProviderConfigPath("deepseek", map[string]any{"deepseek": map[string]any{"request_timeout_seconds": 30}, "other": map[string]any{"name": "deepseek", "base_url": "https://x/v1"}}); p != nil {
		t.Fatalf("a tuning only block keeps the builtin and a display name never claims it: %+v", p)
	}
}

func TestExpandDirectAPIAlias(t *testing.T) {
	r := testResolver(t, nil, &fakeCreds{}, "")
	if p, b := r.expandDirectAPIAlias("openai", ""); p != "custom" || b != OpenAIBaseURL {
		t.Fatalf("got %q %q", p, b)
	}
	t.Setenv("OPENAI_BASE_URL", "https://proxy.example/v1/")
	if p, b := r.expandDirectAPIAlias("openai", ""); p != "custom" || b != "https://proxy.example/v1" {
		t.Fatalf("env base: %q %q", p, b)
	}
	if p, b := r.expandDirectAPIAlias("openai", "https://mine/v1"); p != "custom" || b != "https://mine/v1" {
		t.Fatalf("existing base: %q %q", p, b)
	}
	if p, b := r.expandDirectAPIAlias("deepseek", ""); p != "deepseek" || b != "" {
		t.Fatalf("non alias: %q %q", p, b)
	}
	r = testResolver(t, map[string]any{"providers": map[string]any{"openai": map[string]any{"base_url": "https://named/v1"}}}, &fakeCreds{}, "")
	if p, _ := r.expandDirectAPIAlias("openai", ""); p != "openai" {
		t.Fatalf("named entry keeps the name: %q", p)
	}
}

func TestLlamacppDeadServerRaisesFriendlyError(t *testing.T) {
	r := testResolver(t, map[string]any{"local_runtime": map[string]any{"enabled": true}}, &fakeCreds{}, "")
	_, err := r.ResolveRuntime("llamacpp", "", "", nil, "")
	var lr *LocalRuntimeUnavailableError
	if err == nil {
		t.Fatal("expected error")
	}
	if e, ok := err.(*LocalRuntimeUnavailableError); !ok || !e.Retryable || e.ErrorCode() != "local_runtime_unavailable" {
		t.Fatalf("got %T %v", err, err)
	} else {
		lr = e
	}
	if !strings.Contains(lr.Message, "isn't running") {
		t.Fatalf("message: %s", lr.Message)
	}
	r = testResolver(t, nil, &fakeCreds{}, "")
	if _, err := r.ResolveRuntime("llamacpp", "", "", nil, ""); err == nil || !strings.Contains(err.Error(), "turned off") {
		t.Fatalf("disabled: %v", err)
	}
	r = testResolver(t, nil, &fakeCreds{}, "")
	if rt := mustResolve(t, r, "llamacpp", "", "http://127.0.0.1:9999/v1", ""); rt.Provider != "custom" || rt.BaseURL != "http://127.0.0.1:9999/v1" {
		t.Fatalf("explicit url wins: %+v", rt)
	}
}

func TestNamedCustomProviderExportResolvesTheConfiguredEntry(t *testing.T) {
	h := runtimeConfigHome(t, "model:\n  default: m\nproviders:\n  local-box:\n    name: Local Box\n    base_url: http://10.0.0.5:8000/v1\n    api_key: sk-box\n")
	ctx := context.Background()
	entry, err := NamedCustomProvider(ctx, h, "local-box")
	if err != nil || entry == nil || entry["base_url"] != "http://10.0.0.5:8000/v1" || entry["api_key"] != "sk-box" {
		t.Fatalf("entry %v err %v", entry, err)
	}
	if entry, err := NamedCustomProvider(ctx, h, "nowhere"); err != nil || entry != nil {
		t.Fatalf("unknown entry %v err %v", entry, err)
	}
	bad := runtimeConfigHome(t, "model: [unclosed\n")
	if entry, err := NamedCustomProvider(ctx, bad, "local-box"); err != nil || entry != nil {
		t.Fatalf("malformed config runs on defaults, entry %v err %v", entry, err)
	}
}

func TestNamedCustomProviderEntry(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	cfg := map[string]any{"providers": map[string]any{"team": map[string]any{"base_url": "http://127.0.0.1:8001/v1", "api_key": "sk-team"}}}
	entry := NamedCustomProviderEntry(WithFrozenRuntimeConfig(context.Background(), cfg), "custom:team")
	if entry == nil || entry["base_url"] != "http://127.0.0.1:8001/v1" {
		t.Fatalf("entry %v", entry)
	}
	if got := NamedCustomProviderEntry(WithFrozenRuntimeConfig(context.Background(), cfg), "openrouter"); got != nil {
		t.Fatalf("unknown name matched %v", got)
	}
}

func TestEntryURLMatchFormAndNamedCustomProviderPath(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	if got := EntryURL(map[string]any{"base_url": "http://b/v1"}); got != "http://b/v1" {
		t.Fatalf("entry url %q", got)
	}
	if got := NormalizeBaseURLForMatch(" HTTP://Host/V1// "); got != "http://host/v1" {
		t.Fatalf("match form %q", got)
	}
	cfg := map[string]any{"providers": map[string]any{"gw": map[string]any{"base_url": "http://x/v1"}}}
	if p := NamedCustomProviderPath(WithFrozenRuntimeConfig(context.Background(), cfg), "custom:gw"); p == nil || p.Section != "providers" || p.Key != "gw" {
		t.Fatalf("named path %+v", p)
	}
}

func TestRouteKeyExport(t *testing.T) {
	if got := RouteKey(" HTTP://Example.COM/V1/ "); got != routeKey(" HTTP://Example.COM/V1/ ") || got == "" {
		t.Fatalf("RouteKey = %q", got)
	}
	if RouteKey(nil) != "" {
		t.Fatal("RouteKey(nil) not empty")
	}
}

func TestExpandDirectAPIAliasReadsTheLiveConfig(t *testing.T) {
	ctx := contextWithTempHome(t)
	t.Setenv("OPENAI_BASE_URL", "")
	if p, b := ExpandDirectAPIAlias(ctx, "openai", ""); p != "custom" || b != OpenAIBaseURL {
		t.Fatalf("got %q %q", p, b)
	}
	if p, b := ExpandDirectAPIAlias(ctx, "openai", "https://mine/v1"); p != "custom" || b != "https://mine/v1" {
		t.Fatalf("existing base: %q %q", p, b)
	}
	if p, b := ExpandDirectAPIAlias(ctx, "deepseek", ""); p != "deepseek" || b != "" {
		t.Fatalf("non alias: %q %q", p, b)
	}
}

func TestCustomEntryForEndpointWrapper(t *testing.T) {
	cfg := map[string]any{"providers": map[string]any{"plain": map[string]any{"base_url": "http://127.0.0.1:1/v1"}}}
	name, entry, ok := CustomEntryForEndpoint(WithFrozenRuntimeConfig(context.Background(), cfg), "http://127.0.0.1:1/v1", "")
	match := customEntryForEndpoint("http://127.0.0.1:1/v1", "", cfg["providers"].(map[string]any))
	if !ok || match == nil || name != match.name || entry["base_url"] != match.entry["base_url"] {
		t.Fatalf("CustomEntryForEndpoint = %q %v %v", name, entry, ok)
	}
	if _, _, ok := CustomEntryForEndpoint(WithFrozenRuntimeConfig(context.Background(), cfg), "", ""); ok {
		t.Fatal("empty base url matched")
	}
}
