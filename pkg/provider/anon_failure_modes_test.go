package provider

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/tools"
)

// A forced first mint expected to fail, returning its coded error
func anonMintError(t *testing.T, env PoolEnv) *AuthError {
	t.Helper()
	state, err := EnsurePortalIdentity(env, PortalIdentityOptions{Explicit: true})
	if err == nil {
		t.Fatalf("mint succeeded: %v", state)
	}
	return anonCodeOf(t, err)
}

// Claims the trial then exchanges it at first use, returning the exchange error
func anonExchangeError(t *testing.T, env PoolEnv) error {
	t.Helper()
	creds, err := anonResolve(t, env)
	if err == nil {
		t.Fatalf("exchange succeeded: %v", creds)
	}
	return err
}

func anonRestoreMonotonic(t *testing.T) {
	prev := anonMonotonic
	t.Cleanup(func() { anonMonotonic = prev })
}

func TestAnonFailureSurfaceNotEnabledIsATerminalGate(t *testing.T) {
	portal, env := installAnonPortal(t)
	portal.set(func(p *anonFakePortal) {
		p.createResponse = &anonCanned{status: 404, body: map[string]any{"error": "not_found"}}
	})
	ae := anonMintError(t, env)
	if ae.Code != AnonGateClosed || ae.Retryable == nil || *ae.Retryable {
		t.Fatalf("%+v", ae)
	}
	if !strings.Contains(ae.Error(), "Nous account") || !strings.Contains(ae.Error(), "free") {
		t.Fatal(ae.Error())
	}
	if state, err := EnsurePortalIdentity(env, PortalIdentityOptions{Explicit: true}); state != nil || err != nil {
		t.Fatalf("terminal gate retried: %v %v", state, err)
	}
	if portal.count("/api/anonymous/create") != 1 {
		t.Fatal(portal.paths())
	}
	want := map[string]any{"error_code": AnonGateClosed, "error": ae.Error(), "retryable": false, "retry_after": 0}
	if got := lastMintFailure(env); !reflect.DeepEqual(got, want) {
		t.Fatalf("%v", got)
	}
}

func TestAnonFailureOpsBreakerIsARetryablePauseWithAFloor(t *testing.T) {
	portal, env := installAnonPortal(t)
	portal.set(func(p *anonFakePortal) {
		p.createResponse = &anonCanned{status: 503, body: map[string]any{"error": "temporarily_disabled", "error_description": "switched off"}}
	})
	if ae := anonMintError(t, env); ae.Code != AnonGatePaused {
		t.Fatal(ae.Code)
	}
	failure := lastMintFailure(env)
	if failure["retryable"] != true || failure["retry_after"].(int) < 59 {
		t.Fatalf("%v", failure)
	}
}

func TestAnonFailureTooManySignUpsHonoursRetryAfter(t *testing.T) {
	portal, env := installAnonPortal(t)
	portal.set(func(p *anonFakePortal) {
		p.createResponse = &anonCanned{status: 429, body: map[string]any{"error": "temporarily_unavailable"}, headers: map[string]string{"Retry-After": "42"}}
	})
	ae := anonMintError(t, env)
	if ae.Code != AnonRateLimited || !ae.HasRetryAfter || ae.RetryAfter != 42*time.Second || !strings.Contains(ae.Error(), "about a minute") {
		t.Fatalf("%+v", ae)
	}
	if got := lastMintFailure(env)["retry_after"]; got != 42 {
		t.Fatalf("%v", got)
	}
}

func TestAnonFailureProofOfWorkIsDeferredWithTheAgreedSentence(t *testing.T) {
	portal, env := installAnonPortal(t)
	portal.set(func(p *anonFakePortal) {
		p.tokenResponse = &anonCanned{status: 428, body: map[string]any{"error": "pow_required", "pow": map[string]any{"bits": 31}}}
	})
	ae := anonCodeOf(t, anonExchangeError(t, env))
	if ae.Code != AnonPowRequired || ae.Retryable == nil || *ae.Retryable || !strings.Contains(ae.Error(), "proof of work") {
		t.Fatalf("%+v", ae)
	}
	nous, _ := anonProviders(t, env)["nous"].(map[string]any)
	if !isGuestState(nous) || portal.count("/api/anonymous/create") != 1 {
		t.Fatalf("%v %v", nous, portal.paths())
	}
}

