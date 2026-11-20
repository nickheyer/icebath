package provider

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/home"
)

// Replaces the router module slot and per home slots for one test, restoring them afterwards
func swapRouterState(t *testing.T, efforts map[string][]string, diskChecked bool) {
	t.Helper()
	routerMu.Lock()
	prevModule, prevHomes, prevSpawn, prevSuppressed := routerModuleState, routerStateByHome, routerWarmSpawn, routerWarmSuppressed
	routerModuleState = &routerCacheState{efforts: efforts, diskChecked: diskChecked}
	routerStateByHome = map[string]*routerCacheState{}
	routerMu.Unlock()
	t.Cleanup(func() {
		routerMu.Lock()
		routerModuleState, routerStateByHome, routerWarmSpawn, routerWarmSuppressed = prevModule, prevHomes, prevSpawn, prevSuppressed
		routerMu.Unlock()
	})
}

func seededRouterCatalog(t *testing.T) Profile {
	p := bundledPluginProfile(t, "router")
	swapRouterState(t, map[string][]string{
		"grok-4.6":                          {"minimal", "low", "medium", "high", "xhigh"},
		"gpt-4.1-mini":                      {},
		"accounts/fireworks/models/kimi-k3": {"minimal", "low", "medium", "high", "xhigh", "max"},
	}, true)
	return p
}

func TestRouterPluginProfileMatchesUpstreamRegistration(t *testing.T) {
	t.Setenv("RAMP_ROUTER_BASE_URL", "")
	p := bundledPluginProfile(t, "router")
	assertProfileFields(t, p, map[string]any{
		"name": "router", "aliases": []string{"ramp-router", "ramp", "router.com"}, "api_mode": "codex_responses",
		"display_name": "Ramp Router", "description": "Ramp Router (router.com) \u2014 routes each request to the cheapest model that clears your quality bar",
		"signup_url": "https://app.router.com/keys", "env_vars": []string{"RAMP_ROUTER_API_KEY", "ROUTER_API_KEY", "RAMP_ROUTER_BASE_URL"},
		"base_url": "https://api.router.com/v1", "auth_type": "api_key", "supports_vision": true, "default_aux_model": "gpt-5.4-mini",
		"fallback_models": []string{}, "base_url_env_var": "", "aggregator": false, "supports_model_listing": true, "supports_health_check": true,
	})
	if got := p.DefaultHeaders(); !reflect.DeepEqual(got, map[string]string{"User-Agent": UserAgent("Icebath-Agent")}) {
		t.Fatalf("headers = %v", got)
	}
}

func TestRouterBaseURLComesFromTheEnvironmentAtRegistration(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	t.Setenv("RAMP_ROUTER_BASE_URL", "https://eu.router.test/v1/")
	if p := routerPluginProfiles()[0]; p.BaseURL != "https://eu.router.test/v1" {
		t.Fatalf("base = %s", p.BaseURL)
	}
}

func TestRouterDeclaredVocabulary(t *testing.T) {
	p := seededRouterCatalog(t)
	if got, ok, err := p.SupportedReasoningEfforts(context.Background(), "grok-4.6"); err != nil || !ok || !reflect.DeepEqual(got, []string{"minimal", "low", "medium", "high", "xhigh"}) {
		t.Fatalf("efforts = %v %v", got, ok)
	}
}

func TestRouterNonReasoningModelIsDefinitiveEmpty(t *testing.T) {
	p := seededRouterCatalog(t)
	if got, ok, err := p.SupportedReasoningEfforts(context.Background(), "gpt-4.1-mini"); err != nil || !ok || len(got) != 0 {
		t.Fatalf("efforts = %v %v", got, ok)
	}
}

func TestRouterUnknownModelIsNone(t *testing.T) {
	p := seededRouterCatalog(t)
	if got, ok, err := p.SupportedReasoningEfforts(context.Background(), "some-byok-route"); err != nil || ok {
		t.Fatalf("efforts = %v", got)
	}
}

