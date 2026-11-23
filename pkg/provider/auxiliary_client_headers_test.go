package provider

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"testing"
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
	t.Setenv("ICEBATH_HOME", t.TempDir())
}

func TestBuildORHeadersReadsDiskConfig(t *testing.T) {
	auxORHeadersEnv(t)
	dir := os.Getenv("ICEBATH_HOME")
	_ = os.WriteFile(filepath.Join(dir, "config.yaml"), []byte("openrouter:\n  response_cache: true\n  response_cache_ttl: 120\n"), 0o600)
	h := BuildORHeaders(context.Background())
	if h["X-OpenRouter-Cache"] != "true" || h["X-OpenRouter-Cache-TTL"] != "120" {
		t.Fatalf("disk config %v", h)
	}
	_ = os.WriteFile(filepath.Join(dir, "config.yaml"), []byte("openrouter:\n  response_cache: true\n  response_cache_ttl: -5\n"), 0o600)
	if h := BuildORHeaders(context.Background()); h["X-OpenRouter-Cache"] != "true" || h["X-OpenRouter-Cache-TTL"] != "" {
		t.Fatalf("negative ttl %v", h)
	}
	_ = os.WriteFile(filepath.Join(dir, "config.yaml"), []byte("openrouter:\n  response_cache: false\n"), 0o600)
	if h := BuildORHeaders(context.Background()); h["X-OpenRouter-Cache"] != "" || h["HTTP-Referer"] == "" {
		t.Fatalf("cache off %v", h)
	}
}

func TestApplyUserDefaultHeadersOverrideBase(t *testing.T) {
	auxORHeadersEnv(t)
	dir := os.Getenv("ICEBATH_HOME")
	base := map[string]string{"User-Agent": "sdk", "X-Keep": "k"}
	if got := auxApplyUserDefaultHeaders(context.Background(), base); !reflect.DeepEqual(got, base) {
		t.Fatalf("no user headers %v", got)
	}
	_ = os.WriteFile(filepath.Join(dir, "config.yaml"), []byte("model:\n  default_headers:\n    User-Agent: waf-ok\n    X-Drop: null\n    X-Alias: 1\n"), 0o600)
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

func TestApplyUserDefaultHeadersMergesModelHeaders(t *testing.T) {
	auxORHeadersEnv(t)
	_ = os.WriteFile(filepath.Join(os.Getenv("ICEBATH_HOME"), "config.yaml"), []byte("model:\n  default_headers:\n    X-User: u\n"), 0o600)
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
