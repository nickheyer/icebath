package provider

import (
	"context"
	"net/http"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/tools"
)

func TestGuestHandoffResultLinesCopy(t *testing.T) {
	gmail := tools.AppTitle("gmail")
	cases := []struct {
		status map[string]any
		want   []string
	}{
		{map[string]any{"state": "pending"}, []string{"Moving your free-tier apps to this account; Icebath keeps trying in the background."}},
		{map[string]any{"state": "failed", "error": "client_not_allowed"}, []string{"Your free-tier apps could not be moved. This sign-in can't receive them. Sign in again with `icebath auth add nous`."}},
		{map[string]any{"state": "failed", "error": "weird_code"}, []string{"Your free-tier apps could not be moved."}},
		{map[string]any{"state": "gone", "probably_moved": true}, []string{"Your free-tier apps were probably moved already."}},
		{map[string]any{"state": "gone"}, []string{"Nothing left to move from the free tier."}},
		{map[string]any{"state": "idle"}, []string{}},
		{map[string]any{"state": "claimed", "account_email": "a@b.c", "connectors_kind": "moved", "moved_toolkits": []any{"gmail"}, "dropped_toolkits": []any{"gmail"}},
			[]string{"Your apps moved to a@b.c: " + gmail + ".", "Reconnect in a@b.c: " + gmail + "."}},
		{map[string]any{"state": "claimed", "connectors_kind": "conflict"}, []string{"your account already had its own apps, so the free-tier apps stayed behind."}},
		{map[string]any{"state": "claimed", "connectors_kind": "moved", "moved_toolkits": []any{}}, []string{"There were no free-tier apps to move."}},
		{map[string]any{"state": "claimed", "connectors_kind": "unknown"}, []string{"Any free-tier apps are in this account now."}},
	}
	for _, c := range cases {
		if got := guestHandoffResultLines(c.status); !reflect.DeepEqual(got, c.want) {
			t.Fatalf("%v: got %q want %q", c.status, got, c.want)
		}
	}
	lines := guestHandoffStatusLines(map[string]any{"state": "claimed", "account_email": "a@b.c", "connectors_kind": "none"})
	if lines[0] != "Signed in as a@b.c. There were no free-tier apps to move." {
		t.Fatalf("status lines %q", lines)
	}
}

func TestGuestHandoffApplyWalksTheRetryLadder(t *testing.T) {
	now := 1000.0
	saved := guestHandoffNow
	guestHandoffNow = func() float64 { return now }
	t.Cleanup(func() { guestHandoffNow = saved })
	active := map[string]any{"state": "pending", "anon_token": "anon_x", "attempts": 1}
	retry := guestHandoffApply(active, handoffOutcomeRetry, map[string]any{"error": "busy"}, 0, false, 0)
	if retry["state"] != "retrying" || retry["next_attempt_at"] != now+60 || retry["anon_token"] != "anon_x" {
		t.Fatalf("first retry %v", retry)
	}
	if hinted := guestHandoffApply(active, handoffOutcomeRetry, map[string]any{}, 5, true, 1); hinted["next_attempt_at"] != now+5 {
		t.Fatalf("retry after hint ignored %v", hinted)
	}
	if final := guestHandoffApply(active, handoffOutcomeRetry, map[string]any{}, 0, false, 4); final["state"] != "failed" {
		t.Fatalf("the attempt after the last delay must be final %v", final)
	}
	gone := guestHandoffApply(active, handoffOutcomeGone, map[string]any{"error": "unknown_token"}, 0, false, 1)
	if gone["state"] != "gone" || gone["probably_moved"] != true || gone["anon_token"] != nil {
		t.Fatalf("gone after an earlier attempt %v", gone)
	}
	finished := map[string]any{"state": "claimed"}
	if got := guestHandoffApply(finished, handoffOutcomeFail, map[string]any{}, 0, false, 0); !reflect.DeepEqual(got, finished) {
		t.Fatalf("a finished move must stay finished %v", got)
	}
	if guestHandoffClassify(200, map[string]any{"status": "queued"}) != handoffOutcomeRetry || guestHandoffClassify(409, map[string]any{"error": "already_retired"}) != handoffOutcomeGone ||
		guestHandoffClassify(403, map[string]any{"error": "account_locked"}) != handoffOutcomeFail || guestHandoffClassify(500, map[string]any{}) != handoffOutcomeRetry {
		t.Fatal("claim classification")
	}
}

