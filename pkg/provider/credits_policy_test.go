package provider

import (
	"slices"
	"strings"
	"testing"
)

type cpOpts struct {
	kind      string
	purchased int64
	purUSD    string
	limitUSD  *string
}

func cpStr(s string) *string { return &s }

func cpState(uf *float64, o cpOpts) *CreditsState {
	s := NewCreditsState()
	s.PurchasedMicros, s.PurchasedUSD = o.purchased, "0.00"
	if o.purUSD != "" {
		s.PurchasedUSD = o.purUSD
	}
	if uf == nil {
		return s
	}
	lim := int64(20_000_000)
	s.SubscriptionLimitMicros, s.SubscriptionMicros = &lim, int64(float64(lim)*(1.0-*uf))
	s.SubscriptionLimitUSD = cpStr("20.00")
	if o.limitUSD != nil {
		s.SubscriptionLimitUSD = o.limitUSD
	}
	s.DenominatorKind = "subscription_cap"
	if o.kind != "" {
		s.DenominatorKind = o.kind
	}
	return s
}

func cpFrac(uf float64) *CreditsState { return cpState(&uf, cpOpts{}) }

func cpPaid(paid bool) *CreditsState {
	s := NewCreditsState()
	s.PaidAccess = paid
	return s
}

func cpKeys(ns []AgentNotice) []string {
	var out []string
	for _, n := range ns {
		out = append(out, n.Key)
	}
	return out
}

func cpFind(ns []AgentNotice, key string) *AgentNotice {
	for i := range ns {
		if ns[i].Key == key {
			return &ns[i]
		}
	}
	return nil
}

func cpEval(s *CreditsState, latch map[string]any) ([]AgentNotice, []string) {
	return EvaluateCreditsNotices(s, latch, false, false)
}

func TestCreditsWarn90Crossing(t *testing.T) {
	latch := NewCreditsLatch()
	show, clear := cpEval(cpFrac(0.10), latch)
	if cpFind(show, CreditsUsageKey) != nil || slices.Contains(clear, CreditsUsageKey) || latch["seen_below_90"] != true {
		t.Fatal("below the lowest band only sets the latch")
	}
	show, clear = cpEval(cpFrac(0.95), latch)
	if cpFind(show, CreditsUsageKey) == nil || slices.Contains(clear, CreditsUsageKey) {
		t.Fatal("crossing to 90 fires")
	}
	show, clear = cpEval(cpFrac(0.95), latch)
	if cpFind(show, CreditsUsageKey) != nil || slices.Contains(clear, CreditsUsageKey) {
		t.Fatal("no refire over 90")
	}
	_, clear = cpEval(cpFrac(0.10), latch)
	if !slices.Contains(clear, CreditsUsageKey) || latchActive(latch)[CreditsUsageKey] {
		t.Fatal("recovery clears")
	}
	if show, _ = cpEval(cpFrac(0.95), latch); cpFind(show, CreditsUsageKey) == nil {
		t.Fatal("recross fires again")
	}
	latch = NewCreditsLatch()
	if show, clear = cpEval(cpFrac(0.95), latch); cpFind(show, CreditsUsageKey) != nil || slices.Contains(clear, CreditsUsageKey) {
		t.Fatal("open already over does not fire")
	}
	latch = NewCreditsLatch()
	cpEval(cpFrac(0.10), latch)
	lim := int64(20_000_000)
	exact := NewCreditsState()
	exact.SubscriptionLimitMicros, exact.SubscriptionLimitUSD, exact.SubscriptionMicros, exact.DenominatorKind = &lim, cpStr("20.00"), 2_000_000, "subscription_cap"
	if uf, _ := exact.UsedFraction(); uf != 0.9 {
		t.Fatalf("exact fraction %v", uf)
	}
	if show, clear = cpEval(exact, latch); cpFind(show, CreditsUsageKey) == nil || slices.Contains(clear, CreditsUsageKey) {
		t.Fatal("exact 0.9 fires")
	}
}

