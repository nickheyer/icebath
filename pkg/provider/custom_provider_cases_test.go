package provider

import (
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/config"
)

func TestNormalizeExtraHeadersStringifiesAndDropsNone(t *testing.T) {
	got := NormalizeExtraHeaders(map[string]any{"X-Int": 7, "X-Str": "v", "X-None": nil})
	if !reflect.DeepEqual(got, map[string]string{"X-Int": "7", "X-Str": "v"}) {
		t.Fatalf("got %v", got)
	}
}

func TestNormalizeExtraHeadersRejectsNonDictAndEmpty(t *testing.T) {
	for _, bad := range []any{nil, "x", 42, []any{"a"}, map[string]any{}} {
		if got := NormalizeExtraHeaders(bad); got == nil || len(got) != 0 {
			t.Fatalf("%#v -> %#v, want an empty mapping", bad, got)
		}
	}
}

func TestNormalizeEntryDropsInvalidExtraHeaders(t *testing.T) {
	for _, bad := range []any{"not-a-dict", map[string]any{}, 42, []any{"a"}} {
		n := NormalizeCustomProviderEntry(map[string]any{"name": "my-proxy", "base_url": "https://llm.internal.example.com/v1", "extra_headers": bad}, "")
		if n == nil {
			t.Fatalf("%#v dropped the entry", bad)
		}
		if _, has := n["extra_headers"]; has {
			t.Fatalf("%#v kept extra_headers", bad)
		}
	}
}

func TestGetCustomProviderExtraHeadersMatchesBaseUrl(t *testing.T) {
	providers := []map[string]any{{"name": "my-proxy", "base_url": "https://llm.internal.example.com/v1", "extra_headers": map[string]any{"CF-Access-Client-Id": "xxxx.access"}}}
	got := CustomProviderExtraHeaders("https://LLM.internal.example.com/v1/", providers, nil)
	if !reflect.DeepEqual(got, map[string]string{"CF-Access-Client-Id": "xxxx.access"}) {
		t.Fatalf("got %v", got)
	}
}

func TestGetCustomProviderExtraHeadersNoMatchReturnsEmpty(t *testing.T) {
	providers := []map[string]any{{"name": "my-proxy", "base_url": "https://llm.internal.example.com/v1", "extra_headers": map[string]any{"X-Secret": "s"}}}
	for _, url := range []string{"https://other.example.com/v1", "https://llm.internal.example.com.attacker.test/v1"} {
		if got := CustomProviderExtraHeaders(url, providers, nil); got == nil || len(got) != 0 {
			t.Fatalf("%s -> %#v, want an empty mapping", url, got)
		}
	}
}

func TestGetCustomProviderExtraHeadersPreservesExtraPathSegment(t *testing.T) {
	providers := []map[string]any{{"base_url": "https://llm.internal.example.com/v1//", "extra_headers": map[string]any{"Authorization": "secret"}}}
	if got := CustomProviderExtraHeaders("https://llm.internal.example.com/v1", providers, nil); got == nil || len(got) != 0 {
		t.Fatalf("got %#v", got)
	}
}

func TestGetCustomProviderExtraHeadersSkipsAliasWithoutHeaders(t *testing.T) {
	providers := []map[string]any{
		{"name": "direct", "base_url": "http://127.0.0.1:8787/v1"},
		{"name": "aether-router", "base_url": "http://127.0.0.1:8787/v1", "extra_headers": map[string]any{"X-Aether-Route": "my-route"}},
	}
	if got := CustomProviderExtraHeaders("http://127.0.0.1:8787/v1", providers, nil); !reflect.DeepEqual(got, map[string]string{"X-Aether-Route": "my-route"}) {
		t.Fatalf("got %v", got)
	}
}

func TestGetCustomProviderExtraHeadersSkipsEmptyHeaderDictAlias(t *testing.T) {
	providers := []map[string]any{
		{"name": "bare-alias", "base_url": "http://127.0.0.1:8787/v1", "extra_headers": map[string]any{}},
		{"name": "proxied", "base_url": "http://127.0.0.1:8787/v1", "extra_headers": map[string]any{"Authorization": "Bearer tok"}},
	}
	if got := CustomProviderExtraHeaders("http://127.0.0.1:8787/v1", providers, nil); !reflect.DeepEqual(got, map[string]string{"Authorization": "Bearer tok"}) {
		t.Fatalf("got %v", got)
	}
}

