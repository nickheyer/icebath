package provider

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"
)

// Swaps every account seam for the test and clears the snapshot cache on both ends
func nousAccountSeams(t *testing.T, state func() map[string]any, token func() string, payload map[string]any) {
	t.Helper()
	prevState, prevToken, prevFetch, prevPool := nousAccountAuthStateFn, nousAccountTokenFn, fetchNousAccountInfoFn, nousAccountLoadPoolFn
	ResetNousPortalAccountInfoCache()
	t.Cleanup(func() {
		nousAccountAuthStateFn, nousAccountTokenFn, fetchNousAccountInfoFn, nousAccountLoadPoolFn = prevState, prevToken, prevFetch, prevPool
		ResetNousPortalAccountInfoCache()
	})
	if state != nil {
		nousAccountAuthStateFn = func(PoolEnv) (map[string]any, error) { return state(), nil }
	}
	if token != nil {
		nousAccountTokenFn = func(PoolEnv) (string, error) { return token(), nil }
	}
	if payload != nil {
		wire := nousWirePayload(t, payload)
		fetchNousAccountInfoFn = func(PoolEnv, string, string) (map[string]any, error) { return wire, nil }
	}
}

// Payload as the account endpoint decodes it from the wire
func nousWirePayload(t *testing.T, payload map[string]any) map[string]any {
	t.Helper()
	raw, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := decodeExactJSON(string(raw))
	if err != nil {
		t.Fatal(err)
	}
	return decoded.(map[string]any)
}

func nousAccountState(token string) map[string]any {
	return map[string]any{"access_token": token, "portal_base_url": "https://portal.example.test", "client_id": "icebath-cli"}
}

// Account endpoint body with the paid service access block the portal sends
func nousAccountPayload(allowed bool, subscription map[string]any, subscriptionCredits, purchasedCredits float64, extra map[string]any) map[string]any {
	reason := "no_usable_credits"
	if allowed {
		reason = "usable_credits"
	}
	charge, _ := subscription["monthly_charge"].(int)
	psa := map[string]any{
		"allowed": allowed, "paid_access": allowed, "reason": reason, "organisation_id": "org_123", "effective_at_ms": 123456789,
		"has_active_subscription": subscription != nil, "active_subscription_is_paid": subscription != nil && charge > 0,
		"subscription_tier": subscription["tier"], "subscription_monthly_charge": subscription["monthly_charge"],
		"subscription_credits_remaining": subscriptionCredits, "purchased_credits_remaining": purchasedCredits,
		"total_usable_credits": subscriptionCredits + purchasedCredits,
	}
	if _, ok := extra["member_spend_cap_exceeded"]; ok {
		psa["reason"] = "member_spend_cap_exceeded"
	}
	for k, v := range extra {
		psa[k] = v
	}
	var sub any
	if subscription != nil {
		sub = subscription
	}
	return map[string]any{
		"user": map[string]any{"email": "alice@example.test", "privy_did": "did:privy:alice"}, "organisation": map[string]any{"id": "org_123"},
		"subscription": sub, "purchased_credits_remaining": purchasedCredits, "paid_service_access": psa,
	}
}

func nousAccountJWT(claims map[string]any) string {
	if _, ok := claims["exp"]; !ok {
		claims["exp"] = time.Now().Unix() + 900
	}
	return jwtWithClaims(claims)
}

func TestNousAccountFreshPayloadNormalization(t *testing.T) {
	tier2 := func(credits, rollover float64) map[string]any {
		return map[string]any{"plan": "Tier 2", "tier": 2, "monthly_charge": 20, "current_period_end": "2026-05-01T00:00:00.000Z", "credits_remaining": credits, "rollover_credits": rollover}
	}
	cases := []struct {
		payload map[string]any
		paid    bool
	}{
		{nousAccountPayload(true, tier2(12.25, 3.5), 12.25, 7.75, nil), true},
		{nousAccountPayload(false, tier2(0, 0), 0, 0, nil), false},
		{nousAccountPayload(true, nil, 0, 7.75, nil), true},
		{nousAccountPayload(false, nil, 0, 0, nil), false},
	}
	for i, tc := range cases {
		token := nousAccountJWT(map[string]any{"sub": "user_123", "org_id": "org_123"})
		nousAccountSeams(t, func() map[string]any { return nousAccountState(token) }, func() string { return "fresh-token" }, tc.payload)
		info := GetNousPortalAccountInfo(context.Background(), true)
		if info.Source != "account_api" || !info.Fresh || info.Email != "alice@example.test" || info.PrivyDID != "did:privy:alice" || info.OrgID != "org_123" {
			t.Fatalf("case %d: %+v", i, info)
		}
		if info.PaidServiceAccess == nil || *info.PaidServiceAccess != tc.paid || info.IsPaid() != tc.paid || info.IsFreeTier() != !tc.paid {
			t.Fatalf("case %d: paid %v want %v", i, info.PaidServiceAccess, tc.paid)
		}
	}
}

