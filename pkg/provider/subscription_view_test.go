package provider

import (
	"strings"
	"testing"
	"time"
)

func svTier(tid string, order int64, dpm, mc string, current, enabled bool) SubscriptionTier {
	t := SubscriptionTier{TierID: tid, Name: strings.ToUpper(tid[:1]) + tid[1:], TierOrder: order, IsCurrent: current, IsEnabled: enabled}
	if dpm != "" {
		t.DollarsPerMonth = decPtr(MustDecimal(dpm))
	}
	if mc != "" {
		t.MonthlyCredits = decPtr(MustDecimal(mc))
	}
	return t
}

func TestSubscriptionViewManageURLOmitsOrgWhenAbsent(t *testing.T) {
	s := &SubscriptionState{LoggedIn: true, PortalURL: "https://p.example.com/"}
	url, ok := SubscriptionManageURL(s, "")
	if !ok || url != "https://p.example.com/manage-subscription" || strings.Contains(url, "org_id") {
		t.Fatalf("%q %v", url, ok)
	}
}

func TestSubscriptionViewManageURLQueryContract(t *testing.T) {
	s := &SubscriptionState{OrgID: "org 1", PortalURL: "HTTPS://p.example.com/billing?plan=x&a=1&org_id=old&a=2&blank&b=%E2%82%AC+c#frag"}
	url, ok := SubscriptionManageURL(s, "ultra")
	if want := "https://p.example.com/manage-subscription?a=2&blank=&b=%E2%82%AC+c&org_id=org+1&plan=ultra"; !ok || url != want {
		t.Fatalf("got %q want %q", url, want)
	}
	for _, bad := range []any{"ftp://p.example.com/", "https:///nohost", "http://[::1/", 5, nil} {
		if url, ok := SubscriptionManageURL(&SubscriptionState{PortalURL: bad}, ""); ok {
			t.Fatalf("%v resolved to %q", bad, url)
		}
	}
}

func TestSubscriptionViewFormatTierRowHidesAbsentOrZeroCredits(t *testing.T) {
	for _, mc := range []string{"0", ""} {
		row, err := FormatTierRow(svTier("plus", 1, "20", mc, false, true))
		if err != nil || row != "Plus · $20/mo" || strings.Contains(row, "credits/mo") {
			t.Fatalf("%q %v", row, err)
		}
	}
	if row, _ := FormatTierRow(svTier("ultra", 3, "1200", "7000.5", false, true)); row != "Ultra · $1,200/mo · $7,000.50 credits/mo" {
		t.Fatalf("grouped row %q", row)
	}
}

func TestSubscriptionViewIsUpgradeByTierOrder(t *testing.T) {
	s := &SubscriptionState{LoggedIn: true, Current: &CurrentSubscription{TierID: "plus", TierName: "Plus"},
		Tiers: []SubscriptionTier{svTier("plus", 1, "20", "22", false, true), svTier("ultra", 3, "200", "220", false, true)}}
	if up, err := IsUpgrade(s, "ultra"); err != nil || !up {
		t.Fatal("ultra is an upgrade")
	}
	if up, err := IsUpgrade(s, "plus"); err != nil || up {
		t.Fatal("plus is not an upgrade")
	}
	s.Current = nil
	s.Tiers[1].IsCurrent = true
	if up, _ := IsUpgrade(s, "plus"); up {
		t.Fatal("the current marker ranks ultra as the plan")
	}
	s.Tiers[0].TierID = map[string]any{}
	if _, err := IsUpgrade(s, "plus"); err == nil || err.Error() != "unhashable type: 'dict'" {
		t.Fatalf("unhashable tier id: %v", err)
	}
}

func TestSubscriptionViewSelectableTiers(t *testing.T) {
	s := &SubscriptionState{Tiers: []SubscriptionTier{svTier("ultra", 3, "80", "", false, true), svTier("free", 0, "0", "", false, true),
		svTier("plus", 1, "20", "", true, true), svTier("old", 2, "30", "", false, false), svTier("super", 2, "40", "", false, true)}}
	var ids []string
	for _, tier := range SelectableTiers(s) {
		ids = append(ids, tier.TierID.(string))
	}
	if strings.Join(ids, ",") != "super,ultra" {
		t.Fatalf("selectable %v", ids)
	}
}

