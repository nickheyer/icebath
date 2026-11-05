package provider

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/nickheyer/icebath/pkg/home"
)

// Isolated home with an emptied alias table, restored after the test
func switchAliasHome(t *testing.T, configYAML string) (context.Context, string) {
	t.Helper()
	h := newTestHome(t)
	if configYAML != "" {
		writeConfigYAML(t, h, configYAML)
	}
	saved := DirectAliases.Entries()
	directAliasMu.Lock()
	id, ok, loaded := directAliasIdentity, directAliasIdentityOK, directAliasLoaded
	directAliasIdentity, directAliasIdentityOK, directAliasLoaded = "", false, nil
	directAliasMu.Unlock()
	DirectAliases.Clear()
	t.Cleanup(func() {
		DirectAliases.replace(saved)
		directAliasMu.Lock()
		directAliasIdentity, directAliasIdentityOK, directAliasLoaded = id, ok, loaded
		directAliasMu.Unlock()
	})
	return home.WithOverride(context.Background(), h), h
}

func seedDirectAliases(entries ...NamedDirectAlias) {
	DirectAliases.Clear()
	for _, e := range entries {
		DirectAliases.Set(e.Name, e.Alias)
	}
}

func TestProviderFlagAndScopes(t *testing.T) {
	req := ParseModelSwitchArgs("sonnet --provider anthropic --global")
	if req.Target != "sonnet" || req.ExplicitProvider != "anthropic" || !req.IsGlobal || req.Scope != "global" || len(req.Errors) != 0 {
		t.Fatalf("%+v", req)
	}
	for raw, scope := range map[string]string{"sonnet": "default", "sonnet --session": "session", "sonnet \u2013session": "session", "sonnet --once": "once"} {
		if got := ParseModelSwitchArgs(raw).Scope; got != scope {
			t.Fatalf("%q scope %q", raw, got)
		}
	}
	if !ParseModelSwitchArgs("--refresh").ForceRefresh {
		t.Fatal("refresh flag lost")
	}
}

func TestOnceWithGlobalConflict(t *testing.T) {
	req := ParseModelSwitchArgs("sonnet --once --global")
	if !containsString(req.Errors, ModelSwitchErrOnceWithGlobal) ||
		!containsString(req.ErrorMessages(), ModelSwitchErrorText[ModelSwitchErrOnceWithGlobal]) {
		t.Fatalf("%+v", req)
	}
}

func TestResetIsItsOwnRequestAndTakesNoFlags(t *testing.T) {
	if !ParseModelSwitchArgs("  Reset ").IsReset() {
		t.Fatal("reset not recognized")
	}
	for _, raw := range []string{"reset --global", "reset --once", "reset --provider nous", "reset --reasoning high"} {
		req := ParseModelSwitchArgs(raw)
		if !reflect.DeepEqual(req.Errors, []string{ModelSwitchErrResetWithFlags}) || req.IsReset() {
			t.Fatalf("%s %+v", raw, req)
		}
	}
	for _, raw := range []string{"default", "my-reset", "vendor/reset", "vendor:reset"} {
		if ParseModelSwitchArgs(raw).IsReset() {
			t.Fatal(raw)
		}
	}
}

func TestReasoningFlagRidesWithThePickAndValidates(t *testing.T) {
	req := ParseModelSwitchArgs("sonnet --provider anthropic --reasoning high --session")
	if req.Target != "sonnet" || req.ExplicitProvider != "anthropic" || req.ReasoningEffort != "high" || req.Scope != "session" || len(req.Errors) != 0 {
		t.Fatalf("%+v", req)
	}
	if !containsString(ParseModelSwitchArgs("sonnet --reasoning turbo").Errors, ModelSwitchErrBadReasoning) {
		t.Fatal("bad reasoning accepted")
	}
	if got := ParseModelSwitchArgs("sonnet \u2014reasoning low").ReasoningEffort; got != "low" {
		t.Fatal(got)
	}
}