func TestNousAccountNoOAuthTokenReportsInferenceKeyPresent(t *testing.T) {
	nousAccountSeams(t, func() map[string]any { return map[string]any{} }, nil, nil)
	nousAccountLoadPoolFn = func(env PoolEnv) *Pool {
		return NewPool(env, "nous", []PooledCredential{{Provider: "nous", Label: "manual-nous", AgentKey: invokeJWT(3600), AgentKeyExpiresAt: futureISO(3600),
			InferenceBaseURL: "https://inference.example.test/v1", BaseURL: "https://inference.example.test/v1"}})
	}
	info := GetNousPortalAccountInfo(context.Background(), false)
	if info.LoggedIn || info.Source != "inference_key" || !info.InferenceCredentialPresent || info.CredentialSource != "pool:manual-nous" || info.PaidServiceAccess != nil {
		t.Fatalf("%+v", info)
	}
}

func TestNousAccountPoolOAuthEntryForceFreshUsesAccountAPI(t *testing.T) {
	token := nousAccountJWT(map[string]any{"sub": "user_123", "org_id": "org_123", "paid_access": false})
	nousAccountSeams(t, func() map[string]any { return map[string]any{} }, nil, nousAccountPayload(true, nil, 0, 3, nil))
	nousAccountLoadPoolFn = func(env PoolEnv) *Pool {
		return NewPool(env, "nous", []PooledCredential{{Provider: "nous", Label: "dashboard device_code", AuthType: "oauth", AccessToken: token, RefreshToken: "refresh-token",
			AgentKey: "opaque-runtime-key", AgentKeyExpiresAt: "2099-01-01T00:00:00+00:00", ExpiresAt: "2099-01-01T00:00:00+00:00",
			InferenceBaseURL: "https://inference.example.test/v1", BaseURL: "https://inference.example.test/v1", Extra: map[string]any{"portal_base_url": "https://portal.example.test"}}})
	}
	info := GetNousPortalAccountInfo(context.Background(), true)
	if !info.LoggedIn || info.Source != "account_api" || !info.Fresh || !info.IsPaid() || info.CredentialSource != "pool:dashboard device_code" {
		t.Fatalf("%+v", info)
	}
}

func TestNousAccountMemberSpendCapExceededMessage(t *testing.T) {
	payload := nousAccountPayload(false, nil, 0, 222990.17, map[string]any{"member_spend_cap_exceeded": true, "member_spend_cap_usd": "500", "member_spend_usd": "520.51", "member_spend_cap_remaining_usd": "0"})
	token := nousAccountJWT(map[string]any{"sub": "user_123", "org_id": "org_123"})
	nousAccountSeams(t, func() map[string]any { return nousAccountState(token) }, func() string { return "fresh-token" }, payload)
	info := GetNousPortalAccountInfo(context.Background(), true)
	acc := info.PaidServiceAccessInfo
	if info.PaidServiceAccess == nil || *info.PaidServiceAccess || acc == nil || acc.MemberSpendCapExceeded == nil || !*acc.MemberSpendCapExceeded ||
		acc.MemberSpendCapUSD == nil || *acc.MemberSpendCapUSD != 500.0 || acc.MemberSpendUSD == nil || *acc.MemberSpendUSD != 520.51 {
		t.Fatalf("%+v %+v", info, acc)
	}
	msg := FormatNousPortalEntitlementMessage(info, "Nous model access", false)
	for _, want := range []string{"spend cap", "$500.00", "$520.51", "$222990.17"} {
		if !strings.Contains(msg, want) {
			t.Fatalf("missing %q in %q", want, msg)
		}
	}
	if strings.Contains(msg, "no active subscription") {
		t.Fatal(msg)
	}
}

