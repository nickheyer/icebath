package provider

import (
	"math"
	"net/http"
	"strconv"
	"testing"
	"time"
)

func ctMicros(dollars float64) string {
	return strconv.FormatInt(int64(math.RoundToEven(dollars*1_000_000)), 10)
}

func ctHeaders(overrides map[string]string, drop ...string) http.Header {
	base := map[string]string{
		"x-nous-credits-version": "1", "x-nous-credits-remaining-micros": ctMicros(0), "x-nous-credits-remaining-usd": "0.00",
		"x-nous-credits-subscription-micros": ctMicros(0), "x-nous-credits-subscription-usd": "0.00",
		"x-nous-credits-rollover-micros": ctMicros(0), "x-nous-credits-purchased-micros": ctMicros(0),
		"x-nous-credits-purchased-usd": "0.00", "x-nous-tool-pool-micros": ctMicros(0), "x-nous-tool-pool-gated-off": "false",
		"x-nous-credits-denominator-kind": "none", "x-nous-credits-paid-access": "true", "x-nous-credits-as-of-ms": "1717000000000",
	}
	for k, v := range overrides {
		base[k] = v
	}
	for _, k := range drop {
		delete(base, k)
	}
	h := http.Header{}
	for k, v := range base {
		h.Set(k, v)
	}
	return h
}

var (
	ctHealthy = map[string]string{
		"x-nous-credits-remaining-micros": ctMicros(30.34), "x-nous-credits-remaining-usd": "30.34",
		"x-nous-credits-subscription-micros": ctMicros(18), "x-nous-credits-subscription-usd": "18.00",
		"x-nous-credits-subscription-limit-micros": ctMicros(20), "x-nous-credits-subscription-limit-usd": "20.00",
		"x-nous-credits-purchased-micros": ctMicros(12.34), "x-nous-credits-purchased-usd": "12.34",
		"x-nous-tool-pool-micros": ctMicros(2), "x-nous-tool-pool-gated-off": "true", "x-nous-credits-denominator-kind": "subscription_cap",
	}
	ctSub90 = map[string]string{
		"x-nous-credits-remaining-micros": ctMicros(2), "x-nous-credits-remaining-usd": "2.00",
		"x-nous-credits-subscription-micros": ctMicros(2), "x-nous-credits-subscription-usd": "2.00",
		"x-nous-credits-subscription-limit-micros": ctMicros(20), "x-nous-credits-subscription-limit-usd": "20.00",
		"x-nous-credits-denominator-kind": "subscription_cap",
	}
	ctGrantExhausted = map[string]string{
		"x-nous-credits-remaining-micros": ctMicros(12.34), "x-nous-credits-remaining-usd": "12.34",
		"x-nous-credits-subscription-limit-micros": ctMicros(20), "x-nous-credits-subscription-limit-usd": "20.00",
		"x-nous-credits-purchased-micros": ctMicros(12.34), "x-nous-credits-purchased-usd": "12.34",
		"x-nous-credits-denominator-kind": "subscription_cap",
	}
)

func ctParse(t *testing.T, h http.Header) *CreditsState {
	t.Helper()
	s := ParseCreditsHeaders(h, "nous")
	if s == nil {
		t.Fatalf("headers did not parse: %v", h)
	}
	return s
}

func ctApprox(t *testing.T, s *CreditsState, want float64) {
	t.Helper()
	got, ok := s.UsedFraction()
	if !ok || math.Abs(got-want) > 1e-6 {
		t.Fatalf("used fraction = %v %v, want %v", got, ok, want)
	}
}

func TestCreditsHealthyState(t *testing.T) {
	before := float64(time.Now().UnixNano()) / 1e9
	s := ctParse(t, ctHeaders(ctHealthy))
	after := float64(time.Now().UnixNano()) / 1e9
	if s.CapturedAt < before || s.CapturedAt > after {
		t.Fatalf("captured_at %v outside [%v, %v]", s.CapturedAt, before, after)
	}
	if s.SubscriptionMicros != 18_000_000 || s.SubscriptionUSD != "18.00" || *s.SubscriptionLimitMicros != 20_000_000 || *s.SubscriptionLimitUSD != "20.00" {
		t.Fatalf("subscription fields %+v", s)
	}
	if s.RolloverMicros != 0 || s.PurchasedMicros != 12_340_000 || s.PurchasedUSD != "12.34" {
		t.Fatalf("rollover and purchased %+v", s)
	}
	if s.DenominatorKind != "subscription_cap" || !s.PaidAccess || s.DisabledReason != "" {
		t.Fatalf("denominator and access %+v", s)
	}
}