func TestGuestHandoffNoteThenDrainMovesTheAppsOnce(t *testing.T) {
	fake, env := installAnonPortal(t)
	guest, err := EnsurePortalIdentity(env, PortalIdentityOptions{Explicit: true})
	if err != nil || guest == nil {
		t.Fatalf("mint %v %v", guest, err)
	}
	var auth []string
	fake.routes = map[string]func(*http.Request, map[string]any) (int, map[string]any){
		"/api/anonymous/claim": func(r *http.Request, req map[string]any) (int, map[string]any) {
			auth = append(auth, r.Header.Get("authorization"))
			if req["token"] != guest["anon_token"] {
				return 400, map[string]any{"error": "invalid_request"}
			}
			return 200, map[string]any{"status": "claimed", "account_email": "a@b.c", "connectors": map[string]any{"kind": "moved", "moved_toolkits": []any{"gmail"}}}
		},
	}
	account := map[string]any{"access_token": anonJWT(map[string]any{"client_id": "hermes-cli", "email": "a@b.c"}), "refresh_token": "rt",
		"expires_at": "2099-01-01T00:00:00+00:00", "portal_base_url": fake.URL, "client_id": "hermes-cli", "auth_method": "oauth_device_code"}
	if _, err := env.Store.SaveActiveProviderState("nous", account); err != nil {
		t.Fatal(err)
	}
	records, err := guestHandoffRecords(env)
	if err != nil || len(records) != 1 {
		t.Fatalf("one pending record expected, got %v %v", records, err)
	}
	for _, raw := range records {
		record := raw.(map[string]any)
		if record["state"] != "pending" || !slices.Contains(record["profiles"].([]any), any(guestHandoffHomeKey(env))) {
			t.Fatalf("pending record %v", record)
		}
	}
	views, err := guestHandoffDrain(env)
	if err != nil || len(views) != 1 || views[0]["state"] != "claimed" || views[0]["account_email"] != "a@b.c" {
		t.Fatalf("drain %v %v", views, err)
	}
	if len(auth) != 1 || !strings.HasPrefix(auth[0], "Bearer ") {
		t.Fatalf("claim must carry the account bearer %q", auth)
	}
	again, err := guestHandoffDrain(env)
	if err != nil || len(again) != 0 || len(auth) != 1 {
		t.Fatalf("a claimed record is never sent again %v %v", again, err)
	}
	status, err := guestHandoffRecordStatus(env, anyString(guest["anon_token"]))
	if err != nil || status["state"] != "claimed" {
		t.Fatalf("record status %v %v", status, err)
	}
}

func TestSignInStatesCarryTheirOwnCopy(t *testing.T) {
	if FormatWaitLine(30) != "Waiting for sign-in, up to 1 minute." || FormatWaitLine(900) != "Waiting for sign-in, up to 15 minutes." {
		t.Fatal("wait line")
	}
	done := SignInCompleted{Email: "a@b.c", ModelChanged: true, Handoff: []string{"moved"}}
	if done.Copy() != "Signed in as a@b.c.\nmoved\n"+UpgradeNoDefaultChat || done.CopyTerminal() != "Signed in as a@b.c.\nmoved\n"+UpgradeNoDefaultTerminal {
		t.Fatalf("completed copy %q", done.Copy())
	}
	busy := SignInFailed{Reason: AnonRateLimited}
	if !busy.Retryable() || !strings.Contains(busy.Copy(), "about a minute") {
		t.Fatalf("busy copy %q", busy.Copy())
	}
	if (SignInFailed{Reason: AnonUnreachable}).Copy() != UpgradeServiceUnreachable {
		t.Fatal("unreachable copy")
	}
	plain := SignInFailed{Detail: "disk full"}
	if plain.Copy() != UpgradeNotCompleted || plain.CopyTerminal() != "Sign-in failed: disk full" {
		t.Fatal("unnamed failure keeps the detail out of chat")
	}
	if (SignInFailed{Reason: AnonGateClosed}).Retryable() {
		t.Fatal("terminal code is not retryable")
	}
}

