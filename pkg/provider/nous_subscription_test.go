package provider

import (
	"io"
	"reflect"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/tools"
)

const subFalDefaultModel = "fal-ai/flux-2/klein/9b"

func subAccount(loggedIn bool, paid *bool) *NousPortalAccountInfo {
	source := "none"
	if loggedIn {
		source = "jwt"
	}
	return &NousPortalAccountInfo{LoggedIn: loggedIn, Source: source, PaidServiceAccess: paid}
}

func subPoolAccount() *NousPortalAccountInfo {
	paid := false
	return &NousPortalAccountInfo{LoggedIn: true, Source: "jwt", PaidServiceAccess: &paid, ToolAccess: &NousToolAccessInfo{Enabled: true,
		Coverage: map[string]bool{"firecrawl": true, "fal": true, "fal-video": false, "openai-audio": true, "browser-use": true, "modal": true}}}
}

func subPaid() *NousPortalAccountInfo { yes := true; return subAccount(true, &yes) }

// Restores every subscription seam and pins the account, faster whisper probe off
func subSeams(t *testing.T, account *NousPortalAccountInfo) PoolEnv {
	t.Helper()
	envFn, acct, toolset, chromium, whisper := nousSubEnvValueFn, nousSubAccountInfoFn, nousSubToolsetEnabledFn, nousSubChromiumFn, nousSubFasterWhisperFn
	audio, modal, fal, ready, creds, checklist, save := nousSubAudioKeyFn, nousSubDirectModalFn, nousSubFalKeyFn, nousSubGatewayReadyFn, nousSubDirectCredsFn, nousSubChecklistFn, nousSubSaveConfigFn
	t.Cleanup(func() {
		nousSubEnvValueFn, nousSubAccountInfoFn, nousSubToolsetEnabledFn, nousSubChromiumFn, nousSubFasterWhisperFn = envFn, acct, toolset, chromium, whisper
		nousSubAudioKeyFn, nousSubDirectModalFn, nousSubFalKeyFn, nousSubGatewayReadyFn, nousSubDirectCredsFn, nousSubChecklistFn, nousSubSaveConfigFn = audio, modal, fal, ready, creds, checklist, save
	})
	nousSubAccountInfoFn = func(PoolEnv, bool) *NousPortalAccountInfo { return account }
	nousSubFasterWhisperFn = func() bool { return false }
	return testEnv(t, newTestHome(t))
}

// The web feature tests patch env reads, the toolset gate, audio key and modal probes
func subWebProbes(t *testing.T, vals map[string]string, toolsetKey string) PoolEnv {
	env := subSeams(t, subAccount(false, nil))
	nousSubEnvValueFn = func(_ PoolEnv, name string) string { return vals[name] }
	nousSubToolsetEnabledFn = func(_ PoolEnv, _ map[string]any, key string) bool { return key == toolsetKey }
	nousSubAudioKeyFn = func(PoolEnv) string { return "" }
	nousSubDirectModalFn = func(PoolEnv) bool { return false }
	return env
}

func TestNousSubscriptionRecognizesDirectExaBackend(t *testing.T) {
	env := subWebProbes(t, map[string]string{"EXA_API_KEY": "exa-test"}, "web")
	web := GetNousSubscriptionFeatures(env, map[string]any{"web": map[string]any{"backend": "exa"}}, false).Feature("web")
	if !web.Available || !web.Active || web.ManagedByNous || !web.DirectOverride || web.CurrentProvider != "exa" {
		t.Fatalf("web = %+v", web)
	}
}

func TestNousSubscriptionRecognizesKeylessTavilyBackend(t *testing.T) {
	env := subWebProbes(t, nil, "web")
	web := GetNousSubscriptionFeatures(env, map[string]any{"web": map[string]any{"backend": "tavily"}}, false).Feature("web")
	if !web.Available || !web.Active || web.ManagedByNous || !web.DirectOverride || web.CurrentProvider != "tavily" || !web.ExplicitConfigured {
		t.Fatalf("web = %+v", web)
	}
}