func TestRouterColdCacheIsNoneAndNeverBlocks(t *testing.T) {
	p := bundledPluginProfile(t, "router")
	swapRouterState(t, nil, true)
	routerWarmSpawn = func(func()) {}
	if got, ok, err := p.SupportedReasoningEfforts(context.Background(), "grok-4.6"); err != nil || ok {
		t.Fatalf("efforts = %v", got)
	}
}

func TestRouterParseEffortsCatalogShapes(t *testing.T) {
	var items []any
	if err := json.Unmarshal([]byte(`[
		{"id": "grok-4.6", "router": {"capabilities": {"reasoning": {"supported": true, "efforts": [{"value": "low"}, {"value": "high"}]}}}},
		{"id": "gpt-4.1", "router": {"capabilities": {"reasoning": {"supported": false, "efforts": []}}}},
		{"id": "mystery-model", "router": {"capabilities": {"reasoning": {"supported": true, "efforts": []}}}},
		{"id": "bare-model"}]`), &items); err != nil {
		t.Fatal(err)
	}
	if got := routerParseEfforts(items); !reflect.DeepEqual(got, map[string][]string{"grok-4.6": {"low", "high"}, "gpt-4.1": {}}) {
		t.Fatalf("parsed = %#v", got)
	}
}

func TestRouterUnrecognizedEffortLevelsAreDroppedAtIngest(t *testing.T) {
	var items []any
	if err := json.Unmarshal([]byte(`[
		{"id": "future-model", "router": {"capabilities": {"reasoning": {"supported": true, "efforts": [{"value": "low"}, {"value": "hyperthink"}, {"value": "high"}]}}}},
		{"id": "alien-model", "router": {"capabilities": {"reasoning": {"supported": true, "efforts": [{"value": "hyperthink"}, {"value": "galaxy"}]}}}}]`), &items); err != nil {
		t.Fatal(err)
	}
	if got := routerParseEfforts(items); !reflect.DeepEqual(got, map[string][]string{"future-model": {"low", "high"}}) {
		t.Fatalf("parsed = %#v", got)
	}
}

