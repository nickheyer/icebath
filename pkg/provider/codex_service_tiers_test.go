package provider

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"reflect"
	"strings"
	"testing"
	"time"
)

const tierBase = "https://chatgpt.com/backend-api/codex"

// JWT shaped token carrying the ChatGPT account id claim
func tierToken(account string) string {
	payload, _ := json.Marshal(map[string]any{"https://api.openai.com/auth": map[string]any{"chatgpt_account_id": account}})
	return "test." + strings.TrimRight(base64.URLEncoding.EncodeToString(payload), "=") + ".test"
}

// Isolated metadata caches and an empty tier catalog store answering the catalog per account
func tierTest(t *testing.T, catalog func(account string) (int, any, error)) *[]string {
	t.Helper()
	_, ft, _ := metaTest(t)
	resetTierCatalogs()
	t.Cleanup(resetTierCatalogs)
	seen := &[]string{}
	ft.handle = func(req *http.Request) (*http.Response, error) {
		account := req.Header.Get("ChatGPT-Account-ID")
		*seen = append(*seen, account)
		status, body, err := catalog(account)
		if err != nil {
			return nil, err
		}
		return jsonResponse(nil, status, body, nil), nil
	}
	return seen
}

func resetTierCatalogs() {
	codexTierCatalogsMu.Lock()
	codexTierCatalogs = map[string]codexTierCatalog{}
	codexTierCatalogsMu.Unlock()
}

func tierWire(t *testing.T, route Route, overrides map[string]any) map[string]any {
	t.Helper()
	return build(t, newCodexTransport(), route.Model, userMsgs("hello"), nil,
		codexParams{requestOverrides: overrides, provider: route.Provider, baseURL: route.BaseURL, isCodexBackend: true})
}

func tierSet(ids ...string) map[string]bool {
	out := map[string]bool{}
	for _, id := range ids {
		out[id] = true
	}
	return out
}

func TestRotationRestoresRequestedTier(t *testing.T) {
	seen := tierTest(t, func(account string) (int, any, error) {
		tiers := []any{map[string]any{"id": "priority"}}
		if account == "B" {
			tiers = append(tiers, map[string]any{"id": "ultrafast"})
		}
		return 200, map[string]any{"models": []any{map[string]any{"slug": "gpt-6-astra", "context_window": 272000, "service_tiers": tiers}}}, nil
	})
	fw := NewFastWindow("ultrafast", map[string]any{"service_tier": "ultrafast"})
	route := Route{Provider: "openai-codex", Mode: ModeCodexResponses, BaseURL: tierBase, Model: "gpt-6-astra-900k"}
	var emitted []any
	for _, account := range []string{"B", "A", "B"} {
		route.APIKey = tierToken(account)
		wire := tierWire(t, route, fw.EffectiveRequestOverrides(route, ""))
		emitted = append(emitted, wire["service_tier"])
		mustEqual(t, wire["model"], any("gpt-6-astra"), "wire model")
		if !reflect.DeepEqual(fw.RequestOverrides, map[string]any{"service_tier": "ultrafast"}) || fw.ServiceTier != "ultrafast" {
			t.Fatalf("user preference mutated %v %q", fw.RequestOverrides, fw.ServiceTier)
		}
	}
	if !reflect.DeepEqual(emitted, []any{"ultrafast", "priority", "ultrafast"}) || !reflect.DeepEqual(*seen, []string{"B", "A"}) {
		t.Fatalf("emitted %v seen %v", emitted, *seen)
	}
}