func TestAnonFailureLockedAccountIsDeadAndNeverReplaced(t *testing.T) {
	portal, env := installAnonPortal(t)
	portal.set(func(p *anonFakePortal) {
		p.tokenResponse = &anonCanned{status: 403, body: map[string]any{"error": "account_locked"}}
	})
	err := anonExchangeError(t, env)
	var dead *AnonCredentialDeadError
	if !errors.As(err, &dead) || dead.Auth.Code != AnonAccountLocked {
		t.Fatalf("%T %v", err, err)
	}
	if _, ok := anonProviders(t, env)["nous"]; ok || portal.count("/api/anonymous/create") != 1 {
		t.Fatalf("%v %v", anonProviders(t, env), portal.paths())
	}
}

func TestAnonFailureLockedAccountIsNeverReplacedThroughConnectorsEither(t *testing.T) {
	portal, env := installAnonPortal(t)
	if _, err := EnsurePortalIdentity(env, PortalIdentityOptions{Explicit: true}); err != nil {
		t.Fatal(err)
	}
	anonPatchNous(t, env, map[string]any{"expires_at": "2000-01-01T00:00:00+00:00", "access_token": anonJWT(map[string]any{"exp": 1})})
	portal.set(func(p *anonFakePortal) {
		p.tokenResponse = &anonCanned{status: 403, body: map[string]any{"error": "account_locked"}}
	})
	if token, err := ReadNousAccessToken(env); err != nil || token != "" {
		t.Fatalf("%q %v", token, err)
	}
	if _, ok := anonProviders(t, env)["nous"]; ok || portal.count("/api/anonymous/create") != 1 {
		t.Fatalf("%v %v", anonProviders(t, env), portal.paths())
	}
}

func TestAnonFailureA5xxOrNonJSONBodyIsARetryableServerError(t *testing.T) {
	portal, env := installAnonPortal(t)
	portal.set(func(p *anonFakePortal) { p.createResponse = &anonCanned{status: 500, text: "<html>oops</html>"} })
	ae := anonMintError(t, env)
	if ae.Code != AnonServerError || (ae.Retryable != nil && !*ae.Retryable) || !strings.Contains(ae.Error(), "hiccup") {
		t.Fatalf("%+v", ae)
	}
}

func TestAnonFailureABare401OnSignUpRidesTheLadder(t *testing.T) {
	portal, env := installAnonPortal(t)
	portal.set(func(p *anonFakePortal) { p.createResponse = &anonCanned{status: 401, body: map[string]any{}} })
	if ae := anonMintError(t, env); ae.Code != AnonCredentialDead {
		t.Fatal(ae.Code)
	}
	if lastMintFailure(env)["retryable"] != true {
		t.Fatal(lastMintFailure(env))
	}
}

func TestAnonFailureTheWireFailingIsUnreachable(t *testing.T) {
	portal, env := installAnonPortal(t)
	portal.set(func(p *anonFakePortal) { p.raiseTransport = true })
	ae := anonMintError(t, env)
	var urlErr *url.Error
	var netErr net.Error
	if ae.Code != AnonUnreachable || !(errors.As(ae.Err, &urlErr) || errors.As(ae.Err, &netErr)) || !strings.Contains(ae.Error(), "internet connection") {
		t.Fatalf("%+v cause %T", ae, ae.Err)
	}
}

func TestAnonMintCooldownTransientFailureIsRetriedAfterItsCooldown(t *testing.T) {
	portal, env := installAnonPortal(t)
	anonRestoreMonotonic(t)
	portal.set(func(p *anonFakePortal) { p.raiseTransport = true })
	anonMintError(t, env)
	if state, err := EnsurePortalIdentity(env, PortalIdentityOptions{Explicit: true}); state != nil || err != nil {
		t.Fatalf("inside the cooldown: %v %v", state, err)
	}
	if portal.count("/api/anonymous/create") != 1 {
		t.Fatal(portal.paths())
	}
	failure := mintFailureForProfile(env)
	if failure.RetryAfter != mintRetryLadder[0] {
		t.Fatal(failure.RetryAfter)
	}
	anonMonotonic = func() time.Time { return failure.NotBefore.Add(time.Second) }
	portal.set(func(p *anonFakePortal) { p.raiseTransport = false })
	state, err := EnsurePortalIdentity(env, PortalIdentityOptions{Explicit: true})
	if err != nil || !isGuestState(state) || portal.count("/api/anonymous/create") != 2 || lastMintFailure(env) != nil {
		t.Fatalf("%v %v %v", state, err, lastMintFailure(env))
	}
}