func TestResolvePersistBehavior(t *testing.T) {
	cases := []struct {
		yaml                  string
		global, session, once bool
		explicit              string
		want                  bool
	}{
		{"model:\n  persist_switch_by_default: true\n", false, true, false, "", false},
		{"model:\n  persist_switch_by_default: true\n", false, false, false, "", true},
		{"model:\n  default: agnes-3.0-flash\n  provider: custom:family-snowflake\n  persist_switch_by_default: true\n", false, false, false, "custom:family-xizhao", true},
		{"model:\n  default: agnes-3.0-flash\n  provider: custom:family-snowflake\n  persist_switch_by_default: true\n", false, true, false, "custom:family-xizhao", false},
		{"model:\n  default: agnes-3.0-flash\n  provider: custom:family-snowflake\n  persist_switch_by_default: true\n", false, false, true, "custom:family-xizhao", false},
		{"model:\n  default: agnes-3.0-flash\n  provider: custom:family-snowflake\n  persist_switch_by_default: true\n", true, false, false, "custom:family-xizhao", true},
		{"model:\n  default: agnes-3.0-flash\n  provider: custom:family-snowflake\n", false, false, false, "custom:family-xizhao", false},
		{"model:\n  default: agnes-3.0-flash\n  provider: custom:family-snowflake\n", false, false, false, "", false},
		{"model: {}\n", false, false, false, "anthropic", true},
		{"model: \"\"\n", false, false, false, "", true},
		{"model:\n  default: gpt-5.6\n  provider: openai-codex\n", false, false, false, "openai-api", false},
		{"model:\n  default: gpt-5.6\n  provider: openai-codex\n", false, false, false, "", false},
		{"model:\n  default: gpt-5.6\n  provider: openai-codex\n", true, false, false, "openai-api", true},
		{"model: gpt-5.6\n", false, false, false, "", false},
	}
	for i, c := range cases {
		ctx, _ := switchAliasHome(t, c.yaml)
		if got := ResolvePersistBehavior(ctx, c.global, c.session, c.once, c.explicit); got != c.want {
			t.Fatalf("case %d %q got %v", i, c.yaml, got)
		}
	}
}

func TestCodexOAuthOverridesModelsDev(t *testing.T) {
	prev := displayContextLengthFn
	displayContextLengthFn = func(context.Context, ContextLengthRequest) int { return 272_000 }
	t.Cleanup(func() { displayContextLengthFn = prev })
	n, ok := ResolveDisplayContextLength(context.Background(), DisplayContextRequest{Model: "gpt-5.5", Provider: "openai-codex",
		BaseURL: "https://chatgpt.com/backend-api/codex", ModelInfo: &ModelsDevEntry{ContextWindow: 1_050_000}})
	if !ok || n != 272_000 {
		t.Fatal(n, ok)
	}
}

func TestCustomProvidersOverrideHonored(t *testing.T) {
	ctx, _, _ := metaTest(t)
	n, ok := ResolveDisplayContextLength(ctx, DisplayContextRequest{Model: "gpt-5.5", Provider: "custom",
		BaseURL: "https://example.invalid/v1", APIKey: "k", CustomProviders: []map[string]any{{"name": "my-custom-endpoint",
			"base_url": "https://example.invalid/v1", "models": map[string]any{"gpt-5.5": map[string]any{"context_length": 1_050_000}}}}})
	if !ok || n != 1_050_000 {
		t.Fatal(n, ok)
	}
}

func sortedByKey(t *testing.T, models ...string) []string {
	t.Helper()
	if err := sortModelsByKey(models, "gpt"); err != nil {
		t.Fatal(err)
	}
	return models
}

func TestGpt56SortInvariants(t *testing.T) {
	if got := sortedByKey(t, "gpt-5.6-luna", "gpt-5.6-terra", "gpt-5.6-sol"); got[0] != "gpt-5.6-sol" {
		t.Fatal(got)
	}
	if got := sortedByKey(t, "gpt-5.6-sol-pro", "gpt-5.6-sol"); got[0] != "gpt-5.6-sol" {
		t.Fatal(got)
	}
}

