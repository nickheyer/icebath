package tuiserver

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/provider"
	"github.com/nickheyer/icebath/pkg/provider/anonportaltest"
	"github.com/nickheyer/icebath/pkg/tools"
)

// Fake account service read on the local Desktop backend of a first install, with every portal memo reset
func freeTierNAS(t *testing.T) (*anonportaltest.Portal, context.Context) {
	t.Helper()
	provider.ResetAnonPortalMemosForTests()
	portal := anonportaltest.Install(t)
	t.Cleanup(provider.ResetAnonPortalMemosForTests)
	t.Cleanup(tools.ConfineInstallProbeForTests(t.TempDir()))
	return portal, tools.WithServingLocalDesktop(context.Background(), true)
}

// Guest that claimed the welcome trial through the fake portal on the local Desktop backend
func freeTierGuest(t *testing.T) (*anonportaltest.Portal, context.Context) {
	t.Helper()
	portal, ctx := freeTierNAS(t)
	state, err := provider.ClaimDesktopFreeTier(scopedPoolEnv(ctx), false, false)
	if err != nil || state == nil {
		blocker, _ := provider.WelcomeClaimBlocker(scopedPoolEnv(ctx))
		t.Fatalf("claim %v %v blocker %q", state, err, blocker)
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

func TestStatusCarriesThePendingBrowserChallengeAndDropsItOnceCleared(t *testing.T) {
	_, ctx := freeTierGuest(t)
	if _, has := freeTierCall(t, ctx, "free_tier.status", nil)["challenge"]; has {
		t.Fatal("a challenge before any was recorded")
	}
	env := scopedPoolEnv(ctx)
	challenge := provider.BrowserChallenge{URL: "https://portal.example.test/challenge?code=t", Required: true,
		ExpiresIn: 600 * 1e9, Interval: 2 * 1e9, Message: "A quick check first."}
	provider.RecordChallengeForTests(env, challenge, false)
	func() {
		defer provider.ClearPendingChallengeForTests(env)
		if got := freeTierCall(t, ctx, "free_tier.status", nil)["challenge"]; !pyjson.Equal(got, challenge.AsPayload()) {
			t.Fatalf("challenge %v", got)
		}
	}()
	if _, has := freeTierCall(t, ctx, "free_tier.status", nil)["challenge"]; has {
		t.Fatal("the cleared challenge is still reported")
	}
}

func TestWindowOutcomesAreScopedAndDoNotGrantAuth(t *testing.T) {
	_, ctx := freeTierGuest(t)
	a := provider.BrowserChallenge{URL: "https://portal.example.test/challenge?code=a", Required: true, ExpiresIn: 600 * 1e9, Interval: 2 * 1e9, Message: "check"}
	b := provider.BrowserChallenge{URL: "https://portal.example.test/challenge?code=b", Required: false, ExpiresIn: 600 * 1e9, Interval: 2 * 1e9, Message: "check"}
	provider.RecordChallengeForTests(scopedPoolEnv(ctx), a, false)
	secondaryHome := filepath.Join(t.TempDir(), "secondary")
	secondary := provider.HomePoolEnv(secondaryHome)
	secondary.Ctx = home.WithOverride(ctx, secondaryHome)
	provider.RecordChallengeForTests(secondary, b, false)
	if provider.RecordHostOutcome(secondary, a.URL, 0, "done") {
		t.Fatal("another home's window outcome was accepted")
	}
	if got := provider.PendingChallenge(secondary)["url"]; got != b.URL {
		t.Fatalf("secondary pending %v", got)
	}
	if got := freeTierCall(t, ctx, "free_tier.status", nil)["challenge"].(map[string]any)["url"]; got != a.URL {
		t.Fatalf("status challenge %v", got)
	}
	if got := freeTierCall(t, ctx, "free_tier.challenge_result", map[string]any{"url": a.URL, "outcome": "error"}); !pyjson.Equal(got, map[string]any{"accepted": true}) {
		t.Fatalf("challenge_result %v", got)
	}
	if got := freeTierCall(t, ctx, "free_tier.status", nil)["challenge"].(map[string]any)["url"]; got != a.URL {
		t.Fatalf("status challenge after the outcome %v", got)
	}
}

func TestStatusWithoutAnIdentityIsAPureRead(t *testing.T) {
	portal, ctx := freeTierNAS(t)
	status := freeTierCall(t, ctx, "free_tier.status", nil)
	if status["has_guest"] != false || status["available"] != false || status["enabled"] != true {
		t.Fatalf("status %v", status)
	}
	if calls := portal.Calls(); len(calls) != 0 {
		t.Fatalf("portal calls %v", calls)
	}
}

func TestProvisionClaimsTheTrialOnceWhileItIsClaimable(t *testing.T) {
	portal, ctx := freeTierNAS(t)
	claimed := map[string]any{"has_guest": true, "enabled": true}
	if got := freeTierCall(t, ctx, "free_tier.provision", nil); !pyjson.Equal(got, claimed) {
		t.Fatalf("provision %v", got)
	}
	if portal.Creates() != 1 || !provider.FreeInferenceActive(scopedPoolEnv(ctx)) {
		t.Fatalf("creates %d", portal.Creates())
	}
	if got := freeTierCall(t, ctx, "free_tier.provision", nil); !pyjson.Equal(got, claimed) {
		t.Fatalf("second provision %v", got)
	}
	if portal.Creates() != 1 {
		t.Fatalf("minted again %d", portal.Creates())
	}
}

func TestProvisionReportsARefusedClaimAndTheUsersRetrySkipsTheCooldown(t *testing.T) {
	portal, ctx := freeTierNAS(t)
	portal.SetGateClosed(true)
	result := freeTierCall(t, ctx, "free_tier.provision", nil)
	if result["has_guest"] != false || result["enabled"] != true || result["error_code"] != "anon_gate_closed" || result["retryable"] != false {
		t.Fatalf("refused provision %v", result)
	}
	portal.SetGateClosed(false)
	if got := freeTierCall(t, ctx, "free_tier.provision", nil); !pyjson.Equal(got, map[string]any{"has_guest": true, "enabled": true}) {
		t.Fatalf("retry %v", got)
	}
	if !provider.FreeInferenceActive(scopedPoolEnv(ctx)) {
		t.Fatal("the retry did not activate free inference")
	}
}

func TestProvisionDoesNothingWithGuestsSwitchedOff(t *testing.T) {
	portal, ctx := freeTierNAS(t)
	freeTierGuestOff(t, ctx)
	if got := freeTierCall(t, ctx, "free_tier.provision", nil); !pyjson.Equal(got, map[string]any{"has_guest": false, "enabled": false}) {
		t.Fatalf("provision %v", got)
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
