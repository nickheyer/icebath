package tuiserver

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/provider"
	"github.com/nickheyer/icebath/pkg/provider/anonportaltest"
)

// Fake account service of a first install, with every portal memo reset
func freeTierNAS(t *testing.T) (*anonportaltest.Portal, context.Context) {
	t.Helper()
	provider.ResetAnonPortalMemosForTests()
	portal := anonportaltest.Install(t)
	t.Cleanup(provider.ResetAnonPortalMemosForTests)
	return portal, context.Background()
}

// Guest minted through the fake portal and stamped with the welcome trial claim
func freeTierGuest(t *testing.T) (*anonportaltest.Portal, context.Context) {
	t.Helper()
	portal, ctx := freeTierNAS(t)
	env := scopedPoolEnv(ctx)
	state, err := provider.EnsurePortalIdentity(env, provider.PortalIdentityOptions{Explicit: true})
	if err != nil || state == nil {
		t.Fatalf("mint %v %v", state, err)
	}
	state[provider.WelcomeClaimFlag] = true
	data, err := env.Store.Load()
	if err != nil {
		t.Fatal(err)
	}
	if err := env.Store.StoreProviderState(data, "nous", state, false); err != nil {
		t.Fatal(err)
	}
	if err := env.Store.Save(data); err != nil {
		t.Fatal(err)
	}
	return portal, ctx
}

// Result of one registered free tier handler called on ctx
func freeTierCall(t *testing.T, ctx context.Context, method string, params map[string]any) map[string]any {
	t.Helper()
	if params == nil {
		params = map[string]any{}
	}
	reply := pyjson.Plain(handleRequest(ctx, map[string]any{"jsonrpc": "2.0", "id": 1, "method": method, "params": params})).(map[string]any)
	result, isDict := reply["result"].(map[string]any)
	if !isDict {
		t.Fatalf("%s answered %v", method, reply)
	}
	return result
}

// Switches guests off in the profile config
func freeTierGuestOff(t *testing.T, ctx context.Context) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(home.Dir(ctx), "config.yaml"), []byte("nous:\n  guest: false\n"), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestStatusIsPullFromLocalStateAndAckPersistsOnTheIdentity(t *testing.T) {
	_, ctx := freeTierGuest(t)
	status := freeTierCall(t, ctx, "free_tier.status", nil)
	want := map[string]any{"has_guest": true, "enabled": true, "available": true, "notice_pending": true,
		"model": "nous/welcome", "label": provider.FreeTierLabel}
	if !pyjson.Equal(status, want) {
		t.Fatalf("status %v", status)
	}
	if got := freeTierCall(t, ctx, "free_tier.ack_notice", nil); !pyjson.Equal(got, map[string]any{"acked": true}) {
		t.Fatalf("ack %v", got)
	}
	if got := freeTierCall(t, ctx, "free_tier.status", nil)["notice_pending"]; got != false {
		t.Fatalf("notice_pending %v", got)
	}
	data, err := scopedPoolEnv(ctx).Store.Load()
	if err != nil {
		t.Fatal(err)
	}
	nous := pyjson.Plain(data["providers"]).(map[string]any)["nous"].(map[string]any)
	if nous[provider.GuestNoticeFlag] != true {
		t.Fatalf("nous state %v", nous)
	}
	freeTierGuestOff(t, ctx)
	status = freeTierCall(t, ctx, "free_tier.status", nil)
	if status["has_guest"] != true || status["enabled"] != false || status["available"] != false || status["notice_pending"] != false {
		t.Fatalf("guest off status %v", status)
	}
}

// test_free_tier_rpc.py test_status_carries_the_pending_browser_challenge_and_drops_it_once_cleared
func TestStatusCarriesThePendingBrowserChallengeAndDropsItOnceCleared(t *testing.T) {
	_, ctx := freeTierGuest(t)
	if _, carried := freeTierCall(t, ctx, "free_tier.status", nil)["challenge"]; carried {
		t.Fatal("a status with no challenge carried one")
	}
	env := scopedPoolEnv(ctx)
	challenge := provider.RecordChallengeForTests(env, provider.BrowserChallenge{URL: "https://portal.example.test/challenge?code=t",
		Required: true, ExpiresIn: 600 * time.Second, Interval: 2 * time.Second, Message: "A quick check first."}, false)
	carried := freeTierCall(t, ctx, "free_tier.status", nil)["challenge"]
	provider.ClearPendingChallengeForTests(env)
	if !pyjson.Equal(carried, challenge.AsPayload()) {
		t.Fatalf("challenge %v want %v", carried, challenge.AsPayload())
	}
	if _, carried := freeTierCall(t, ctx, "free_tier.status", nil)["challenge"]; carried {
		t.Fatal("a cleared challenge stayed on the status")
	}
}

