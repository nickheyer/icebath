package provider

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"
)

const csNousBase = "https://inference-api.nousresearch.com/v1"

func csSubscriptionCatalog() PricingTable {
	return PricingTable{"openai/gpt-5.6-luna": {"prompt": "0.0000002000", "completion": "0.0000012000", "billing_mode": "subscription"}}
}

func csColdStartNotices(s *CreditsState) []string {
	latch := NewCreditsLatch()
	if _, ok := s.UsedFraction(); ok {
		latch["seen_below_90"] = true
	}
	show, _ := EvaluateCreditsNotices(s, latch, false, false)
	keys := []string{}
	for _, n := range show {
		keys = append(keys, n.Key)
	}
	return keys
}

func csState(mods ...func(*CreditsState)) *CreditsState {
	s := NewCreditsState()
	s.FromHeader, s.CapturedAt = false, float64(time.Now().UnixNano())/1e9
	for _, m := range mods {
		m(s)
	}
	return s
}

func csCap(remaining, subscription, limit int64) func(*CreditsState) {
	return func(s *CreditsState) {
		usd := "20.00"
		s.RemainingMicros, s.SubscriptionMicros, s.SubscriptionLimitMicros, s.SubscriptionLimitUSD = remaining, subscription, &limit, &usd
		s.DenominatorKind, s.PaidAccess = "subscription_cap", true
	}
}

// Fake agent capture recording shown and cleared keys since the last reset
type csAgent struct {
	c       *CreditsCapture
	mu      sync.Mutex
	shown   []string
	cleared []string
}

func csNewAgent(t *testing.T, provider, model, baseURL, sessionID string) *csAgent {
	a := &csAgent{c: NewCreditsCapture(nil, provider, model, baseURL, sessionID)}
	a.c.Notice = func(n AgentNotice) { a.mu.Lock(); a.shown = append(a.shown, n.Key); a.mu.Unlock() }
	a.c.NoticeClear = func(k string) { a.mu.Lock(); a.cleared = append(a.cleared, k); a.mu.Unlock() }
	t.Cleanup(func() { seenUsageBands.drop(sessionID); ForgetWindowNotices(sessionID) })
	return a
}

func (a *csAgent) reset() { a.mu.Lock(); a.shown, a.cleared = nil, nil; a.mu.Unlock() }

func (a *csAgent) got() string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return strings.Join(a.shown, ",") + "|" + strings.Join(a.cleared, ",")
}

func csSeed(t *testing.T, a *csAgent, fixture string) bool {
	t.Setenv(DevCreditsEnv, "1")
	t.Setenv(DevCreditsFixtureEnv, fixture)
	return SeedCreditsAtSessionStart(a.c)
}

func TestCreditsColdStartHealthyNoNotice(t *testing.T) {
	s := csState(csCap(30_340_000, 18_000_000, 20_000_000))
	if f, ok := s.UsedFraction(); !ok || f < 0.1-1e-9 || f > 0.1+1e-9 || len(csColdStartNotices(s)) != 0 {
		t.Fatalf("%v %v", f, csColdStartNotices(s))
	}
}

func TestCreditsColdStartDevFixturesDriveColdStart(t *testing.T) {
	t.Setenv(DevCreditsEnv, "1")
	for name, want := range map[string]string{"healthy": "", "sub_90pct": CreditsUsageKey, "depleted": CreditsDepletedKey} {
		t.Setenv(DevCreditsFixtureEnv, name)
		fx := DevFixtureCreditsState()
		if fx == nil || strings.Join(csColdStartNotices(fx), ",") != want {
			t.Fatalf("%s: %v", name, fx)
		}
	}
}

func TestCreditsColdStartLiveCrossingAfterSeedStillFiresGrantSpent(t *testing.T) {
	a := csNewAgent(t, "nous", "", "", "cs-"+t.Name())
	if !csSeed(t, a, "healthy") {
		t.Fatal("seeded")
	}
	a.reset()
	a.c.credits = csState(csCap(12_340_000, 0, 20_000_000), func(s *CreditsState) { s.PurchasedMicros, s.PurchasedUSD = 12_340_000, "12.34" })
	a.c.EmitCreditsNotices()
	if a.got() != CreditsGrantSpentKey+"|" {
		t.Fatalf("%s", a.got())
	}
}