func TestAnonMintCooldownRepeatedTransientFailuresClimbTheLadder(t *testing.T) {
	portal, env := installAnonPortal(t)
	anonRestoreMonotonic(t)
	portal.set(func(p *anonFakePortal) { p.raiseTransport = true })
	var waits []time.Duration
	for range 4 {
		anonMintError(t, env)
		failure := mintFailureForProfile(env)
		waits = append(waits, failure.RetryAfter)
		anonMonotonic = func() time.Time { return failure.NotBefore.Add(time.Second) }
	}
	last := mintRetryLadder[len(mintRetryLadder)-1]
	want := append(append([]time.Duration{}, mintRetryLadder...), last, last, last, last)[:4]
	if !reflect.DeepEqual(waits, want) {
		t.Fatalf("%v want %v", waits, want)
	}
}

func TestAnonMintCooldownTheUsersOwnRetryBypassesTheCooldownOnce(t *testing.T) {
	portal, env := installAnonPortal(t)
	portal.set(func(p *anonFakePortal) { p.raiseTransport = true })
	anonMintError(t, env)
	portal.set(func(p *anonFakePortal) { p.raiseTransport = false })
	if state, err := EnsurePortalIdentity(env, PortalIdentityOptions{Explicit: true}); state != nil || err != nil {
		t.Fatalf("%v %v", state, err)
	}
	state, err := EnsurePortalIdentity(env, PortalIdentityOptions{Explicit: true, Force: true})
	if err != nil || !isGuestState(state) || portal.count("/api/anonymous/create") != 2 {
		t.Fatalf("%v %v %v", state, err, portal.paths())
	}
}

func TestAnonMintCooldownATerminalCodeMakesOneForcedAttemptOnly(t *testing.T) {
	portal, env := installAnonPortal(t)
	portal.set(func(p *anonFakePortal) {
		p.createResponse = &anonCanned{status: 404, body: map[string]any{"error": "not_found"}}
	})
	anonMintError(t, env)
	_, err := EnsurePortalIdentity(env, PortalIdentityOptions{Explicit: true, Force: true})
	anonCodeOf(t, err)
	if portal.count("/api/anonymous/create") != 2 {
		t.Fatal(portal.paths())
	}
	if state, err := EnsurePortalIdentity(env, PortalIdentityOptions{Explicit: true}); state != nil || err != nil {
		t.Fatalf("%v %v", state, err)
	}
}

// Isolates the bootstrap record and install probe, returning the desktop backend env
func anonBootstrapEnv(t *testing.T, env PoolEnv) PoolEnv {
	t.Helper()
	tools.InstallStateProbeClear()
	resetFreeTierBootstrapForTests()
	prev, probe, clear, mark := setupInventoryFn, welcomeInstallProbe, welcomeProbeClear, welcomeMarkUsed
	used := false
	welcomeInstallProbe = func(_ context.Context) tools.InstallState {
		if used {
			return tools.InstallState{Findings: []tools.InstallFinding{{}}}
		}
		return tools.InstallState{}
	}
	welcomeProbeClear = func() {}
	welcomeMarkUsed = func(_ context.Context) error {
		used = true
		return nil
	}
	t.Cleanup(func() {
		setupInventoryFn, welcomeInstallProbe, welcomeProbeClear, welcomeMarkUsed = prev, probe, clear, mark
		tools.InstallStateProbeClear()
		resetFreeTierBootstrapForTests()
	})
	return anonDesktopEnv(env)
}

