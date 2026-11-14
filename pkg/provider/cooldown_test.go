package provider

import (
	"context"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/config"
)

func frozenCooldown(epoch int64, mono time.Time) *Cooldown {
	c := NewCooldown(nil)
	c.now = func() time.Time { return mono }
	_ = epoch
	return c
}

func TestFallbackSwitchBenchesPrimaryUntilValidFutureProviderReset(t *testing.T) {
	wall := time.Unix(1_700_000_000, 0)
	cases := []struct {
		resetAt any
		want    int64
	}{
		{1_700_000_090.2, 91}, {1_700_274_291, 274_291}, {nil, 60}, {"not-a-timestamp", 60}, {1_699_999_999, 60},
	}
	for _, c := range cases {
		cd := frozenCooldown(0, wall)
		primary := Route{Provider: "openrouter", Model: "primary/model", BaseURL: "https://openrouter.ai/api/v1"}
		chain := NewChain(ChainOptions{Primary: primary, Fallbacks: []Route{{Provider: "openai-codex", Model: "gpt-5.5", BaseURL: "https://chatgpt.com/backend-api/codex"}}, Cooldown: cd, Now: func() time.Time { return wall }})
		if !chain.TryActivateFallback(context.Background(), ReasonRateLimit, c.resetAt) {
			t.Fatalf("reset_at %v: fallback not activated", c.resetAt)
		}
		if chain.Current().Model != "gpt-5.5" {
			t.Fatalf("reset_at %v: model %q", c.resetAt, chain.Current().Model)
		}
		if until := cd.Until(); until.Sub(wall) != time.Duration(c.want)*time.Second {
			t.Fatalf("reset_at %v: cooldown %v want %ds", c.resetAt, until.Sub(wall), c.want)
		}
	}
}

func TestMinSwitchResetSecondsKeepsPrimaryWhenResetIsImminent(t *testing.T) {
	wall := time.Unix(1_700_000_000, 0)
	for _, c := range []struct {
		threshold float64
		switched  bool
	}{{0, true}, {120, false}} {
		cd := NewCooldown(config.Tree{"fallback": map[string]any{"min_switch_reset_seconds": c.threshold}})
		cd.now = func() time.Time { return wall }
		chain := NewChain(ChainOptions{Primary: Route{Provider: "openrouter", Model: "primary/model"}, Fallbacks: []Route{{Provider: "openai-codex", Model: "gpt-5.5"}}, Cooldown: cd, Now: func() time.Time { return wall }})
		activated := chain.TryActivateFallback(context.Background(), ReasonRateLimit, 1_700_000_030)
		if activated != c.switched {
			t.Fatalf("threshold %v: activated %v want %v", c.threshold, activated, c.switched)
		}
		if (chain.Current().Model == "gpt-5.5") != c.switched {
			t.Fatalf("threshold %v: model %q", c.threshold, chain.Current().Model)
		}
		if (!cd.Until().IsZero()) != c.switched {
			t.Fatalf("threshold %v: cooldown armed %v", c.threshold, !cd.Until().IsZero())
		}
	}
}

func exhaustionChain(fallbacks []Route, now time.Time) (*Chain, *Cooldown) {
	cd := NewCooldown(nil)
	cd.now = func() time.Time { return now }
	primary := Route{Provider: "openrouter", Model: "primary", BaseURL: "https://openrouter.ai/api/v1"}
	return NewChain(ChainOptions{Primary: primary, Fallbacks: fallbacks, Cooldown: cd, Now: func() time.Time { return now }}), cd
}

func TestNonRetryableExhaustionArmsCooldown(t *testing.T) {
	frozen := time.Unix(1000, 0)
	chain, cd := exhaustionChain([]Route{{Provider: "openai", Model: "gpt-4o"}, {Provider: "zai", Model: "glm-4.7"}}, frozen)
	ctx := context.Background()
	if !chain.TryActivateFallback(ctx, "", nil) || !chain.TryActivateFallback(ctx, "", nil) {
		t.Fatal("two activations expected")
	}
	if chain.TryActivateFallback(ctx, "", nil) {
		t.Fatal("chain should be exhausted")
	}
	if cd.Until() != frozen.Add(FallbackExhaustedCooldown) {
		t.Fatalf("cooldown %v want %v", cd.Until(), frozen.Add(FallbackExhaustedCooldown))
	}
}

func TestNoChainDoesNotArmCooldown(t *testing.T) {
	chain, cd := exhaustionChain(nil, time.Unix(1000, 0))
	if chain.TryActivateFallback(context.Background(), "", nil) {
		t.Fatal("empty chain activated")
	}
	if !cd.Until().IsZero() {
		t.Fatal("empty chain must not arm a cooldown")
	}
}

