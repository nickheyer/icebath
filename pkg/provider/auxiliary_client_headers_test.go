package provider

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/nickheyer/icebath/pkg/config"
)

func TestToOpenaiBaseUrl(t *testing.T) {
	check := func(in, want string) func(t *testing.T) {
		return func(t *testing.T) {
			if got := auxToOpenAIBaseURL(in); got != want {
				t.Fatalf("%q got %q want %q", in, got, want)
			}
		}
	}
	t.Run("test_minimax_global_anthropic_suffix_replaced", check("https://api.minimax.io/anthropic", "https://api.minimax.io/v1"))
	t.Run("test_minimax_chat_host_rewritten", check("https://api.minimax.chat/anthropic", "https://api.minimax.chat/v1"))
	t.Run("test_anthropic_only_custom_gateway_kept", check("https://token-plan.cn-beijing.maas.aliyuncs.com/apps/anthropic", "https://token-plan.cn-beijing.maas.aliyuncs.com/apps/anthropic"))
	t.Run("test_marker_in_path_does_not_false_positive", check("https://gateway.example.com/api.minimax.io/anthropic", "https://gateway.example.com/api.minimax.io/anthropic"))
	t.Run("test_minimax_cn_api_prefix_host_rewritten", check("https://api.minimaxi.com/anthropic", "https://api.minimaxi.com/v1"))
	t.Run("test_lookalike_host_suffix_not_matched", check("https://minimax.io.evil.example.com/anthropic", "https://minimax.io.evil.example.com/anthropic"))
	t.Run("test_zai_anthropic_routes_to_coding_plan_openai_endpoint", check("https://api.z.ai/api/anthropic", "https://api.z.ai/api/coding/paas/v4"))
	t.Run("test_bigmodel_anthropic_routes_to_coding_plan_openai_endpoint", check("https://open.bigmodel.cn/api/anthropic", "https://open.bigmodel.cn/api/coding/paas/v4"))
	t.Run("test_bigmodel_marker_in_path_does_not_false_positive", check("https://gateway.example.com/proxy/bigmodel-fallback/anthropic", "https://gateway.example.com/proxy/bigmodel-fallback/anthropic"))
	t.Run("test_zai_marker_in_path_does_not_false_positive", check("https://gateway.example.com/api.z.ai-mirror/anthropic", "https://gateway.example.com/api.z.ai-mirror/anthropic"))
	t.Run("test_kimi_coding_host_rewritten", check("https://api.kimi.com/coding", "https://api.kimi.com/coding/v1"))
	t.Run("test_kimi_marker_in_path_does_not_false_positive", check("https://gateway.example.com/some/api.kimi.com-proxy/coding", "https://gateway.example.com/some/api.kimi.com-proxy/coding"))
	t.Run("test_none", check("", ""))
}
func TestToOpenAIBaseURLActualBaseURLsNormalizeThroughTheActualProvider(t *testing.T) {
	if auxToOpenAIBaseURL("https://api.actual.inc/v1/") != NormalizeActualBaseURL("https://api.actual.inc/v1") {
		t.Fatal("actual base urls normalize through the actual provider")
	}
}

func auxORHeadersEnv(t *testing.T) {
	t.Helper()
	t.Setenv("ICEBATH_OPENROUTER_CACHE", "")
	t.Setenv("ICEBATH_OPENROUTER_CACHE_TTL", "")
	t.Setenv("ICEBATH_HOME", t.TempDir())
}