func TestNousAccountMemberSpendCapExceededWithoutAmounts(t *testing.T) {
	token := nousAccountJWT(map[string]any{"sub": "user_123", "org_id": "org_123"})
	nousAccountSeams(t, func() map[string]any { return nousAccountState(token) }, func() string { return "fresh-token" },
		nousAccountPayload(false, nil, 0, 100, map[string]any{"member_spend_cap_exceeded": true}))
	msg := FormatNousPortalEntitlementMessage(GetNousPortalAccountInfo(context.Background(), true), "Nous model access", false)
	if msg == "" || !strings.Contains(msg, "spend cap") || strings.Contains(msg, "no active subscription") {
		t.Fatal(msg)
	}
}

func TestNousAccountCachedSnapshotIsOnlyServedToTheLoginThatFetchedIt(t *testing.T) {
	current := nousAccountState("token-a")
	nousAccountSeams(t, func() map[string]any { return current }, func() string { return current["access_token"].(string) }, nousAccountPayload(true, nil, 0, 5, nil))
	ctx := context.Background()
	info := GetNousPortalAccountInfo(ctx, true)
	if CachedNousPortalAccountInfo(ctx, false) != info || CachedNousPortalAccountInfo(ctx, true) != info {
		t.Fatal("snapshot not served to its own login")
	}
	current = nousAccountState("token-b")
	if CachedNousPortalAccountInfo(ctx, false) != nil || CachedNousPortalAccountInfo(ctx, true) != nil {
		t.Fatal("another login read the snapshot")
	}
	current = map[string]any{}
	if CachedNousPortalAccountInfo(ctx, true) != nil {
		t.Fatal("a logged out profile read the snapshot")
	}
}

func TestNousAccountCachedSnapshotSurvivesATokenRefreshButNotAnAccountOrOrgSwitch(t *testing.T) {
	token := func(sub, org string, n int) string {
		return nousAccountJWT(map[string]any{"sub": sub, "org_id": org, "jti": "t" + string(rune('0'+n))})
	}
	current := nousAccountState(token("user_a", "org_a", 1))
	nousAccountSeams(t, func() map[string]any { return current }, func() string { return current["access_token"].(string) }, nousAccountPayload(true, nil, 0, 5, nil))
	ctx := context.Background()
	info := GetNousPortalAccountInfo(ctx, true)
	current = nousAccountState(token("user_a", "org_a", 2))
	if CachedNousPortalAccountInfo(ctx, false) != info {
		t.Fatal("a refreshed token for the same account lost the snapshot")
	}
	current = nousAccountState(token("user_a", "org_b", 3))
	if CachedNousPortalAccountInfo(ctx, true) != nil {
		t.Fatal("another org read the snapshot")
	}
	current = nousAccountState(token("user_b", "org_a", 4))
	if CachedNousPortalAccountInfo(ctx, true) != nil {
		t.Fatal("another user read the snapshot")
	}
}

func TestNousPortalPolicyPresent(t *testing.T) {
	cases := []struct {
		name           string
		token          any
		present, known bool
	}{
		{"claim true", nousAccountJWT(map[string]any{"policy_present": true}), true, true},
		{"claim false", nousAccountJWT(map[string]any{"policy_present": false}), false, true},
		{"absent claim is unknown not false", nousAccountJWT(map[string]any{"org_id": "org_1"}), false, false},
		{"non boolean claim is unknown", nousAccountJWT(map[string]any{"policy_present": "yes"}), false, false},
		{"no token is unknown", nil, false, false},
		{"undecodable token is unknown", "not-a-jwt", false, false},
	}
	for _, tc := range cases {
		nousAccountSeams(t, func() map[string]any {
			if tc.token == nil {
				return map[string]any{}
			}
			return map[string]any{"access_token": tc.token}
		}, nil, nil)
		if present, known := nousPolicyPresent(context.Background()); present != tc.present || known != tc.known {
			t.Fatalf("%s: got %v %v", tc.name, present, known)
		}
	}
}

