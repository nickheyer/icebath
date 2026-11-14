package provider

import (
	"context"
	"maps"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/config"
)

func TestNormalizeEntryKeepsExtraHeaders(t *testing.T) {
	n := NormalizeCustomProviderEntry(map[string]any{
		"name": "my-proxy", "base_url": "https://llm.internal.example.com/v1",
		"extra_headers": map[string]any{"X-Custom-Auth": "tok", "X-Client-Name": "hermes"},
	}, "")
	if n == nil || !reflect.DeepEqual(n["extra_headers"], map[string]string{"X-Custom-Auth": "tok", "X-Client-Name": "hermes"}) {
		t.Fatalf("got %v", n)
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
	t.Setenv("ICEBATH_HOME", t.TempDir())
	FillProviderEnvVars()
	core, err := config.CoreDeclaredEnvNames()
	if err != nil {
		t.Fatal(err)
	}
	count := 0
	for _, p := range ListProviders(context.Background()) {
		if p.AuthType != AuthTypeAPIKey {
			continue
		}
		for _, v := range profileEnvVars(p) {
			count++
			if !config.OptionalEnvVars.Has(v) {
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

// Profile double of the hermes cases with a title cased display name and the env vars in tuple order
func envCaseProfile(name, authType string, envVars ...string) Profile {
	return Profile{Name: name, DisplayName: strings.ToUpper(name[:1]) + name[1:], AuthType: authType, KeyEnv: envVars,
		SignupURL: "https://" + name + ".test/keys"}
}

// Table of the hermes fixture over one core name and one platform name, armed with the provider fill
func providerEnvCaseTable(t *testing.T) *config.OptionalEnvTable {
	t.Helper()
	providerEnvMu.Lock()
	prevStatic, prevNames, prevCatalog, prevSet := providerEnvStatic, providerEnvNames, providerEnvCatalog, providerEnvCatalogSet
	prevTable, prevBundled := providerEnvTable, providerEnvBundled
	providerEnvStatic, providerEnvNames, providerEnvCatalog, providerEnvCatalogSet = map[string]bool{"CORE_KEY": true}, map[string]bool{}, nil, false
	providerEnvMu.Unlock()
	t.Cleanup(func() {
		providerEnvMu.Lock()
		providerEnvStatic, providerEnvNames, providerEnvCatalog, providerEnvCatalogSet = prevStatic, prevNames, prevCatalog, prevSet
		providerEnvTable, providerEnvBundled = prevTable, prevBundled
		providerEnvMu.Unlock()
	})
	table := config.NewOptionalEnvTable([]config.NamedEnvVar{
		{Name: "CORE_KEY", Entry: config.EnvVar{Description: "core"}}, {Name: "PLUG_KEY", Entry: config.EnvVar{Description: "platform"}}})
	table.Arm(ProviderEnvEntries, publishProviderEnvNames)
	return table
}

func TestTheCatalogBuildsUnderABackendThatRefusesEveryRead(t *testing.T) {
	reads := refuseConfigReads(t)
	providerEnvMu.Lock()
	prevCatalog, prevSet := providerEnvCatalog, providerEnvCatalogSet
	providerEnvCatalog, providerEnvCatalogSet = nil, false
	providerEnvMu.Unlock()
	t.Cleanup(func() {
		providerEnvMu.Lock()
		providerEnvCatalog, providerEnvCatalogSet = prevCatalog, prevSet
		providerEnvMu.Unlock()
	})
	entries := ProviderEnvEntries()
	hasAnthropic := slices.ContainsFunc(entries, func(e config.NamedEnvVar) bool { return e.Name == "ANTHROPIC_API_KEY" })
	if !hasAnthropic && len(entries) <= 10 {
		t.Fatalf("catalog of %d entries", len(entries))
	}
	if got := reads.list(); len(got) != 0 {
		t.Fatalf("config reads %v", got)
	}
}

func TestFillRunsOnFirstReadCoreWinsProviderReplacesPlatformFirstProviderWins(t *testing.T) {
	table := providerEnvCaseTable(t)
	calls := 0
	profiles := []Profile{envCaseProfile("alpha", AuthTypeAPIKey, "CORE_KEY", "PLUG_KEY", "ALPHA_API_KEY", "ALPHA_BASE_URL"),
		envCaseProfile("beta", AuthTypeAPIKey, "ALPHA_API_KEY"), envCaseProfile("oauth", "oauth", "OAUTH_TOKEN")}
	providerEnvMu.Lock()
	providerEnvBundled = func() []Profile {
		calls++
		return profiles
	}
	providerEnvMu.Unlock()
	table.Set("LATE", config.EnvVar{})
	if _, ok := table.RawGet("LATE"); !ok || !slices.Equal(table.RawNames(), []string{"CORE_KEY", "PLUG_KEY", "LATE"}) || calls != 0 {
		t.Fatalf("writes and raw access never fill, calls %d names %v", calls, table.RawNames())
	}
	if e, ok := table.Get("CORE_KEY"); !ok || !reflect.DeepEqual(e, config.EnvVar{Description: "core"}) || calls != 1 {
		t.Fatalf("core entry %+v %v calls %d", e, ok, calls)
	}
	if got := table.Entry("PLUG_KEY").Description; got != "Alpha API key" {
		t.Fatalf("provider entry replaces the platform one, got %q", got)
	}
	if e := table.Entry("ALPHA_API_KEY"); e.Description != "Alpha API key" || !e.Password || e.URL != "https://alpha.test/keys" ||
		e.Prompt != "Alpha API key" || e.Category != "provider" || !e.Advanced {
		t.Fatalf("alpha key %+v", e)
	}
	if e := table.Entry("ALPHA_BASE_URL"); e.Password || e.Description != "Alpha base URL override" ||
		e.Prompt != "Alpha base URL (leave empty for default)" || table.Has("OAUTH_TOKEN") {
		t.Fatalf("alpha url %+v oauth %v", e, table.Has("OAUTH_TOKEN"))
	}
	items := table.Items()
	if calls != 1 {
		t.Fatalf("fill ran %d times", calls)
	}
	names := make([]string, len(items))
	for i, item := range items {
		names[i] = item.Name
	}
	if want := []string{"CORE_KEY", "PLUG_KEY", "LATE", "ALPHA_API_KEY", "ALPHA_BASE_URL"}; !slices.Equal(names, want) || !slices.Equal(table.Names(), want) {
		t.Fatalf("insertion order %v", names)
	}
	providerEnvMu.Lock()
	published := maps.Clone(providerEnvNames)
	providerEnvMu.Unlock()
	if !maps.Equal(published, map[string]bool{"PLUG_KEY": true, "ALPHA_API_KEY": true, "ALPHA_BASE_URL": true}) {
		t.Fatalf("published names %v", published)
	}
}

func TestDiscoveryCompletionAddsRegisteredPluginProviders(t *testing.T) {
	table := providerEnvCaseTable(t)
	alpha := envCaseProfile("alpha", AuthTypeAPIKey, "ALPHA_API_KEY")
	plug := envCaseProfile("plug", AuthTypeAPIKey, "PLUG_API_KEY", "ALPHA_API_KEY")
	providerEnvMu.Lock()
	providerEnvBundled = func() []Profile { return []Profile{alpha} }
	providerEnvTable = table
	providerEnvMu.Unlock()
	regMu.Lock()
	prevProfiles, prevOrder, prevSources := regProfiles, regOrder, regSources
	regProfiles, regOrder = map[string]*Profile{"alpha": &alpha, "plug": &plug}, []string{"alpha", "plug"}
	regSources = map[string]string{"alpha": "bundled", "plug": "runtime"}
	regMu.Unlock()
	t.Cleanup(func() {
		regMu.Lock()
		regProfiles, regOrder, regSources = prevProfiles, prevOrder, prevSources
		regMu.Unlock()
	})
	if !table.Has("ALPHA_API_KEY") {
		t.Fatal("the first read runs the fill")
	}
	SyncRegisteredProviders(processWideRegisteredProfiles())
	if got := table.Entry("PLUG_API_KEY").Description; got != "Plug API key" {
		t.Fatalf("plugin entry %q", got)
	}
	if got := table.Entry("ALPHA_API_KEY").Description; got != "Alpha API key" {
		t.Fatalf("an earlier entry is kept, got %q", got)
	}
	providerEnvMu.Lock()
	named := providerEnvNames["PLUG_API_KEY"]
	providerEnvMu.Unlock()
	if !named {
		t.Fatal("the plugin name joins the provider declared names")
	}
}

func TestSyncBeforeTheFillLetsTheBundledEntryReplaceThePluginOneInPlace(t *testing.T) {
	table := providerEnvCaseTable(t)
	alpha := envCaseProfile("alpha", AuthTypeAPIKey, "ALPHA_API_KEY", "ALPHA_BASE_URL")
	providerEnvMu.Lock()
	providerEnvBundled = func() []Profile { return []Profile{alpha} }
	providerEnvTable = table
	providerEnvMu.Unlock()
	SyncRegisteredProviders([]Profile{envCaseProfile("plug", AuthTypeAPIKey, "ALPHA_BASE_URL", "PLUG_API_KEY")})
	if got := table.RawNames(); !slices.Equal(got, []string{"CORE_KEY", "PLUG_KEY", "ALPHA_BASE_URL", "PLUG_API_KEY"}) {
		t.Fatalf("a sync never runs the fill, names %v", got)
	}
	if e, _ := table.RawGet("ALPHA_BASE_URL"); e.Description != "Plug base URL override" {
		t.Fatalf("plugin entry before the fill %+v", e)
	}
	if got := table.Names(); !slices.Equal(got, []string{"CORE_KEY", "PLUG_KEY", "ALPHA_BASE_URL", "PLUG_API_KEY", "ALPHA_API_KEY"}) {
		t.Fatalf("names after the fill %v", got)
	}
	if got := table.Entry("ALPHA_BASE_URL").Description; got != "Alpha base URL override" {
		t.Fatalf("the bundled entry replaces the plugin one, got %q", got)
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
	t.Setenv("ICEBATH_HOME", h)
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
	t.Setenv("ICEBATH_HOME", t.TempDir())
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
	t.Setenv("ICEBATH_HOME", t.TempDir())
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