func TestCreditsGrantSpent(t *testing.T) {
	one := 1.0
	grant := func() *CreditsState { return cpState(&one, cpOpts{purchased: 12_340_000, purUSD: "12.34"}) }
	latch := NewCreditsLatch()
	cpEval(cpFrac(0.75), latch)
	show, _ := cpEval(grant(), latch)
	if n := cpFind(show, CreditsGrantSpentKey); n == nil || !strings.Contains(n.Text, "$12.34") {
		t.Fatal("grant spent fires on a live crossing with the verbatim top-up")
	}
	show, clear := cpEval(grant(), latch)
	if cpFind(show, CreditsGrantSpentKey) != nil || slices.Contains(clear, CreditsGrantSpentKey) {
		t.Fatal("grant spent does not refire")
	}
	latch = NewCreditsLatch()
	lim := int64(20_000_000)
	residue := NewCreditsState()
	residue.SubscriptionLimitMicros, residue.SubscriptionLimitUSD, residue.SubscriptionMicros = &lim, cpStr("20.00"), 4_000
	residue.DenominatorKind, residue.PurchasedMicros, residue.PurchasedUSD = "subscription_cap", 12_340_000, "12.34"
	if uf, ok := residue.UsedFraction(); !ok || uf >= 1.0 {
		t.Fatal("residue is below one")
	}
	cpEval(residue, latch)
	if show, _ = cpEval(grant(), latch); cpFind(show, CreditsGrantSpentKey) != nil {
		t.Fatal("sub cent residue does not open the gate")
	}
}

func TestCreditsDepletedNotices(t *testing.T) {
	latch := NewCreditsLatch()
	show, _ := cpEval(cpPaid(false), latch)
	if n := cpFind(show, CreditsDepletedKey); n == nil || n.Level != "error" || n.Kind != CreditsNoticeKind {
		t.Fatal("depleted is a sticky error")
	}
	show, clear := cpEval(cpPaid(true), latch)
	r := cpFind(show, CreditsRestoredKey)
	if !slices.Contains(clear, CreditsDepletedKey) || r == nil || r.Level != "success" || r.Kind != "ttl" || r.TTLMs != CreditsRestoredTTLMs {
		t.Fatal("recovery clears and shows restored")
	}
	if show, _ = cpEval(cpPaid(false), latch); cpFind(show, CreditsDepletedKey) == nil {
		t.Fatal("depleted refires after recovery")
	}
	latch = NewCreditsLatch()
	show, clear = EvaluateCreditsNotices(cpPaid(false), latch, true, false)
	if cpFind(show, CreditsDepletedKey) != nil || latchActive(latch)[CreditsDepletedKey] || len(clear) != 0 {
		t.Fatal("free model suppresses depleted")
	}
	latch = NewCreditsLatch()
	cpEval(cpPaid(false), latch)
	show, clear = EvaluateCreditsNotices(cpPaid(true), latch, true, false)
	if !slices.Contains(clear, CreditsDepletedKey) || cpFind(show, CreditsRestoredKey) == nil {
		t.Fatal("genuine recovery on a free model still restores")
	}
}

func TestCreditsIsFreeTierModelPricingPeek(t *testing.T) {
	ResetPricingCache()
	t.Cleanup(ResetPricingCache)
	cacheCatalog("https://inference-api.nousresearch.com", PricingTable{
		"some/zero-priced":  {"prompt": "0", "completion": "0"},
		"some/paid":         {"prompt": "0.000001", "completion": "0.000002"},
		"some/subscription": {"prompt": "0.000001", "completion": "0.000002", "billing_mode": "subscription"},
	}, 0)
	base := "https://inference-api.nousresearch.com/v1"
	if !IsFreeTierModel("some/zero-priced", base) || IsFreeTierModel("some/paid", base) || !IsFreeTierModel("some/subscription", base) {
		t.Fatal("pricing peek")
	}
	if !IsFreeTierModel("some/zero-priced", "https://inference-api.nousresearch.com/") || !IsFreeTierModel("some/zero-priced", "https://inference-api.nousresearch.com/v1/") {
		t.Fatal("slash variants")
	}
	ResetPricingCache()
	if !IsFreeTierModel("nous/welcome", "https://welcome-api.nousresearch.com/v1") || !IsFreeTierModel("some/other", "https://welcome-api.nousresearch.com") {
		t.Fatal("welcome host is free without pricing")
	}
	if IsFreeTierModel("nous/welcome", base) || IsFreeTierModel("some/paid", base) || IsFreeTierModel("nous/welcome", "") {
		t.Fatal("paid host needs pricing evidence")
	}
	if !IsFreeTierModel("stealth/ox-alpha", "") || !IsFreeTierModel("stealth/ox-alpha", base) || IsFreeTierModel("some/paid-model", "") {
		t.Fatal("stealth prefix")
	}
}

func TestCreditsNoticeCopyAndDenominatorNone(t *testing.T) {
	latch := NewCreditsLatch()
	cpEval(cpFrac(0.10), latch)
	uf := 0.95
	show, _ := cpEval(cpState(&uf, cpOpts{limitUSD: cpStr("20.00")}), latch)
	if n := cpFind(show, CreditsUsageKey); n == nil || !strings.Contains(n.Text, "$20.00") {
		t.Fatal("usage copy carries the verbatim limit")
	}
	_, clear := cpEval(cpState(nil, cpOpts{}), latch)
	if !slices.Contains(clear, CreditsUsageKey) || latchActive(latch)[CreditsUsageKey] {
		t.Fatal("usage clears when the denominator vanishes")
	}
}