// The desktop's welcome trial claim refused by the service
func anonFailedClaim(t *testing.T, env PoolEnv) *AuthError {
	t.Helper()
	state, err := ClaimDesktopFreeTier(env, false, false)
	if err == nil {
		t.Fatalf("claim succeeded: %v", state)
	}
	return anonCodeOf(t, err)
}

func TestAnonBootstrapRecordCarriesTheFailedClaimsCodeAndWait(t *testing.T) {
	portal, env := installAnonPortal(t)
	denv := anonBootstrapEnv(t, env)
	portal.set(func(p *anonFakePortal) {
		p.createResponse = &anonCanned{status: 429, body: map[string]any{"error": "temporarily_unavailable"}, headers: map[string]string{"Retry-After": "30"}}
	})
	anonFailedClaim(t, denv)
	record := RunFreeTierBootstrap(denv, false)
	if record.HasIdentity || record.FreeTierAccount || record.Failure["error_code"] != AnonRateLimited || record.Failure["retryable"] != true {
		t.Fatalf("%+v", record)
	}
	if wait, _ := record.Failure["retry_after"].(int); wait < 28 || wait > 30 {
		t.Fatalf("%v", record.Failure)
	}
	want := map[string]any{"error": record.Error, "error_code": AnonRateLimited, "retryable": true, "retry_after": record.Failure["retry_after"]}
	if got := record.FailureFields(); !reflect.DeepEqual(got, want) {
		t.Fatalf("%v want %v", got, want)
	}
	if portal.count("/api/anonymous/create") != 1 {
		t.Fatal("the boot record never retries the mint")
	}
}

func TestAnonBootstrapRecordAClaimedTrialCarriesNoFailureBlock(t *testing.T) {
	_, env := installAnonPortal(t)
	denv := anonBootstrapEnv(t, env)
	anonClaimedTrial(t, env)
	if record := RunFreeTierBootstrap(denv, false); !record.FreeTierAccount || len(record.FailureFields()) != 0 {
		t.Fatalf("%+v", record)
	}
}

func TestAnonBootstrapRecordARefreshReInventoriesAProviderConnectedMeanwhile(t *testing.T) {
	portal, env := installAnonPortal(t)
	denv := anonBootstrapEnv(t, env)
	portal.set(func(p *anonFakePortal) { p.raiseTransport = true })
	anonFailedClaim(t, denv)
	RunFreeTierBootstrap(denv, false)
	setupInventoryFn = func(PoolEnv) bool { return true }
	record := RefreshSetupRecord(denv, false)
	if !record.OtherProviders || !record.ProviderConfigured {
		t.Fatalf("%+v", record)
	}
	data, err := env.Store.Load()
	if err != nil || data["active_provider"] == "nous" {
		t.Fatalf("%v %v", data, err)
	}
}

func TestAnonBootstrapRecordTheDesktopRetryRefreshesTheBootRecord(t *testing.T) {
	portal, env := installAnonPortal(t)
	denv := anonBootstrapEnv(t, env)
	portal.set(func(p *anonFakePortal) { p.raiseTransport = true })
	anonFailedClaim(t, denv)
	RunFreeTierBootstrap(denv, false)
	portal.set(func(p *anonFakePortal) { p.raiseTransport = false })
	if state, err := ClaimDesktopFreeTier(denv, false, true); err != nil || state == nil {
		t.Fatalf("retry: %v %v", state, err)
	}
	record := RefreshSetupRecord(denv, false)
	if !record.FreeTierAccount || len(record.FailureFields()) != 0 || CurrentSetupRecord() != record {
		t.Fatalf("%+v", record)
	}
}

