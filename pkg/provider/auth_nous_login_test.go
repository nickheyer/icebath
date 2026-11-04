package provider

import (
	"bytes"
	"context"
	"io"
	"strings"
	"testing"
	"time"
)

// ---- test_auth_nous_provider.py TestLoginNousSkipKeepsCurrent ----

func setupHomeWithOpenRouter(t *testing.T, h string) {
	t.Helper()
	writeConfigYAML(t, h, "model:\n  provider: openrouter\n  default: anthropic/claude-opus-4.6\n")
	writeAuthStore(t, h, map[string]any{"version": 1, "active_provider": "openrouter", "providers": map[string]any{"openrouter": map[string]any{"api_key": "sk-or-fake"}}})
}

// Stubs the OAuth flow, the model list and the picker so the login never touches the network
func patchLoginInternals(t *testing.T, promptReturns string) {
	t.Helper()
	fakeState := map[string]any{"access_token": "fake-nous-token", "agent_key": "fake-agent-key", "inference_base_url": "https://inference-api.nousresearch.com",
		"portal_base_url": "https://portal.nousresearch.com", "refresh_token": "fake-refresh", "token_expires_at": 9999999999}
	nousDeviceCodeLoginFn = func(env PoolEnv, out io.Writer, args NousDeviceLoginArgs) (map[string]any, error) { return cloneMap(fakeState), nil }
	testPromptModelSelection = func(p ModelSelectionPrompt) (string, bool) { return promptReturns, promptReturns != "" }
	nousLoginCuratedIDsFn = func(ctx context.Context) []string { return []string{"xiaomi/mimo-v2-pro", "nous/other"} }
	nousLoginPricingFn = func(ctx context.Context) PricingTable { return PricingTable{} }
	nousLoginFreeTierFn = func(ctx context.Context) bool { return false }
	nousLoginPolicyAllowedFn = func(ctx context.Context) (map[string]bool, bool) { return nil, false }
	fetchNousRecommendedModelsFn = func(ctx context.Context, portalURL string, forceRefresh bool) map[string]any { return nil }
}

func modelSection(t *testing.T, h string) map[string]any {
	t.Helper()
	section, _ := LoadConfigSafe(h)["model"].(map[string]any)
	return section
}

func TestSkipKeepCurrentPreservesProviderAndModel(t *testing.T) {
	resetNousSeams(t)
	h := newTestHome(t)
	setupHomeWithOpenRouter(t, h)
	patchLoginInternals(t, "")
	var out bytes.Buffer
	if err := LoginNous(testEnv(t, h), &out, NousLoginArgs{NoBrowser: true, Timeout: 15 * time.Second}); err != nil {
		t.Fatalf("login failed: %v\n%s", err, out.String())
	}
	cfg := modelSection(t, h)
	if cfg["provider"] != "openrouter" || cfg["default"] != "anthropic/claude-opus-4.6" {
		t.Fatalf("model config changed: %v", cfg)
	}
	if _, has := cfg["base_url"]; has {
		t.Fatalf("base_url written on skip")
	}
	auth := readAuthStore(t, h)
	providers := auth["providers"].(map[string]any)
	if auth["active_provider"] != "openrouter" || providers["nous"].(map[string]any)["access_token"] != "fake-nous-token" ||
		providers["openrouter"].(map[string]any)["api_key"] != "sk-or-fake" {
		t.Fatalf("auth = %v", auth)
	}
	if !strings.Contains(out.String(), "No provider change. Nous credentials saved for future use.") {
		t.Fatalf("output = %s", out.String())
	}
}

func TestPickingModelSwitchesToNous(t *testing.T) {
	resetNousSeams(t)
	h := newTestHome(t)
	setupHomeWithOpenRouter(t, h)
	patchLoginInternals(t, "xiaomi/mimo-v2-pro")
	var out bytes.Buffer
	if err := LoginNous(testEnv(t, h), &out, NousLoginArgs{NoBrowser: true, Timeout: 15 * time.Second}); err != nil {
		t.Fatalf("login failed: %v\n%s", err, out.String())
	}
	cfg := modelSection(t, h)
	if cfg["provider"] != "nous" || cfg["default"] != "xiaomi/mimo-v2-pro" {
		t.Fatalf("model config = %v", cfg)
	}
	if readAuthStore(t, h)["active_provider"] != "nous" {
		t.Fatalf("active provider not switched")
	}
	if len(testSaveModelChoiceCalls) != 1 || testSaveModelChoiceCalls[0] != "xiaomi/mimo-v2-pro" {
		t.Fatalf("model choice saves = %v", testSaveModelChoiceCalls)
	}
	if !strings.Contains(out.String(), "Default model set to: xiaomi/mimo-v2-pro") || !strings.Contains(out.String(), "(model.provider=nous)") {
		t.Fatalf("output = %s", out.String())
	}
}

func TestSkipWithNoPriorActiveProviderClearsIt(t *testing.T) {
	resetNousSeams(t)
	h := newTestHome(t)
	writeConfigYAML(t, h, "model: {}\n")
	patchLoginInternals(t, "")
	if err := LoginNous(testEnv(t, h), io.Discard, NousLoginArgs{NoBrowser: true, Timeout: 15 * time.Second}); err != nil {
		t.Fatalf("login failed: %v", err)
	}
	auth := readAuthStore(t, h)
	if active := anyString(auth["active_provider"]); active != "" {
		t.Fatalf("active_provider = %q", active)
	}
	if _, has := auth["providers"].(map[string]any)["nous"]; !has {
		t.Fatalf("nous credentials not saved")
	}
}

