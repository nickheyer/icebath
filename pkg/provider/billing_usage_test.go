package provider

import (
	"math"
	"strings"
	"testing"
)

func buF(v float64) *float64 { return &v }

func buAccount(paid *bool, sub *NousPortalSubscriptionInfo, acc *NousPaidServiceAccessInfo) *NousPortalAccountInfo {
	return &NousPortalAccountInfo{LoggedIn: true, PaidServiceAccess: paid, Subscription: sub, PaidServiceAccessInfo: acc}
}

func buPlus() *NousPortalSubscriptionInfo {
	return &NousPortalSubscriptionInfo{Plan: cpStr("Plus"), MonthlyCredits: buF(20)}
}

func TestBillingUsageStatusClassification(t *testing.T) {
	cases := []struct {
		info *NousPortalAccountInfo
		want string
	}{
		{buAccount(nil, nil, &NousPaidServiceAccessInfo{}), "free"},
		{buAccount(plBool(false), buPlus(), &NousPaidServiceAccessInfo{SubscriptionCreditsRemaining: buF(0), TotalUsableCredits: buF(0)}), "depleted"},
		{buAccount(plBool(true), buPlus(), &NousPaidServiceAccessInfo{SubscriptionCreditsRemaining: buF(14), TotalUsableCredits: buF(14)}), "healthy"},
		{buAccount(plBool(true), buPlus(), &NousPaidServiceAccessInfo{SubscriptionCreditsRemaining: buF(3.4), TotalUsableCredits: buF(3.4)}), "low"},
		{buAccount(plBool(true), buPlus(), &NousPaidServiceAccessInfo{SubscriptionCreditsRemaining: buF(5), TotalUsableCredits: buF(5)}), "healthy"},
		{buAccount(plBool(true), nil, &NousPaidServiceAccessInfo{PurchasedCreditsRemaining: buF(30), TotalUsableCredits: buF(30)}), "healthy"},
	}
	for i, c := range cases {
		m := UsageModelFromAccount(c.info)
		if !m.Available || m.Status != c.want {
			t.Fatalf("case %d status %q available %v, want %q", i, m.Status, m.Available, c.want)
		}
	}
	if m := UsageModelFromAccount(&NousPortalAccountInfo{}); m.Available {
		t.Fatal("logged out is unavailable")
	}
	if m := UsageModelFromAccount(nil); m.Available {
		t.Fatal("no account is unavailable")
	}
}

func TestBillingUsageBars(t *testing.T) {
	m := UsageModelFromAccount(buAccount(plBool(true), buPlus(), &NousPaidServiceAccessInfo{SubscriptionCreditsRemaining: buF(14), TotalUsableCredits: buF(14)}))
	bar := m.PlanBar
	if bar == nil || bar.Kind != "plan" || bar.RemainingUSD != 14 || bar.TotalUSD != 20 || math.Abs(bar.SpentUSD-6) > 1e-9 {
		t.Fatalf("plan bar %+v", bar)
	}
	if pct, ok := bar.PctUsed(); !ok || pct != 30 {
		t.Fatalf("pct used %d", pct)
	}
	m = UsageModelFromAccount(buAccount(plBool(true), buPlus(), &NousPaidServiceAccessInfo{SubscriptionCreditsRemaining: buF(14), PurchasedCreditsRemaining: buF(12), TotalUsableCredits: buF(26)}))
	tb := m.TopupBar
	if tb == nil || tb.Kind != "topup" || tb.RemainingUSD != 12 || tb.FillFraction() != 1 {
		t.Fatalf("topup bar %+v", tb)
	}
	if _, ok := tb.PctUsed(); ok || *m.TotalSpendableUSD != 26 || !m.HasTopup() {
		t.Fatal("topup has no denominator")
	}
	nan := math.NaN()
	m = UsageModelFromAccount(buAccount(plBool(true), buPlus(), &NousPaidServiceAccessInfo{SubscriptionCreditsRemaining: &nan}))
	if m.PlanBar != nil || m.SubscriptionRemainingUSD != nil {
		t.Fatal("non finite fields degrade to fewer bars")
	}
}

