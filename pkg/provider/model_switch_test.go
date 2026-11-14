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
	persist := func(t *testing.T, yaml string, global, session, once bool, explicit string) bool {
		t.Helper()
		ctx, _ := switchAliasHome(t, yaml)
		return ResolvePersistBehavior(ctx, global, session, once, explicit)
	}
	flagged := "model:\n  persist_switch_by_default: true\n"
	family := "model:\n  default: agnes-3.0-flash\n  provider: custom:family-snowflake\n"
	t.Run("test_session_flag_always_session_only", func(t *testing.T) {
		if persist(t, flagged, false, true, false, "") {
			t.Fatal("session flag persisted")
		}
	})
	t.Run("test_no_provider_uses_config_default", func(t *testing.T) {
		if !persist(t, flagged, false, false, false, "") {
			t.Fatal("config default ignored")
		}
	})
	t.Run("test_provider_pick_honors_persist_switch_by_default", func(t *testing.T) {
		cfg := family + "  persist_switch_by_default: true\n"
		if !persist(t, cfg, false, false, false, "custom:family-xizhao") || persist(t, cfg, false, true, false, "custom:family-xizhao") ||
			persist(t, cfg, false, false, true, "custom:family-xizhao") || !persist(t, cfg, true, false, false, "custom:family-xizhao") {
			t.Fatal("provider pick ignored the persist flag")
		}
	})
	t.Run("test_provider_pick_stays_exploratory_without_the_flag", func(t *testing.T) {
		if persist(t, family, false, false, false, "custom:family-xizhao") || persist(t, family, false, false, false, "") {
			t.Fatal("provider pick persisted without the flag")
		}
	})
	t.Run("test_first_pick_persists_then_session_only", func(t *testing.T) {
		codex := "model:\n  default: gpt-5.6\n  provider: openai-codex\n"
		if !persist(t, "model: {}\n", false, false, false, "anthropic") || !persist(t, "model: \"\"\n", false, false, false, "") {
			t.Fatal("first pick did not persist")
		}
		if persist(t, codex, false, false, false, "openai-api") || persist(t, codex, false, false, false, "") || !persist(t, codex, true, false, false, "openai-api") ||
			persist(t, "model: gpt-5.6\n", false, false, false, "") {
			t.Fatal("later pick persisted")
		}
	})
}

func TestCodexOAuthOverridesModelsDev(t *testing.T) {
	prev := GetModelContextLength
	GetModelContextLength = func(context.Context, ContextLengthRequest) int { return 272_000 }
	t.Cleanup(func() { GetModelContextLength = prev })
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
	t.Run("test_api_key_and_key_env_are_loaded_from_config", func(t *testing.T) {
		ctx, _ := switchAliasHome(t, "model:\n  default: gpt-4\n  provider: openrouter\nmodel_aliases:\n  theta:\n    model: theta-1\n"+
			"    provider: custom\n    base_url: https://theta.example.com/v1\n    api_key: sk-literal\n    key_env: THETA_API_KEY\n")
		a, _ := loadDirectAliases(ctx, nil).Get("theta")
		if a.APIKey != "sk-literal" || a.KeyEnv != "THETA_API_KEY" {
			t.Fatalf("%+v", a)
		}
	})
	nested := func(t *testing.T, name, entry string) DirectAlias {
		ctx, _ := switchAliasHome(t, "model:\n  default: gpt-4\n  provider: openrouter\n  aliases:\n    "+name+":\n"+entry)
		a, _ := loadDirectAliases(ctx, nil).Get(name)
		return a
	}
	t.Run("test_nested_alias_keeps_key_env", func(t *testing.T) {
		q := nested(t, "qwen-local", "      model: qwen3-next\n      provider: custom\n      base_url: http://192.168.1.50:8000/v1\n      key_env: QWEN27B_KEY\n")
		if q.KeyEnv != "QWEN27B_KEY" || q.BaseURL != "http://192.168.1.50:8000/v1" {
			t.Fatalf("%+v", q)
		}
	})
	t.Run("test_nested_alias_keeps_api_key", func(t *testing.T) {
		if n := nested(t, "theta-nested", "      model: theta-1\n      provider: custom\n      base_url: https://theta.example.com/v1\n      api_key: sk-literal\n"); n.APIKey != "sk-literal" {
			t.Fatalf("%+v", n)
		}
	})
	t.Run("test_direct_alias_loaded_from_config", func(t *testing.T) {
		ctx, _ := switchAliasHome(t, "model_aliases:\n  mymodel:\n    model: custom-model:latest\n    provider: custom\n    base_url: https://example.com/v1\n")
		m, ok := loadDirectAliases(ctx, nil).Get("mymodel")
		if !ok || m != (DirectAlias{Model: "custom-model:latest", Provider: "custom", BaseURL: "https://example.com/v1"}) {
			t.Fatalf("%+v %v", m, ok)
		}
	})
	t.Run("test_empty_model_string_skipped", func(t *testing.T) {
		ctx, _ := switchAliasHome(t, "model_aliases:\n  empty:\n    model: \"\"\n    provider: custom\n  good:\n    model: real\n    provider: custom\n")
		loaded := loadDirectAliases(ctx, nil)
		_, hasEmpty := loaded.Get("empty")
		_, hasGood := loaded.Get("good")
		if hasEmpty || !hasGood {
			t.Fatalf("%v %v", hasEmpty, hasGood)
		}
	})
	t.Run("test_model_aliases_dict_entries_are_loaded", func(t *testing.T) {
		ctx, _ := switchAliasHome(t, "model:\n  aliases:\n    localqwen:\n      model: qwen3.5:4b\n      provider: custom\n      base_url: http://localhost:11434/v1\n")
		if a, _ := loadDirectAliases(ctx, nil).Get("localqwen"); a != (DirectAlias{Model: "qwen3.5:4b", Provider: "custom", BaseURL: "http://localhost:11434/v1"}) {
			t.Fatalf("%+v", a)
		}
	})
}

