package provider

import (
	"encoding/json"
	"testing"
	"time"
)

// Server allowance list day, week and top tier in display order, decoded as the wire delivers it
func usageGaugesWire(t *testing.T, dayBp, weekBp, topBp int) []any {
	t.Helper()
	far := time.Now().UnixMilli() + 30*86_400_000
	raw, _ := json.Marshal([]map[string]any{
		{"id": "day", "scope": "all", "label": "Today", "used_bp": dayBp, "resets_at_ms": far},
		{"id": "week", "scope": "all", "label": "This week", "used_bp": weekBp, "resets_at_ms": far},
		{"id": "top_tier", "scope": "top_tier", "label": "Large models (monthly)", "used_bp": topBp, "resets_at_ms": far},
	})
	decoded, err := decodeExactJSON(string(raw))
	if err != nil {
		t.Fatal(err)
	}
	return decoded.([]any)
}

func TestUsageGaugeParseKeepsOrderAndDropsWhatItCannotRead(t *testing.T) {
	far := time.Now().UnixMilli() + 86_400_000
	tooFar := time.Now().UnixMilli() + 400*86_400_000
	header, _ := json.Marshal([]any{
		map[string]any{"id": "week", "scope": "all", "label": " This week ", "used_bp": 4000, "resets_at_ms": far},
		map[string]any{"id": "day", "scope": "all", "label": "  ", "used_bp": 10000, "resets_at_ms": tooFar},
		map[string]any{"id": "day", "scope": "all", "label": "dup", "used_bp": 1},
		map[string]any{"id": "hour", "scope": "some", "used_bp": 1},
		map[string]any{"id": "x", "scope": "all", "used_bp": 10001},
		map[string]any{"id": "y", "scope": "all", "used_bp": -1},
		map[string]any{"id": "z", "scope": "all", "used_bp": true},
		map[string]any{"id": "w", "scope": "all", "used_bp": 1.5},
		map[string]any{"id": "", "scope": "all", "used_bp": 1},
		"not an object",
		map[string]any{"id": "top_tier", "scope": "top_tier", "label": "Large", "used_bp": 0, "resets_at_ms": false},
	})
	gauges := ParseGauges(string(header))
	if len(gauges) != 3 {
		t.Fatalf("%+v", gauges)
	}
	if g := gauges[0]; g.ID != "week" || g.Label != "This week" || g.UsedBp != 4000 || g.ResetsAtMs == nil || *g.ResetsAtMs != far {
		t.Fatalf("%+v", g)
	}
	if g := gauges[1]; g.ID != "day" || g.Label != "day" || !g.Full() || g.ResetsAtMs != nil {
		t.Fatalf("%+v", g)
	}
	if g := gauges[2]; g.Scope != ScopeTopTier || g.ResetsAtMs != nil {
		t.Fatalf("%+v", g)
	}
	for _, absent := range []any{nil, "", "not json", "[1] trailing", "{}", "[]", []any{}, []WindowGauge{}, 5} {
		if got := ParseGauges(absent); got != nil {
			t.Fatalf("%#v parsed to %+v", absent, got)
		}
	}
	if got := ParseGauges(gauges); len(got) != 3 || got[0].ID != "week" {
		t.Fatalf("typed gauges not passed through: %+v", got)
	}
	if got := ParseGauges(usageGaugesWire(t, 1, 2, 3)); len(got) != 3 || got[2].UsedBp != 3 {
		t.Fatalf("decoded wire list: %+v", got)
	}
	if got := ParseGauges([]any{map[string]any{"id": "d", "scope": "all", "used_bp": 7.0, "resets_at_ms": 0.0}}); len(got) != 1 || got[0].UsedBp != 7 || got[0].ResetsAtMs != nil {
		t.Fatalf("float decoded integers: %+v", got)
	}
}

func TestUsageGaugeBindingTopTierAndWords(t *testing.T) {
	r := func(ms int64) *int64 { return &ms }
	day := WindowGauge{ID: "day", Scope: ScopeAll, Label: "Today", UsedBp: BPFull, ResetsAtMs: r(1_000)}
	week := WindowGauge{ID: "week", Scope: ScopeAll, Label: "This week", UsedBp: BPFull, ResetsAtMs: r(9_000)}
	top := WindowGauge{ID: "top_tier", Scope: ScopeTopTier, Label: "Large", UsedBp: 20_000}
	if g := BindingGauge([]WindowGauge{day, week, top}); g == nil || g.ID != "week" {
		t.Fatalf("both full binds the later reset: %+v", g)
	}
	day.UsedBp, week.UsedBp = 7_000, 7_000
	if g := BindingGauge([]WindowGauge{day, week, top}); g == nil || g.ID != "day" {
		t.Fatalf("a tie binds the first listed: %+v", g)
	}
	week.UsedBp = 8_000
	if g := BindingGauge([]WindowGauge{day, week}); g == nil || g.ID != "week" {
		t.Fatalf("none full binds the fullest: %+v", g)
	}
	if BindingGauge([]WindowGauge{top}) != nil || TopTierGauge([]WindowGauge{day}) != nil {
		t.Fatal("missing scopes must read as absent")
	}
	if g := TopTierGauge([]WindowGauge{day, top}); g == nil || g.ID != "top_tier" || g.UsedFraction() != 1.0 {
		t.Fatalf("%+v", g)
	}
	if got := AllScope([]WindowGauge{top, day, week}); len(got) != 2 || got[0].ID != "day" {
		t.Fatalf("%+v", got)
	}
	if (WindowGauge{UsedBp: -5}).UsedFraction() != 0 || (WindowGauge{UsedBp: 2_500}).UsedFraction() != 0.25 {
		t.Fatal("fraction clamp")
	}
	other := WindowGauge{ID: "fortnight", Scope: ScopeAll, Label: "This fortnight"}
	for _, tc := range []struct {
		g                   WindowGauge
		title, limit, short string
	}{
		{day, "Today", "daily", "today"}, {week, "This week", "weekly", "week"},
		{WindowGauge{ID: "month"}, "This month", "monthly", "month"},
		{top, "Large models (monthly)", "monthly large-model", "large models (monthly)"},
		{other, "This fortnight", "This fortnight", "This fortnight"},
	} {
		if GaugeTitle(tc.g) != tc.title || GaugeLimitName(tc.g) != tc.limit || GaugeShort(tc.g) != tc.short {
			t.Fatalf("%s words: %q %q %q", tc.g.ID, GaugeTitle(tc.g), GaugeLimitName(tc.g), GaugeShort(tc.g))
		}
	}
	if !PausedForPayment("payment_past_due") || PausedForPayment("unknown") || PausedForPayment("") {
		t.Fatal("payment pause reasons")
	}
}