func TestBillingUsageFormatting(t *testing.T) {
	for in, want := range map[string]string{"2026-07-24": "Jul 24, 2026", "2026-07-24T10:00:00Z": "Jul 24, 2026", "2026-07-04T23:59:59.123456+05:30": "Jul 4, 2026", "2026-7-4": "Jul 4, 2026", "not a date": "not a date"} {
		if got, ok := FormatRenews(in); !ok || got != want {
			t.Fatalf("format renews %q = %q, want %q", in, got, want)
		}
	}
	if _, ok := FormatRenews("  "); ok {
		t.Fatal("blank renews is none")
	}
	if FormatUSD(buF(1234.5)) != "$1,234.50" || FormatUSD(nil) != "$0.00" || FormatUSD(buF(-1234567.891)) != "$-1,234,567.89" {
		t.Fatal("usd formatting")
	}
}

func TestBillingUsageDevFixture(t *testing.T) {
	t.Setenv(DevCreditsFixtureEnv, "top-up")
	m, ok := devFixtureUsageModel()
	if !ok || m.Status != "healthy" || m.TopupBar == nil || *m.TotalSpendableUSD != 26 || m.PlanName != "Plus" {
		t.Fatalf("topup fixture %+v", m)
	}
	t.Setenv(DevCreditsFixtureEnv, "unknown")
	if _, ok = devFixtureUsageModel(); ok {
		t.Fatal("unknown fixture")
	}
}

func TestBillingLinksBlock(t *testing.T) {
	if !IsNousInferenceRoute("nous", "") || !IsNousInferenceRoute("", "https://inference-api.nousresearch.com/v1") || IsNousInferenceRoute("openai", "https://api.openai.com/v1") {
		t.Fatal("nous inference route")
	}
	b := BuildBillingBlock("openai", "", "gpt-5", "")
	if b.IsNous || b.ProviderLabel != "OpenAI" || !strings.Contains(b.BillingURL, "openai.com") {
		t.Fatalf("openai block %+v", b)
	}
	b = BuildBillingBlock("custom", "https://api.together.xyz/v1", "m", "")
	if b.ProviderLabel != "Together AI" {
		t.Fatalf("host fallback %+v", b)
	}
	b = BuildBillingBlock("my_cool-provider", "", "m", "")
	if b.ProviderLabel != "My Cool Provider" || b.BillingURL != "" || b.ToDict()["billing_url"] != nil {
		t.Fatalf("readable label %+v", b)
	}
	if b = BuildBillingBlock("", "", "m", ""); b.ProviderLabel != "your provider" {
		t.Fatalf("empty slug %+v", b)
	}
}

func TestBillingUnverifiedCopy(t *testing.T) {
	if got := BillingTerminalLabel("boom", true); strings.HasPrefix(got, "Billing or credits exhausted") || !strings.Contains(got, "unverified") || !strings.Contains(got, "content-filter") {
		t.Fatalf("unverified label %q", got)
	}
	if !strings.HasPrefix(BillingTerminalLabel("boom", false), "Billing or credits exhausted") {
		t.Fatal("confirmed label stays assertive")
	}
	if BillingBlockDict("openai", "", "m", "", true)["unverified"] != true {
		t.Fatal("unverified block flag")
	}
	if _, ok := BillingBlockDict("openai", "", "m", "", false)["unverified"]; ok {
		t.Fatal("confirmed block has no flag")
	}
	var lines []string
	emit := func(ls ...string) { lines = append(lines, ls...) }
	if !PrintBillingOrEntitlementGuidance(emit, "model access", "anthropic", "", "claude-x", true) || !strings.Contains(strings.Join(lines, "\n"), "content filter") || !strings.HasPrefix(lines[0], "   💡 anthropic reported") {
		t.Fatalf("anthropic unverified guidance %q", lines)
	}
	lines = nil
	if !PrintBillingOrEntitlementGuidance(emit, "model access", "openrouter", "", "m", false) || len(lines) != 4 || lines[2] != "   💡 OpenRouter billing: https://openrouter.ai/settings/credits" {
		t.Fatalf("provider guidance %q", lines)
	}
}