func TestBuildORHeadersConfigAndEnvPrecedence(t *testing.T) {
	auxORHeadersEnv(t)
	t.Run("test_ttl_negative", func(t *testing.T) {
		h, err := BuildORHeaders(context.Background(), map[string]any{"response_cache": true, "response_cache_ttl": -5})
		if err != nil || h["X-OpenRouter-Cache"] != "true" || h["X-OpenRouter-Cache-TTL"] != "" {
			t.Fatalf("negative ttl %v %v", h, err)
		}
	})
	t.Run("test_returns_fresh_dict", func(t *testing.T) {
		cfg := map[string]any{"response_cache": true, "response_cache_ttl": 600}
		h1, _ := BuildORHeaders(context.Background(), cfg)
		h2, _ := BuildORHeaders(context.Background(), cfg)
		if !reflect.DeepEqual(h1, h2) || h1["X-OpenRouter-Cache-TTL"] != "600" {
			t.Fatalf("fresh equal dicts %v %v", h1, h2)
		}
		h1["only-here"] = "1"
		if _, shared := h2["only-here"]; shared {
			t.Fatal("each call returns its own map")
		}
	})
	t.Run("test_none_config_load_config_fails_gracefully", func(t *testing.T) {
		oldLoad := auxLoadConfigReadonlyFn
		t.Cleanup(func() { auxLoadConfigReadonlyFn = oldLoad })
		auxLoadConfigReadonlyFn = func(context.Context) (config.Tree, error) { return nil, errors.New("boom") }
		h, err := BuildORHeaders(context.Background(), nil)
		if err != nil || h["HTTP-Referer"] == "" || h["X-OpenRouter-Categories"] != "productivity,cli-agent" || h["X-OpenRouter-Cache"] != "" {
			t.Fatalf("disk default %v", h)
		}
	})
	t.Run("test_invalid_env_ttl_dropped", func(t *testing.T) {
		for _, ttl := range []string{"0", "86401", "abc", "-1", "12.5"} {
			t.Setenv("ICEBATH_OPENROUTER_CACHE", "1")
			t.Setenv("ICEBATH_OPENROUTER_CACHE_TTL", ttl)
			h, err := BuildORHeaders(context.Background(), map[string]any{})
			if err != nil || h["X-OpenRouter-Cache"] != "true" || h["X-OpenRouter-Cache-TTL"] != "" {
				t.Fatalf("ttl %q kept %v %v", ttl, h, err)
			}
		}
	})
	t.Run("test_valid_env_ttl_boundaries", func(t *testing.T) {
		for _, ttl := range []string{"1", "300", "86400"} {
			t.Setenv("ICEBATH_OPENROUTER_CACHE", "yes")
			t.Setenv("ICEBATH_OPENROUTER_CACHE_TTL", ttl)
			if h, _ := BuildORHeaders(context.Background(), map[string]any{}); h["X-OpenRouter-Cache-TTL"] != ttl {
				t.Fatalf("ttl %q got %v", ttl, h)
			}
		}
	})
	t.Run("test_no_env_vars_falls_through_to_config", func(t *testing.T) {
		for _, name := range []string{"ICEBATH_OPENROUTER_CACHE", "ICEBATH_OPENROUTER_CACHE_TTL"} {
			if prev, had := os.LookupEnv(name); had {
				t.Cleanup(func() { os.Setenv(name, prev) })
				os.Unsetenv(name)
			}
		}
		h, err := BuildORHeaders(context.Background(), map[string]any{"response_cache": true, "response_cache_ttl": 600})
		if err != nil || h["X-OpenRouter-Cache"] != "true" || h["X-OpenRouter-Cache-TTL"] != "600" {
			t.Fatalf("config controls %v %v", h, err)
		}
	})
	t.Setenv("ICEBATH_OPENROUTER_CACHE", "yes")
	t.Setenv("ICEBATH_OPENROUTER_CACHE_TTL", "²")
	if _, err := BuildORHeaders(context.Background(), map[string]any{}); err == nil || err.Error() != "invalid literal for int() with base 10: '²'" {
		t.Fatalf("superscript ttl err %v", err)
	}
	t.Setenv("ICEBATH_OPENROUTER_CACHE", "off")
	t.Setenv("ICEBATH_OPENROUTER_CACHE_TTL", "")
	if h, _ := BuildORHeaders(context.Background(), map[string]any{"response_cache": true}); h["X-OpenRouter-Cache"] != "" {
		t.Fatal("the env switch wins over config")
	}
}