func TestDirectAliasAPIKeyPrecedence(t *testing.T) {
	ctx, _ := switchAliasHome(t, "")
	host := "https://theta.example.com/v1"
	check := func(t *testing.T, alias DirectAlias, want string) {
		t.Helper()
		if got, err := DirectAliasAPIKey(ctx, alias); err != nil || got != want {
			t.Fatalf("%+v got %q %v", alias, got, err)
		}
	}
	t.Run("test_resolves_literal_env_template_and_key_env", func(t *testing.T) {
		t.Setenv("THETA_API_KEY", "sk-from-env")
		check(t, DirectAlias{"theta-1", "custom", host, "sk-literal", ""}, "sk-literal")
		check(t, DirectAlias{"theta-1", "custom", host, "${THETA_API_KEY}", ""}, "sk-from-env")
		check(t, DirectAlias{"theta-1", "custom", host, "", "THETA_API_KEY"}, "sk-from-env")
		check(t, DirectAlias{"theta-1", "custom", host, "", ""}, "")
	})
	t.Run("test_api_key_wins_over_key_env", func(t *testing.T) {
		t.Setenv("THETA_API_KEY", "sk-from-key-env")
		check(t, DirectAlias{"theta-1", "custom", host, "sk-literal", "THETA_API_KEY"}, "sk-literal")
	})
	t.Run("test_env_template_api_key_also_wins_over_key_env", func(t *testing.T) {
		t.Setenv("PRIMARY", "sk-from-template")
		t.Setenv("FALLBACK", "sk-from-key-env")
		check(t, DirectAlias{"theta-1", "custom", host, "${PRIMARY}", "FALLBACK"}, "sk-from-template")
	})
	t.Run("test_key_env_used_when_api_key_is_blank", func(t *testing.T) {
		t.Setenv("FALLBACK", "sk-from-key-env")
		check(t, DirectAlias{"theta-1", "custom", host, "   ", "FALLBACK"}, "sk-from-key-env")
	})
}