func TestSubscriptionViewParserMapsCamelCasePayloadFields(t *testing.T) {
	payload := bvJSON(t, `{"org": {"name": "Acme", "id": "org_1", "role": "ADMIN"}, "context": "personal",
	 "current": {"tierId": "plus", "tierName": "Plus", "monthlyCredits": "1000", "creditsRemaining": "420",
	 "cycleEndsAt": "2026-07-01", "cancelAtPeriodEnd": true, "cancellationEffectiveAt": "2026-07-01"}}`)
	s, err := SubscriptionStateFromPayload(payload, "https://p/billing")
	if err != nil || !s.LoggedIn || s.OrgName != "Acme" || s.OrgID != "org_1" {
		t.Fatalf("%+v %v", s, err)
	}
	if !bvOK(t)(s.IsAdmin()) || !bvOK(t)(s.CanChangePlan()) {
		t.Fatal("admin can change plan")
	}
	if s.Current == nil || s.Current.TierName != "Plus" || !s.Current.CancelAtPeriodEnd {
		t.Fatalf("current %+v", s.Current)
	}
	bvDecEq(t, "monthly credits", s.Current.MonthlyCredits, "1000")
}

func TestSubscriptionViewParserFiveRoles(t *testing.T) {
	yes := true
	for _, c := range []struct {
		role               string
		raw                *bool
		isAdmin, canChange bool
	}{{"OWNER", nil, true, true}, {"ADMIN", nil, true, true}, {"FINANCE_ADMIN", &yes, false, true},
		{"SECURITY_ADMIN", nil, false, false}, {"MEMBER", nil, false, false}} {
		payload := map[string]any{"org": map[string]any{"role": c.role}}
		if c.raw != nil {
			payload["canChangePlan"] = *c.raw
		}
		s, err := SubscriptionStateFromPayload(payload, nil)
		if err != nil || bvOK(t)(s.IsAdmin()) != c.isAdmin || bvOK(t)(s.CanChangePlan()) != c.canChange ||
			(s.CanChangePlanRaw == nil) != (c.raw == nil) {
			t.Fatalf("%s: %+v %v", c.role, s, err)
		}
	}
}

func TestSubscriptionViewParserMapsTiersCatalog(t *testing.T) {
	payload := bvJSON(t, `{"tiers": [
	 {"tierId": "free", "name": "Free", "tierOrder": 0, "dollarsPerMonthDisplay": "0", "monthlyCredits": "0", "isCurrent": false, "isEnabled": true},
	 {"tierId": "plus", "name": "Plus", "tierOrder": 1, "dollarsPerMonthDisplay": "20", "monthlyCredits": "1000", "isCurrent": true, "isEnabled": true},
	 {"name": "no id"}, 7]}`)
	s, err := SubscriptionStateFromPayload(payload, nil)
	if err != nil || len(s.Tiers) != 2 {
		t.Fatalf("%+v %v", s, err)
	}
	free, plus := s.Tiers[0], s.Tiers[1]
	if free.TierID != "free" || free.TierOrder != 0 || !free.IsEnabled || !plus.IsCurrent {
		t.Fatalf("free %+v plus %+v", free, plus)
	}
	bvDecEq(t, "free price", free.DollarsPerMonth, "0")
	bvDecEq(t, "plus price", plus.DollarsPerMonth, "20")
	bvDecEq(t, "plus credits", plus.MonthlyCredits, "1000")
	s, err = SubscriptionStateFromPayload(bvJSON(t, `{"tiers": [{"id": "x", "tierOrder": "2", "isEnabled": null}], "context": "team"}`), nil)
	if err != nil || s.Tiers[0].TierOrder != 2 || !s.Tiers[0].IsEnabled || s.Tiers[0].Name != "" || s.Context != "team" {
		t.Fatalf("string order and null enabled: %+v %v", s, err)
	}
	if _, err := SubscriptionStateFromPayload(bvJSON(t, `{"tiers": [{"id": "x", "tierOrder": "two"}]}`), nil); err == nil ||
		err.Error() != "invalid literal for int() with base 10: 'two'" {
		t.Fatalf("bad tier order: %v", err)
	}
}

func TestSubscriptionViewPreviewParserChargeNow(t *testing.T) {
	p, err := SubscriptionChangePreviewFromPayload(bvJSON(t, `{"effect": "charge_now", "reason": null, "currentTierId": "plus",
	 "currentTierName": "Plus", "targetTierId": "ultra", "targetTierName": "Ultra", "monthlyCreditsDelta": "6000",
	 "amountDueNowCents": 1234, "effectiveAt": null}`))
	if err != nil || p.Effect != "charge_now" || p.AmountDueNowCents == nil || *p.AmountDueNowCents != 1234 || p.TargetTierName != "Ultra" {
		t.Fatalf("%+v %v", p, err)
	}
	bvDecEq(t, "delta", p.MonthlyCreditsDelta, "6000")
	p, _ = SubscriptionChangePreviewFromPayload(bvJSON(t, `{"effect": 3, "amountDueNowCents": "5", "reason": ""}`))
	if p.Effect != "blocked" || p.AmountDueNowCents != nil || p.Reason != nil {
		t.Fatalf("malformed quote %+v", p)
	}
}