func TestApplyExtraHeadersMergesOntoExistingDefaults(t *testing.T) {
	defaults := map[string]string{"User-Agent": "curl/8.7.1", "X-Keep": "1"}
	providers := []map[string]any{{"name": "my-proxy", "base_url": "https://llm.internal.example.com/v1", "extra_headers": map[string]any{"User-Agent": "override", "X-New": "2"}}}
	got := MergeExtraHeaders(defaults, "https://llm.internal.example.com/v1", providers, nil)
	if !reflect.DeepEqual(got, map[string]string{"User-Agent": "override", "X-Keep": "1", "X-New": "2"}) {
		t.Fatalf("got %v", got)
	}
	if !reflect.DeepEqual(defaults, map[string]string{"User-Agent": "curl/8.7.1", "X-Keep": "1"}) {
		t.Fatalf("defaults mutated: %v", defaults)
	}
}

func TestApplyExtraHeadersNoopWithoutMatch(t *testing.T) {
	providers := []map[string]any{{"name": "my-proxy", "base_url": "https://llm.internal.example.com/v1", "extra_headers": map[string]any{"X-Secret": "s"}}}
	if got := MergeExtraHeaders(nil, "https://other.example.com/v1", providers, nil); got != nil {
		t.Fatalf("default headers created without a match: %v", got)
	}
}

func TestFetchApiModelsSendsExtraHeadersToModelsProbe(t *testing.T) {
	var path string
	var headers http.Header
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path, headers = r.URL.Path, r.Header.Clone()
		w.Write([]byte(`{"data": [{"id": "proxy-model"}]}`))
	}))
	defer srv.Close()
	models, ok := FetchAPIModels(t.Context(), "proxy-key", srv.URL+"/v1", 5*time.Second, "", map[string]string{
		"sleeve-harness": "hermes", "sleeve-base-url": "http://localhost:8081/v1",
	})
	if !ok || !reflect.DeepEqual(models, []string{"proxy-model"}) {
		t.Fatalf("models %v %v", models, ok)
	}
	if path != "/v1/models" {
		t.Fatalf("probe path %q", path)
	}
	if headers.Get("Authorization") != "Bearer proxy-key" || headers.Get("sleeve-harness") != "hermes" || headers.Get("sleeve-base-url") != "http://localhost:8081/v1" {
		t.Fatalf("headers %v", headers)
	}
}

func TestNormalizerDoesNotMutateEntryApiKeyEnv(t *testing.T) {
	entry := map[string]any{"base_url": "https://x.example/v1", "api_key_env": "MY_KEY"}
	snapshot := config.Clone(entry)
	out := NormalizeCustomProviderEntry(entry, "p")
	if !reflect.DeepEqual(entry, snapshot) {
		t.Fatalf("input entry gained keys: %v", entry)
	}
	if out == nil || out["key_env"] != "MY_KEY" {
		t.Fatalf("out %v", out)
	}
}

func TestNormalizerDoesNotMutateEntryCamelAliases(t *testing.T) {
	entry := map[string]any{"baseUrl": "https://x.example/v1", "apiKey": "sk-test-not-real"}
	snapshot := config.Clone(entry)
	NormalizeCustomProviderEntry(entry, "p")
	if !reflect.DeepEqual(entry, snapshot) {
		t.Fatalf("camelCase aliasing wrote into the input: %v", entry)
	}
}

func TestProvidersDictRoundtripLeavesCachedConfigUntouched(t *testing.T) {
	cfg := map[string]any{"providers": map[string]any{"myprov": map[string]any{
		"base_url": "https://x.example/v1", "api_key_env": "MY_KEY", "models": map[string]any{"m1": map[string]any{"context_length": 8192}},
	}}}
	snapshot := config.Clone(cfg)
	ProvidersDictToCustomProviders(cfg["providers"])
	if !reflect.DeepEqual(cfg, snapshot) {
		t.Fatalf("providers view mutated the config: %v", cfg)
	}
	CompatibleCustomProviders(cfg)
	if !reflect.DeepEqual(cfg, snapshot) {
		t.Fatalf("compatible view mutated the config: %v", cfg)
	}
}

func TestNormalizedModelsMappingIsNotSharedWithInput(t *testing.T) {
	entry := map[string]any{"base_url": "https://x.example/v1", "api_key": "sk-test-not-real", "models": map[string]any{"m1": map[string]any{}}}
	out := NormalizeCustomProviderEntry(entry, "p")
	if out == nil {
		t.Fatal("entry dropped")
	}
	out["models"].(map[string]any)["injected"] = map[string]any{}
	if _, has := entry["models"].(map[string]any)["injected"]; has {
		t.Fatal("normalized models mapping aliases the caller's dict")
	}
}