func TestNousSubscriptionKeylessTavilySearchBackendWithoutSharedBackend(t *testing.T) {
	env := subWebProbes(t, nil, "web")
	web := GetNousSubscriptionFeatures(env, map[string]any{"web": map[string]any{"search_backend": "tavily"}}, false).Feature("web")
	if !web.Available || !web.Active || web.CurrentProvider != "tavily" {
		t.Fatalf("web = %+v", web)
	}
}

func TestNousSubscriptionUnconfiguredWebWithoutKeysIsUnavailable(t *testing.T) {
	env := subWebProbes(t, nil, "web")
	web := GetNousSubscriptionFeatures(env, map[string]any{}, false).Feature("web")
	if web.Available || web.Active || web.ExplicitConfigured {
		t.Fatalf("web = %+v", web)
	}
}

func subBrowserProbes(t *testing.T, chromium bool) PoolEnv {
	env := subWebProbes(t, nil, "browser")
	nousSubGatewayReadyFn = func(PoolEnv, string) bool { return false }
	nousSubChromiumFn = func(PoolEnv) string {
		if chromium {
			return "/pm/chromium/chrome"
		}
		return ""
	}
	return env
}

func TestNousSubscriptionLocalBrowserUnavailableWithoutChromium(t *testing.T) {
	env := subBrowserProbes(t, false)
	browser := GetNousSubscriptionFeatures(env, map[string]any{"browser": map[string]any{"cloud_provider": "local"}}, false).Feature("browser")
	if browser.Available || browser.Active || browser.ManagedByNous || browser.CurrentProvider != "Local browser" {
		t.Fatalf("browser = %+v", browser)
	}
}

func TestNousSubscriptionLocalBrowserAvailabilityFollowsEngineAndChromium(t *testing.T) {
	for _, row := range []struct {
		chromium  bool
		engine    string
		available bool
	}{{true, "auto", true}, {false, "lightpanda", true}, {false, "auto", false}} {
		env := subBrowserProbes(t, row.chromium)
		browser := GetNousSubscriptionFeatures(env, map[string]any{"browser": map[string]any{"cloud_provider": "local", "engine": row.engine}}, false).Feature("browser")
		if browser.Available != row.available || browser.CurrentProvider != "Local browser" {
			t.Fatalf("%+v browser = %+v", row, browser)
		}
	}
}

func TestNousSubscriptionLoggedInEntitledAccountYieldsAStateForEveryFeature(t *testing.T) {
	env := subSeams(t, subPoolAccount())
	nousSubGatewayReadyFn = func(PoolEnv, string) bool { return true }
	nousSubEnvValueFn = func(PoolEnv, string) string { return "" }
	nousSubAudioKeyFn = func(PoolEnv) string { return "" }
	nousSubDirectModalFn = func(PoolEnv) bool { return false }
	result := GetNousSubscriptionFeatures(env, map[string]any{"model": map[string]any{"provider": "nous"}}, false)
	if len(result.Features) != len(nousFeatures) {
		t.Fatalf("features = %v", result.Features)
	}
	for key := range nousFeatures {
		if _, ok := result.Features[key]; !ok {
			t.Fatalf("missing %s", key)
		}
	}
	if !result.Feature("modal").Available {
		t.Fatalf("modal = %+v", result.Feature("modal"))
	}
}

type subChecklist struct {
	title       string
	items       []string
	preSelected []int
}

// Records the checklist call and answers with the given indices, config saves recorded
func subCaptureChecklist(selected []int, saved *[]map[string]any) *subChecklist {
	captured := &subChecklist{}
	nousSubChecklistFn = func(title string, items []string, preSelected []int) ([]int, error) {
		captured.title, captured.items, captured.preSelected = title, append([]string{}, items...), append([]int{}, preSelected...)
		return selected, nil
	}
	nousSubSaveConfigFn = func(_ PoolEnv, cfg map[string]any) error {
		if saved != nil {
			*saved = append(*saved, cloneMap(cfg))
		}
		return nil
	}
	return captured
}

