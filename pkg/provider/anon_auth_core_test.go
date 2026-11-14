package provider

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Removes every AWS credential source so the Bedrock rung of auto detection never answers
func anonNoAWS(t *testing.T) {
	t.Helper()
	dir := t.TempDir()
	for _, name := range []string{"AWS_ACCESS_KEY_ID", "AWS_SECRET_ACCESS_KEY", "AWS_SESSION_TOKEN", "AWS_SECURITY_TOKEN", "AWS_PROFILE",
		"AWS_DEFAULT_PROFILE", "AWS_BEARER_TOKEN_BEDROCK", "AWS_CONTAINER_CREDENTIALS_FULL_URI", "AWS_CONTAINER_CREDENTIALS_RELATIVE_URI",
		"AWS_WEB_IDENTITY_TOKEN_FILE", "AWS_ROLE_ARN"} {
		t.Setenv(name, "")
		os.Unsetenv(name)
	}
	t.Setenv("AWS_SHARED_CREDENTIALS_FILE", filepath.Join(dir, "credentials"))
	t.Setenv("AWS_CONFIG_FILE", filepath.Join(dir, "config"))
	t.Setenv("AWS_EC2_METADATA_DISABLED", "true")
}

// Fails unless resolving the auto provider raises an AuthError
func anonResolveRaises(t *testing.T, env PoolEnv) {
	t.Helper()
	pid, err := resolveProvider(env, "auto")
	var ae *AuthError
	if !errors.As(err, &ae) {
		t.Fatalf("resolve_provider(auto) must raise AuthError, got %q %v", pid, err)
	}
}

func TestAnonConnectorGuestMintsOnceAndNeverCarriesInference(t *testing.T) {
	portal, env := installAnonPortal(t)
	anonNoAWS(t)
	state, err := EnsurePortalIdentity(env, PortalIdentityOptions{Explicit: true})
	if err != nil || !isGuestState(state) {
		t.Fatalf("%v %v", state, err)
	}
	if _, ok := state["refresh_token"]; ok || pyTruthy(state[WelcomeClaimFlag]) {
		t.Fatalf("connector guest shape %v", state)
	}
	store, err := env.Store.Load()
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := store["active_provider"]; ok {
		t.Fatalf("active_provider written %v", store["active_provider"])
	}
	anonResolveRaises(t, env)
	if !isGuestState(anonProviders(t, env)["nous"].(map[string]any)) {
		t.Fatal("guest must stay stored")
	}
	if anonSharedStore(t, env)["anon_token"] != state["anon_token"] || portal.minted != 1 {
		t.Fatalf("shared %v minted %d", anonSharedStore(t, env), portal.minted)
	}
	before := len(portal.paths())
	again, err := EnsurePortalIdentity(env, PortalIdentityOptions{Explicit: true})
	if err != nil || again["anon_token"] != state["anon_token"] || len(portal.paths()) != before {
		t.Fatalf("second call %v %v %v", again, err, portal.paths())
	}
}

func TestAnonOptOutBoolDisablesEverything(t *testing.T) {
	portal, env := installAnonPortal(t)
	anonWriteNousConfig(t, env, "  guest: false\n")
	anonNoAWS(t)
	state, err := EnsurePortalIdentity(env, PortalIdentityOptions{Explicit: true})
	if state != nil || err != nil || len(portal.paths()) != 0 {
		t.Fatalf("%v %v %v", state, err, portal.paths())
	}
	anonResolveRaises(t, env)
}

func TestAnonAClaimedTrialCarriesInferenceOnEverySurface(t *testing.T) {
	_, env := installAnonPortal(t)
	anonClaimedTrial(t, env)
	if !freeInferenceActive(env, currentNousState(env)) {
		t.Fatal("a claimed trial carries inference off the desktop")
	}
	if pid, err := resolveProvider(env, "auto"); pid != "nous" || err != nil {
		t.Fatalf("%q %v", pid, err)
	}
}