func TestCatalogCeilingUnknownExpiryAndRouteIsolation(t *testing.T) {
	catalog := []any{map[string]any{"slug": "gpt-6-astra", "context_window": 272000, "service_tiers": []any{}}}
	failed := false
	seen := tierTest(t, func(string) (int, any, error) {
		if failed {
			return 0, nil, errors.New("offline")
		}
		return 200, map[string]any{"models": catalog}, nil
	})
	clock := time.Unix(10000, 0)
	wallNow = func() time.Time { return clock }
	route := Route{Provider: "openai-codex", Mode: ModeCodexResponses, BaseURL: tierBase, APIKey: tierToken("C"), Model: "gpt-6-astra"}
	fw := NewFastWindow("", map[string]any{"service_tier": "ultrafast"})
	effective := func() map[string]any { return fw.EffectiveRequestOverrides(route, "") }
	if _, has := effective()["service_tier"]; has {
		t.Fatal("an empty tier list is explicit Standard only")
	}
	catalog[0] = map[string]any{"slug": "gpt-6-astra", "context_window": 272000}
	route.APIKey = tierToken("D")
	mustEqual(t, effective()["service_tier"], any("ultrafast"), "absent fields mean unknown")
	catalog[0].(map[string]any)["additional_speed_tiers"] = []any{"fast", "ultrafast"}
	route.APIKey = tierToken("E")
	fw.RequestOverrides = map[string]any{"service_tier": "priority"}
	mustEqual(t, effective()["service_tier"], any("priority"), "legacy fast maps to priority")
	fw.RequestOverrides = map[string]any{}
	if got := effective(); len(got) != 0 {
		t.Fatalf("no request leaves no overrides %v", got)
	}
	fw.RequestOverrides = map[string]any{"service_tier": "ultrafast"}
	route.Model = "another-model"
	mustEqual(t, effective()["service_tier"], any("ultrafast"), "absent model is unknown")
	route.Model = "gpt-6-astra"
	route.APIKey = tierToken("C")
	if _, has := effective()["service_tier"]; has {
		t.Fatal("cached Standard only catalog clamps")
	}
	clock = clock.Add(3601 * time.Second)
	failed = true
	mustEqual(t, effective()["service_tier"], any("ultrafast"), "expired catalog is unknown")
	count := len(*seen)
	mustEqual(t, effective()["service_tier"], any("ultrafast"), "negative cache keeps unknown")
	mustEqual(t, len(*seen), count, "no per request probing")
	for _, r := range []struct {
		mode     APIMode
		provider string
		base     string
	}{{"codex", "openai-codex", tierBase}, {ModeCodexResponses, "openai", tierBase}, {ModeCodexResponses, "openai-codex", "https://proxy.example/codex"}} {
		route.Mode, route.Provider, route.BaseURL = r.mode, r.provider, r.base
		mustEqual(t, effective()["service_tier"], any("ultrafast"), "other routes are never negotiated")
	}
	mustEqual(t, len(*seen), count, "other routes never probe")
	if got := advertisedTiers(map[string]any{"service_tiers": []any{}, "additional_speed_tiers": []any{"fast"}}); !reflect.DeepEqual(got, tierSet("priority")) {
		t.Fatalf("legacy fast beside empty modern list %v", got)
	}
	if got := advertisedTiers(map[string]any{"service_tiers": []any{map[string]any{"id": "priority"}}, "additional_speed_tiers": []any{"fast", "ultrafast"}}); !reflect.DeepEqual(got, tierSet("priority")) {
		t.Fatalf("modern list wins %v", got)
	}
}

func TestUltrafastIsOfferedOnTheCodexPlanOnlyWhenItsCatalogListsIt(t *testing.T) {
	down := false
	tierTest(t, func(account string) (int, any, error) {
		if down {
			return 503, map[string]any{}, nil
		}
		tiers := map[string]map[string][]string{
			"A": {"gpt-6-astra": {"priority", "ultrafast"}, "gpt-6.1-sol": {"priority"}},
			"B": {"gpt-6-astra": {"priority", "ultrafast"}, "gpt-6.1-sol": {"priority", "ultrafast"}},
		}
		var models []any
		for _, slug := range []string{"gpt-6-astra", "gpt-6.1-sol"} {
			var ids []any
			for _, id := range tiers[account][slug] {
				ids = append(ids, map[string]any{"id": id})
			}
			models = append(models, map[string]any{"slug": slug, "context_window": 272000, "service_tiers": ids})
		}
		return 200, map[string]any{"models": models}, nil
	})
	offered := func(model, providerName, baseURL, token string) bool {
		got, err := UltrafastOffered(model, providerName, baseURL, token)
		if err != nil {
			t.Fatal(err)
		}
		return got
	}
	if !offered("gpt-6-astra", "openai-codex", tierBase, tierToken("A")) {
		t.Fatal("astra lists ultrafast for A")
	}
	if offered("gpt-6.1-sol-900k", "openai-codex", tierBase, tierToken("A")) {
		t.Fatal("plan lacks Sol Ultrafast")
	}
	if !offered("gpt-6.1-sol-900k", "openai-codex", tierBase, tierToken("B")) {
		t.Fatal("B lists Sol Ultrafast")
	}
	if !offered("gpt-6.1-sol", "openai-api", "https://api.openai.com/v1", "") {
		t.Fatal("the API key route is not plan gated")
	}
	down = true
	if !offered("gpt-6.1-sol", "openai-codex", tierBase, tierToken("C")) {
		t.Fatal("an unreadable catalog never refuses")
	}
	if got, err := UltrafastOfferedFor(Route{Provider: "openai-codex", BaseURL: tierBase, APIKey: tierToken("A")}, "gpt-6.1-sol"); err != nil || got {
		t.Fatalf("route form follows the cached plan %v %v", got, err)
	}
}

