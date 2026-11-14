package provider

import (
	"context"
	"reflect"
	"testing"

	"github.com/nickheyer/icebath/pkg/home"
)

func TestOpencodeGoStripsDeepseekPrefix(t *testing.T) {
	if got := NormalizeModelForProvider("deepseek/deepseek-v4-flash", "opencode-go"); got != "deepseek-v4-flash" {
		t.Fatalf("got %q", got)
	}
}

func TestOverlayProviderUsesHermesSlug(t *testing.T) {
	ctx := pickerCredentialHome(t, map[string]any{})
	t.Setenv("KIMI_API_KEY", "fake-kimi")
	rows := mustListProviders(t, ctx, AuthenticatedProvidersOptions{CurrentProvider: "kimi-coding"})
	kimi := pickerRow(rows, "kimi-coding")
	if kimi == nil {
		t.Fatalf("kimi-coding should appear when KIMI_API_KEY is set %v", pickerSlugs(rows))
	}
	if kimi["total_models"].(int) <= 0 || kimi["is_current"] != true {
		t.Fatalf("kimi-coding row %v", kimi)
	}
	if pickerRow(rows, "kimi-for-coding") != nil {
		t.Fatal("kimi-for-coding slug should not appear (resolved to kimi-coding)")
	}
}

func TestMappedProviderCredentialPoolVisibility(t *testing.T) {
	ctx := pickerCredentialHome(t, map[string]any{})
	mdevID := ModelsDevProviderMap()["gemini"]
	pickerFetchModelsDevFn = func(context.Context, bool) map[string]any {
		return map[string]any{mdevID: map[string]any{"env": []any{"GEMINI_API_KEY"}}}
	}
	writeAuthStore(t, home.Dir(ctx), map[string]any{"providers": map[string]any{}, "credential_pool": map[string]any{"gemini": map[string]any{"token": "fake"}}})
	rows := mustListProviders(t, ctx, AuthenticatedProvidersOptions{CurrentProvider: "gemini"})
	gemini := pickerRow(rows, "gemini")
	if gemini == nil {
		t.Fatalf("gemini should appear when auth-store credential_pool has creds %v", pickerSlugs(rows))
	}
	if gemini["is_current"] != true || gemini["total_models"].(int) <= 0 {
		t.Fatalf("gemini row %v", gemini)
	}
}

func TestKimiCnAppearsWhenOnlyCnKeySet(t *testing.T) {
	ctx := pickerCredentialHome(t, map[string]any{})
	t.Setenv("KIMI_CN_API_KEY", "sk-cn-fake")
	rows := mustListProviders(t, ctx, AuthenticatedProvidersOptions{CurrentProvider: "kimi-coding-cn"})
	cn := pickerRow(rows, "kimi-coding-cn")
	if cn == nil {
		t.Fatalf("kimi-coding-cn should appear when KIMI_CN_API_KEY is set %v", pickerSlugs(rows))
	}
	if cn["is_current"] != true || cn["total_models"].(int) <= 0 {
		t.Fatalf("kimi-coding-cn row %v", cn)
	}
	if pickerRow(rows, "kimi-coding") != nil {
		t.Fatal("kimi-coding should NOT appear when only KIMI_CN_API_KEY is set")
	}
}

func TestResolveProviderFullPreservesKimiCnProviderIdentity(t *testing.T) {
	ctx, _ := pickerTestHome(t, nil)
	t.Setenv("KIMI_API_KEY", "sk-intl-fake")
	t.Setenv("KIMI_CN_API_KEY", "sk-cn-fake")
	pdef := ResolveProviderFull(ctx, "kimi-coding-cn", nil, nil)
	if pdef == nil || pdef.ID != "kimi-coding-cn" || pdef.BaseURL != "https://api.moonshot.cn/v1" || !reflect.DeepEqual(pdef.KeyEnv, []string{"KIMI_CN_API_KEY"}) {
		t.Fatalf("pdef %+v", pdef)
	}
}