func TestAnonSignInFailureAServiceVerdictKeepsItsCodeWaitAndCopy(t *testing.T) {
	cases := []struct {
		code       string
		retryAfter int
		retryable  bool
		needle     string
	}{
		{AnonRateLimited, 45, true, "busy"},
		{AnonGatePaused, 0, true, "busy"},
		{AnonUnreachable, 0, true, "internet connection"},
		{AnonGateClosed, 0, false, "Nous account"},
		{AnonPowRequired, 0, false, "proof of work"},
	}
	for _, c := range cases {
		state := signInFailedFromError(anonErr("x", c.code, time.Duration(c.retryAfter)*time.Second, c.retryAfter != 0))
		if state.Kind() != "failed" || state.Reason != c.code || state.Retryable() != c.retryable || state.RetryAfter != float64(c.retryAfter) {
			t.Fatalf("%s: %+v", c.code, state)
		}
		copy := state.Copy()
		if !strings.Contains(copy, c.needle) || strings.Contains(copy, "Hermes ") || strings.Contains(copy, "icebath ") || strings.Contains(copy, "http") {
			t.Fatalf("%s: %q", c.code, copy)
		}
	}
}

func TestAnonSignInFailureTheWireFailingReadsAsUnreachable(t *testing.T) {
	state := signInFailedFromError(&url.Error{Op: "Post", URL: "https://portal.invalid", Err: os.ErrDeadlineExceeded})
	if state.Reason != AnonUnreachable || !state.Retryable() {
		t.Fatalf("%+v", state)
	}
}

func TestAnonSignInFailureAnUnnamedLocalFailureKeepsTheGenericCopy(t *testing.T) {
	state := signInFailedFromError(errors.New("bad CA bundle"))
	if state.Reason != "" || state.Copy() != UpgradeNotCompleted || !strings.Contains(state.CopyTerminal(), "bad CA bundle") {
		t.Fatalf("%+v %q", state, state.CopyTerminal())
	}
}

func TestAnonExtraWelcomeHostsMakeALocalStandInTheWelcomeHost(t *testing.T) {
	t.Setenv(ExtraWelcomeHostsEnv, "")
	os.Unsetenv(ExtraWelcomeHostsEnv)
	if RouteIsWelcomeHost("http://127.0.0.1:8765/v1") {
		t.Fatal("loopback is not the welcome host by default")
	}
	t.Setenv(ExtraWelcomeHostsEnv, "127.0.0.1, localhost")
	if !RouteIsWelcomeHost("http://127.0.0.1:8765/v1") || !RouteIsWelcomeHost("http://localhost:9/v1") {
		t.Fatal("extra hosts ignored")
	}
	if got := WelcomeRouteRefusal(403, "You tried to access something", "http://127.0.0.1:8765/v1"); got != "tier_disabled" {
		t.Fatal(got)
	}
	if RouteIsWelcomeHost("https://inference-api.nousresearch.com/v1") {
		t.Fatal("the paid host is never the welcome host")
	}
}

func TestAnonRetryAfterIsTheWholeWaitNotTheWaitPlusFloatDust(t *testing.T) {
	anonRestoreMonotonic(t)
	now := time.Unix(4080, 785514275)
	anonMonotonic = func() time.Time { return now }
	failure := &MintFailure{Code: AnonServerError, Message: "x", Retryable: true, RetryAfter: 60 * time.Second, NotBefore: now.Add(60 * time.Second)}
	if got := failure.AsPayload()["retry_after"]; got != 60 {
		t.Fatal(got)
	}
}

var closureSurfaceForbidden = regexp.MustCompile(`(?i)guest|anonymous|user id|org id|nas_user|nas_organisation`)

func closureSurfaceJWT(claims map[string]any) string {
	payload := map[string]any{"sub": "nas_user:abc", "client_id": "nas-anonymous", "account_tier": "anonymous",
		"scope": "inference:invoke", "exp": time.Now().Unix() + 100_000_000}
	for k, v := range claims {
		payload[k] = v
	}
	return jwtWithClaims(payload)
}

func closureGuestState() map[string]any {
	return map[string]any{"auth_method": "anonymous", "account_tier": "anonymous", "anon_token": "anon_t",
		"access_token": closureSurfaceJWT(nil), "expires_at": "2030-01-01T00:00:00+00:00", "inference_base_url": anonWelcome,
		"user_id": "nas_user:abc", "org_id": "nas_organisation:def", WelcomeClaimFlag: true}
}