func TestStatusWithoutAnIdentityIsAPureRead(t *testing.T) {
	portal, ctx := freeTierNAS(t)
	status := freeTierCall(t, ctx, "free_tier.status", nil)
	if status["has_guest"] != false || status["available"] != false || status["enabled"] != false {
		t.Fatalf("status %v", status)
	}
	if calls := portal.Calls(); len(calls) != 0 {
		t.Fatalf("portal calls %v", calls)
	}
}

func TestSignInOfferIsReportedThenClaimedOnce(t *testing.T) {
	_, ctx := freeTierGuest(t)
	t.Cleanup(provider.PinFreeTierOfferClockForTests(10_000.0))
	if _, has := freeTierCall(t, ctx, "free_tier.status", nil)["nudge_due_in"]; has {
		t.Fatal("an offer before any task finished")
	}
	unclaimed := map[string]any{"claimed": false}
	if got := freeTierCall(t, ctx, "free_tier.claim_nudge", nil); !pyjson.Equal(got, unclaimed) {
		t.Fatalf("claim before a task %v", got)
	}
	if err := provider.RecordTaskDone(scopedPoolEnv(ctx), provider.FreeTierOfferTrial); err != nil {
		t.Fatal(err)
	}
	if got := freeTierCall(t, ctx, "free_tier.status", nil)["nudge_due_in"]; !pyjson.Equal(got, provider.OfferDelaySeconds) {
		t.Fatalf("due in %v", got)
	}
	if got := freeTierCall(t, ctx, "free_tier.claim_nudge", nil); !pyjson.Equal(got, unclaimed) {
		t.Fatalf("claim before due %v", got)
	}
	t.Cleanup(provider.PinFreeTierOfferClockForTests(10_000.0 + provider.OfferDelaySeconds))
	if got := freeTierCall(t, ctx, "free_tier.status", nil)["nudge_due_in"]; !pyjson.Equal(got, 0) {
		t.Fatalf("due in once due %v", got)
	}
	if got := freeTierCall(t, ctx, "free_tier.claim_nudge", nil); !pyjson.Equal(got, map[string]any{"claimed": true}) {
		t.Fatalf("claim when due %v", got)
	}
	if got := freeTierCall(t, ctx, "free_tier.claim_nudge", nil); !pyjson.Equal(got, unclaimed) {
		t.Fatalf("second claim %v", got)
	}
	if _, has := freeTierCall(t, ctx, "free_tier.status", nil)["nudge_due_in"]; has {
		t.Fatal("an offer with nothing finished since it was claimed")
	}
}

func TestSignInOfferIsMootOnceSignedIn(t *testing.T) {
	_, ctx := freeTierGuest(t)
	t.Cleanup(provider.PinFreeTierOfferClockForTests(10_000.0))
	env := scopedPoolEnv(ctx)
	if err := provider.RecordTaskDone(env, provider.FreeTierOfferTrial); err != nil {
		t.Fatal(err)
	}
	err := env.Store.Update(func(data provider.AuthData) (bool, error) {
		providers, _ := data["providers"].(map[string]any)
		if providers == nil {
			providers = map[string]any{}
			data["providers"] = providers
		}
		providers["nous"] = map[string]any{"auth_method": "oauth", "access_token": "acct-token"}
		return true, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, has := freeTierCall(t, ctx, "free_tier.status", nil)["nudge_due_in"]; has {
		t.Fatal("a sign in offer for a signed in profile")
	}
	if got := freeTierCall(t, ctx, "free_tier.claim_nudge", nil); !pyjson.Equal(got, map[string]any{"claimed": false}) {
		t.Fatalf("claim %v", got)
	}
}