func TestModelGptResolvesFlagshipAcrossGpt6Tiers(t *testing.T) {
	got := sortedByKey(t, "gpt-6-luna", "gpt-5.6-sol", "gpt-6-sol", "gpt-6-astra")
	if got[0] != "gpt-6-astra" || got[1] != "gpt-6-sol" || sortedIndex(got, "gpt-6-luna") > sortedIndex(got, "gpt-5.6-sol") {
		t.Fatal(got)
	}
}

func sortedIndex(list []string, s string) int {
	for i, v := range list {
		if v == s {
			return i
		}
	}
	return -1
}

func TestMatchesRealNousHermesChatModels(t *testing.T) {
	for _, m := range []string{"NousResearch/Hermes-3-Llama-3.1-70B", "NousResearch/Hermes-3-Llama-3.1-405B", "hermes-3", "Hermes-3",
		"hermes-4", "hermes-4-405b", "hermes_4_70b", "openrouter/hermes3:70b", "openrouter/nousresearch/hermes-4-405b", "NousResearch/Hermes3", "hermes-3.1"} {
		if !IsNousHermesNonAgentic(m) || checkHermesModelWarning(m) != hermesModelWarning {
			t.Fatal(m)
		}
	}
	for _, m := range []string{"hermes-brain:qwen3-14b-ctx16k", "qwen3:14b", "claude-opus-4-6", ""} {
		if IsNousHermesNonAgentic(m) || checkHermesModelWarning(m) != "" {
			t.Fatal(m)
		}
	}
}

func TestDirectAliasCredentialLoading(t *testing.T) {
	ctx, _ := switchAliasHome(t, "model:\n  default: gpt-4\n  provider: openrouter\nmodel_aliases:\n  theta:\n    model: theta-1\n"+
		"    provider: custom\n    base_url: https://theta.example.com/v1\n    api_key: sk-literal\n    key_env: THETA_API_KEY\n")
	a, _ := loadDirectAliases(ctx, nil).Get("theta")
	if a.APIKey != "sk-literal" || a.KeyEnv != "THETA_API_KEY" {
		t.Fatalf("%+v", a)
	}
	ctx, _ = switchAliasHome(t, "model:\n  default: gpt-4\n  provider: openrouter\n  aliases:\n    qwen-local:\n      model: qwen3-next\n"+
		"      provider: custom\n      base_url: http://192.168.1.50:8000/v1\n      key_env: QWEN27B_KEY\n    theta-nested:\n"+
		"      model: theta-1\n      provider: custom\n      base_url: https://theta.example.com/v1\n      api_key: sk-literal\n")
	loaded := loadDirectAliases(ctx, nil)
	q, _ := loaded.Get("qwen-local")
	n, _ := loaded.Get("theta-nested")
	if q.KeyEnv != "QWEN27B_KEY" || q.BaseURL != "http://192.168.1.50:8000/v1" || n.APIKey != "sk-literal" {
		t.Fatalf("%+v %+v", q, n)
	}
	ctx, _ = switchAliasHome(t, "model_aliases:\n  mymodel:\n    model: custom-model:latest\n    provider: custom\n    base_url: https://example.com/v1\n"+
		"  empty:\n    model: \"\"\n    provider: custom\n  good:\n    model: real\n    provider: custom\n")
	loaded = loadDirectAliases(ctx, nil)
	m, _ := loaded.Get("mymodel")
	_, hasEmpty := loaded.Get("empty")
	_, hasGood := loaded.Get("good")
	if m != (DirectAlias{Model: "custom-model:latest", Provider: "custom", BaseURL: "https://example.com/v1"}) || hasEmpty || !hasGood {
		t.Fatalf("%+v %v %v", m, hasEmpty, hasGood)
	}
	ctx, _ = switchAliasHome(t, "model:\n  aliases:\n    localqwen:\n      model: qwen3.5:4b\n      provider: custom\n      base_url: http://localhost:11434/v1\n")
	if a, _ := loadDirectAliases(ctx, nil).Get("localqwen"); a != (DirectAlias{Model: "qwen3.5:4b", Provider: "custom", BaseURL: "http://localhost:11434/v1"}) {
		t.Fatalf("%+v", a)
	}
}