func TestBuildORHeadersReadsDiskConfig(t *testing.T) {
	auxORHeadersEnv(t)
	dir := os.Getenv("ICEBATH_HOME")
	_ = os.WriteFile(filepath.Join(dir, "config.yaml"), []byte("openrouter:\n  response_cache: true\n  response_cache_ttl: 120\n"), 0o600)
	h, err := BuildORHeaders(context.Background(), nil)
	if err != nil || h["X-OpenRouter-Cache"] != "true" || h["X-OpenRouter-Cache-TTL"] != "120" {
		t.Fatalf("disk config %v %v", h, err)
	}
}

func TestApplyUserDefaultHeadersAliasWins(t *testing.T) {
	auxORHeadersEnv(t)
	dir := os.Getenv("ICEBATH_HOME")
	base := map[string]string{"User-Agent": "sdk", "X-Keep": "k"}
	if got := auxApplyUserDefaultHeaders(context.Background(), base); !reflect.DeepEqual(got, base) {
		t.Fatalf("no user headers %v", got)
	}
	_ = os.WriteFile(filepath.Join(dir, "config.yaml"), []byte("model:\n  default_headers:\n    User-Agent: waf-ok\n    X-Drop: null\n  extra_headers:\n    X-Alias: 1\n"), 0o600)
	got := auxApplyUserDefaultHeaders(context.Background(), base)
	want := map[string]string{"User-Agent": "waf-ok", "X-Keep": "k", "X-Alias": "1"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v", got)
	}
}

func TestNvidiaNIMAndGatewayHeaders(t *testing.T) {
	if h := BuildNvidiaNIMHeaders("https://integrate.api.nvidia.com/v1"); h["X-BILLING-INVOKE-ORIGIN"] != "IcebathAgent" {
		t.Fatalf("nim %v", h)
	}
	if h := BuildNvidiaNIMHeaders("https://example.com/integrate.api.nvidia.com"); len(h) != 0 {
		t.Fatalf("lookalike %v", h)
	}
	if h := auxAIGatewayHeaders(); h["User-Agent"] == "" || h["X-Title"] != attributionTitle {
		t.Fatalf("gateway %v", h)
	}
}

func TestAuthJSONPathFollowsTheActiveProfile(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("ICEBATH_HOME", dir)
	if got := auxAuthJSONPath(context.Background()); got != filepath.Join(dir, "auth.json") {
		t.Fatalf("got %q", got)
	}
	auxAuthJSONPathOverride = "/patched/auth.json"
	defer func() { auxAuthJSONPathOverride = "" }()
	if auxAuthJSONPath(context.Background()) != "/patched/auth.json" {
		t.Fatal("a patched path wins")
	}
}

func TestApplyUserDefaultHeadersMergesModelHeaders(t *testing.T) {
	orig := auxLoadConfigReadonlyFn
	t.Cleanup(func() { auxLoadConfigReadonlyFn = orig })
	auxLoadConfigReadonlyFn = func(context.Context) (config.Tree, error) {
		return config.Tree{"model": map[string]any{"default_headers": map[string]any{"X-User": "u"}}}, nil
	}
	got := ApplyUserDefaultHeaders(context.Background(), map[string]string{"X-Base": "b"})
	if got["X-User"] != "u" || got["X-Base"] != "b" {
		t.Fatalf("merged headers = %v", got)
	}
}

func TestToOpenAIBaseURLExportMatchesTheAuxRewrite(t *testing.T) {
	for _, raw := range []string{"https://api.z.ai/api/anthropic", "https://api.kimi.com/coding/v1", "https://openrouter.ai/api/v1/"} {
		if got, want := ToOpenAIBaseURL(raw), auxToOpenAIBaseURL(raw); got != want {
			t.Fatalf("%s: got %q want %q", raw, got, want)
		}
	}
}