func TestNousAccountDTOCarriesKeepGoingOnCredits(t *testing.T) {
	if w := nousCoerceUsageWindow(map[string]any{"keep_going_on_credits": false}); w.KeepGoingOnCredits == nil || *w.KeepGoingOnCredits {
		t.Fatalf("%+v", w)
	}
	if w := nousCoerceUsageWindow(map[string]any{}); w.KeepGoingOnCredits != nil {
		t.Fatalf("%+v", w)
	}
}

func TestNousAccountPartialOrEmptyUsageWindowBlockIsTreatedAsAbsent(t *testing.T) {
	all := ParseGauges(usageGaugesWire(t, 100, 50, 0))
	complete := &NousUsageWindowInfo{Gauges: all}
	topOnly := &NousUsageWindowInfo{Gauges: []WindowGauge{*TopTierGauge(all)}}
	f := func(v float64) *float64 { return &v }
	plan := "Credit Plan"
	for _, tc := range []struct {
		window   *NousUsageWindowInfo
		isWindow bool
	}{{complete, true}, {&NousUsageWindowInfo{}, false}, {topOnly, false}} {
		info := &NousPortalAccountInfo{LoggedIn: true, PaidServiceAccess: plBool(true), UsageWindow: tc.window,
			PaidServiceAccessInfo: &NousPaidServiceAccessInfo{PurchasedCreditsRemaining: f(1.0), SubscriptionCreditsRemaining: f(18.0), TotalUsableCredits: f(19.0)},
			Subscription:          &NousPortalSubscriptionInfo{Plan: &plan, MonthlyCredits: f(20.0), CreditsRemaining: f(18.0), RolloverCredits: f(0)}}
		if UsageModelFromAccount(info).Available != !tc.isWindow {
			t.Fatalf("usage model availability for window=%v", tc.isWindow)
		}
		if (FormatUsageWindowBlockGuidance(info) != "") != tc.isWindow {
			t.Fatalf("guidance for window=%v: %q", tc.isWindow, FormatUsageWindowBlockGuidance(info))
		}
	}
}

func TestNousPortalUsageWindowGuidanceAndTopupURL(t *testing.T) {
	f := func(v float64) *float64 { return &v }
	off := false
	window := &NousUsageWindowInfo{Gauges: ParseGauges(usageGaugesWire(t, 7_600, 4_000, 10_000)), AutoTopUpEnabled: &off, KeepGoingOnCredits: &off}
	info := &NousPortalAccountInfo{LoggedIn: true, PaidServiceAccess: plBool(true), PortalBaseURL: "https://portal.example.test/",
		PaidServiceAccessInfo: &NousPaidServiceAccessInfo{PurchasedCreditsRemaining: f(5.0)}, UsageWindow: window}
	if got := FormatUsageWindowBlockGuidance(info); got != "Turn on Keep going in /topup to use your credits now: https://portal.example.test/billing" {
		t.Fatal(got)
	}
	info.PaidServiceAccessInfo.PurchasedCreditsRemaining = f(0)
	if got := FormatUsageWindowBlockGuidance(info); got != "Add credits and turn on Keep going in /topup to keep going now: https://portal.example.test/billing" {
		t.Fatal(got)
	}
	window.KeepGoingOnCredits = nil
	info.OrgSlug = " acme/team "
	want := "Add credits to keep going now: https://portal.example.test/orgs/acme%2Fteam/billing?topup=open\nTurn on auto top-up in the same place so this doesn't stop you next time."
	if got := FormatUsageWindowBlockGuidance(info); got != want {
		t.Fatal(got)
	}
	window.PausedReason = "payment_past_due"
	if got := FormatUsageWindowBlockGuidance(info); got != "Update your card to restore included usage, or add credits to keep going now: https://portal.example.test/billing" {
		t.Fatal(got)
	}
	window.PausedReason = "unknown"
	if got := FormatUsageWindowBlockGuidance(info); got != "Included usage is paused, or add credits to keep going now: https://portal.example.test/billing" {
		t.Fatal(got)
	}
	if NousPortalTopupURL(nil) != "https://portal.nousresearch.com/billing?topup=open" || NousPortalBillingURL("  ") != "https://portal.nousresearch.com/billing" {
		t.Fatal("default portal URLs")
	}
}