func TestDirectAliasAPIKeyPrecedence(t *testing.T) {
	ctx, _ := switchAliasHome(t, "")
	t.Setenv("THETA_API_KEY", "sk-from-env")
	t.Setenv("PRIMARY", "sk-from-template")
	t.Setenv("FALLBACK", "sk-from-key-env")
	host := "https://theta.example.com/v1"
	cases := []struct {
		alias DirectAlias
		want  string
	}{
		{DirectAlias{"theta-1", "custom", host, "sk-literal", ""}, "sk-literal"},
		{DirectAlias{"theta-1", "custom", host, "${THETA_API_KEY}", ""}, "sk-from-env"},
		{DirectAlias{"theta-1", "custom", host, "", "THETA_API_KEY"}, "sk-from-env"},
		{DirectAlias{"theta-1", "custom", host, "", ""}, ""},
		{DirectAlias{"theta-1", "custom", host, "sk-literal", "THETA_API_KEY"}, "sk-literal"},
		{DirectAlias{"theta-1", "custom", host, "${PRIMARY}", "FALLBACK"}, "sk-from-template"},
		{DirectAlias{"theta-1", "custom", host, "   ", "FALLBACK"}, "sk-from-key-env"},
	}
	for _, c := range cases {
		if got, err := DirectAliasAPIKey(ctx, c.alias); err != nil || got != c.want {
			t.Fatalf("%+v got %q %v", c.alias, got, err)
		}
	}
}

func TestBaseURLOriginAndSessionReuse(t *testing.T) {
	type origin struct {
		scheme, host string
		port         int
	}
	for url, want := range map[string]origin{"https://h/v1": {"https", "h", 443}, "https://h:443/v1": {"https", "h", 443}, "http://h/v1": {"http", "h", 80},
		"https://h:8443/v1": {"https", "h", 8443}, "https://H./v1": {"https", "h", 443}, "": {}, "https://h:99999/v1": {},
		"localhost:11434/v1": {"", "localhost", 11434}, "127.0.0.1:11434": {"", "127.0.0.1", 11434}} {
		s, h, p := baseURLOriginPort(url)
		if (origin{s, h, p}) != want {
			t.Fatalf("%q got %v %v %v", url, s, h, p)
		}
	}
	for _, url := range []string{"localhost:11434/v1", "127.0.0.1:11434/v1"} {
		if !mayReuseSessionCredential(url, url) {
			t.Fatal(url)
		}
	}
	if mayReuseSessionCredential("http://localhost:11434/v1", "localhost:11434/v1") {
		t.Fatal("schemeless treated as schemed origin")
	}
}

func TestAliasCacheIsProfileScoped(t *testing.T) {
	_, root := switchAliasHome(t, "")
	profile := func(name, body string) context.Context {
		h := filepath.Join(root, name)
		if err := os.MkdirAll(h, 0o700); err != nil {
			t.Fatal(err)
		}
		writeConfigYAML(t, h, body)
		return home.WithOverride(context.Background(), h)
	}
	load := func(ctx context.Context) *DirectAliasTable {
		ensureDirectAliases(ctx)
		return DirectAliases
	}
	before := DirectAliases
	a := profile("a", "model_aliases:\n  theta:\n    model: a-model\n    provider: custom\n    base_url: \"https://a.example.com/v1\"\n    api_key: \"sk-PROFILE-A-SECRET\"\n")
	b := profile("b", "model_aliases:\n  theta:\n    model: b-model\n    provider: custom\n    base_url: \"https://b.example.com/v1\"\n    api_key: \"sk-PROFILE-B-SECRET\"\n")
	if theta, _ := load(a).Get("theta"); theta.APIKey != "sk-PROFILE-A-SECRET" {
		t.Fatal(theta)
	}
	if theta, _ := load(b).Get("theta"); theta != (DirectAlias{"b-model", "custom", "https://b.example.com/v1", "sk-PROFILE-B-SECRET", ""}) {
		t.Fatal(theta)
	}
	a2 := profile("a2", "model_aliases:\n  only-in-a:\n    model: x\n    provider: custom\n    base_url: \"https://a.example.com/v1\"\n")
	b2 := profile("b2", "model_aliases:\n  only-in-b:\n    model: y\n    provider: custom\n    base_url: \"https://b.example.com/v1\"\n")
	if _, ok := load(a2).Get("only-in-a"); !ok {
		t.Fatal("only-in-a missing")
	}
	loaded := load(b2)
	if _, ok := loaded.Get("only-in-a"); ok {
		t.Fatal("first profile alias persisted")
	}
	if _, ok := loaded.Get("only-in-b"); !ok {
		t.Fatal("only-in-b missing")
	}
	rot := profile("rot", "model_aliases:\n  theta:\n    model: m\n    provider: custom\n    base_url: \"https://h.example.com/v1\"\n    api_key: \"sk-BEFORE-ROTATION\"\n")
	if theta, _ := load(rot).Get("theta"); theta.APIKey != "sk-BEFORE-ROTATION" {
		t.Fatal(theta)
	}
	writeConfigYAML(t, home.Dir(rot), "model_aliases:\n  theta:\n    model: m\n    provider: custom\n    base_url: \"https://h.example.com/v1\"\n    api_key: \"sk-AFTER-ROTATION-XYZ\"\n")
	if theta, _ := load(rot).Get("theta"); theta.APIKey != "sk-AFTER-ROTATION-XYZ" {
		t.Fatal(theta)
	}
	if DirectAliases != before {
		t.Fatal("alias table rebound")
	}
}