func closureAccountState() map[string]any {
	return map[string]any{"auth_method": "oauth_device_code", "client_id": "icebath-cli",
		"access_token":  closureSurfaceJWT(map[string]any{"sub": "nas_user:real", "client_id": "icebath-cli", "account_tier": "standard", "paid_access": true}),
		"refresh_token": "rt_live", "expires_at": "2030-01-01T00:00:00+00:00", "portal_base_url": "https://portal.nousresearch.com",
		"inference_base_url": "https://inference-api.nousresearch.com/v1"}
}

// Isolated profile and shared stores on the desktop backend where any portal fetch fails the test
func closureIsolatedStore(t *testing.T) PoolEnv {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("ICEBATH_HOME", filepath.Join(dir, "home"))
	t.Setenv("ICEBATH_SHARED_AUTH_DIR", filepath.Join(dir, "shared-store"))
	for _, name := range []string{"OPENROUTER_API_KEY", "OPENAI_API_KEY", "ANTHROPIC_API_KEY", "NOUS_API_KEY"} {
		t.Setenv(name, "")
		os.Unsetenv(name)
	}
	prev := fetchNousAccountInfoFn
	fetchNousAccountInfoFn = func(PoolEnv, string, string) (map[string]any, error) {
		t.Error("portal fetch must not happen on a read-only surface")
		return nil, errors.New("portal fetch")
	}
	ResetNousPortalAccountInfoCache()
	t.Cleanup(func() {
		fetchNousAccountInfoFn = prev
		ResetNousPortalAccountInfoCache()
	})
	return anonDesktopEnv(modelsPoolEnv(context.Background()))
}

func closureWriteAuth(t *testing.T, env PoolEnv, nous map[string]any) {
	t.Helper()
	raw, _ := json.Marshal(map[string]any{"active_provider": "nous", "providers": map[string]any{"nous": nous}})
	if err := os.MkdirAll(env.Store.Home(), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(env.Store.Home(), "auth.json"), raw, 0o600); err != nil {
		t.Fatal(err)
	}
	ResetNousPortalAccountInfoCache()
}

func TestAnonSurfacesFreeTierEntitlementNamesTheAccountPath(t *testing.T) {
	env := closureIsolatedStore(t)
	closureWriteAuth(t, env, closureGuestState())
	info := GetNousPortalAccountInfo(env.ctx(), false)
	if !info.IsAnonymousTier() {
		t.Fatalf("%+v", info)
	}
	message := FormatNousPortalEntitlementMessage(info, "managed web tools", false)
	if message != FreeTierNeedsAccount || strings.Contains(strings.ToLower(message), "billing") || closureSurfaceForbidden.MatchString(message) {
		t.Fatal(message)
	}
}

func TestAnonSurfacesRealAccountKeepsAccountEntitlement(t *testing.T) {
	env := closureIsolatedStore(t)
	closureWriteAuth(t, env, closureAccountState())
	info := GetNousPortalAccountInfo(env.ctx(), false)
	if info.IsAnonymousTier() || FormatNousPortalEntitlementMessage(info, "", false) != "" {
		t.Fatalf("%+v", info)
	}
}

func TestAnonSurfacesAnonymousTierMatchesCaseAndWhitespaceInsensitively(t *testing.T) {
	for _, tier := range []string{"Anonymous", "ANONYMOUS", " anonymous "} {
		t.Run(tier, func(t *testing.T) {
			env := closureIsolatedStore(t)
			state := closureGuestState()
			state["account_tier"], state["access_token"] = tier, closureSurfaceJWT(map[string]any{"account_tier": tier})
			closureWriteAuth(t, env, state)
			info := GetNousPortalAccountInfo(env.ctx(), false)
			if !info.IsAnonymousTier() || FormatNousPortalEntitlementMessage(info, "", false) != FreeTierNeedsAccount {
				t.Fatalf("%+v", info)
			}
		})
	}
}

func TestAnonSurfacesGuestWhoseLookupFailsStillGetsTheSignInCopy(t *testing.T) {
	env := closureIsolatedStore(t)
	state := closureGuestState()
	state["access_token"] = closureSurfaceJWT(map[string]any{"exp": time.Now().Unix() + 30})
	state["portal_base_url"] = "http://127.0.0.1:9"
	closureWriteAuth(t, env, state)
	fetchNousAccountInfoFn = func(PoolEnv, string, string) (map[string]any, error) { return nil, errors.New("portal down") }
	info := GetNousPortalAccountInfo(env.ctx(), false)
	if info.Error == "" || !info.IsAnonymousTier() || FormatNousPortalEntitlementMessage(info, "", false) != FreeTierNeedsAccount {
		t.Fatalf("%+v", info)
	}
}