func TestRateLimitExhaustionKeeps60sCooldown(t *testing.T) {
	frozen := time.Unix(1000, 0)
	chain, cd := exhaustionChain([]Route{{Provider: "openai", Model: "gpt-4o"}}, frozen)
	ctx := context.Background()
	if !chain.TryActivateFallback(ctx, ReasonRateLimit, nil) {
		t.Fatal("first activation")
	}
	if chain.TryActivateFallback(ctx, ReasonRateLimit, nil) {
		t.Fatal("chain should be exhausted")
	}
	if cd.Until() != frozen.Add(60*time.Second) {
		t.Fatalf("cooldown %v", cd.Until().Sub(frozen))
	}
}

func TestCooldownNeverShrinksExistingWindow(t *testing.T) {
	frozen := time.Unix(1000, 0)
	chain, cd := exhaustionChain([]Route{{Provider: "openai", Model: "gpt-4o"}}, frozen)
	far := frozen.Add(999 * time.Second)
	cd.Extend(far)
	ctx := context.Background()
	if !chain.TryActivateFallback(ctx, "", nil) || chain.TryActivateFallback(ctx, "", nil) {
		t.Fatal("activation sequence")
	}
	if cd.Until() != far {
		t.Fatalf("cooldown shrank to %v", cd.Until())
	}
}

func backOnPrimary(chain *Chain) {
	chain.SetCurrent(chain.Primary())
	chain.ResetForFreshAttempt()
}

func TestBackoffDoublesPerConsecutiveRateLimit(t *testing.T) {
	frozen := time.Unix(1000, 0)
	chain, cd := exhaustionChain([]Route{{Provider: "openai", Model: "gpt-4o"}}, frozen)
	for n, want := range []int{60, 120, 240} {
		backOnPrimary(chain)
		chain.TryActivateFallback(context.Background(), ReasonRateLimit, nil)
		if cd.Until() != frozen.Add(time.Duration(want)*time.Second) {
			t.Fatalf("backoff #%d: cooldown %v want %ds", n+1, cd.Until().Sub(frozen), want)
		}
		if cd.BackoffCount() != n+1 {
			t.Fatalf("backoff count %d", cd.BackoffCount())
		}
	}
}

func TestBackoffCapsAtFourHours(t *testing.T) {
	frozen := time.Unix(1000, 0)
	chain, cd := exhaustionChain([]Route{{Provider: "openai", Model: "gpt-4o"}}, frozen)
	cd.backoffCount = 10
	chain.TryActivateFallback(context.Background(), ReasonRateLimit, nil)
	if cd.Until() != frozen.Add(14400*time.Second) {
		t.Fatalf("cooldown %v", cd.Until().Sub(frozen))
	}
}

func TestBackoffCounterResetsOnSuccessfulPrimaryRestore(t *testing.T) {
	frozen := time.Unix(1000, 0)
	chain, cd := exhaustionChain([]Route{{Provider: "openai", Model: "gpt-4o"}}, frozen)
	ctx := context.Background()
	chain.TryActivateFallback(ctx, ReasonRateLimit, nil)
	backOnPrimary(chain)
	chain.TryActivateFallback(ctx, ReasonRateLimit, nil)
	if cd.BackoffCount() != 2 {
		t.Fatalf("backoff count %d", cd.BackoffCount())
	}
	cd.Clear()
	if _, restored := chain.RestorePrimary(ctx); !restored {
		t.Fatal("primary should restore once the cooldown cleared")
	}
	if cd.BackoffCount() != 0 || chain.Activated() || chain.Current().Provider != "openrouter" {
		t.Fatalf("restore state: count %d activated %v current %+v", cd.BackoffCount(), chain.Activated(), chain.Current())
	}
	chain.TryActivateFallback(ctx, ReasonRateLimit, nil)
	if cd.Until() != frozen.Add(60*time.Second) {
		t.Fatalf("fresh first failure cooldown %v", cd.Until().Sub(frozen))
	}
}

type fakePool struct {
	provider  string
	available bool
	entries   int
	nextAt    time.Time
	hasNext   bool
	lifted    bool
	askedFor  []string
}

func (p *fakePool) Provider() string     { return p.provider }
func (p *fakePool) HasCredentials() bool { return p.entries > 0 }
func (p *fakePool) HasAvailable(model string) bool {
	p.askedFor = append(p.askedFor, model)
	return p.available
}
func (p *fakePool) NextAvailableAt(model string) (time.Time, bool) { return p.nextAt, p.hasNext }
func (p *fakePool) EntryCount() int                                { return p.entries }
func (p *fakePool) LiftReopenedCooldowns(model string) bool        { return p.lifted }

func entitlementError(model string) *APIError {
	return &APIError{StatusCode: 400, Message: "The '" + model + "' model is not supported when using Codex with a ChatGPT account."}
}