func subNoDirect(stt bool) func(PoolEnv) map[string]bool {
	return func(PoolEnv) map[string]bool {
		m := map[string]bool{"web": false, "image_gen": false, "video_gen": false, "tts": false, "browser": false}
		if stt {
			m["stt"] = false
		}
		return m
	}
}

func TestNousSubscriptionPromptEnableToolGatewayPoolOffersCoveredToolsOnly(t *testing.T) {
	env := subSeams(t, subPoolAccount())
	nousSubDirectCredsFn = subNoDirect(false)
	captured := subCaptureChecklist(nil, nil)
	if _, err := PromptEnableToolGateway(env, io.Discard, map[string]any{"model": map[string]any{"provider": "nous"}}, true); err != nil {
		t.Fatal(err)
	}
	blob := strings.ToLower(strings.Join(captured.items, " "))
	if !strings.Contains(blob, "web search & extract") || strings.Contains(blob, "video") {
		t.Fatalf("items = %v", captured.items)
	}
}

func TestNousSubscriptionGatewayEligibleToolsTreatsExplicitBackendAsConfigured(t *testing.T) {
	env := subSeams(t, subPaid())
	nousSubDirectCredsFn = subNoDirect(false)
	cfg := map[string]any{"model": map[string]any{"provider": "nous"}, "web": map[string]any{"backend": "searxng"}}
	unconfigured, hasDirect, explicit, managed := GetGatewayEligibleTools(env, cfg, false)
	if containsString(unconfigured, "web") || containsString(hasDirect, "web") || !containsString(explicit, "web") || containsString(managed, "web") {
		t.Fatalf("lists %v %v %v %v", unconfigured, hasDirect, explicit, managed)
	}
}

func TestNousSubscriptionGatewayEligibleToolsTreatsBrowserUseSelectionAsExplicit(t *testing.T) {
	env := subSeams(t, subPaid())
	nousSubDirectCredsFn = func(PoolEnv) map[string]bool {
		return map[string]bool{"web": false, "image_gen": false, "video_gen": false, "tts": false, "browser": true}
	}
	cfg := map[string]any{"model": map[string]any{"provider": "nous"}, "browser": map[string]any{"cloud_provider": "browser-use"}}
	unconfigured, hasDirect, explicit, managed := GetGatewayEligibleTools(env, cfg, false)
	if containsString(unconfigured, "browser") || containsString(hasDirect, "browser") || !containsString(explicit, "browser") || containsString(managed, "browser") {
		t.Fatalf("lists %v %v %v %v", unconfigured, hasDirect, explicit, managed)
	}
}

func TestNousSubscriptionPromptEnableToolGatewayNotEntitledDoesNotCrash(t *testing.T) {
	no := false
	env := subSeams(t, subAccount(true, &no))
	changed, err := PromptEnableToolGateway(env, io.Discard, map[string]any{"model": map[string]any{"provider": "nous"}}, true)
	if err != nil || len(changed) != 0 {
		t.Fatalf("changed %v err %v", changed, err)
	}
}

func TestNousSubscriptionPromptEnableToolGatewayNeverOffersExplicitBackend(t *testing.T) {
	env := subSeams(t, subPaid())
	nousSubDirectCredsFn = subNoDirect(false)
	captured := subCaptureChecklist(nil, nil)
	cfg := map[string]any{"model": map[string]any{"provider": "nous"}, "web": map[string]any{"backend": "searxng"}}
	if _, err := PromptEnableToolGateway(env, io.Discard, cfg, true); err != nil {
		t.Fatal(err)
	}
	blob := strings.ToLower(strings.Join(captured.items, " "))
	if strings.Contains(blob, "firecrawl") || !strings.Contains(blob, "image") {
		t.Fatalf("items = %v", captured.items)
	}
}

func TestNousSubscriptionGatewayDirectCredentialsHonorEnvConfiguredLocalBackends(t *testing.T) {
	env := subSeams(t, nil)
	nousSubEnvValueFn = func(_ PoolEnv, name string) string {
		if name == "SEARXNG_URL" || name == "CAMOFOX_URL" {
			return "http://localhost:9377"
		}
		return ""
	}
	nousSubFalKeyFn = func(PoolEnv) bool { return false }
	nousSubAudioKeyFn = func(PoolEnv) string { return "" }
	direct := nousGatewayDirectCredentials(env)
	if !direct["web"] || !direct["browser"] || direct["image_gen"] {
		t.Fatalf("direct = %v", direct)
	}
}