func TestCreditsUsedFractionBands(t *testing.T) {
	ctApprox(t, ctParse(t, ctHeaders(ctSub90)), 0.90)
	ctApprox(t, ctParse(t, ctHeaders(ctGrantExhausted)), 1.0)
	s := ctParse(t, ctHeaders(map[string]string{"x-nous-credits-remaining-micros": ctMicros(30), "x-nous-credits-remaining-usd": "30.00", "x-nous-credits-purchased-micros": ctMicros(30), "x-nous-credits-purchased-usd": "30.00"}))
	if s.SubscriptionLimitMicros != nil || s.SubscriptionLimitUSD != nil {
		t.Fatal("purchased only has no limit pair")
	}
	s = ctParse(t, ctHeaders(map[string]string{"x-nous-credits-remaining-micros": ctMicros(0.05), "x-nous-credits-remaining-usd": "0.05", "x-nous-tool-pool-micros": ctMicros(0.05)}))
	if s.ToolPoolMicros != 50_000 {
		t.Fatalf("tool pool micros %d", s.ToolPoolMicros)
	}
}

func TestCreditsMissingAndIrrelevantHeaders(t *testing.T) {
	if ParseCreditsHeaders(http.Header{}, "") != nil {
		t.Fatal("empty headers")
	}
	h := http.Header{}
	h.Set("content-type", "application/json")
	h.Set("x-request-id", "abc123")
	h.Set("server", "nginx")
	if ParseCreditsHeaders(h, "") != nil {
		t.Fatal("irrelevant headers")
	}
}

func TestCreditsVersionAndBoolStrings(t *testing.T) {
	if s := ctParse(t, ctHeaders(nil)); s.Version != 1 {
		t.Fatalf("version %d", s.Version)
	}
	if s := ctParse(t, ctHeaders(map[string]string{"x-nous-credits-paid-access": "false"})); s.PaidAccess || !s.Depleted() {
		t.Fatal("string false means depleted")
	}
	if s := ctParse(t, ctHeaders(map[string]string{"x-nous-credits-paid-access": "true"})); !s.PaidAccess || s.Depleted() {
		t.Fatal("string true means not depleted")
	}
	if ParseCreditsHeaders(ctHeaders(map[string]string{"x-nous-credits-version": "2"}), "") != nil || ParseCreditsHeaders(ctHeaders(map[string]string{"x-nous-credits-version": "x"}), "") != nil {
		t.Fatal("unsupported version is a miss")
	}
}

func TestCreditsToolPoolOptional(t *testing.T) {
	s := ctParse(t, ctHeaders(nil, "x-nous-tool-pool-micros", "x-nous-tool-pool-gated-off"))
	if s.ToolPoolGatedOff {
		t.Fatal("absent gated off defaults false")
	}
	if s = ctParse(t, ctHeaders(nil, "x-nous-tool-pool-micros")); s.ToolPoolMicros != 0 {
		t.Fatal("absent tool pool micros defaults zero")
	}
}

func TestCreditsHalfPairLimit(t *testing.T) {
	s := ctParse(t, ctHeaders(map[string]string{"x-nous-credits-subscription-limit-usd": "20.00", "x-nous-credits-denominator-kind": "subscription_cap"}))
	if s.SubscriptionLimitMicros != nil || s.SubscriptionLimitUSD != nil {
		t.Fatal("half pair parses as both absent")
	}
	s = ctParse(t, ctHeaders(map[string]string{
		"x-nous-credits-subscription-micros": ctMicros(10), "x-nous-credits-subscription-usd": "10.00",
		"x-nous-credits-subscription-limit-micros": ctMicros(20), "x-nous-credits-subscription-limit-usd": "20.00",
		"x-nous-credits-denominator-kind": "subscription_cap",
	}))
	if *s.SubscriptionLimitMicros != 20_000_000 || *s.SubscriptionLimitUSD != "20.00" {
		t.Fatal("full pair")
	}
	ctApprox(t, s, 0.50)
}