var daybreakCatalog = map[string][]any{
	"A": {
		map[string]any{"slug": "gpt-6-sol", "context_window": 272000, "available_access_programs": map[string]any{"cyber": []any{"standard", "daybreak_blue"}}},
		map[string]any{"slug": "gpt-6.1-sol", "context_window": 272000, "available_access_programs": map[string]any{"cyber": []any{"standard"}}},
		map[string]any{"slug": "gpt-6-luna", "context_window": 272000},
	},
	"B": {
		map[string]any{"slug": "gpt-6-sol", "context_window": 272000, "available_access_programs": map[string]any{"cyber": []any{"standard"}}},
	},
}

func daybreakTest(t *testing.T) *[]string {
	return tierTest(t, func(account string) (int, any, error) {
		return 200, map[string]any{"models": daybreakCatalog[account]}, nil
	})
}

// Route and fast window of a Daybreak enabled Codex subscription turn
func daybreakAgent(overrides map[string]any) (Route, *FastWindow) {
	fw := NewFastWindow("", overrides)
	fw.Daybreak = true
	return Route{Provider: "openai-codex", Mode: ModeCodexResponses, BaseURL: tierBase, APIKey: tierToken("A"), Model: "gpt-6-sol"}, fw
}

func daybreakWire(t *testing.T, route Route, fw *FastWindow) map[string]any {
	t.Helper()
	extra, _ := tierWire(t, route, fw.EffectiveRequestOverrides(route, ""))["extra_body"].(map[string]any)
	return extra
}

func TestEnabledOnAListedModelSendsDaybreakBlue(t *testing.T) {
	daybreakTest(t)
	route, fw := daybreakAgent(map[string]any{"extra_body": map[string]any{"kept": 1}})
	want := map[string]any{"kept": 1, "access_programs": map[string]any{"cyber": "daybreak_blue"}}
	if got := daybreakWire(t, route, fw); !reflect.DeepEqual(got, want) {
		t.Fatalf("extra body %v", got)
	}
	if !reflect.DeepEqual(fw.RequestOverrides, map[string]any{"extra_body": map[string]any{"kept": 1}}) {
		t.Fatalf("user overrides mutated %v", fw.RequestOverrides)
	}
}

func TestDaybreakIsNeverSentWithoutAnExplicitCatalogListing(t *testing.T) {
	changes := map[string]func(*Route, *FastWindow){
		"switch off":         func(_ *Route, fw *FastWindow) { fw.Daybreak = false },
		"standard only":      func(r *Route, _ *FastWindow) { r.Model = "gpt-6.1-sol" },
		"catalog silent":     func(r *Route, _ *FastWindow) { r.Model = "gpt-6-luna" },
		"account without":    func(r *Route, _ *FastWindow) { r.APIKey = tierToken("B") },
		"api key route":      func(r *Route, _ *FastWindow) { r.Provider, r.BaseURL = "openai-api", "https://api.openai.com/v1" },
		"unofficial backend": func(r *Route, _ *FastWindow) { r.BaseURL = "https://proxy.example/codex" },
	}
	for name, change := range changes {
		t.Run(name, func(t *testing.T) {
			daybreakTest(t)
			route, fw := daybreakAgent(map[string]any{})
			change(&route, fw)
			if _, has := daybreakWire(t, route, fw)["access_programs"]; has {
				t.Fatal("daybreak sent without a listing")
			}
		})
	}
}