func TestBaseURLOriginAndSessionReuse(t *testing.T) {
	type origin struct {
		scheme, host string
		port         int
	}
	check := func(t *testing.T, cases map[string]origin) {
		t.Helper()
		for url, want := range cases {
			s, h, p := baseURLOriginPort(url)
			if (origin{s, h, p}) != want {
				t.Fatalf("%q got %v %v %v", url, s, h, p)
			}
		}
	}
	t.Run("test_origin_normalisation", func(t *testing.T) {
		check(t, map[string]origin{"https://h/v1": {"https", "h", 443}, "https://h:443/v1": {"https", "h", 443}, "http://h/v1": {"http", "h", 80},
			"https://h:8443/v1": {"https", "h", 8443}, "https://H./v1": {"https", "h", 443}, "": {}, "https://h:99999/v1": {}})
	})
	t.Run("test_hostname_and_port_survive_without_a_scheme", func(t *testing.T) {
		check(t, map[string]origin{"localhost:11434/v1": {"", "localhost", 11434}, "127.0.0.1:11434": {"", "127.0.0.1", 11434}})
	})
	t.Run("test_schemeless_loopback_alias_keeps_the_session_credential", func(t *testing.T) {
		for _, url := range []string{"localhost:11434/v1", "127.0.0.1:11434/v1"} {
			if !mayReuseSessionCredential(url, url) {
				t.Fatal(url)
			}
		}
	})
	t.Run("test_schemeless_is_not_treated_as_the_schemed_origin", func(t *testing.T) {
		if mayReuseSessionCredential("http://localhost:11434/v1", "localhost:11434/v1") {
			t.Fatal("schemeless treated as schemed origin")
		}
	})
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
	t.Run("test_second_profile_does_not_inherit_the_first_profiles_key", func(t *testing.T) {
		a := profile("a", "model_aliases:\n  theta:\n    model: a-model\n    provider: custom\n    base_url: \"https://a.example.com/v1\"\n    api_key: \"sk-PROFILE-A-SECRET\"\n")
		b := profile("b", "model_aliases:\n  theta:\n    model: b-model\n    provider: custom\n    base_url: \"https://b.example.com/v1\"\n    api_key: \"sk-PROFILE-B-SECRET\"\n")
		if theta, _ := load(a).Get("theta"); theta.APIKey != "sk-PROFILE-A-SECRET" {
			t.Fatal(theta)
		}
		if theta, _ := load(b).Get("theta"); theta != (DirectAlias{"b-model", "custom", "https://b.example.com/v1", "sk-PROFILE-B-SECRET", ""}) {
			t.Fatal(theta)
		}
	})
	t.Run("test_alias_absent_from_the_second_profile_does_not_persist", func(t *testing.T) {
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
	})
	t.Run("test_key_rotation_in_place_is_picked_up", func(t *testing.T) {
		rot := profile("rot", "model_aliases:\n  theta:\n    model: m\n    provider: custom\n    base_url: \"https://h.example.com/v1\"\n    api_key: \"sk-BEFORE-ROTATION\"\n")
		if theta, _ := load(rot).Get("theta"); theta.APIKey != "sk-BEFORE-ROTATION" {
			t.Fatal(theta)
		}
		writeConfigYAML(t, home.Dir(rot), "model_aliases:\n  theta:\n    model: m\n    provider: custom\n    base_url: \"https://h.example.com/v1\"\n    api_key: \"sk-AFTER-ROTATION-XYZ\"\n")
		if theta, _ := load(rot).Get("theta"); theta.APIKey != "sk-AFTER-ROTATION-XYZ" {
			t.Fatal(theta)
		}
	})
	t.Run("test_cache_is_still_mutated_in_place", func(t *testing.T) {
		before := DirectAliases
		load(profile("inplace", "model_aliases:\n  theta:\n    model: m\n    provider: custom\n    base_url: \"https://h.example.com/v1\"\n"))
		if DirectAliases != before {
			t.Fatal("alias table rebound")
		}
	})
}

func TestOneShotUsesTheSameHostInvariant(t *testing.T) {
	t.Run("test_no_key_alias_on_a_foreign_host_gets_no_provider_token", func(t *testing.T) {
		for provider, envVar := range map[string]string{"anthropic": "ANTHROPIC_API_KEY", "deepseek": "DEEPSEEK_API_KEY", "xai": "XAI_API_KEY"} {
			t.Run(provider, func(t *testing.T) {
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
			})
		}
	})
	ctx, _ := switchAliasHome(t, "")
	request := func(t *testing.T, alias DirectAlias, provider, key string) {
		t.Helper()
		p, k, err := DirectAliasRuntimeRequest(ctx, alias)
		if err != nil || p != provider || k != key {
			t.Fatalf("%+v got %q %q %v", alias, p, k, err)
		}
	}
	t.Run("test_label_is_kept_when_the_alias_has_no_url", func(t *testing.T) {
		request(t, DirectAlias{Model: "c", Provider: "anthropic"}, "anthropic", "")
	})
	t.Run("test_url_bearing_alias_is_forced_to_custom", func(t *testing.T) {
		request(t, DirectAlias{Model: "c", Provider: "anthropic", BaseURL: "https://evil.test/v1"}, "custom", "")
	})
	t.Run("test_declared_key_is_carried_through", func(t *testing.T) {
		request(t, DirectAlias{Model: "c", Provider: "anthropic", BaseURL: "https://evil.test/v1", APIKey: "sk-own"}, "custom", "sk-own")
	})
}

