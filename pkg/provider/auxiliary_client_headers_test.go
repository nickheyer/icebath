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

func TestToOpenAIBaseURLRewritesDualSurfaceHosts(t *testing.T) {
	cases := [][2]string{
		{"https://api.minimax.io/anthropic", "https://api.minimax.io/v1"},
		{"https://api.minimax.chat/anthropic", "https://api.minimax.chat/v1"},
		{"https://token-plan.cn-beijing.maas.aliyuncs.com/apps/anthropic", "https://token-plan.cn-beijing.maas.aliyuncs.com/apps/anthropic"},
		{"https://gateway.example.com/api.minimax.io/anthropic", "https://gateway.example.com/api.minimax.io/anthropic"},
		{"https://api.minimaxi.com/anthropic", "https://api.minimaxi.com/v1"},
		{"https://minimax.io.evil.example.com/anthropic", "https://minimax.io.evil.example.com/anthropic"},
		{"https://api.z.ai/api/anthropic", "https://api.z.ai/api/coding/paas/v4"},
		{"https://open.bigmodel.cn/api/anthropic", "https://open.bigmodel.cn/api/coding/paas/v4"},
		{"https://gateway.example.com/proxy/bigmodel-fallback/anthropic", "https://gateway.example.com/proxy/bigmodel-fallback/anthropic"},
		{"https://gateway.example.com/api.z.ai-mirror/anthropic", "https://gateway.example.com/api.z.ai-mirror/anthropic"},
		{"https://api.kimi.com/coding", "https://api.kimi.com/coding/v1"},
		{"https://gateway.example.com/api.kimi.com/coding", "https://gateway.example.com/api.kimi.com/coding"},
		{"", ""},
	}
	for _, tc := range cases {
		if got := auxToOpenAIBaseURL(tc[0]); got != tc[1] {
			t.Fatalf("%q got %q want %q", tc[0], got, tc[1])
		}
	}
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
	h, err := BuildORHeaders(context.Background(), map[string]any{"response_cache": true, "response_cache_ttl": -5})
	if err != nil || h["X-OpenRouter-Cache"] != "true" || h["X-OpenRouter-Cache-TTL"] != "" {
		t.Fatalf("negative ttl %v %v", h, err)
	}
	cfg := map[string]any{"response_cache": true, "response_cache_ttl": 600}
	h1, _ := BuildORHeaders(context.Background(), cfg)
	h2, _ := BuildORHeaders(context.Background(), cfg)
	h1["X"] = "mutated"
	delete(h1, "X")
	if !reflect.DeepEqual(h1, h2) || h1["X-OpenRouter-Cache-TTL"] != "600" {
		t.Fatalf("fresh equal dicts %v %v", h1, h2)
	}
	h1["only-here"] = "1"
	if _, shared := h2["only-here"]; shared {
		t.Fatal("each call returns its own map")
	}
	oldLoad := auxLoadConfigReadonlyFn
	auxLoadConfigReadonlyFn = func(context.Context) (config.Tree, error) { return nil, errors.New("boom") }
	h, err = BuildORHeaders(context.Background(), nil)
	auxLoadConfigReadonlyFn = oldLoad
	if err != nil || h["HTTP-Referer"] == "" || h["X-OpenRouter-Categories"] != "productivity,cli-agent" || h["X-OpenRouter-Cache"] != "" {
		t.Fatalf("disk default %v", h)
	}
	for _, ttl := range []string{"0", "86401", "abc", "-1", "12.5"} {
		t.Setenv("ICEBATH_OPENROUTER_CACHE", "1")
		t.Setenv("ICEBATH_OPENROUTER_CACHE_TTL", ttl)
		h, err = BuildORHeaders(context.Background(), map[string]any{})
		if err != nil || h["X-OpenRouter-Cache"] != "true" || h["X-OpenRouter-Cache-TTL"] != "" {
			t.Fatalf("ttl %q kept %v %v", ttl, h, err)
		}
	}
	for _, ttl := range []string{"1", "300", "86400"} {
		t.Setenv("ICEBATH_OPENROUTER_CACHE", "yes")
		t.Setenv("ICEBATH_OPENROUTER_CACHE_TTL", ttl)
		if h, _ := BuildORHeaders(context.Background(), map[string]any{}); h["X-OpenRouter-Cache-TTL"] != ttl {
			t.Fatalf("ttl %q got %v", ttl, h)
		}
	}
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