func TestLoginNousPrintsHandoffLinesAndFailureCopy(t *testing.T) {
	resetNousSeams(t)
	h := newTestHome(t)
	setupHomeWithOpenRouter(t, h)
	patchLoginInternals(t, "")
	testGuestHandoffDrainResult = []map[string]any{{"line": "Moved 2 apps to your account."}}
	var out bytes.Buffer
	if err := LoginNous(testEnv(t, h), &out, NousLoginArgs{NoBrowser: true}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "  Moved 2 apps to your account.") || testSettleAfterUpgradeCalls != 1 {
		t.Fatalf("output = %s settles %d", out.String(), testSettleAfterUpgradeCalls)
	}
	nousDeviceCodeLoginFn = func(env PoolEnv, out io.Writer, args NousDeviceLoginArgs) (map[string]any, error) {
		return nil, DeviceFlowError("access_denied", "The user denied the request", "")
	}
	out.Reset()
	err := LoginNous(testEnv(t, h), &out, NousLoginArgs{NoBrowser: true})
	var exit *NousLoginExitError
	if !asNousExit(err, &exit) || exit.Code != 1 {
		t.Fatalf("err = %v", err)
	}
	if strings.TrimSpace(out.String()) == "" {
		t.Fatalf("no failure copy printed")
	}
}

func asNousExit(err error, target **NousLoginExitError) bool {
	for err != nil {
		if e, ok := err.(*NousLoginExitError); ok {
			*target = e
			return true
		}
		u, ok := err.(interface{ Unwrap() error })
		if !ok {
			return false
		}
		err = u.Unwrap()
	}
	return false
}

// ---- test_nous_picker_on_sale.py post login picker ----

var listPrice = map[string]any{"prompt": "0.000002", "completion": "0.00001"}

func salePrice(pctOff int) map[string]any {
	now := 0.000002 * float64(100-pctOff) / 100
	return map[string]any{"prompt": strings.TrimRight(strings.TrimRight(formatFloat10(now), "0"), ".") + "", "completion": "0.00001", "original": cloneMap(listPrice)}
}

func formatFloat10(f float64) string {
	return strings.TrimSpace(stringifyAny(f))
}

// Model ids the post login picker offers, selectable then greyed out
func loginPickerIDs(t *testing.T, freeTier bool, pricing PricingTable, allowed map[string]bool) []string {
	t.Helper()
	h := newTestHome(t)
	nousLoginPricingFn = func(ctx context.Context) PricingTable { return pricing }
	nousLoginFreeTierFn = func(ctx context.Context) bool { return freeTier }
	nousLoginPolicyAllowedFn = func(ctx context.Context) (map[string]bool, bool) { return allowed, allowed != nil }
	fetchNousRecommendedModelsFn = func(ctx context.Context, portalURL string, forceRefresh bool) map[string]any { return nil }
	nousLoginCuratedIDsFn = func(ctx context.Context) []string { return []string{"curated/only"} }
	testNousEntitlementMessage = ""
	var offered, unavailable []string
	testPromptModelSelection = func(p ModelSelectionPrompt) (string, bool) {
		offered = append([]string(nil), p.ModelIDs...)
		unavailable = append([]string(nil), p.UnavailableModels...)
		if len(p.ModelIDs) == 0 {
			return "", false
		}
		return p.ModelIDs[0], true
	}
	if _, err := nousPickModelAfterLogin(testEnv(t, h), io.Discard, map[string]any{"agent_key": "k", "portal_base_url": ""}, "https://x.test/v1"); err != nil {
		t.Fatal(err)
	}
	return append(offered, unavailable...)
}

func TestPostLoginPickerPaidTierShowsOnSaleModel(t *testing.T) {
	resetNousSeams(t)
	pricing := PricingTable{"curated/only": cloneMap(listPrice), "vendor/sale": salePrice(80)}
	if got := loginPickerIDs(t, false, pricing, nil); strings.Join(got, ",") != "curated/only,vendor/sale" {
		t.Fatalf("ids = %v", got)
	}
}

func TestPostLoginPickerFreeTierDoesNotListPaidSales(t *testing.T) {
	resetNousSeams(t)
	pricing := PricingTable{"curated/only": map[string]any{"prompt": "0", "completion": "0"}, "vendor/sale": salePrice(80)}
	for _, id := range loginPickerIDs(t, true, pricing, nil) {
		if id == "vendor/sale" {
			t.Fatalf("free tier listed a paid sale")
		}
	}
}

func TestPostLoginPickerOrgPolicyStillNarrowsOnSaleModels(t *testing.T) {
	resetNousSeams(t)
	pricing := PricingTable{"curated/only": cloneMap(listPrice), "vendor/sale": salePrice(80)}
	if got := loginPickerIDs(t, false, pricing, map[string]bool{"curated/only": true}); strings.Join(got, ",") != "curated/only" {
		t.Fatalf("ids = %v", got)
	}
}

func TestPostLoginPickerSalesDeepestFirst(t *testing.T) {
	resetNousSeams(t)
	pricing := PricingTable{"curated/only": cloneMap(listPrice), "sale/x": salePrice(80), "sale/y": salePrice(30)}
	if got := loginPickerIDs(t, false, pricing, nil); strings.Join(got, ",") != "curated/only,sale/x,sale/y" {
		t.Fatalf("ids = %v", got)
	}
}