func TestGetCustomProviderTlsSettingsMatchesBaseUrl(t *testing.T) {
	providers := []map[string]any{{"name": "Ollama", "base_url": "https://ollama.example.com/v1", "ssl_ca_cert": "/etc/ssl/mkcert-root.pem"}}
	tls := CustomProviderTLSSettings("https://ollama.example.com/v1/", providers, nil)
	if tls != (TLSSettings{CACert: "/etc/ssl/mkcert-root.pem"}) {
		t.Fatalf("tls %+v", tls)
	}
}

func TestApplyCustomProviderTlsToClientKwargs(t *testing.T) {
	clientKwargs := map[string]any{"api_key": "x", "base_url": "https://ollama.example.com/v1"}
	providers := []map[string]any{{"name": "Ollama", "base_url": "https://ollama.example.com/v1", "ssl_ca_cert": "/etc/ssl/mkcert-root.pem", "ssl_verify": true}}
	ApplyCustomProviderTLSToClientKwargs(clientKwargs, "https://ollama.example.com/v1", providers, nil)
	if clientKwargs["ssl_ca_cert"] != "/etc/ssl/mkcert-root.pem" || clientKwargs["ssl_verify"] != true {
		t.Fatalf("client kwargs %v", clientKwargs)
	}
	bare := map[string]any{"api_key": "x"}
	ApplyCustomProviderTLSToClientKwargs(bare, "https://other.example.com/v1", providers, nil)
	if len(bare) != 1 {
		t.Fatalf("kwargs of an unmatched route %v", bare)
	}
}

func TestNormalizeCustomProviderEntry(t *testing.T) {
	reset := func(t *testing.T) {
		resetProviderNormalizeWarnings()
		t.Cleanup(resetProviderNormalizeWarnings)
	}
	t.Run("test_unknown_keys_warned_once_per_signature", func(t *testing.T) {
		reset(t)
		log := captureDefaultLog(t)
		for range 5 {
			NormalizeCustomProviderEntry(map[string]any{"base_url": "https://api.example.com/v1", "api_key": "***", "unknownField": "value"}, "test")
		}
		if n := strings.Count(strings.ToLower(log.String()), "unknown config keys"); n != 1 {
			t.Fatalf("%d unknown key warnings: %q", n, log.String())
		}
	})
	t.Run("test_env_var_placeholder_in_base_url_not_rejected", func(t *testing.T) {
		reset(t)
		result := NormalizeCustomProviderEntry(map[string]any{"name": "PROVIDER_A", "base_url": "${PROVIDER_A_BASE_URL}", "key_env": "PROVIDER_A_API_KEY"}, "PROVIDER_A")
		if result == nil || result["base_url"] != "${PROVIDER_A_BASE_URL}" {
			t.Fatalf("result %v", result)
		}
	})
	t.Run("test_catalog_provider_is_a_known_key_and_survives_normalization", func(t *testing.T) {
		reset(t)
		log := captureDefaultLog(t)
		result := NormalizeCustomProviderEntry(map[string]any{"base_url": "https://gw.example.com/v1", "key_env": "GW_KEY", "catalog_provider": "deepseek"}, "925llm")
		if result == nil || result["catalog_provider"] != "deepseek" {
			t.Fatalf("result %v", result)
		}
		if strings.Contains(strings.ToLower(log.String()), "unknown config keys") {
			t.Fatalf("log %q", log.String())
		}
	})
	t.Run("test_numeric_yaml_name_and_key_become_strings", func(t *testing.T) {
		reset(t)
		result := NormalizeCustomProviderEntry(map[string]any{"name": 2070, "base_url": "http://192.168.1.10:8082/v1"}, CoerceProviderID(2070))
		if result == nil || result["name"] != "2070" || result["provider_key"] != "2070" {
			t.Fatalf("result %v", result)
		}
		doc, err := config.SafeLoad("2070:\n  base_url: http://x\n")
		if err != nil {
			t.Fatal(err)
		}
		mapped := StringifyProviderMap(doc)
		if len(mapped) != 1 || mapped["2070"] == nil {
			t.Fatalf("mapped %v", mapped)
		}
		stored, entry := FindProviderEntry(doc, "2070")
		if stored != config.PyKeyString(2070) || !reflect.DeepEqual(entry, map[string]any{"base_url": "http://x"}) {
			t.Fatalf("stored %q entry %v", stored, entry)
		}
		if stored, entry := FindProviderEntry(doc, 2070); stored != config.PyKeyString(2070) || entry == nil {
			t.Fatalf("numeric lookup stored %q entry %v", stored, entry)
		}
	})
}