func TestNousPortalPolicyNotice(t *testing.T) {
	prev := nousPolicyPresentFn
	t.Cleanup(func() { nousPolicyPresentFn = prev })
	set := func(present, known bool) {
		nousPolicyPresentFn = func(context.Context) (bool, bool) { return present, known }
	}
	env := modelsPoolEnv(context.Background())
	set(true, true)
	if strings.TrimSpace(nousPolicyNotice(env, true)) == "" {
		t.Fatal("a governed org got no notice")
	}
	for _, unknown := range []bool{false, true} {
		set(false, !unknown)
		if got := nousPolicyNotice(env, true); got != "" {
			t.Fatalf("absent or false claim produced %q", got)
		}
	}
	set(true, true)
	if got := nousPolicyNotice(env, false); got != "" {
		t.Fatalf("a filter that removed nothing produced %q", got)
	}
}

func TestNousPortalPaidToolNoticeSwitchesWordingInsideAChat(t *testing.T) {
	info := &NousPortalAccountInfo{LoggedIn: true, Source: "token", Fresh: true, AccountTier: "anonymous"}
	if got := FormatNousPortalEntitlementMessage(info, "", true); got != FreeTierNeedsAccountChat {
		t.Fatal(got)
	}
	if got := FormatNousPortalEntitlementMessage(info, "", false); got != FreeTierNeedsAccount {
		t.Fatal(got)
	}
}

// Guest store of the anon surfaces fixture on the desktop backend, a portal fetch failing the test unless replaced
func nousAccountGuestHome(t *testing.T, overrides map[string]any) PoolEnv {
	t.Helper()
	h := newTestHome(t)
	t.Setenv("ICEBATH_SHARED_AUTH_DIR", t.TempDir())
	state := map[string]any{"auth_method": "anonymous", "account_tier": "anonymous", "anon_token": "anon_t",
		"access_token": anonJWT(map[string]any{"sub": "nas_user:abc", "exp": time.Now().Unix() + 100_000_000}), "expires_at": "2030-01-01T00:00:00+00:00",
		"inference_base_url": "https://welcome-api.nousresearch.com/v1", "user_id": "nas_user:abc", "org_id": "nas_organisation:def", WelcomeClaimFlag: true}
	for k, v := range overrides {
		state[k] = v
	}
	writeAuthStore(t, h, map[string]any{"active_provider": "nous", "providers": map[string]any{"nous": state}})
	nousAccountSeams(t, nil, nil, nil)
	fetchNousAccountInfoFn = func(PoolEnv, string, string) (map[string]any, error) {
		t.Error("portal fetch must not happen on a read-only surface")
		return nil, errors.New("unexpected fetch")
	}
	return anonDesktopEnv(testEnv(t, h))
}

func TestNousPortalAnonymousTierMatchesCaseAndWhitespaceInsensitively(t *testing.T) {
	for _, tier := range []string{"Anonymous", "ANONYMOUS", " anonymous "} {
		env := nousAccountGuestHome(t, map[string]any{"account_tier": tier, "access_token": anonJWT(map[string]any{"account_tier": tier, "exp": time.Now().Unix() + 100_000_000})})
		info := getNousPortalAccountInfoIn(env, false, 60)
		if !info.IsAnonymousTier() {
			t.Fatalf("%q: %+v", tier, info)
		}
		if got := FormatNousPortalEntitlementMessage(info, "", false); got != FreeTierNeedsAccount {
			t.Fatalf("%q: %q", tier, got)
		}
	}
}

func TestNousPortalGuestWhoseLookupFailsStillGetsTheSignInCopy(t *testing.T) {
	env := nousAccountGuestHome(t, map[string]any{"access_token": anonJWT(map[string]any{"exp": time.Now().Unix() + 30}), "portal_base_url": "http://127.0.0.1:9"})
	fetchNousAccountInfoFn = func(PoolEnv, string, string) (map[string]any, error) { return nil, errors.New("portal down") }
	info := getNousPortalAccountInfoIn(env, false, 60)
	if info.Error == "" || !info.IsAnonymousTier() {
		t.Fatalf("%+v", info)
	}
	if got := FormatNousPortalEntitlementMessage(info, "", false); got != FreeTierNeedsAccount {
		t.Fatal(got)
	}
}