func TestAnonSurfacesKeepaliveDoesNotStartForFreeTier(t *testing.T) {
	env := closureIsolatedStore(t)
	var mu sync.Mutex
	var started []string
	prevSpawn := spawnNamedFn
	spawnNamedFn = func(name string, fn func()) {
		mu.Lock()
		started = append(started, name)
		mu.Unlock()
	}
	reset := func() {
		nousKeepalive.mu.Lock()
		nousKeepalive.stop, nousKeepalive.done = nil, nil
		nousKeepalive.mu.Unlock()
		guestHandoffWorkerMu.Lock()
		guestHandoffWorkerStop, guestHandoffWorkerDone = nil, nil
		guestHandoffWorkerMu.Unlock()
	}
	reset()
	t.Cleanup(func() {
		spawnNamedFn = prevSpawn
		reset()
	})
	interval := 900
	closureWriteAuth(t, env, closureGuestState())
	if StartNousAuthKeepalive(env, NousKeepaliveStartOptions{IntervalSeconds: &interval}) {
		t.Fatal("keepalive started for the free tier")
	}
	if !reflect.DeepEqual(started, []string{"nous-guest-handoff"}) {
		t.Fatal(started)
	}
	closureWriteAuth(t, env, closureAccountState())
	if !StartNousAuthKeepalive(env, NousKeepaliveStartOptions{IntervalSeconds: &interval}) {
		t.Fatal("keepalive did not start for an account")
	}
	if !reflect.DeepEqual(started, []string{"nous-guest-handoff", "nous-auth-keepalive"}) {
		t.Fatal(started)
	}
}

func TestAnonSurfacesNoChatCopyOfAnySignInStateLeaksATerminalVerbOrAForbiddenWord(t *testing.T) {
	views := []map[string]any{}
	for _, kind := range []string{"moved", "conflict", "none"} {
		views = append(views, map[string]any{"state": "claimed", "account_email": "person@example.test", "connectors_kind": kind,
			"moved_toolkits": []any{"gmail"}, "dropped_toolkits": []any{"slack"}})
	}
	views = append(views, map[string]any{"state": "failed", "error": "no_destination"}, map[string]any{"state": "gone", "probably_moved": true})
	var handoff []string
	for _, view := range views {
		handoff = append(handoff, guestHandoffResultLines(view)...)
	}
	states := []SignInState{
		SignInCode{Link: "https://example.test/sign-in", Code: "code-1", ExpiresIn: 900, Interval: 1},
		SignInWaiting{},
		SignInCompleted{Email: "person@example.test", Model: "model-1", ModelChanged: true, Handoff: handoff, HandoffStatus: guestHandoffWireStatus(views[0])},
		SignInCancelled{},
		SignInTimedOut{Detail: "private detail"},
		SignInFailed{Reason: "unknown", Detail: "private detail", RetryAfter: 0},
		SignInAlreadySignedIn{},
	}
	forbidden := regexp.MustCompile(`(?i)claim|nous portal|anonymous|guest`)
	terminalOrURL := regexp.MustCompile(`(?i)icebath |https?://`)
	for _, state := range states {
		copy := state.Copy()
		if closureSurfaceForbidden.MatchString(copy) || forbidden.MatchString(copy) || terminalOrURL.MatchString(copy) {
			t.Fatalf("%T: %q", state, copy)
		}
	}
}

func TestAnonSurfacesThePaidToolNoticeSwitchesWordingInsideAChat(t *testing.T) {
	info := &NousPortalAccountInfo{LoggedIn: true, Source: "token", Fresh: true, AccountTier: "anonymous"}
	if FormatNousPortalEntitlementMessage(info, "", true) != FreeTierNeedsAccountChat || FormatNousPortalEntitlementMessage(info, "", false) != FreeTierNeedsAccount {
		t.Fatal("chat wording")
	}
}
