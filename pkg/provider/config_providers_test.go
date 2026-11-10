package provider

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/config"
)

func TestNormalizeExtraHeadersStringifiesAndDropsNil(t *testing.T) {
	got := NormalizeExtraHeaders(map[string]any{"X-Int": 7, "X-Str": "v", "X-None": nil})
	if !reflect.DeepEqual(got, map[string]string{"X-Int": "7", "X-Str": "v"}) {
		t.Fatalf("got %v", got)
	}
	for _, bad := range []any{nil, "x", 42, []any{"a"}, map[string]any{}} {
		if got := NormalizeExtraHeaders(bad); len(got) != 0 {
			t.Errorf("%v: got %v", bad, got)
		}
	}
}

func TestNormalizeEntryKeepsExtraHeaders(t *testing.T) {
	n := NormalizeCustomProviderEntry(map[string]any{
		"name": "my-proxy", "base_url": "https://llm.internal.example.com/v1",
		"extra_headers": map[string]any{"X-Custom-Auth": "tok", "X-Client-Name": "hermes"},
	}, "")
	if n == nil || !reflect.DeepEqual(n["extra_headers"], map[string]string{"X-Custom-Auth": "tok", "X-Client-Name": "hermes"}) {
		t.Fatalf("got %v", n)
	}
	for _, bad := range []any{"not-a-dict", map[string]any{}, 42, []any{"a"}} {
		n := NormalizeCustomProviderEntry(map[string]any{"name": "my-proxy", "base_url": "https://llm.internal.example.com/v1", "extra_headers": bad}, "")
		if n == nil {
			t.Fatal("entry dropped")
		}
		if _, has := n["extra_headers"]; has {
			t.Errorf("%v kept extra_headers", bad)
		}
	}
}

func TestGetCustomProviderExtraHeaders(t *testing.T) {
	providers := []map[string]any{{"name": "my-proxy", "base_url": "https://llm.internal.example.com/v1", "extra_headers": map[string]any{"CF-Access-Client-Id": "xxxx.access"}}}
	if got := CustomProviderExtraHeaders("https://LLM.internal.example.com/v1/", providers, nil); !reflect.DeepEqual(got, map[string]string{"CF-Access-Client-Id": "xxxx.access"}) {
		t.Fatalf("got %v", got)
	}
	if got := CustomProviderExtraHeaders("https://other.example.com/v1", providers, nil); len(got) != 0 {
		t.Fatalf("no match: %v", got)
	}
	if got := CustomProviderExtraHeaders("https://llm.internal.example.com.attacker.test/v1", providers, nil); len(got) != 0 {
		t.Fatalf("lookalike: %v", got)
	}
	doubleSlash := []map[string]any{{"base_url": "https://llm.internal.example.com/v1//", "extra_headers": map[string]any{"Authorization": "secret"}}}
	if got := CustomProviderExtraHeaders("https://llm.internal.example.com/v1", doubleSlash, nil); len(got) != 0 {
		t.Fatalf("extra path segment: %v", got)
	}
	aliases := []map[string]any{
		{"name": "direct", "base_url": "http://127.0.0.1:8787/v1"},
		{"name": "aether-router", "base_url": "http://127.0.0.1:8787/v1", "extra_headers": map[string]any{"X-Aether-Route": "my-route"}},
	}
	if got := CustomProviderExtraHeaders("http://127.0.0.1:8787/v1", aliases, nil); !reflect.DeepEqual(got, map[string]string{"X-Aether-Route": "my-route"}) {
		t.Fatalf("alias without headers shadowed: %v", got)
	}
	empty := []map[string]any{
		{"name": "bare-alias", "base_url": "http://127.0.0.1:8787/v1", "extra_headers": map[string]any{}},
		{"name": "proxied", "base_url": "http://127.0.0.1:8787/v1", "extra_headers": map[string]any{"Authorization": "Bearer tok"}},
	}
	if got := CustomProviderExtraHeaders("http://127.0.0.1:8787/v1", empty, nil); !reflect.DeepEqual(got, map[string]string{"Authorization": "Bearer tok"}) {
		t.Fatalf("empty dict alias shadowed: %v", got)
	}
}

func TestMergeExtraHeaders(t *testing.T) {
	providers := []map[string]any{{"name": "my-proxy", "base_url": "https://llm.internal.example.com/v1", "extra_headers": map[string]any{"User-Agent": "override", "X-New": "2"}}}
	got := MergeExtraHeaders(map[string]string{"User-Agent": "curl/8.7.1", "X-Keep": "1"}, "https://llm.internal.example.com/v1", providers, nil)
	if !reflect.DeepEqual(got, map[string]string{"User-Agent": "override", "X-Keep": "1", "X-New": "2"}) {
		t.Fatalf("got %v", got)
	}
	defaults := map[string]string{"X": "1"}
	if got := MergeExtraHeaders(defaults, "https://other.example.com/v1", providers, nil); !reflect.DeepEqual(got, defaults) {
		t.Fatalf("no match changed defaults: %v", got)
	}
}

