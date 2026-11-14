package provider

import (
	"context"
	"io"
	"slices"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/tools"
)

func TestToolsConfigHooksViewTheSubscription(t *testing.T) {
	paid := false
	info := &NousPortalAccountInfo{LoggedIn: true, PaidServiceAccess: &paid,
		ToolAccess: &NousToolAccessInfo{Enabled: true, Coverage: map[string]bool{"fal-image": true}}}
	f := NousSubscriptionFeatures{NousAuthPresent: true, AccountInfo: info, Features: map[string]NousFeatureState{
		"web": {Key: "web", Available: true}, "image_gen": {Key: "image_gen", ManagedByNous: true},
	}}
	view := nousToolFeatures(f)
	if !view.NousAuthPresent || view.Features["web"] != (tools.NousToolFeature{Available: true}) ||
		view.Features["image_gen"] != (tools.NousToolFeature{ManagedByNous: true}) || len(view.Features) != 2 {
		t.Fatalf("features %+v", view)
	}
	acct := view.Account
	if acct == nil || !acct.LoggedIn || acct.PaidServiceAccess != &paid || !acct.ToolGatewayEntitled || acct.Info != info ||
		!acct.EntitledFor("fal-image") || acct.EntitledFor("krea") {
		t.Fatalf("account %+v", acct)
	}
	if nousToolFeatures(NousSubscriptionFeatures{}).Account != nil {
		t.Fatal("missing account info produced an account")
	}
	hooks := tools.ToolsConfigHooks
	for _, cap := range []string{"Nous Subscription", "FAL"} {
		if got, want := hooks.NousEntitlementMessage(acct, cap), FormatNousPortalEntitlementMessage(info, cap, false); got != want {
			t.Fatalf("message %q want %q", got, want)
		}
	}
	if got, want := hooks.NousEntitlementMessage(nil, "X"), FormatNousPortalEntitlementMessage(nil, "X", false); got != want {
		t.Fatalf("logged out message %q want %q", got, want)
	}
	if c, ok := hooks.ManagedCoverageCategory("image_gen"); !ok || c != ManagedFeatureCoverageCategory["image_gen"] {
		t.Fatalf("coverage %q", c)
	}
	if _, ok := hooks.ManagedCoverageCategory("no_such_feature"); ok {
		t.Fatal("unknown feature has a coverage category")
	}
	rows := []map[string]any{{"slug": "openrouter", "name": "OpenRouter", "models": []string{"a"}}}
	var want []string
	for _, e := range FormatAuxPickerEntries(rows, "", "") {
		want = append(want, e.Label)
	}
	if got := hooks.AuxPickerLabels(rows, "", ""); !slices.Equal(got, want) {
		t.Fatalf("labels %v want %v", got, want)
	}
	for name, set := range map[string]bool{
		"NousFeatures": hooks.NousFeatures != nil, "EnsureNousPortalAccess": hooks.EnsureNousPortalAccess != nil,
		"AuxPickerRows": hooks.AuxPickerRows != nil, "XAIOAuthStatus": hooks.XAIOAuthStatus != nil, "CodexAuthStatus": hooks.CodexAuthStatus != nil,
		"CodexDeviceCodeLogin": hooks.CodexDeviceCodeLogin != nil, "SaveCodexTokens": hooks.SaveCodexTokens != nil,
		"XAIOAuthLoginFromSetup": hooks.XAIOAuthLoginFromSetup != nil, "SetupPromptChoice": hooks.SetupPromptChoice != nil,
		"SetupPrompt": hooks.SetupPrompt != nil, "SetupIsNoninteractive": hooks.SetupIsNoninteractive != nil,
	} {
		if !set {
			t.Fatalf("%s not installed", name)
		}
	}
}

func TestToolsConfigHooksReadTheRunHome(t *testing.T) {
	h, _ := authTestHome(t)
	writeAuthStore(t, h, map[string]any{"version": 1, "active_provider": "openrouter", "providers": map[string]any{}})
	hooks := tools.ToolsConfigHooks
	if hooks.CodexAuthStatus(context.Background(), h)["logged_in"] == true || hooks.XAIOAuthStatus(context.Background(), h)["logged_in"] == true {
		t.Fatal("empty store read as logged in")
	}
	if err := hooks.SaveCodexTokens(context.Background(), h, map[string]any{"access_token": "codex-a", "refresh_token": "codex-r"}, "2026-01-01T00:00:00Z", false); err != nil {
		t.Fatal(err)
	}
	auth := readAuthStore(t, h)
	providers, _ := auth["providers"].(map[string]any)
	state, _ := providers["openai-codex"].(map[string]any)
	tokens, _ := state["tokens"].(map[string]any)
	if auth["active_provider"] != "openrouter" || tokens["access_token"] != "codex-a" || state["last_refresh"] != "2026-01-01T00:00:00Z" {
		t.Fatalf("auth store %v", auth)
	}
	t.Setenv("ICEBATH_NONINTERACTIVE", "yes")
	if !hooks.SetupIsNoninteractive() {
		t.Fatal("noninteractive env not read")
	}
	restore := WithSetupOutput(io.Discard)
	defer restore()
	prev := xaiOAuthDeviceCodeLoginFn
	t.Cleanup(func() { xaiOAuthDeviceCodeLoginFn = prev })
	xaiOAuthDeviceCodeLoginFn = func(PoolEnv, io.Writer, time.Duration, bool) (map[string]any, error) {
		return map[string]any{"discovery": map[string]any{}}, nil
	}
	if hooks.XAIOAuthLoginFromSetup(context.Background(), h) {
		t.Fatal("login without tokens reported success")
	}
}