func TestSignInAlreadySignedInShortCircuitsBeforeAnyNetwork(t *testing.T) {
	fake, env := installAnonPortal(t)
	account := map[string]any{"access_token": anonJWT(map[string]any{"client_id": "hermes-cli"}), "refresh_token": "rt", "expires_at": "2099-01-01T00:00:00+00:00", "portal_base_url": fake.URL}
	if _, err := env.Store.SaveActiveProviderState("nous", account); err != nil {
		t.Fatal(err)
	}
	fake.clearCalls()
	var kinds []string
	for state := range RunSignIn(env, SignInOptions{}) {
		kinds = append(kinds, state.Kind())
	}
	if !reflect.DeepEqual(kinds, []string{"already_signed_in"}) || len(fake.paths()) != 0 {
		t.Fatalf("kinds %v calls %v", kinds, fake.paths())
	}
}

func TestSignInRendererExitCodes(t *testing.T) {
	_, env := installAnonPortal(t)
	seq := func(states ...SignInState) func(func(SignInState) bool) {
		return func(yield func(SignInState) bool) {
			for _, s := range states {
				if !yield(s) {
					return
				}
			}
		}
	}
	var out strings.Builder
	if code := RenderSignInCLI(env, RenderSignInOptions{States: seq(SignInWaiting{}, SignInCompleted{Email: "a@b.c"}), Out: &out}); code != 0 {
		t.Fatalf("completed exit %d", code)
	}
	if want := UpgradeStart + "\n" + UpgradeWaiting + "\nSigned in as a@b.c.\n"; out.String() != want {
		t.Fatalf("output %q", out.String())
	}
	out.Reset()
	if code := RenderSignInCLI(env, RenderSignInOptions{States: seq(SignInAlreadySignedIn{}), Out: &out}); code != 0 || out.String() != UpgradeAlreadySignedIn+"\n" {
		t.Fatalf("precondition prints no start line %q", out.String())
	}
	if code := RenderSignInCLI(env, RenderSignInOptions{States: seq(SignInTimedOut{}), Out: &out}); code != 1 {
		t.Fatal("timed out exits 1")
	}
	copy := DrainSignInCopy(seq(SignInWaiting{}, SignInFailed{Detail: "x"}), false, func(SignInState) error { panic("hook") })
	if copy != "Sign-in failed: x" {
		t.Fatalf("drain copy %q", copy)
	}
}

func TestWelcomeTrialClaimsOnceThenSettles(t *testing.T) {
	_, env := installAnonPortal(t)
	env = anonDesktopEnv(env)
	probe, clear, reserved, mark, lock, resolve := welcomeInstallProbe, welcomeProbeClear, welcomeTrialReserved, welcomeMarkUsed, welcomeClaimLock, welcomeResolveSkipped
	t.Cleanup(func() {
		welcomeInstallProbe, welcomeProbeClear, welcomeTrialReserved, welcomeMarkUsed, welcomeClaimLock, welcomeResolveSkipped = probe, clear, reserved, mark, lock, resolve
	})
	used, marks, isReserved := false, 0, false
	welcomeInstallProbe = func(_ context.Context) tools.InstallState {
		if used {
			return tools.InstallState{Findings: []tools.InstallFinding{{}}}
		}
		return tools.InstallState{}
	}
	welcomeProbeClear = func() {}
	welcomeTrialReserved = func() bool { return isReserved }
	welcomeMarkUsed = func(_ context.Context) error { used, isReserved = true, false; marks++; return nil }
	welcomeClaimLock = func() (func(), error) { return func() {}, nil }
	welcomeResolveSkipped = func(PoolEnv, string) (string, error) { return "", &AuthError{Code: "no_provider_configured"} }
	if reason, err := welcomeClaimBlocker(env, false, nil); err != nil || reason != "" {
		t.Fatalf("fresh desktop must be claimable %q %v", reason, err)
	}
	state, err := ClaimDesktopFreeTier(env, false, false)
	if err != nil || state == nil || state[WelcomeClaimFlag] != true || marks != 1 {
		t.Fatalf("claim %v %v marks %d", state, err, marks)
	}
	again, err := ClaimDesktopFreeTier(env, false, false)
	if err != nil || again != nil || marks != 1 {
		t.Fatalf("second claim must be blocked %v %v", again, err)
	}
	if reason, _ := welcomeClaimBlocker(env, true, nil); reason != "a Nous identity exists" {
		t.Fatalf("blocker %q", reason)
	}
	isReserved = true
	settled, err := SettleInterruptedClaim(env)
	if err != nil || !settled || marks != 2 || isReserved {
		t.Fatalf("interrupted claim not settled %v %v", settled, err)
	}
	if available, err := welcomeInferenceAvailable(env, false); err != nil || !available {
		t.Fatalf("claimed trial carries inference %v %v", available, err)
	}
}