func TestStartupModelRouting(t *testing.T) {
	ctx, _ := switchAliasHome(t, "")
	route := func(t *testing.T, raw string, opts StartupRouteOptions) *StartupModelRoute {
		t.Helper()
		r, err := ResolveStartupModelRoute(ctx, raw, opts)
		if err != nil {
			t.Fatal(err)
		}
		return r
	}
	same := func(t *testing.T, got *StartupModelRoute, want StartupModelRoute) {
		t.Helper()
		if got == nil || *got != want {
			t.Fatalf("got %+v want %+v", got, want)
		}
	}
	nous := map[string]any{"nous": map[string]any{"base_url": "https://inference.example/v1"}}
	t.Run("test_startup_route_uses_configured_nous_provider", func(t *testing.T) {
		same(t, route(t, "nous/deepseek-v4-pro", StartupRouteOptions{UserProviders: nous}), StartupModelRoute{Model: "deepseek-v4-pro", Provider: "nous"})
	})
	t.Run("test_startup_route_keeps_configured_custom_provider_name", func(t *testing.T) {
		same(t, route(t, "ollama/qwen3.5:4b", StartupRouteOptions{UserProviders: map[string]any{"ollama": map[string]any{"base_url": "http://localhost:11434/v1"}}}),
			StartupModelRoute{Model: "qwen3.5:4b", Provider: "ollama"})
	})
	t.Run("test_startup_route_does_not_consume_aggregator_namespace", func(t *testing.T) {
		if r := route(t, "openrouter/anthropic/claude-sonnet", StartupRouteOptions{UserProviders: map[string]any{"openrouter": map[string]any{"base_url": "https://openrouter.ai/api/v1"}}}); r != nil {
			t.Fatal(r)
		}
	})
	t.Run("test_startup_route_aggregator_native_slug_stays_on_aggregator", func(t *testing.T) {
		prevSlug := findOpenRouterSlugFn
		t.Cleanup(func() { findOpenRouterSlugFn = prevSlug })
		findOpenRouterSlugFn = func(context.Context, string) string { return "anthropic/claude-opus-4.6" }
		if r := route(t, "anthropic/claude-opus-4.6", StartupRouteOptions{CurrentProvider: "openrouter", UserProviders: map[string]any{"anthropic": map[string]any{"apiKey": "sk-test"}}}); r != nil {
			t.Fatal(r)
		}
	})
	t.Run("test_startup_route_non_aggregator_current_provider_still_routes", func(t *testing.T) {
		same(t, route(t, "nous/deepseek-v4-pro", StartupRouteOptions{CurrentProvider: "anthropic", UserProviders: nous}), StartupModelRoute{Model: "deepseek-v4-pro", Provider: "nous"})
	})
	seedDirectAliases(NamedDirectAlias{"localqwen", DirectAlias{Model: "qwen3.5:4b", Provider: "custom", BaseURL: "http://localhost:11434/v1"}},
		NamedDirectAlias{"urlalias", DirectAlias{Model: "qwen3.5:4b", Provider: "anthropic", BaseURL: "http://localhost:11434/v1"}},
		NamedDirectAlias{"keyed", DirectAlias{Model: "some-model", Provider: "custom", BaseURL: "https://proxy.example/v1", APIKey: "sk-alias-key"}},
		NamedDirectAlias{"ds", DirectAlias{Model: "deepseek-chat", Provider: "deepseek"}})
	t.Run("test_startup_route_resolves_dict_alias_and_preserves_endpoint", func(t *testing.T) {
		same(t, route(t, "localqwen", StartupRouteOptions{}), StartupModelRoute{Model: "qwen3.5:4b", Provider: "custom", BaseURL: "http://localhost:11434/v1"})
	})
	t.Run("test_startup_route_url_alias_never_keeps_foreign_provider_label", func(t *testing.T) {
		if r := route(t, "urlalias", StartupRouteOptions{}); r == nil || r.Provider != "custom" || r.BaseURL != "http://localhost:11434/v1" {
			t.Fatal(r)
		}
	})
	t.Run("test_startup_route_alias_carries_own_api_key", func(t *testing.T) {
		if r := route(t, "keyed", StartupRouteOptions{}); r == nil || r.APIKey != "sk-alias-key" {
			t.Fatal(r)
		}
	})
	t.Run("test_startup_route_explicit_provider_wins_over_alias_label", func(t *testing.T) {
		if r := route(t, "ds", StartupRouteOptions{ExplicitProvider: "openrouter"}); r == nil || r.Provider != "openrouter" || r.Model != "deepseek-chat" {
			t.Fatal(r)
		}
	})
	DirectAliases.Clear()
	t.Run("test_startup_route_decodes_custom_colon_qualified_model", func(t *testing.T) {
		jetson := map[string]any{"jetson-vllm": map[string]any{"base_url": "http://127.0.0.1:8000/v1", "api_key": "EMPTY",
			"api_mode": "chat_completions", "models": []any{"nemotron-nano-30b"}}}
		same(t, route(t, "custom:jetson-vllm:nemotron-nano-30b", StartupRouteOptions{CurrentProvider: "anthropic", UserProviders: jetson}),
			StartupModelRoute{Model: "nemotron-nano-30b", Provider: "custom:jetson-vllm"})
		if r := route(t, "custom:jetson-vllm:nemotron-nano-30b", StartupRouteOptions{CurrentProvider: "anthropic", UserProviders: map[string]any{}}); r == nil || r.Provider != "custom" {
			t.Fatal(r)
		}
		if r := route(t, "anthropic/claude-3.5-sonnet:beta", StartupRouteOptions{CurrentProvider: "anthropic", UserProviders: jetson}); r != nil {
			t.Fatal(r)
		}
	})
	seedDirectAliases(NamedDirectAlias{"myalias", DirectAlias{Model: "my-model-id", Provider: "custom", BaseURL: "http://alias.example:8000/v1", APIKey: "not-needed"}})
	same(t, route(t, "myalias", StartupRouteOptions{}), StartupModelRoute{Model: "my-model-id", Provider: "custom", BaseURL: "http://alias.example:8000/v1", APIKey: "not-needed"})
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
	t.Run("test_direct_alias_resolved_before_catalog", func(t *testing.T) {
		if m, err := ResolveAlias(ctx, "glm", "openrouter", nil, nil); err != nil || m == nil || *m != (AliasMatch{"custom", "glm-4.7", "glm"}) {
			t.Fatal(m, err)
		}
	})
	t.Run("test_whitespace_input_handled", func(t *testing.T) {
		if m, err := ResolveAlias(ctx, "  myalias  ", "openrouter", nil, nil); err != nil || m == nil || m.Model != "my-model" {
			t.Fatal(m, err)
		}
	})
	DirectAliases.Clear()
	prevList, prevStatic := listProviderModelsFn, staticCatalogFn
	t.Cleanup(func() { listProviderModelsFn, staticCatalogFn = prevList, prevStatic })
	staticCatalogFn = func(context.Context, string) []string { return nil }
	catalog := func(ids ...string) { listProviderModelsFn = func(context.Context, string) []string { return ids } }
	var amb *AmbiguousAliasError
	t.Run("test_anthropic_opus_ambiguous_lists_candidates", func(t *testing.T) {
		catalog("claude-opus-4-1", "claude-opus-4-7", "claude-opus-4-8", "claude-opus-4-20250514")
		_, err := ResolveAlias(ctx, "opus", "anthropic", nil, nil)
		if !errors.As(err, &amb) || amb.Candidates[0] != "claude-opus-4-8" || len(amb.Candidates) != 4 {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(sortedStrings(amb.Candidates), []string{"claude-opus-4-1", "claude-opus-4-20250514", "claude-opus-4-7", "claude-opus-4-8"}) {
			t.Fatalf("candidates %v", amb.Candidates)
		}
	})
	t.Run("test_unsynced_new_model_sorts_first", func(t *testing.T) {
		catalog("claude-opus-4-7", "claude-opus-4-8", "claude-opus-4-20250514", "claude-opus-4-9")
		if _, err := ResolveAlias(ctx, "opus", "anthropic", nil, nil); !errors.As(err, &amb) || amb.Candidates[0] != "claude-opus-4-9" {
			t.Fatal(err)
		}
	})
	t.Run("test_single_match_resolves_without_error", func(t *testing.T) {
		catalog("claude-opus-4-8", "claude-sonnet-4-6")
		if m, err := ResolveAlias(ctx, "opus", "anthropic", nil, nil); err != nil || m == nil || m.Model != "claude-opus-4-8" {
			t.Fatal(m, err)
		}
	})
}