func TestCreditsColdStartSeedIsIdempotent(t *testing.T) {
	a := csNewAgent(t, "nous", "", "", "cs-"+t.Name())
	csSeed(t, a, "sub_90pct")
	a.reset()
	if csSeed(t, a, "sub_90pct") || a.got() != "|" {
		t.Fatalf("%s", a.got())
	}
}

func TestCreditsColdStartSeedSkipsNonNous(t *testing.T) {
	a := csNewAgent(t, "openrouter", "", "", "cs-"+t.Name())
	if SeedCreditsAtSessionStart(a.c) || a.c.Credits() != nil {
		t.Fatal("non nous seeded")
	}
}

func csBandState(used float64) *CreditsState {
	const capMicros = 20_000_000
	left := int64(capMicros * (1 - used))
	return csState(csCap(left, left, capMicros))
}

func csFirstAgent(t *testing.T, sessionID string) {
	a1 := csNewAgent(t, "nous", "", "", sessionID)
	a1.c.credits, a1.c.latch = csBandState(0.5), NewCreditsLatch()
	a1.c.latch["seen_below_90"] = true
	a1.c.EmitCreditsNotices()
	if a1.got() != CreditsUsageKey+"|" {
		t.Fatalf("%s", a1.got())
	}
}

func TestCreditsColdStartRebuildWithSameBandDoesNotRefire(t *testing.T) {
	sid := "desktop-session-1-" + t.Name()
	csFirstAgent(t, sid)
	a2 := csNewAgent(t, "nous", "", "", sid)
	hydrateSeedState(a2.c, csBandState(0.5))
	if a2.got() != "|" {
		t.Fatalf("%s", a2.got())
	}
}

func TestCreditsColdStartRebuildStillFiresOnGenuineBandChange(t *testing.T) {
	sid := "desktop-session-2-" + t.Name()
	csFirstAgent(t, sid)
	a2 := csNewAgent(t, "nous", "", "", sid)
	hydrateSeedState(a2.c, csBandState(0.75))
	if a2.got() != CreditsUsageKey+"|"+CreditsUsageKey {
		t.Fatalf("%s", a2.got())
	}
}

func TestCreditsColdStartUnrelatedSessionIDNotSuppressed(t *testing.T) {
	csFirstAgent(t, "desktop-session-a-"+t.Name())
	a2 := csNewAgent(t, "nous", "", "", "desktop-session-b-"+t.Name())
	hydrateSeedState(a2.c, csBandState(0.5))
	if a2.got() != CreditsUsageKey+"|" {
		t.Fatalf("%s", a2.got())
	}
}

func TestCreditsColdStartAgentWithoutSessionIDFallsBackToPriorBehavior(t *testing.T) {
	a := csNewAgent(t, "nous", "", "", "")
	hydrateSeedState(a.c, csBandState(0.5))
	if a.got() != CreditsUsageKey+"|" {
		t.Fatalf("%s", a.got())
	}
}

func csColdPricing(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	ResetPricingCache()
	t.Cleanup(ResetPricingCache)
}

func csFillCatalog(key string, table PricingTable, ttl time.Duration) {
	cacheCatalog(key, table, ttl)
}

// Swaps the warm and the worker launch, every launched worker joined through the returned group
func csGateWorkers(t *testing.T, warm func()) (*sync.WaitGroup, *int) {
	var wg sync.WaitGroup
	spawned := 0
	prevWarm, prevAcct, prevGo := creditsWarmPricingFn, creditsSeedAccountFn, creditsGo
	creditsWarmPricingFn = func(context.Context) { warm() }
	creditsGo = func(fn func()) {
		spawned++
		wg.Add(1)
		go func() { defer wg.Done(); fn() }()
	}
	t.Cleanup(func() { creditsWarmPricingFn, creditsSeedAccountFn, creditsGo = prevWarm, prevAcct, prevGo })
	return &wg, &spawned
}

func csRunBgSeed(t *testing.T, a *csAgent, warm func()) bool {
	t.Setenv(DevCreditsEnv, "")
	release := make(chan struct{})
	wg, spawned := csGateWorkers(t, func() { <-release; warm() })
	zero, unpaid := 0.0, false
	creditsSeedAccountFn = func(context.Context, bool) *NousPortalAccountInfo {
		return &NousPortalAccountInfo{PaidServiceAccess: &unpaid,
			PaidServiceAccessInfo: &NousPaidServiceAccessInfo{TotalUsableCredits: &zero, SubscriptionCreditsRemaining: &zero, PurchasedCreditsRemaining: &zero}}
	}
	pricingWarmFlights.Store(a.c, true)
	t.Cleanup(func() { pricingWarmFlights.Delete(a.c) })
	result := SeedCreditsAtSessionStart(a.c)
	if *spawned != 1 || a.c.Credits() != nil {
		close(release)
		wg.Wait()
		t.Fatalf("seed must return before its worker runs, spawned %d", *spawned)
	}
	close(release)
	wg.Wait()
	return result
}