func TestApiKeyEnvAliasNormalizesToKeyEnv(t *testing.T) {
	n := NormalizeCustomProviderEntry(map[string]any{"name": "vendor", "base_url": "https://api.vendor.example.com/v1", "api_key_env": "MY_VENDOR_KEY"}, "vendor")
	if n == nil || n["key_env"] != "MY_VENDOR_KEY" {
		t.Fatalf("got %v", n)
	}
	extra := map[string]any{"chat_template_kwargs": map[string]any{"enable_thinking": true}, "include_reasoning": true}
	n = NormalizeCustomProviderEntry(map[string]any{"name": "vendor", "base_url": "https://api.vendor.example.com/v1", "extra_body": extra}, "vendor")
	if n == nil || !reflect.DeepEqual(n["extra_body"], extra) {
		t.Fatalf("extra_body: %v", n)
	}
}

func TestNormalizeEntryShapes(t *testing.T) {
	if NormalizeCustomProviderEntry(map[string]any{"name": "x"}, "") != nil {
		t.Fatal("no url is dropped")
	}
	if NormalizeCustomProviderEntry(map[string]any{"base_url": "nonsense"}, "k") != nil {
		t.Fatal("invalid url is dropped")
	}
	n := NormalizeCustomProviderEntry(map[string]any{"base_url": "https://{region}.example/v1", "transport": "responses",
		"models": []any{"a", map[string]any{"id": "b", "context_length": 1000}}, "capabilities": map[string]any{"vision": true, "bad": "yes"},
		"context_length": 4096, "ssl_verify": "false", "apiKey": "camel"}, "proxy")
	if n == nil || n["name"] != "proxy" || n["api_mode"] != "codex_responses" || n["api_key"] != "camel" {
		t.Fatalf("got %v", n)
	}
	if !reflect.DeepEqual(n["models"], map[string]any{"a": map[string]any{}, "b": map[string]any{"context_length": 1000}}) {
		t.Fatalf("models: %v", n["models"])
	}
	if !reflect.DeepEqual(n["capabilities"], map[string]bool{"vision": true}) || n["context_length"] != 4096 || n["ssl_verify"] != "false" {
		t.Fatalf("fields: %v", n)
	}
	v12 := customProviderEntryToProviderConfig(map[string]any{"name": "l", "base_url": "http://h/v1", "api_mode": "anthropic", "model": "m"}, "")
	if v12["api"] != "http://h/v1" || v12["transport"] != "anthropic_messages" || v12["default_model"] != "m" {
		t.Fatalf("v12: %v", v12)
	}
}

func TestCompatibleCustomProvidersDedupes(t *testing.T) {
	cfg := map[string]any{
		"custom_providers": []any{
			map[string]any{"name": "Local", "base_url": "http://h/v1/", "model": "m"},
			map[string]any{"name": "local", "base_url": "http://h/v1", "model": "m"},
			"garbage",
		},
		"providers": map[string]any{
			"keyed":    map[string]any{"api": "http://k/v1"},
			"disabled": map[string]any{"api": "http://d/v1", "enabled": false},
			"nourl":    map[string]any{"stale_timeout_seconds": 600},
		},
	}
	got := CompatibleCustomProviders(cfg)
	var names []string
	for _, e := range got {
		names = append(names, cfgString(e["name"]))
	}
	if !reflect.DeepEqual(names, []string{"Local", "keyed"}) {
		t.Fatalf("got %v", names)
	}
	if CustomProviderAPIMode("http://K/v1", nil, map[string]any{"providers": map[string]any{"k": map[string]any{"api": "http://k/v1", "transport": "anthropic"}}}) != "anthropic_messages" {
		t.Fatal("api mode by route")
	}
	if n := CustomProviderContextLength("m", "http://k/v1", nil, map[string]any{"providers": map[string]any{"k": map[string]any{"api": "http://k/v1", "context_length": 9000, "models": map[string]any{"m": map[string]any{"context_length": 100}}}}}); n != 100 {
		t.Fatalf("context length: %d", n)
	}
	if v, known := CustomProviderModelCapability("m", "http://k/v1", "vision", nil, map[string]any{"providers": map[string]any{"k": map[string]any{"api": "http://k/v1", "models": map[string]any{"m": map[string]any{"vision": false}}}}}); !known || v {
		t.Fatalf("capability: %v %v", v, known)
	}
	tls := CustomProviderTLSSettings("http://k/v1", nil, map[string]any{"providers": map[string]any{"k": map[string]any{"api": "http://k/v1", "ssl_verify": "no", "ssl_ca_cert": "/ca.pem"}}})
	if tls.CACert != "/ca.pem" || !tls.HasVerify || tls.Verify {
		t.Fatalf("tls: %+v", tls)
	}
	if CustomProviderSessionAffinityHeader("http://k/v1", nil, map[string]any{"providers": map[string]any{"k": map[string]any{"api": "http://k/v1", "session_affinity_header": " X-Session "}}}) != "X-Session" {
		t.Fatal("affinity header")
	}
}