func TestOneShotUsesTheSameHostInvariant(t *testing.T) {
	for provider, envVar := range map[string]string{"anthropic": "ANTHROPIC_API_KEY", "deepseek": "DEEPSEEK_API_KEY", "xai": "XAI_API_KEY"} {
		ctx, _ := switchAliasHome(t, "model:\n  default: m\n  provider: openrouter\n")
		secret := "sk-" + provider + "-LIVE-TOKEN"
		t.Setenv(envVar, secret)
		alias := DirectAlias{Model: "c", Provider: provider, BaseURL: "https://evil.test/v1"}
		requested, explicit, err := DirectAliasRuntimeRequest(ctx, alias)
		if err != nil {
			t.Fatal(err)
		}
		rt, err := modelsResolver(ctx).ResolveRuntime(requested, explicit, alias.BaseURL, nil, "")
		if err == nil && rt != nil && rt.APIKey == secret {
			t.Fatalf("%s token reached the foreign host", provider)
		}
	}
	ctx, _ := switchAliasHome(t, "")
	for _, c := range []struct {
		alias         DirectAlias
		provider, key string
	}{
		{DirectAlias{Model: "c", Provider: "anthropic"}, "anthropic", ""},
		{DirectAlias{Model: "c", Provider: "anthropic", BaseURL: "https://evil.test/v1"}, "custom", ""},
		{DirectAlias{Model: "c", Provider: "anthropic", BaseURL: "https://evil.test/v1", APIKey: "sk-own"}, "custom", "sk-own"},
	} {
		p, k, err := DirectAliasRuntimeRequest(ctx, c.alias)
		if err != nil || p != c.provider || k != c.key {
			t.Fatalf("%+v got %q %q %v", c.alias, p, k, err)
		}
	}
}