func TestCreditsFieldValidation(t *testing.T) {
	misses := []map[string]string{
		{"x-nous-credits-remaining-micros": "-1000"},
		{"x-nous-credits-subscription-limit-micros": "-1000", "x-nous-credits-subscription-limit-usd": "-0.00", "x-nous-credits-denominator-kind": "subscription_cap"},
		{"x-nous-credits-remaining-usd": "18.0"},
		{"x-nous-credits-remaining-micros": "abc"},
		{"x-nous-credits-remaining-micros": "1.5"},
		{"x-nous-credits-as-of-ms": "not-a-timestamp"},
		{"x-nous-credits-denominator-kind": "invalid_kind"},
		{"x-nous-tool-pool-gated-off": "yes"},
	}
	for _, m := range misses {
		if ParseCreditsHeaders(ctHeaders(m), "") != nil {
			t.Fatalf("expected a miss for %v", m)
		}
	}
	s := ctParse(t, ctHeaders(map[string]string{"x-nous-credits-subscription-micros": "-5000000", "x-nous-credits-subscription-usd": "-5.00"}))
	if s.SubscriptionMicros != -5_000_000 || s.SubscriptionUSD != "-5.00" {
		t.Fatal("negative subscription accepted")
	}
	if s = ctParse(t, ctHeaders(map[string]string{"x-nous-credits-as-of-ms": "1717000000000"})); s.AsOfMs != 1717000000000 {
		t.Fatal("as of ms")
	}
	if s = ctParse(t, ctHeaders(map[string]string{"x-nous-credits-remaining-micros": " +1_000 "})); s.RemainingMicros != 1000 {
		t.Fatalf("python int text reads signs, whitespace and underscores, got %d", s.RemainingMicros)
	}
}

func TestCreditsZeroLimitAndUnknownHeaders(t *testing.T) {
	s := ctParse(t, ctHeaders(map[string]string{"x-nous-credits-subscription-limit-micros": "0", "x-nous-credits-subscription-limit-usd": "0.00", "x-nous-credits-denominator-kind": "subscription_cap"}))
	if _, ok := s.UsedFraction(); ok {
		t.Fatal("zero limit has no used fraction")
	}
	ctParse(t, ctHeaders(map[string]string{"x-nous-credits-future-field": "some-value", "x-request-id": "abc123"}))
}

func TestCreditsHeaderNormalization(t *testing.T) {
	h := http.Header{}
	for k, v := range map[string]string{
		"X-Nous-Credits-Version": "1", "X-Nous-Credits-Remaining-Micros": ctMicros(5), "X-Nous-Credits-Remaining-Usd": "5.00",
		"X-Nous-Credits-Subscription-Micros": ctMicros(5), "X-Nous-Credits-Subscription-Usd": "5.00", "X-Nous-Credits-Rollover-Micros": "0",
		"X-Nous-Credits-Purchased-Micros": "0", "X-Nous-Credits-Purchased-Usd": "0.00", "X-Nous-Tool-Pool-Micros": "0",
		"X-Nous-Tool-Pool-Gated-Off": "false", "X-Nous-Credits-Denominator-Kind": "none", "X-Nous-Credits-Paid-Access": "true",
		"X-Nous-Credits-As-Of-Ms": "1717000000000",
	} {
		h[k] = []string{v}
	}
	if s := ctParse(t, h); s.RemainingMicros != 5_000_000 {
		t.Fatalf("mixed case remaining %d", s.RemainingMicros)
	}
}

func TestCreditsStateDefaultsAndProperties(t *testing.T) {
	if !math.IsInf(NewCreditsState().AgeSeconds(), 1) {
		t.Fatal("age is infinite without data")
	}
	now := float64(time.Now().UnixNano()) / 1e9
	s := NewCreditsState()
	s.CapturedAt = now
	if s.Depleted() {
		t.Fatal("paid access with zero balance is not depleted")
	}
	s.DenominatorKind = "subscription_cap"
	if _, ok := s.UsedFraction(); ok {
		t.Fatal("no limit means no fraction")
	}
	lim := int64(10_000_000)
	s.SubscriptionLimitMicros, s.SubscriptionMicros = &lim, -5_000_000
	ctApprox(t, s, 1.0)
}