func TestNousSubscriptionPromptEnableToolGatewayPersistsDecline(t *testing.T) {
	env := subSeams(t, subPaid())
	nousSubDirectCredsFn = subNoDirect(true)
	var saved []map[string]any
	captured := subCaptureChecklist(nil, &saved)
	cfg := map[string]any{"model": map[string]any{"provider": "nous"}}
	changed, err := PromptEnableToolGateway(env, io.Discard, cfg, true)
	if err != nil || len(changed) != 0 {
		t.Fatalf("changed %v err %v", changed, err)
	}
	var all []int
	for i := range captured.items {
		all = append(all, i)
	}
	if !reflect.DeepEqual(captured.preSelected, all) {
		t.Fatalf("pre selected %v of %v", captured.preSelected, captured.items)
	}
	declined, ok := cfg["tool_gateway_declined_tools"].([]any)
	if !ok || !containsAny(declined, "web") || !containsAny(declined, "browser") {
		t.Fatalf("declined = %v", cfg["tool_gateway_declined_tools"])
	}
	if len(saved) == 0 {
		t.Fatal("decline must be persisted via save_config")
	}
	captured2 := subCaptureChecklist(nil, &saved)
	if _, err := PromptEnableToolGateway(env, io.Discard, cfg, true); err != nil {
		t.Fatal(err)
	}
	if len(captured2.preSelected) != 0 {
		t.Fatalf("second pre selected = %v", captured2.preSelected)
	}
}

func containsAny(list []any, want string) bool {
	for _, item := range list {
		if item == want {
			return true
		}
	}
	return false
}

func TestNousSubscriptionPromptEnableToolGatewayChoosingDeclinedToolClearsDecline(t *testing.T) {
	env := subSeams(t, subPaid())
	nousSubDirectCredsFn = subNoDirect(true)
	captured := subCaptureChecklist([]int{0}, nil)
	cfg := map[string]any{"model": map[string]any{"provider": "nous"}, "tool_gateway_declined_tools": []any{"browser", "web"}}
	if _, err := PromptEnableToolGateway(env, io.Discard, cfg, true); err != nil {
		t.Fatal(err)
	}
	chosenKey := ""
	for _, key := range nousGatewayKeys {
		if strings.HasPrefix(captured.items[0], nousGatewayToolLabel[key]) {
			chosenKey = key
			break
		}
	}
	if chosenKey == "" {
		t.Fatalf("items = %v", captured.items)
	}
	if declined, _ := cfg["tool_gateway_declined_tools"].([]any); containsAny(declined, chosenKey) {
		t.Fatalf("declined = %v still holds %s", declined, chosenKey)
	}
}

// Managed defaults with the FAL key absent for the given account
func subDefaultsFor(t *testing.T, account *NousPortalAccountInfo, cfg map[string]any, toolset string) map[string]bool {
	t.Setenv("FAL_KEY", "")
	env := subSeams(t, account)
	nousSubFalKeyFn = func(PoolEnv) bool { return false }
	return ApplyNousManagedDefaults(env, cfg, []string{toolset}, false)
}

func TestNousSubscriptionApplyNousManagedDefaultsWritesVideoGenConfig(t *testing.T) {
	cfg := map[string]any{"model": map[string]any{"provider": "nous"}}
	changed := subDefaultsFor(t, subPaid(), cfg, "video_gen")
	video, _ := cfg["video_gen"].(map[string]any)
	if !changed["video_gen"] || video["provider"] != "nous" {
		t.Fatalf("changed %v video %v", changed, video)
	}
	if _, ok := video["use_gateway"]; ok {
		t.Fatalf("video = %v", video)
	}
}