func TestStartupModelRouting(t *testing.T) {
	ctx, _ := switchAliasHome(t, "")
	route := func(raw string, opts StartupRouteOptions) *StartupModelRoute {
		t.Helper()
		r, err := ResolveStartupModelRoute(ctx, raw, opts)
		if err != nil {
			t.Fatal(err)
		}
		return r
	}
	same := func(got *StartupModelRoute, want StartupModelRoute) {
		t.Helper()
		if got == nil || *got != want {
			t.Fatalf("got %+v want %+v", got, want)
		}
	}
	nous := map[string]any{"nous": map[string]any{"base_url": "https://inference.example/v1"}}
	same(route("nous/deepseek-v4-pro", StartupRouteOptions{UserProviders: nous}), StartupModelRoute{Model: "deepseek-v4-pro", Provider: "nous"})
	same(route("ollama/qwen3.5:4b", StartupRouteOptions{UserProviders: map[string]any{"ollama": map[string]any{"base_url": "http://localhost:11434/v1"}}}),
		StartupModelRoute{Model: "qwen3.5:4b", Provider: "ollama"})
	if r := route("openrouter/anthropic/claude-sonnet", StartupRouteOptions{UserProviders: map[string]any{"openrouter": map[string]any{"base_url": "https://openrouter.ai/api/v1"}}}); r != nil {
		t.Fatal(r)
	}
	prevSlug := findOpenRouterSlugFn
	findOpenRouterSlugFn = func(context.Context, string) string { return "anthropic/claude-opus-4.6" }
	r := route("anthropic/claude-opus-4.6", StartupRouteOptions{CurrentProvider: "openrouter", UserProviders: map[string]any{"anthropic": map[string]any{"apiKey": "sk-test"}}})
	findOpenRouterSlugFn = prevSlug
	if r != nil {
		t.Fatal(r)
	}
	same(route("nous/deepseek-v4-pro", StartupRouteOptions{CurrentProvider: "anthropic", UserProviders: nous}), StartupModelRoute{Model: "deepseek-v4-pro", Provider: "nous"})
	seedDirectAliases(NamedDirectAlias{"localqwen", DirectAlias{Model: "qwen3.5:4b", Provider: "custom", BaseURL: "http://localhost:11434/v1"}},
		NamedDirectAlias{"urlalias", DirectAlias{Model: "qwen3.5:4b", Provider: "anthropic", BaseURL: "http://localhost:11434/v1"}},
		NamedDirectAlias{"keyed", DirectAlias{Model: "some-model", Provider: "custom", BaseURL: "https://proxy.example/v1", APIKey: "sk-alias-key"}},
		NamedDirectAlias{"ds", DirectAlias{Model: "deepseek-chat", Provider: "deepseek"}})
	same(route("localqwen", StartupRouteOptions{}), StartupModelRoute{Model: "qwen3.5:4b", Provider: "custom", BaseURL: "http://localhost:11434/v1"})
	if r := route("urlalias", StartupRouteOptions{}); r == nil || r.Provider != "custom" || r.BaseURL != "http://localhost:11434/v1" {
		t.Fatal(r)
	}
	if r := route("keyed", StartupRouteOptions{}); r == nil || r.APIKey != "sk-alias-key" {
		t.Fatal(r)
	}
	if r := route("ds", StartupRouteOptions{ExplicitProvider: "openrouter"}); r == nil || r.Provider != "openrouter" || r.Model != "deepseek-chat" {
		t.Fatal(r)
	}
	DirectAliases.Clear()
	jetson := map[string]any{"jetson-vllm": map[string]any{"base_url": "http://127.0.0.1:8000/v1", "api_key": "EMPTY",
		"api_mode": "chat_completions", "models": []any{"nemotron-nano-30b"}}}
	same(route("custom:jetson-vllm:nemotron-nano-30b", StartupRouteOptions{CurrentProvider: "anthropic", UserProviders: jetson}),
		StartupModelRoute{Model: "nemotron-nano-30b", Provider: "custom:jetson-vllm"})
	if r := route("custom:jetson-vllm:nemotron-nano-30b", StartupRouteOptions{CurrentProvider: "anthropic", UserProviders: map[string]any{}}); r == nil || r.Provider != "custom" {
		t.Fatal(r)
	}
	if r := route("anthropic/claude-3.5-sonnet:beta", StartupRouteOptions{CurrentProvider: "anthropic", UserProviders: jetson}); r != nil {
		t.Fatal(r)
	}
	seedDirectAliases(NamedDirectAlias{"myalias", DirectAlias{Model: "my-model-id", Provider: "custom", BaseURL: "http://alias.example:8000/v1", APIKey: "not-needed"}})
	same(route("myalias", StartupRouteOptions{}), StartupModelRoute{Model: "my-model-id", Provider: "custom", BaseURL: "http://alias.example:8000/v1", APIKey: "not-needed"})
}