func TestCreditsColdStartBgSeedWarmsPricingSoASubscriptionModelEscapesTheBanner(t *testing.T) {
	csColdPricing(t)
	a := csNewAgent(t, "nous", "openai/gpt-5.6-luna", csNousBase, "cs-"+t.Name())
	if !csRunBgSeed(t, a, func() { csFillCatalog(defaultNousInferenceURL, csSubscriptionCatalog(), NousCatalogTTL) }) {
		t.Fatal("seed result")
	}
	if !a.c.Credits().Depleted() || a.got() != "|" {
		t.Fatalf("%s", a.got())
	}
}

func TestCreditsColdStartBgSeedWithoutTheWarmStillWarns(t *testing.T) {
	csColdPricing(t)
	a := csNewAgent(t, "nous", "openai/gpt-5.6-luna", csNousBase, "cs-"+t.Name())
	csRunBgSeed(t, a, func() {})
	if a.got() != CreditsDepletedKey+"|" {
		t.Fatalf("%s", a.got())
	}
}

func TestCreditsColdStartBgSeedRerunsThePolicyWhenAHeaderBeatIt(t *testing.T) {
	csColdPricing(t)
	a := csNewAgent(t, "nous", "openai/gpt-5.6-luna", csNousBase, "cs-"+t.Name())
	header := csState(func(s *CreditsState) { s.PaidAccess = false })
	var first string
	csRunBgSeed(t, a, func() {
		a.c.mu.Lock()
		a.c.credits = header
		a.c.mu.Unlock()
		a.c.EmitCreditsNotices()
		first = a.got()
		csFillCatalog(defaultNousInferenceURL, csSubscriptionCatalog(), NousCatalogTTL)
	})
	if a.c.Credits() != header || first != CreditsDepletedKey+"|" || a.got() != CreditsDepletedKey+"|"+CreditsDepletedKey {
		t.Fatalf("first %s final %s", first, a.got())
	}
}

func csMixinAgent(t *testing.T) *csAgent {
	a := csNewAgent(t, "nous", "openai/gpt-5.6-luna", csNousBase, "cs-"+t.Name())
	a.c.credits = csState(func(s *CreditsState) { s.PaidAccess = false })
	return a
}

func TestCreditsColdStartHeaderAfterTTLExpiryRewarmsInsteadOfFlashingTheBanner(t *testing.T) {
	csColdPricing(t)
	csFillCatalog(defaultNousInferenceURL+pricingAuthKeyPrefix+"abc", csSubscriptionCatalog(), NousCatalogTTL)
	a := csMixinAgent(t)
	a.c.EmitCreditsNotices()
	if a.got() != "|" {
		t.Fatalf("warm catalog: %s", a.got())
	}
	prevClock := pricingClock
	later := time.Now().Add(NousCatalogTTL + time.Second)
	pricingClock = func() time.Time { return later }
	t.Cleanup(func() { pricingClock = prevClock })
	wg, _ := csGateWorkers(t, func() { csFillCatalog(defaultNousInferenceURL, csSubscriptionCatalog(), NousCatalogTTL) })
	a.c.EmitCreditsNotices()
	wg.Wait()
	if a.got() != "|" {
		t.Fatalf("after ttl: %s", a.got())
	}
}

func TestCreditsColdStartHeaderOnAColdCatalogStillWarnsWhenTheWarmFails(t *testing.T) {
	csColdPricing(t)
	warms := 0
	wg, _ := csGateWorkers(t, func() {
		warms++
		csFillCatalog(defaultNousInferenceURL+pricingAuthKeyPrefix+"abc", PricingTable{}, NousCatalogTTL)
	})
	a := csMixinAgent(t)
	a.c.EmitCreditsNotices()
	wg.Wait()
	if a.got() != CreditsDepletedKey+"|" {
		t.Fatalf("%s", a.got())
	}
	a.c.EmitCreditsNotices()
	wg.Wait()
	if warms != 1 {
		t.Fatalf("warms %d", warms)
	}
}