func TestAnonProvisionMintsOnceThenEverythingAdopts(t *testing.T) {
	portal, env := installAnonPortal(t)
	if !isGuestState(anonClaimedTrial(t, env)) || portal.minted != 1 {
		t.Fatalf("minted %d", portal.minted)
	}
	token := anonProviders(t, env)["nous"].(map[string]any)["anon_token"]
	if _, err := EnsurePortalIdentity(env, PortalIdentityOptions{Explicit: true}); err != nil {
		t.Fatal(err)
	}
	rt, err := authResolver(env).ResolveRuntime("nous", "", "", nil, GuestModel)
	if err != nil || strings.TrimRight(rt.BaseURL, "/") != anonWelcome {
		t.Fatalf("runtime %+v %v", rt, err)
	}
	if pid, err := resolveProvider(env, "auto"); pid != "nous" || err != nil {
		t.Fatalf("%q %v", pid, err)
	}
	sibling := filepath.Join(t.TempDir(), "sibling-profile")
	if err := os.MkdirAll(sibling, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("ICEBATH_HOME", sibling)
	adopted, err := EnsurePortalIdentity(modelsPoolEnv(context.Background()), PortalIdentityOptions{Explicit: true})
	if err != nil || adopted == nil || adopted["anon_token"] != token || portal.minted != 1 {
		t.Fatalf("adopted %v %v minted %d", adopted, err, portal.minted)
	}
}

func TestAnonGuestIsLastResortAndExplicitKeyWins(t *testing.T) {
	_, env := installAnonPortal(t)
	anonClaimedTrial(t, env)
	if pid, err := resolveProvider(env, "auto"); pid != "nous" || err != nil {
		t.Fatalf("%q %v", pid, err)
	}
	t.Setenv("OPENROUTER_API_KEY", "sk-or-test")
	if pid, err := resolveProvider(env, "auto"); pid != "openrouter" || err != nil {
		t.Fatalf("%q %v", pid, err)
	}
}

func TestAnonRuntimeRoutesToWelcomeHost(t *testing.T) {
	_, env := installAnonPortal(t)
	anonClaimedTrial(t, env)
	rt, err := authResolver(env).ResolveRuntime("", "", "", nil, "")
	if err != nil || rt.Provider != "nous" || strings.TrimRight(rt.BaseURL, "/") != anonWelcome || rt.APIKey == "" {
		t.Fatalf("%+v %v", rt, err)
	}
}

func TestAnonRouteFallbackToWelcomeLiteral(t *testing.T) {
	for _, served := range []string{"", "https://welcome-api.staging-nousresearch.com/v1"} {
		t.Run(served, func(t *testing.T) {
			portal, env := installAnonPortal(t)
			portal.set(func(p *anonFakePortal) { p.inferenceBaseURL = served })
			anonClaimedTrial(t, env)
			rt, err := authResolver(env).ResolveRuntime("", "", "", nil, "")
			if err != nil || strings.TrimRight(rt.BaseURL, "/") != anonWelcome {
				t.Fatalf("%+v %v", rt, err)
			}
			if served != "" {
				return
			}
			state := anonProviders(t, env)["nous"].(map[string]any)
			if strings.TrimRight(anyString(state["inference_base_url"]), "/") != anonWelcome {
				t.Fatalf("stored %v", state["inference_base_url"])
			}
		})
	}
}

func TestAnonGuestStateWithoutURLNeverResolvesToThePaidHost(t *testing.T) {
	_, env := installAnonPortal(t)
	_, stored, effective, _ := nousEffectiveRouting(env, map[string]any{"auth_method": "anonymous", "anon_token": "anon_x"})
	if strings.TrimRight(stored, "/") != anonWelcome || strings.TrimRight(effective, "/") != anonWelcome {
		t.Fatalf("%q %q", stored, effective)
	}
	_, stored, _, _ = nousEffectiveRouting(env, map[string]any{"refresh_token": "r"})
	if strings.TrimRight(stored, "/") != "https://inference-api.nousresearch.com/v1" {
		t.Fatalf("%q", stored)
	}
	shape := nousSharedShape(map[string]any{"auth_method": "anonymous", "anon_token": "anon_x"})
	if strings.TrimRight(anyString(shape["inference_base_url"]), "/") != anonWelcome {
		t.Fatalf("shape %v", shape)
	}
}

func TestAnonPinKeysOnTheWelcomeHostNotOnGuestState(t *testing.T) {
	_, env := installAnonPortal(t)
	if _, err := EnsurePortalIdentity(env, PortalIdentityOptions{Explicit: true}); err != nil {
		t.Fatal(err)
	}
	if !RouteIsWelcomeHost(anonWelcome) || RouteIsWelcomeHost("https://inference-api.nousresearch.com/v1") || RouteIsWelcomeHost("") {
		t.Fatal("the pin keys on the welcome host only")
	}
}

func TestAnonLogoutWithOnlyFreeTierIsATrueNoop(t *testing.T) {
	_, env := installAnonPortal(t)
	if _, err := EnsurePortalIdentity(env, PortalIdentityOptions{Explicit: true}); err != nil {
		t.Fatal(err)
	}
	authPath := filepath.Join(env.Store.Home(), "auth.json")
	before, err := os.ReadFile(authPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := LogoutCommand(env, "", io.Discard); err != nil {
		t.Fatal(err)
	}
	if after, _ := os.ReadFile(authPath); !bytes.Equal(before, after) {
		t.Fatalf("auth.json changed\n%s\n%s", before, after)
	}
}

func TestAnonLogoutOfRealAccountClearsSharedStore(t *testing.T) {
	_, env := installAnonPortal(t)
	creds := map[string]any{"access_token": anonJWT(map[string]any{"client_id": DefaultNousClientID, "account_tier": "free"}),
		"refresh_token": "rt-1", "expires_at": "2030-01-01T00:00:00+00:00", "auth_method": "oauth_device_code"}
	if _, _, err := persistNousCredentials(env, creds, ""); err != nil {
		t.Fatal(err)
	}
	if anonSharedStore(t, env)["refresh_token"] != "rt-1" {
		t.Fatalf("shared %v", anonSharedStore(t, env))
	}
	if err := LogoutCommand(env, "nous", io.Discard); err != nil {
		t.Fatal(err)
	}
	if shared := anonSharedStore(t, env); len(shared) != 0 {
		t.Fatalf("shared %v", shared)
	}
	if _, ok := anonProviders(t, env)["nous"]; ok {
		t.Fatal("nous provider state kept")
	}
}

func TestAnonSwitchingAwayFromWelcomeNamesTheAccountPathNotAnotherProvider(t *testing.T) {
	_, env := installAnonPortal(t)
	if _, err := EnsurePortalIdentity(env, PortalIdentityOptions{Explicit: true}); err != nil {
		t.Fatal(err)
	}
	saved := listProviderModelsFn
	listProviderModelsFn = func(context.Context, string) []string { return nil }
	t.Cleanup(func() { listProviderModelsFn = saved })
	result, err := SwitchModel(env.ctx(), SwitchModelOptions{RawInput: "gpt-5", CurrentProvider: "nous", CurrentBaseURL: anonWelcome})
	if err != nil || result.Success {
		t.Fatalf("%+v %v", result, err)
	}
}

func TestAnonPaidConversationRefusesAWelcomeRouteOnEveryWireMode(t *testing.T) {
	installAnonPortal(t)
	for _, tc := range []struct {
		mode  APIMode
		model string
	}{{ModeChatCompletions, "nous/paid-model"}, {ModeAnthropicMessages, "anthropic/claude-sonnet"}} {
		route := Route{Provider: "nous", Mode: tc.mode, BaseURL: "https://inference-api.nousresearch.com/v1", APIKey: "k", Model: tc.model}
		next, ok := SwapCredentialRoute(route, PooledCredential{ID: "g", Provider: "nous", AccessToken: "jwt", InferenceBaseURL: anonWelcome})
		if ok || next.Model != tc.model || next.APIKey != "k" || next.BaseURL != route.BaseURL {
			t.Fatalf("%s: %v %+v", tc.mode, ok, next)
		}
	}
}

func TestAnonWelcomeConversationMayMoveToThePortalHost(t *testing.T) {
	installAnonPortal(t)
	route := Route{Provider: "nous", Mode: ModeChatCompletions, BaseURL: anonWelcome, APIKey: "k", Model: GuestModel}
	next, ok := SwapCredentialRoute(route, PooledCredential{ID: "p2", Provider: "nous", AccessToken: "key", InferenceBaseURL: "https://inference-api.nousresearch.com/v1"})
	if !ok || next.Model != GuestModel {
		t.Fatalf("%v %+v", ok, next)
	}
}

func TestAnonLockOrderIsProfileThenShared(t *testing.T) {
	_, env := installAnonPortal(t)
	unlockShared, err := nousSharedStoreLock(env, AuthLockTimeout)
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		_, err := EnsurePortalIdentity(env, PortalIdentityOptions{Explicit: true})
		done <- err
	}()
	profileHeldWhileWaiting := false
	deadline := time.Now().Add(10 * time.Second)
	var pending chan struct{}
	for time.Now().Before(deadline) && !profileHeldWhileWaiting {
		select {
		case err := <-done:
			unlockShared()
			t.Fatalf("identity provisioned without waiting on the shared lock: %v", err)
		default:
		}
		probe := make(chan struct{})
		go func() {
			if unlock, err := env.Store.Lock(AuthLockTimeout); err == nil {
				unlock()
			}
			close(probe)
		}()
		select {
		case <-probe:
			time.Sleep(10 * time.Millisecond)
		case <-time.After(300 * time.Millisecond):
			profileHeldWhileWaiting = true
			pending = probe
		}
	}
	unlockShared()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if !profileHeldWhileWaiting {
		t.Fatal("the profile lock was never held while the shared lock was awaited")
	}
	<-pending
}

func TestAnonClaimedTrialHasTheNoticePendingOnceAndTheFlagPersists(t *testing.T) {
	_, env := installAnonPortal(t)
	anonClaimedTrial(t, env)
	if !guestNoticePending(env) {
		t.Fatal("notice must be pending")
	}
	if marked, err := markGuestNoticeShown(env); !marked || err != nil {
		t.Fatalf("%v %v", marked, err)
	}
	if anonProviders(t, env)["nous"].(map[string]any)[GuestNoticeFlag] != true || guestNoticePending(env) {
		t.Fatal("flag must persist and the notice stay shown")
	}
}

func TestAnonConnectorOnlyGuestHasNoNoticePending(t *testing.T) {
	_, env := installAnonPortal(t)
	desktop := anonDesktopEnv(env)
	if _, err := EnsurePortalIdentity(desktop, PortalIdentityOptions{Explicit: true}); err != nil {
		t.Fatal(err)
	}
	if !hasGuest(desktop) || guestNoticePending(desktop) {
		t.Fatal("a connector only guest has no notice")
	}
}

func TestAnonSignedInAccountIsNeverToldAboutTheFreeTier(t *testing.T) {
	_, env := installAnonPortal(t)
	err := env.Store.Update(func(data AuthData) (bool, error) {
		section(data, "providers")["nous"] = map[string]any{"auth_method": "oauth", "refresh_token": "rt", "expires_at": "2999-01-01T00:00:00+00:00",
			"access_token": anonJWT(map[string]any{"client_id": DefaultNousClientID, "account_tier": "pro"})}
		return true, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if guestNoticePending(env) {
		t.Fatal("no notice for a signed in account")
	}
	if marked, err := markGuestNoticeShown(env); marked || err != nil {
		t.Fatalf("%v %v", marked, err)
	}
	if _, ok := anonProviders(t, env)["nous"].(map[string]any)[GuestNoticeFlag]; ok {
		t.Fatal("flag written for a signed in account")
	}
}

func TestFreeTierBootstrapOnAFreshMachineMintsNothing(t *testing.T) {
	portal, env := installAnonPortal(t)
	anonNoAWS(t)
	resetFreeTierBootstrapForTests()
	t.Cleanup(resetFreeTierBootstrapForTests)
	record := RunFreeTierBootstrap(anonDesktopEnv(env), false)
	if record.HasIdentity || record.FreeTierAccount {
		t.Fatalf("%+v", record)
	}
	if len(portal.paths()) != 0 {
		t.Fatalf("boot never reaches the portal: %v", portal.paths())
	}
}

func TestFreeTierBootstrapRecordsAClaimedTrialAndASecondRunIsFree(t *testing.T) {
	portal, env := installAnonPortal(t)
	anonClaimedTrial(t, env)
	resetFreeTierBootstrapForTests()
	t.Cleanup(resetFreeTierBootstrapForTests)
	record := RunFreeTierBootstrap(env, false)
	if !record.FreeTierAccount || !record.HasIdentity || !record.ProviderConfigured || record.InferenceProvider != "nous" || record.OtherProviders {
		t.Fatalf("%+v", record)
	}
	if again := RunFreeTierBootstrap(env, false); again != record || portal.minted != 1 {
		t.Fatalf("second run %p %p minted %d", again, record, portal.minted)
	}
	if WaitForSetupRecord(env, 0) != record {
		t.Fatal("wait_for_record(timeout=0) returns the record")
	}
}

func TestFreeTierBootstrapOwnKeyKeepsInferenceAndAConnectorGuestStaysOffActiveProvider(t *testing.T) {
	portal, env := installAnonPortal(t)
	if _, err := EnsurePortalIdentity(env, PortalIdentityOptions{Explicit: true}); err != nil {
		t.Fatal(err)
	}
	t.Setenv("OPENROUTER_API_KEY", "sk-or-own-key")
	resetFreeTierBootstrapForTests()
	t.Cleanup(resetFreeTierBootstrapForTests)
	record := RunFreeTierBootstrap(env, false)
	if !record.OtherProviders || !record.HasIdentity || record.FreeTierAccount || record.InferenceProvider != "openrouter" {
		t.Fatalf("%+v", record)
	}
	store, err := env.Store.Load()
	if err != nil || store["active_provider"] == "nous" || portal.minted != 1 {
		t.Fatalf("active %v minted %d %v", store["active_provider"], portal.minted, err)
	}
}

func TestFreeTierReadsNeverMint(t *testing.T) {
	portal, env := installAnonPortal(t)
	anonNoAWS(t)
	if token, err := ResolveNousAccessToken(env, NousAccessTokenOptions{}); token != "" {
		t.Fatalf("token read minted %q %v", token, err)
	}
	anonResolveRaises(t, env)
	if hasGuest(env) || len(portal.paths()) != 0 {
		t.Fatalf("no read path may reach the portal: %v", portal.paths())
	}
	if _, err := EnsurePortalIdentity(env, PortalIdentityOptions{}); err == nil {
		t.Fatal("an implicit creator must be refused")
	}
}