func TestSubscriptionViewNoFixtureWhenEnvUnset(t *testing.T) {
	t.Setenv(DevSubscriptionFixtureEnv, "")
	if s := DevFixtureSubscriptionState(); s != nil {
		t.Fatalf("%+v", s)
	}
}

func TestSubscriptionViewDevFixtureExposesTierCatalog(t *testing.T) {
	t.Setenv(DevSubscriptionFixtureEnv, "mid")
	s := DevFixtureSubscriptionState()
	if s == nil || len(s.Tiers) < 2 {
		t.Fatalf("%+v", s)
	}
	var current []SubscriptionTier
	for _, tier := range s.Tiers {
		if tier.IsCurrent {
			current = append(current, tier)
		}
	}
	if len(current) != 1 || current[0].TierID != "plus" {
		t.Fatalf("current %+v", current)
	}
	for name, check := range map[string]func(*SubscriptionState) bool{
		"top-tier":  func(s *SubscriptionState) bool { return s.Current.TierID == "ultra" },
		"member":    func(s *SubscriptionState) bool { return s.Role == "MEMBER" && s.Current.TierID == "plus" },
		"downgrade": func(s *SubscriptionState) bool { return s.Current.PendingDowngradeTierName == "Plus" },
		"cancel": func(s *SubscriptionState) bool {
			return s.Current.CancelAtPeriodEnd && s.Current.CancellationEffectiveAt == "2026-07-01"
		},
		"team":       func(s *SubscriptionState) bool { return s.Context == "team" && s.Current == nil && len(s.Tiers) == 0 },
		"free":       func(s *SubscriptionState) bool { return s.Current == nil && len(s.Tiers) == 4 },
		"logged_out": func(s *SubscriptionState) bool { return !s.LoggedIn && s.Error == nil },
		"bogus": func(s *SubscriptionState) bool {
			return !s.LoggedIn && *s.Error == "unknown ICEBATH_DEV_SUBSCRIPTION_FIXTURE: bogus"
		},
	} {
		t.Setenv(DevSubscriptionFixtureEnv, name)
		if s := DevFixtureSubscriptionState(); !check(s) {
			t.Fatalf("%s fixture %+v", name, s)
		}
	}
}

func TestSubscriptionViewBuildUsesFixture(t *testing.T) {
	t.Setenv(DevSubscriptionFixtureEnv, "mid")
	old := nousSubscriptionStateFetch
	nousSubscriptionStateFetch = func(PoolEnv, time.Duration) (any, error) { t.Fatal("portal fetched"); return nil, nil }
	t.Cleanup(func() { nousSubscriptionStateFetch = old })
	s, err := BuildSubscriptionState(t.Context(), PortalStateTimeout)
	if err != nil || !s.LoggedIn || s.Current == nil || s.Current.TierID != "plus" {
		t.Fatalf("%+v %v", s, err)
	}
	url, ok := SubscriptionManageURL(s, "")
	if !ok || !strings.HasSuffix(url, "/manage-subscription?org_id=org_acme") {
		t.Fatalf("%q", url)
	}
}

func TestSubscriptionViewBuildFailOpen(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	t.Setenv(DevSubscriptionFixtureEnv, "")
	t.Setenv("ICEBATH_PORTAL_BASE_URL", "")
	t.Setenv("NOUS_PORTAL_BASE_URL", "")
	old := nousSubscriptionStateFetch
	t.Cleanup(func() { nousSubscriptionStateFetch = old })
	nousSubscriptionStateFetch = func(PoolEnv, time.Duration) (any, error) { return nil, &BillingError{Message: "nas 404", Status: 404} }
	if s, err := BuildSubscriptionState(t.Context(), PortalStateTimeout); err != nil || s.LoggedIn || *s.Error != "nas 404" || s.Context != "personal" {
		t.Fatalf("%+v %v", s, err)
	}
	nousSubscriptionStateFetch = func(PoolEnv, time.Duration) (any, error) {
		return map[string]any{"org": map[string]any{"id": "o"}}, nil
	}
	if s, _ := BuildSubscriptionState(t.Context(), PortalStateTimeout); s.PortalURL != "https://portal.nousresearch.com" || s.OrgID != "o" {
		t.Fatalf("portal fallback is the base: %+v", s)
	}
}