func TestRouterFetchModelsDedupesWhilePreservingCatalogOrder(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"data": [{"id": "b"}, {"id": "a"}, {"id": "b"}, {"id": "c"}]}`))
	}))
	defer srv.Close()
	t.Setenv("RAMP_ROUTER_BASE_URL", srv.URL+"/v1")
	p := bundledPluginProfile(t, "router")
	swapRouterState(t, nil, false)
	if got, ok := p.FetchModels(context.Background(), "", "", 0); !ok || !reflect.DeepEqual(got, []string{"b", "a", "c"}) {
		t.Fatalf("models = %v %v", got, ok)
	}
}

func routerCodexKwargs(t *testing.T, model, provider, baseURL, effort string) map[string]any {
	t.Helper()
	p := codexParams{baseURL: baseURL, sessionID: "sid", provider: provider}
	if effort != "" {
		p.reasoning = reasoningCfg{set: true, effort: effort}
	}
	return build(t, newCodexTransport(), model, userMsgs("Hi"), nil, p)
}

func routerEffort(t *testing.T, kw map[string]any) any {
	t.Helper()
	r, ok := kw["reasoning"].(map[string]any)
	if !ok {
		t.Fatalf("no reasoning in %#v", kw)
	}
	return r["effort"]
}

func TestRouterTransportClampsToCatalogVocabulary(t *testing.T) {
	seededRouterCatalog(t)
	if got := routerEffort(t, routerCodexKwargs(t, "grok-4.6", "router", "https://api.router.com/v1", "max")); got != "xhigh" {
		t.Fatalf("effort = %v", got)
	}
}

func TestRouterTransportSupportedEffortPassesThrough(t *testing.T) {
	seededRouterCatalog(t)
	if got := routerEffort(t, routerCodexKwargs(t, "accounts/fireworks/models/kimi-k3", "router", "https://api.router.com/v1", "max")); got != "max" {
		t.Fatalf("effort = %v", got)
	}
}

func TestRouterTransportNonReasoningModelSuppressesReasoning(t *testing.T) {
	seededRouterCatalog(t)
	kw := routerCodexKwargs(t, "gpt-4.1-mini", "router", "https://api.router.com/v1", "")
	if _, ok := kw["reasoning"]; ok {
		t.Fatalf("reasoning sent %#v", kw)
	}
	if inc := reflect.ValueOf(kw["include"]); !inc.IsValid() || inc.Len() != 0 {
		t.Fatalf("include = %#v", kw["include"])
	}
}

func TestRouterTransportUnknownModelFallsBackToCodexDefault(t *testing.T) {
	seededRouterCatalog(t)
	if got := routerEffort(t, routerCodexKwargs(t, "some-byok-route", "router", "https://api.router.com/v1", "max")); got != "xhigh" {
		t.Fatalf("effort = %v", got)
	}
	if got := routerEffort(t, routerCodexKwargs(t, "some-byok-route", "router", "https://api.router.com/v1", "medium")); got != "medium" {
		t.Fatalf("effort = %v", got)
	}
}

func TestRouterTransportColdCacheKeepsDefaultBehavior(t *testing.T) {
	bundledPluginProfile(t, "router")
	swapRouterState(t, nil, true)
	routerWarmSpawn = func(func()) {}
	if got := routerEffort(t, routerCodexKwargs(t, "grok-4.6", "router", "https://api.router.com/v1", "xhigh")); got != "xhigh" {
		t.Fatalf("effort = %v", got)
	}
}

func TestRouterTransportOtherProvidersUnaffected(t *testing.T) {
	seededRouterCatalog(t)
	if got := routerEffort(t, routerCodexKwargs(t, "gpt-4.1-mini", "some-other-provider", "https://generic.example.com/v1", "medium")); got != "medium" {
		t.Fatalf("effort = %v", got)
	}
}

func TestRouterNamedCustomProviderAtRouterHostGetsTheClamp(t *testing.T) {
	seededRouterCatalog(t)
	if got := routerEffort(t, routerCodexKwargs(t, "grok-4.6", "custom:my-proxy", "https://api.router.com/v1", "max")); got != "xhigh" {
		t.Fatalf("effort = %v", got)
	}
}

func TestRouterForeignHostDoesNotBorrowTheRouterVocabulary(t *testing.T) {
	seededRouterCatalog(t)
	if got := routerEffort(t, routerCodexKwargs(t, "grok-4.6", "my-proxy", "https://generic.example.com/v1", "max")); got != "xhigh" {
		t.Fatalf("effort = %v", got)
	}
}

func TestRouterEffortsCacheAndBaseURLFollowTheActiveProfile(t *testing.T) {
	var mu sync.Mutex
	var fetched []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth := r.Header.Get("Authorization")
		tag := auth[strings.LastIndex(auth, "-")+1:]
		mu.Lock()
		fetched = append(fetched, r.URL.Path)
		mu.Unlock()
		level := "high"
		if tag == "A" {
			level = "low"
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"data": []any{map[string]any{"id": "model-" + tag,
			"router": map[string]any{"capabilities": map[string]any{"reasoning": map[string]any{"supported": true, "efforts": []any{map[string]any{"value": level}}}}}}}})
	}))
	defer srv.Close()
	root := filepath.Join(t.TempDir(), ".icebath")
	a, b := root, filepath.Join(root, "profiles", "B")
	for _, hc := range [][2]string{{a, "A"}, {b, "B"}} {
		if err := os.MkdirAll(hc[0], 0o755); err != nil {
			t.Fatal(err)
		}
		env := "RAMP_ROUTER_API_KEY=router-key-" + hc[1] + "\nRAMP_ROUTER_BASE_URL=" + srv.URL + "/" + strings.ToLower(hc[1]) + "/v1\n"
		if err := os.WriteFile(filepath.Join(hc[0], ".env"), []byte(env), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("ICEBATH_HOME", a)
	t.Setenv("RAMP_ROUTER_API_KEY", "")
	t.Setenv("RAMP_ROUTER_BASE_URL", "")
	swapRouterState(t, nil, false)
	routerWarmSuppressed = func() bool { return true }
	ctxA, ctxB := home.WithOverride(context.Background(), a), home.WithOverride(context.Background(), b)
	p, _ := GetProviderProfile(ctxA, "router")
	if got := routerBaseURL(ctxA); got != srv.URL+"/a/v1" {
		t.Fatalf("A base = %s", got)
	}
	p.FetchModels(ctxA, "", "", 0)
	if got, ok, err := p.SupportedReasoningEfforts(ctxA, "model-A"); err != nil || !ok || !reflect.DeepEqual(got, []string{"low"}) {
		t.Fatalf("A efforts = %v %v", got, ok)
	}
	if got := routerBaseURL(ctxB); got != srv.URL+"/b/v1" {
		t.Fatalf("B base = %s", got)
	}
	if got, ok, err := p.SupportedReasoningEfforts(ctxB, "model-A"); err != nil || ok {
		t.Fatalf("B sees A verdicts %v", got)
	}
	p.FetchModels(ctxB, "", "", 0)
	if got, ok, err := p.SupportedReasoningEfforts(ctxB, "model-B"); err != nil || !ok || !reflect.DeepEqual(got, []string{"high"}) {
		t.Fatalf("B efforts = %v %v", got, ok)
	}
	if got, ok, err := p.SupportedReasoningEfforts(ctxA, "model-A"); err != nil || !ok || !reflect.DeepEqual(got, []string{"low"}) {
		t.Fatalf("A efforts after B = %v %v", got, ok)
	}
	if routerModuleState.efforts != nil {
		t.Fatalf("module slot touched %v", routerModuleState.efforts)
	}
	mu.Lock()
	fetched = nil
	mu.Unlock()
	routerWarmSuppressed = func() bool { return false }
	done := make(chan struct{})
	routerWarmSpawn = func(refresh func()) { go func() { refresh(); close(done) }() }
	routerWarmEffortsAsync(ctxB)
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("warm never ran")
	}
	mu.Lock()
	defer mu.Unlock()
	if len(fetched) == 0 || fetched[0] != "/b/v1/models" {
		t.Fatalf("warm fetched %v", fetched)
	}
}

func writeRouterMirror(t *testing.T, ctx context.Context, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(routerDiskPath(ctx)), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(routerDiskPath(ctx), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestRouterDiskMirrorHitSeedsTheCacheAndFailsTheLookupWithoutWarming(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	t.Setenv("RAMP_ROUTER_API_KEY", "k")
	ctx := context.Background()
	for _, body := range []string{`{"ts": 0, "efforts": {"m": ["high"]}}`, `{"efforts": {"m": ["high"]}}`,
		`{"ts": "1e999", "efforts": {"m": ["high"]}}`, `{"ts": NaN, "efforts": {"m": ["high"]}}`, `{"ts": [], "efforts": {"m": ["high"]}}`} {
		swapRouterState(t, nil, false)
		warmed := 0
		routerWarmSpawn = func(func()) { warmed++ }
		writeRouterMirror(t, ctx, body)
		if got, ok, err := routerSupportedEfforts(ctx, "m"); !errors.Is(err, errRouterDiskTTLUndefined) || ok || got != nil {
			t.Fatalf("%s first lookup = %v %v %v", body, got, ok, err)
		}
		if got, ok, err := routerSupportedEfforts(ctx, "m"); err != nil || !ok || !reflect.DeepEqual(got, []string{"high"}) || warmed != 0 {
			t.Fatalf("%s second lookup = %v %v %v warmed %d", body, got, ok, err, warmed)
		}
	}
	routerSaveDisk(ctx, map[string][]string{"m": {"low"}})
	swapRouterState(t, nil, false)
	routerWarmSpawn = func(func()) { t.Fatal("warmer started from the disk mirror path") }
	if got, ok := profileDeclaredEfforts(ctx, "router", "m", "https://api.router.com/v1"); ok || got != nil {
		t.Fatalf("codex lookup did not fail open = %v %v", got, ok)
	}
	if got, ok, err := routerSupportedEfforts(ctx, "m"); err != nil || !ok || !reflect.DeepEqual(got, []string{"low"}) {
		t.Fatalf("mirror not installed = %v %v %v", got, ok, err)
	}
	swapRouterState(t, nil, false)
	routerWarmSpawn = func(func()) { t.Fatal("warmer started from the disk mirror path") }
	if got, ok := profileDeclaredEfforts(ctx, "custom:my-proxy", "m", "https://api.router.com/v1"); ok || got != nil {
		t.Fatalf("custom route at the router host fell through to its own vocabulary = %v %v", got, ok)
	}
}

func TestRouterDiskMirrorUnparseableStampIsNoCache(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	t.Setenv("RAMP_ROUTER_API_KEY", "k")
	ctx := context.Background()
	for _, body := range []string{`{"ts": "soon", "efforts": {"m": ["high"]}}`, `{"ts": [1], "efforts": {"m": ["high"]}}`,
		`{"ts": {"a": 1}, "efforts": {"m": ["high"]}}`, "{\"ts\": 1" + strings.Repeat("0", 400) + ", \"efforts\": {\"m\": [\"high\"]}}",
		`["m"]`, `{"ts": 0, "efforts": {}}`, `{"ts": 0, "efforts": {"m": "high"}}`, "\xef\xbb\xbf\xef\xbb\xbf{}"} {
		swapRouterState(t, nil, false)
		warmed := 0
		routerWarmSpawn = func(func()) { warmed++ }
		writeRouterMirror(t, ctx, body)
		if got, ok, err := routerSupportedEfforts(ctx, "m"); err != nil || ok || got != nil || warmed != 1 {
			t.Fatalf("%q lookup = %v %v %v warmed %d", body, got, ok, err, warmed)
		}
		if st := routerState(ctx); st.efforts != nil || !st.diskChecked {
			t.Fatalf("%q state = %+v", body, st)
		}
	}
	if got := routerLoadDisk(ctx); got != nil {
		t.Fatalf("mirror = %v", got)
	}
	writeRouterMirror(t, ctx, "\xef\xbb\xbf{\"ts\": \"12.5\", \"efforts\": {\"m\": [1, true, null, \"x\"], \"n\": 3}}")
	if got := routerLoadDisk(ctx); !reflect.DeepEqual(got, map[string][]string{"m": {"1", "True", "None", "x"}}) {
		t.Fatalf("mirror = %v", got)
	}
}

func TestRouterSupportedEffortsFollowTheHomeOverride(t *testing.T) {
	a, b := t.TempDir(), t.TempDir()
	t.Setenv("ICEBATH_HOME", a)
	swapRouterState(t, nil, true)
	routerWarmSuppressed = func() bool { return true }
	p := bundledPluginProfile(t, "router")
	ctxB := home.WithOverride(context.Background(), b)
	routerSeedEfforts(ctxB, []any{map[string]any{"id": "m", "router": map[string]any{"capabilities": map[string]any{"reasoning": map[string]any{
		"efforts": []any{map[string]any{"value": "low"}, map[string]any{"value": "high"}}}}}}})
	if got, ok, err := p.SupportedReasoningEfforts(ctxB, "m"); err != nil || !ok || !reflect.DeepEqual(got, []string{"low", "high"}) {
		t.Fatalf("override home efforts = %v %v %v", got, ok, err)
	}
	if got, ok, err := p.SupportedReasoningEfforts(context.Background(), "m"); err != nil || ok {
		t.Fatalf("launch home borrowed the override efforts = %v %v %v", got, ok, err)
	}
}