func TestNousSubscriptionPaidAccountDefaultsToTheKreaImageModel(t *testing.T) {
	cfg := map[string]any{"model": map[string]any{"provider": "nous"}}
	subDefaultsFor(t, subPaid(), cfg, "image_gen")
	if !reflect.DeepEqual(cfg["image_gen"], map[string]any{"provider": "nous", "model": ManagedImageDefaultKreaModel}) {
		t.Fatalf("image_gen = %v", cfg["image_gen"])
	}
}

func TestNousSubscriptionPoolAccountKeepsTheFalImageDefault(t *testing.T) {
	cfg := map[string]any{"model": map[string]any{"provider": "nous"}}
	subDefaultsFor(t, subPoolAccount(), cfg, "image_gen")
	section, _ := cfg["image_gen"].(map[string]any)
	if _, ok := section["model"]; section["provider"] != "nous" || ok {
		t.Fatalf("image_gen = %v", section)
	}
}

func TestNousSubscriptionFirstRunGatewayAcceptanceWritesTheKreaImageModel(t *testing.T) {
	fresh := map[string]any{}
	ApplyGatewayDefaults(fresh, []string{"image_gen"}, subPaid())
	if !reflect.DeepEqual(fresh["image_gen"], map[string]any{"provider": "nous", "model": ManagedImageDefaultKreaModel}) {
		t.Fatalf("fresh = %v", fresh["image_gen"])
	}
	pooled := map[string]any{}
	ApplyGatewayDefaults(pooled, []string{"image_gen"}, subPoolAccount())
	if _, ok := pooled["image_gen"].(map[string]any)["model"]; ok {
		t.Fatalf("pooled = %v", pooled["image_gen"])
	}
	kept := map[string]any{"image_gen": map[string]any{"model": subFalDefaultModel}}
	ApplyGatewayDefaults(kept, []string{"image_gen"}, subPaid())
	if kept["image_gen"].(map[string]any)["model"] != subFalDefaultModel {
		t.Fatalf("kept = %v", kept["image_gen"])
	}
}

func TestNousSubscriptionConfiguredImageSectionIsLeftAlone(t *testing.T) {
	cfg := map[string]any{"model": map[string]any{"provider": "nous"}, "image_gen": map[string]any{"provider": "nous", "model": subFalDefaultModel}}
	subDefaultsFor(t, subPaid(), cfg, "image_gen")
	if cfg["image_gen"].(map[string]any)["model"] != subFalDefaultModel {
		t.Fatalf("image_gen = %v", cfg["image_gen"])
	}
}

func TestNousSubscriptionAuthPresentReadsTheToolEnvHome(t *testing.T) {
	h := t.TempDir()
	var seen string
	prev := nousSubAccountInfoFn
	nousSubAccountInfoFn = func(env PoolEnv, forceFresh bool) *NousPortalAccountInfo {
		seen = envHome(env)
		return &NousPortalAccountInfo{LoggedIn: !forceFresh}
	}
	t.Cleanup(func() { nousSubAccountInfoFn = prev })
	cfg := map[string]any{"model": map[string]any{"provider": "openrouter"}}
	if !tools.NousSubscriptionAuthPresent(&tools.Env{Home: h}, cfg) || seen != h {
		t.Fatalf("auth present false or home %q, want %q", seen, h)
	}
	nousSubAccountInfoFn = func(PoolEnv, bool) *NousPortalAccountInfo { return nil }
	if tools.NousSubscriptionAuthPresent(&tools.Env{Home: h}, cfg) {
		t.Fatal("no account info reported Nous auth")
	}
}

func TestSwapNousSubAccountInfoReturnsThePreviousLookup(t *testing.T) {
	account := &NousPortalAccountInfo{LoggedIn: true, Source: "jwt"}
	prev := SwapNousSubAccountInfo(func(PoolEnv, bool) *NousPortalAccountInfo { return account })
	if got := nousSubAccountInfoFn(PoolEnv{}, false); got != account {
		SwapNousSubAccountInfo(prev)
		t.Fatalf("lookup %v", got)
	}
	if installed := SwapNousSubAccountInfo(prev); installed(PoolEnv{}, true) != account {
		t.Fatal("swap did not return the installed lookup")
	}
}