func TestIsProviderEnabled(t *testing.T) {
	if !IsProviderEnabled(nil) || !IsProviderEnabled(map[string]any{}) || !IsProviderEnabled(map[string]any{"enabled": "yes"}) {
		t.Fatal("default enabled")
	}
	if IsProviderEnabled(map[string]any{"enabled": false}) || IsProviderEnabled(map[string]any{"enabled": "off"}) {
		t.Fatal("explicit false hides")
	}
}

func TestFindProviderEntryAndStringify(t *testing.T) {
	providers := map[string]any{"2070": map[string]any{"api": "http://x/v1"}}
	key, entry := FindProviderEntry(providers, 2070)
	if key != "2070" || entry == nil {
		t.Fatalf("got %q %v", key, entry)
	}
	if got := StringifyProviderMap(map[string]any{" a ": 1}); !reflect.DeepEqual(got, map[string]any{"a": 1}) {
		t.Fatalf("got %v", got)
	}
}

func TestCanonicalAPIMode(t *testing.T) {
	cases := map[string]string{"openai": "chat_completions", "Responses": "codex_responses", "anthropic": "anthropic_messages", "bedrock": "bedrock_converse", "weird": "weird", " chat_completions ": "chat_completions"}
	for raw, want := range cases {
		if got := CanonicalAPIMode(raw); got != want {
			t.Errorf("%q: got %q", raw, got)
		}
	}
}

// ---- provider env entries ----

func TestEveryAPIKeyProviderVarIsDeclaredInCore(t *testing.T) {
	FillProviderEnvVars()
	core := CoreDeclaredEnvNames()
	count := 0
	for _, p := range ListProviders(context.Background()) {
		if p.AuthType != AuthTypeAPIKey {
			continue
		}
		for _, v := range profileEnvVars(p) {
			count++
			if _, ok := config.OptionalEnvVars[v]; !ok {
				t.Errorf("%s not in OptionalEnvVars", v)
			}
			if !core[v] {
				t.Errorf("%s not core declared", v)
			}
		}
	}
	if count == 0 {
		t.Fatal("no api_key provider profiles registered")
	}
}

func TestProviderEnvEntriesCoreWinsFirstProfileWins(t *testing.T) {
	profiles := []Profile{
		{Name: "alpha", DisplayName: "Alpha", AuthType: AuthTypeAPIKey, KeyEnv: []string{"CORE_KEY", "ALPHA_API_KEY", "ALPHA_BASE_URL"}, SignupURL: "https://alpha.test/keys"},
		{Name: "beta", DisplayName: "Beta", AuthType: AuthTypeAPIKey, KeyEnv: []string{"ALPHA_API_KEY"}},
		{Name: "oauth", DisplayName: "Oauth", AuthType: AuthTypeOAuthDeviceCode, KeyEnv: []string{"OAUTH_TOKEN"}},
	}
	entries := providerEnvEntriesFor(profiles, map[string]bool{"CORE_KEY": true})
	if _, has := entries["CORE_KEY"]; has {
		t.Fatal("core name must be skipped")
	}
	if e := entries["ALPHA_API_KEY"]; e.Description != "Alpha API key" || !e.Password || e.URL != "https://alpha.test/keys" {
		t.Fatalf("alpha key: %+v", e)
	}
	if e := entries["ALPHA_BASE_URL"]; e.Password || e.Description != "Alpha base URL override" {
		t.Fatalf("alpha url: %+v", e)
	}
	if _, has := entries["OAUTH_TOKEN"]; has {
		t.Fatal("oauth profile vars are not api key entries")
	}
	SyncRegisteredProviders([]Profile{{Name: "plug", DisplayName: "Plug", AuthType: AuthTypeAPIKey, KeyEnv: []string{"PLUG_TEST_API_KEY", "OPENAI_API_KEY"}}})
	if e, ok := config.OptionalEnvVars["PLUG_TEST_API_KEY"]; !ok || e.Description != "Plug API key" {
		t.Fatalf("plugin entry: %+v %v", e, ok)
	}
	if config.OptionalEnvVars["OPENAI_API_KEY"].Description == "Plug API key" {
		t.Fatal("an earlier entry is kept")
	}
}

// ---- env routing ----