func TestCheckedPerRequestSoAModelSwitchDropsIt(t *testing.T) {
	seen := daybreakTest(t)
	route, fw := daybreakAgent(map[string]any{})
	for _, step := range []struct {
		model string
		want  bool
	}{{"gpt-6-sol", true}, {"gpt-6.1-sol", false}, {"gpt-6-sol", true}} {
		route.Model = step.model
		_, has := daybreakWire(t, route, fw)["access_programs"]
		mustEqual(t, has, step.want, step.model)
	}
	if !reflect.DeepEqual(*seen, []string{"A"}) {
		t.Fatalf("one catalog read then the cache %v", *seen)
	}
}

func TestUnreachableCatalogHidesAndNeverSends(t *testing.T) {
	tierTest(t, func(string) (int, any, error) { return 503, map[string]any{}, nil })
	if got, err := DaybreakOffered("gpt-6-sol", "openai-codex", tierBase, tierToken("A")); err != nil || got {
		t.Fatalf("unreachable catalog offered %v %v", got, err)
	}
	route, fw := daybreakAgent(map[string]any{})
	if _, has := daybreakWire(t, route, fw)["access_programs"]; has {
		t.Fatal("daybreak sent on an unreachable catalog")
	}
}

func TestOfferedOnlyWhereTheAccountListsIt(t *testing.T) {
	daybreakTest(t)
	for _, c := range []struct {
		model, provider, base, account string
		want                           bool
	}{
		{"gpt-6-sol", "openai-codex", tierBase, "A", true},
		{"gpt-6.1-sol", "openai-codex", tierBase, "A", false},
		{"gpt-6-sol", "openai-codex", tierBase, "B", false},
		{"gpt-6-sol", "openai-api", "https://api.openai.com/v1", "A", false},
	} {
		got, err := DaybreakOffered(c.model, c.provider, c.base, tierToken(c.account))
		if err != nil || got != c.want {
			t.Fatalf("%+v got %v %v", c, got, err)
		}
	}
}

func TestARefreshedCatalogPublishesProgramsWithItsTimestamp(t *testing.T) {
	daybreakTest(t)
	route, fw := daybreakAgent(map[string]any{})
	if _, has := daybreakWire(t, route, fw)["access_programs"]; !has {
		t.Fatal("listed model sends daybreak")
	}
	codexTierCatalogsMu.Lock()
	var keys []string
	for key := range codexTierCatalogs {
		keys = append(keys, key)
	}
	codexTierCatalogsMu.Unlock()
	if len(keys) != 1 {
		t.Fatalf("one record per credential %v", keys)
	}
	rememberCodexCatalog(keys[0], []any{map[string]any{"slug": "gpt-6-sol", "available_access_programs": map[string]any{"cyber": []any{"standard"}}}})
	codexTierCatalogsMu.Lock()
	programs := codexTierCatalogs[keys[0]].programs
	codexTierCatalogsMu.Unlock()
	if !reflect.DeepEqual(programs, map[string]map[string]bool{"gpt-6-sol": tierSet("standard")}) {
		t.Fatalf("programs %v", programs)
	}
	if _, has := daybreakWire(t, route, fw)["access_programs"]; has {
		t.Fatal("revoked listing still sent")
	}
}

func TestCatalogStoreEvictsTheOldestPastTheCap(t *testing.T) {
	resetTierCatalogs()
	t.Cleanup(func() { wallNow = time.Now; resetTierCatalogs() })
	clock := time.Unix(5000, 0)
	wallNow = func() time.Time { return clock }
	for i := 0; i < codexTierMaxCatalogs; i++ {
		clock = clock.Add(time.Second)
		rememberCodexCatalog("key-"+strings.Repeat("x", i), nil)
	}
	clock = clock.Add(time.Second)
	rememberCodexCatalog("newest", nil)
	codexTierCatalogsMu.Lock()
	defer codexTierCatalogsMu.Unlock()
	_, oldest := codexTierCatalogs["key-"]
	_, newest := codexTierCatalogs["newest"]
	if len(codexTierCatalogs) != codexTierMaxCatalogs || oldest || !newest {
		t.Fatalf("eviction kept %d oldest %v newest %v", len(codexTierCatalogs), oldest, newest)
	}
}