func TestMarkerFiresOnlyOnSingleCredentialEntitlement400AndFallbackWalkSkipsIt(t *testing.T) {
	cd := NewCooldown(nil)
	for _, err := range []*APIError{
		{StatusCode: 500, Message: "server error"}, {StatusCode: 429, Message: "rate limited"},
		{StatusCode: 400, Message: "invalid request body"}, {StatusCode: 403, Message: "model is not supported when using Codex with a ChatGPT account."},
	} {
		if cd.MarkEntitlementRejected(err, "openai-codex", "gpt-5.6-sol", nil) {
			t.Fatalf("marker fired for %+v", err)
		}
	}
	pool := &fakePool{provider: "openai-codex", available: true, entries: 2}
	if cd.MarkEntitlementRejected(entitlementError("gpt-5.6-sol"), "openai-codex", "gpt-5.6-sol", pool.HasAvailable) {
		t.Fatal("rotation owns the recovery while a pool entry is eligible")
	}
	if len(pool.askedFor) != 1 || pool.askedFor[0] != "gpt-5.6-sol" {
		t.Fatalf("pool asked %v", pool.askedFor)
	}
	if len(cd.EntitlementRejected()) != 0 {
		t.Fatal("no marker expected yet")
	}
	pool.available = false
	if !cd.MarkEntitlementRejected(entitlementError("gpt-5.6-sol"), "openai-codex", "gpt-5.6-sol", pool.HasAvailable) {
		t.Fatal("marker should fire once every entry is benched")
	}
	var diagnostics []string
	cd.Diagnostic = func(s string) { diagnostics = append(diagnostics, s) }
	if !cd.MarkEntitlementRejected(entitlementError("gpt-5.6-sol"), "openai-codex", "gpt-5.6-sol", nil) {
		t.Fatal("marker without a pool")
	}
	if rejected := cd.EntitlementRejected(); len(rejected) != 1 || rejected[0] != [2]string{"openai-codex", "gpt-5.6-sol"} {
		t.Fatalf("rejected %v", rejected)
	}
	if len(diagnostics) != 0 {
		t.Fatal("a repeated marker must not announce again")
	}
	if !cd.IsEntitlementRejected("openai-codex", "gpt-5.6-sol") || cd.IsEntitlementRejected("openai-codex", "other") {
		t.Fatal("rejected lookup")
	}
	activated := 0
	chain := NewChain(ChainOptions{Primary: Route{Provider: "openai-codex", Model: "gpt-5.6-sol"}, Fallbacks: []Route{{Provider: "openai-codex", Model: "gpt-5.6-sol"}}, Cooldown: cd,
		OnActivate: func(ctx context.Context, old, next Route) error { activated++; return nil }})
	if chain.TryActivateFallback(context.Background(), "", nil) || activated != 0 {
		t.Fatal("the rejected slug must be skipped and the chain exhausts")
	}
}

func TestRestorePrimaryRuntimeIsGatedOnRejectedPrimarySlug(t *testing.T) {
	run := func(rejectedSlug string) (*Chain, bool, []string) {
		cd := NewCooldown(nil)
		cd.rejected[[2]string{"openai-codex", rejectedSlug}] = true
		var emitted []string
		chain := NewChain(ChainOptions{Primary: Route{Provider: "openai-codex", Model: "gpt-5.6-sol"}, Fallbacks: []Route{{Provider: "zai", Model: "glm-5.2"}}, Cooldown: cd, Emit: func(s string) { emitted = append(emitted, s) }})
		if !chain.TryActivateFallback(context.Background(), "", nil) {
			t.Fatal("fallback activation")
		}
		cd.Clear()
		_, restored := chain.RestorePrimary(context.Background())
		return chain, restored, emitted
	}
	chain, restored, emitted := run("gpt-5.6-sol")
	if restored || chain.Current().Provider != "zai" || chain.Current().Model != "glm-5.2" || !chain.Activated() {
		t.Fatalf("restore must stay on the fallback: %v %+v", restored, chain.Current())
	}
	for _, n := range emitted {
		if contains(n, "Primary model restored") {
			t.Fatal("unverified restore announced")
		}
	}
	_, restored, emitted = run("some-other-slug")
	if !restored || len(emitted) != 1 || !contains(emitted[0], "Primary model restored") {
		t.Fatalf("restore expected: %v %q", restored, emitted)
	}
}

func contains(s, sub string) bool {
	return len(sub) == 0 || (len(s) >= len(sub) && indexOf(s, sub) >= 0)
}

func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}

func TestProviderResetDelay(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	if d, ok := ProviderResetDelay(1_700_000_090.5, now); !ok || d != 90500*time.Millisecond {
		t.Fatalf("future %v %v", d, ok)
	}
	if _, ok := ProviderResetDelay(1_699_999_999, now); ok {
		t.Fatal("past reset")
	}
	if _, ok := ProviderResetDelay(nil, now); ok {
		t.Fatal("nil reset")
	}
	if _, ok := ProviderResetDelay("junk", now); ok {
		t.Fatal("junk reset")
	}
	if MinSwitchResetFromConfig(config.Tree{"fallback": map[string]any{"min_switch_reset_seconds": "30"}}) != 30*time.Second {
		t.Fatal("string threshold")
	}
	if MinSwitchResetFromConfig(config.Tree{}) != 0 || MinSwitchResetFromConfig(nil) != 0 {
		t.Fatal("missing threshold is off")
	}
}