func TestIsEnvSettingKey(t *testing.T) {
	for _, key := range []string{"FEISHU_HOME_CHANNEL", "DISCORD_ALLOWED_USERS", "TELEGRAM_GROUP_ALLOWED_USERS", "ICEBATH_TIMEZONE", "SOME_PLUGIN_ONLY_KNOB", "discord_home_channel"} {
		if !IsEnvSettingKey(key) {
			t.Errorf("%s should route to .env", key)
		}
	}
	for _, key := range []string{"my_custom_flag", "model.default", "WHATSAPP.MODE"} {
		if IsEnvSettingKey(key) {
			t.Errorf("%s should stay in config.yaml", key)
		}
	}
	if !IsSetupHiddenEnv("IRC_HOME_CHANNEL") || IsSetupHiddenEnv("IRC_SERVER") {
		t.Fatal("hidden suffixes")
	}
}

func TestEnvSettingRoundTripDropsConfigYAMLCopies(t *testing.T) {
	h := t.TempDir()
	if err := os.WriteFile(filepath.Join(h, "config.yaml"), []byte("model:\n  default: test/model\nDISCORD_ALLOWED_USERS: '111'\nWHATSAPP_MODE: web\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := SaveEnvSetting(context.Background(), h, "DISCORD_ALLOWED_USERS", "222"); err != nil {
		t.Fatal(err)
	}
	env, _ := os.ReadFile(filepath.Join(h, ".env"))
	if !strings.Contains(string(env), "DISCORD_ALLOWED_USERS=222") {
		t.Fatalf("env: %s", env)
	}
	doc, err := config.ReadUser(h)
	if err != nil {
		t.Fatal(err)
	}
	if _, stale := doc["DISCORD_ALLOWED_USERS"]; stale {
		t.Fatal("config.yaml copy not dropped")
	}
	if doc["WHATSAPP_MODE"] != "web" {
		t.Fatalf("unrelated key touched: %v", doc)
	}
	if v, ok := ReadEnvSetting(h, "DISCORD_ALLOWED_USERS"); !ok || v != "222" {
		t.Fatalf("read: %q %v", v, ok)
	}
	if v, ok := ReadEnvSetting(h, "WHATSAPP_MODE"); !ok || v != "web" {
		t.Fatalf("stale yaml copy read: %q %v", v, ok)
	}
	removed, err := RemoveEnvSetting(context.Background(), h, "WHATSAPP_MODE")
	if err != nil || !removed {
		t.Fatalf("remove stale copy: %v %v", removed, err)
	}
	if _, ok := ReadEnvSetting(h, "WHATSAPP_MODE"); ok {
		t.Fatal("still readable")
	}
	removed, err = RemoveEnvSetting(context.Background(), h, "DISCORD_ALLOWED_USERS")
	if err != nil || !removed {
		t.Fatalf("remove env: %v %v", removed, err)
	}
	removed, _ = RemoveEnvSetting(context.Background(), h, "NEVER_SET")
	if removed {
		t.Fatal("nothing existed")
	}
}

// ---- provider catalog ----

func TestProfilelessProvidersStillPresent(t *testing.T) {
	by := ProviderCatalogBySlug()
	for _, slug := range []string{"openai-api", "tencent-tokenplan", "xai-oauth"} {
		d, ok := by[slug]
		if !ok {
			t.Fatalf("%s dropped from catalog", slug)
		}
		if d.Label == "" || d.Description == "" {
			t.Fatalf("%s empty label or description: %+v", slug, d)
		}
	}
	if by["xai-oauth"].Tab != "accounts" || by["openai-api"].Tab != "keys" {
		t.Fatalf("tabs: %+v %+v", by["xai-oauth"], by["openai-api"])
	}
}

func TestAPIKeyProvidersExposeACredentialEnvVar(t *testing.T) {
	for _, d := range ProviderCatalog() {
		if d.AuthType == AuthTypeAPIKey && d.Slug != "custom" && len(d.APIKeyEnvVars) == 0 {
			t.Errorf("%s is api_key but exposes no env var", d.Slug)
		}
	}
	if TabForAuthType(AuthTypeExternalProcess) != "accounts" || TabForAuthType(AuthTypeAWSSDK) != "keys" {
		t.Fatal("tab mapping")
	}
}

func TestEntriesForRouteMatchesOnlyTheNormalizedRoute(t *testing.T) {
	entries := []map[string]any{{"name": "local", "base_url": "http://localhost:8000/v1/"}, {"name": "other", "base_url": "http://example.com/v1"}}
	got := EntriesForRoute("http://localhost:8000/v1", entries, nil)
	if len(got) != 1 || got[0]["name"] != "local" {
		t.Fatalf("EntriesForRoute = %v", got)
	}
	if got := EntriesForRoute("", entries, nil); len(got) != 0 {
		t.Fatalf("blank base url matched %v", got)
	}
}