func TestCreditsSeverityOrder(t *testing.T) {
	lim := int64(20_000_000)
	s := NewCreditsState()
	s.SubscriptionLimitMicros, s.SubscriptionLimitUSD, s.DenominatorKind = &lim, cpStr("20.00"), "subscription_cap"
	s.PurchasedMicros, s.PurchasedUSD, s.PaidAccess = 5_000_000, "5.00", false
	latch := map[string]any{"active": map[string]bool{}, "seen_below_90": true, "usage_band": nil, "seen_grant_unspent": true}
	keys := cpKeys(func() []AgentNotice { n, _ := cpEval(s, latch); return n }())
	if slices.Contains(keys, CreditsUsageKey) || slices.Index(keys, CreditsGrantSpentKey) < 0 || slices.Index(keys, CreditsGrantSpentKey) > slices.Index(keys, CreditsDepletedKey) {
		t.Fatalf("grant spent before depleted, got %v", keys)
	}
	s.PurchasedMicros, s.PurchasedUSD = 0, "0.00"
	latch = map[string]any{"active": map[string]bool{}, "seen_below_90": true, "usage_band": nil}
	keys = cpKeys(func() []AgentNotice { n, _ := cpEval(s, latch); return n }())
	if slices.Index(keys, CreditsUsageKey) < 0 || slices.Index(keys, CreditsUsageKey) > slices.Index(keys, CreditsDepletedKey) {
		t.Fatalf("usage before depleted, got %v", keys)
	}
}

func TestCreditsTopUpSuppression(t *testing.T) {
	uf := 0.95
	topped := func() *CreditsState { return cpState(&uf, cpOpts{purchased: 10_000_000, purUSD: "10.00"}) }
	latch := NewCreditsLatch()
	cpEval(cpFrac(0.10), latch)
	cpEval(cpFrac(0.95), latch)
	if latch["usage_band"] != 90 {
		t.Fatal("band 90")
	}
	show, clear := cpEval(topped(), latch)
	if !slices.Contains(clear, CreditsUsageKey) || latch["usage_band"] != nil || cpFind(show, CreditsUsageKey) != nil {
		t.Fatal("top-up clears the band")
	}
	latch = NewCreditsLatch()
	cpEval(cpFrac(0.10), latch)
	cpEval(topped(), latch)
	if latch["usage_band"] != nil {
		t.Fatal("no band with top-up")
	}
	show, _ = cpEval(cpFrac(0.95), latch)
	if n := cpFind(show, CreditsUsageKey); n == nil || !strings.Contains(n.Text, "$19.00") || latch["usage_band"] != 90 {
		t.Fatal("band resumes after the top-up is spent")
	}
}

func TestCreditsNoFireAndClearSameKey(t *testing.T) {
	check := func(states []*CreditsState) {
		latch := NewCreditsLatch()
		for _, s := range states {
			show, clear := cpEval(s, latch)
			for _, k := range cpKeys(show) {
				if slices.Contains(clear, k) {
					t.Fatalf("key %s both fired and cleared", k)
				}
			}
		}
	}
	check([]*CreditsState{cpFrac(0.10), cpFrac(0.95), cpFrac(0.10), cpFrac(0.95), cpState(nil, cpOpts{})})
	check([]*CreditsState{cpPaid(false), cpPaid(true), cpPaid(false)})
}

func TestCreditsUsageBands(t *testing.T) {
	latch := NewCreditsLatch()
	cpEval(cpFrac(0.10), latch)
	show, _ := cpEval(cpFrac(0.55), latch)
	if n := cpFind(show, CreditsUsageKey); n == nil || !strings.Contains(n.Text, "$11.00") || n.Level != "info" || latch["usage_band"] != 50 {
		t.Fatal("50 band is info")
	}
	latch = NewCreditsLatch()
	cpEval(cpFrac(0.10), latch)
	cpEval(cpFrac(0.95), latch)
	show, clear := cpEval(cpFrac(0.80), latch)
	if n := cpFind(show, CreditsUsageKey); !slices.Contains(clear, CreditsUsageKey) || n == nil || !strings.Contains(n.Text, "$16.00") {
		t.Fatal("step down to 75")
	}
	if show, _ = cpEval(cpFrac(0.55), latch); !strings.Contains(cpFind(show, CreditsUsageKey).Text, "$11.00") {
		t.Fatal("step down to 50")
	}
	if _, clear = cpEval(cpFrac(0.10), latch); !slices.Contains(clear, CreditsUsageKey) || latch["usage_band"] != nil {
		t.Fatal("clears below every band")
	}
}