func TestAliasLaunchConfigSnapshotCarriesAliasEndpointAndKey(t *testing.T) {
	ctx, _ := switchAliasHome(t, "")
	t.Setenv("ALIAS_HOST_KEY", "sk-alias-env-secret")
	cfg := map[string]any{
		"model":     map[string]any{"provider": "custom", "base_url": "http://127.0.0.1:1/v1", "default": "model-main"},
		"providers": map[string]any{"named-host": map[string]any{"base_url": "http://127.0.0.1:2/v1", "model": "model-named"}},
		"model_aliases": map[string]any{
			"alias-host": map[string]any{"model": "model-alias", "provider": "custom", "base_url": "http://127.0.0.1:3/v1", "api_key": "sk-alias-literal"},
			"alias-env":  map[string]any{"model": "model-alias-env", "provider": "custom", "base_url": "http://127.0.0.1:3/v1", "key_env": "ALIAS_HOST_KEY"},
			"fast":       map[string]any{"model": "model-alias", "provider": "custom", "base_url": "http://127.0.0.1:3/v1"},
		},
	}
	opts := StartupRouteOptions{UserProviders: cfg["providers"].(map[string]any), AliasConfig: cfg}
	for raw, want := range map[string]StartupModelRoute{
		"alias-host":                    {"model-alias", "custom", "http://127.0.0.1:3/v1", "sk-alias-literal"},
		"alias-env":                     {"model-alias-env", "custom", "http://127.0.0.1:3/v1", "sk-alias-env-secret"},
		"fast":                          {"model-alias", "custom", "http://127.0.0.1:3/v1", ""},
		"custom:named-host:model-named": {"model-named", "custom:named-host", "", ""},
	} {
		got, err := ResolveStartupModelRoute(ctx, raw, opts)
		if err != nil || got == nil || *got != want {
			t.Fatalf("%s got %+v %v", raw, got, err)
		}
	}
	if got, _ := ResolveStartupModelRoute(ctx, "model-main", opts); got != nil {
		t.Fatal(got)
	}
	if DirectAliases.Len() != 0 {
		t.Fatal("a config snapshot leaked into the shared alias table")
	}
}

func TestResolveAliasDirectAndSorting(t *testing.T) {
	ctx, _ := switchAliasHome(t, "")
	seedDirectAliases(NamedDirectAlias{"glm", DirectAlias{Model: "glm-4.7", Provider: "custom", BaseURL: "https://ollama.com/v1"}},
		NamedDirectAlias{"myalias", DirectAlias{Model: "my-model", Provider: "custom", BaseURL: "https://example.com"}})
	if m, err := ResolveAlias(ctx, "glm", "openrouter", nil, nil); err != nil || m == nil || *m != (AliasMatch{"custom", "glm-4.7", "glm"}) {
		t.Fatal(m, err)
	}
	if m, err := ResolveAlias(ctx, "  myalias  ", "openrouter", nil, nil); err != nil || m == nil || m.Model != "my-model" {
		t.Fatal(m, err)
	}
	DirectAliases.Clear()
	prevList, prevStatic := listProviderModelsFn, staticCatalogFn
	t.Cleanup(func() { listProviderModelsFn, staticCatalogFn = prevList, prevStatic })
	staticCatalogFn = func(context.Context, string) []string { return nil }
	catalog := func(ids ...string) { listProviderModelsFn = func(context.Context, string) []string { return ids } }
	var amb *AmbiguousAliasError
	catalog("claude-opus-4-1", "claude-opus-4-7", "claude-opus-4-8", "claude-opus-4-20250514")
	if _, err := ResolveAlias(ctx, "opus", "anthropic", nil, nil); !errors.As(err, &amb) || amb.Candidates[0] != "claude-opus-4-8" || len(amb.Candidates) != 4 {
		t.Fatal(err)
	}
	catalog("claude-opus-4-7", "claude-opus-4-8", "claude-opus-4-20250514", "claude-opus-4-9")
	if _, err := ResolveAlias(ctx, "opus", "anthropic", nil, nil); !errors.As(err, &amb) || amb.Candidates[0] != "claude-opus-4-9" {
		t.Fatal(err)
	}
	catalog("claude-opus-4-8", "claude-sonnet-4-6")
	if m, err := ResolveAlias(ctx, "opus", "anthropic", nil, nil); err != nil || m == nil || m.Model != "claude-opus-4-8" {
		t.Fatal(m, err)
	}
}
