package provider

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/home"
)

func TestAnonSecondProfileUnderSameRootAdoptsFromSharedStore(t *testing.T) {
	portal, env := installAnonPortal(t)
	first, err := EnsurePortalIdentity(env, PortalIdentityOptions{Explicit: true})
	if err != nil || first == nil {
		t.Fatalf("first: %v %v", first, err)
	}
	other := filepath.Join(t.TempDir(), "profiles", "two")
	t.Setenv("ICEBATH_HOME", other)
	second := modelsPoolEnv(context.Background())
	before := len(portal.paths())
	state, err := EnsurePortalIdentity(second, PortalIdentityOptions{Explicit: true})
	if err != nil || state["anon_token"] != first["anon_token"] {
		t.Fatalf("second: %v %v", state, err)
	}
	if len(portal.paths()) != before {
		t.Fatal("adoption must not touch the network")
	}
	if portal.minted != 1 {
		t.Fatalf("minted %d", portal.minted)
	}
}

func TestAnonGateClosedPersistsNothingAndRaisesGateCode(t *testing.T) {
	portal, env := installAnonPortal(t)
	portal.set(func(p *anonFakePortal) { p.gateClosed = true })
	_, err := EnsurePortalIdentity(env, PortalIdentityOptions{Explicit: true})
	if ae := anonCodeOf(t, err); ae.Code != AnonGateClosed {
		t.Fatalf("code %q", ae.Code)
	}
	if _, ok := anonProviders(t, env)["nous"]; ok {
		t.Fatal("nothing may be persisted")
	}
	state, err := EnsurePortalIdentity(env, PortalIdentityOptions{Explicit: true})
	if state != nil || err != nil {
		t.Fatalf("a process tries once: %v %v", state, err)
	}
	if portal.count("/api/anonymous/create") != 1 {
		t.Fatalf("creates %v", portal.paths())
	}
}

func TestAnonGuestOffBeatsAnExplicitProvision(t *testing.T) {
	portal, env := installAnonPortal(t)
	anonWriteNousConfig(t, env, "  guest: false\n")
	state, err := EnsurePortalIdentity(env, PortalIdentityOptions{Explicit: true})
	if state != nil || err != nil {
		t.Fatalf("%v %v", state, err)
	}
	if portal.minted != 0 {
		t.Fatalf("minted %d", portal.minted)
	}
}

func TestAnonPreviewFullConnectorsRidesTheCreateCall(t *testing.T) {
	cases := []struct {
		raw  *string
		body map[string]any
	}{
		{strPtr("true"), map[string]any{"preview_full_connectors": true}},
		{strPtr("1"), map[string]any{"preview_full_connectors": true}},
		{strPtr("false"), map[string]any{"preview_full_connectors": false}},
		{strPtr("0"), map[string]any{"preview_full_connectors": false}},
		{nil, map[string]any{}},
		{strPtr("yes"), map[string]any{}},
	}
	for _, tc := range cases {
		name := "unset"
		if tc.raw != nil {
			name = *tc.raw
		}
		t.Run(name, func(t *testing.T) {
			portal, env := installAnonPortal(t)
			if tc.raw != nil {
				t.Setenv(PreviewFullConnectorsEnv, *tc.raw)
			}
			if _, err := EnsurePortalIdentity(env, PortalIdentityOptions{Explicit: true}); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(portal.createRequests, []map[string]any{tc.body}) {
				t.Fatalf("create bodies %v", portal.createRequests)
			}
		})
	}
}

func strPtr(s string) *string { return &s }

func TestAnonStaleProfileGuestAdoptsANewerSharedAccount(t *testing.T) {
	_, env := installAnonPortal(t)
	if _, err := EnsurePortalIdentity(env, PortalIdentityOptions{Explicit: true}); err != nil {
		t.Fatal(err)
	}
	writeSharedNousState(env, map[string]any{"access_token": anonJWT(map[string]any{"client_id": "hermes-cli", "account_tier": "free"}),
		"refresh_token": "rt-sibling", "expires_at": "2030-01-01T00:00:00+00:00", "auth_method": "oauth_device_code"}, false)
	state, err := EnsurePortalIdentity(env, PortalIdentityOptions{Explicit: true})
	if err != nil || isGuestState(state) || state["refresh_token"] != "rt-sibling" {
		t.Fatalf("%v %v", state, err)
	}
	if anonProviders(t, env)["nous"].(map[string]any)["refresh_token"] != "rt-sibling" {
		t.Fatal("profile store must hold the adopted account")
	}
	if anonSharedStore(t, env)["refresh_token"] != "rt-sibling" {
		t.Fatal("the profile must never overwrite the shared account")
	}
}

func TestAnonClearingADeadGuestLeavesASiblingIdentityAlone(t *testing.T) {
	_, env := installAnonPortal(t)
	if _, err := EnsurePortalIdentity(env, PortalIdentityOptions{Explicit: true}); err != nil {
		t.Fatal(err)
	}
	writeSharedNousState(env, map[string]any{"access_token": anonJWT(map[string]any{"client_id": "hermes-cli"}), "refresh_token": "rt-sibling",
		"expires_at": "2030-01-01T00:00:00+00:00", "auth_method": "oauth_device_code"}, false)
	if err := clearDeadGuest(env, "test", ""); err != nil {
		t.Fatal(err)
	}
	if _, ok := anonProviders(t, env)["nous"]; ok {
		t.Fatal("the dead guest must be removed from the profile")
	}
	if anonSharedStore(t, env)["refresh_token"] != "rt-sibling" {
		t.Fatal("the sibling identity must stay in the shared store")
	}
}

func TestAnonToolGatewayTokenPathReexchanges(t *testing.T) {
	portal, env := installAnonPortal(t)
	if _, err := EnsurePortalIdentity(env, PortalIdentityOptions{Explicit: true}); err != nil {
		t.Fatal(err)
	}
	anonPatchNous(t, env, map[string]any{"expires_at": "2000-01-01T00:00:00+00:00"})
	portal.clearCalls()
	token, err := ResolveNousAccessToken(env, NousAccessTokenOptions{})
	if err != nil || token == "" {
		t.Fatalf("%q %v", token, err)
	}
	if got := portal.paths(); !reflect.DeepEqual(got, []string{"/api/anonymous/token"}) {
		t.Fatalf("paths %v", got)
	}
}

func TestAnonExpiredGuestJWTReexchangesAndNeverHitsOAuthToken(t *testing.T) {
	portal, env := installAnonPortal(t)
	anonClaimedTrial(t, env)
	anonPatchNous(t, env, map[string]any{"access_token": anonJWT(map[string]any{"exp": 1}), "expires_at": "2000-01-01T00:00:00+00:00"})
	portal.clearCalls()
	creds, err := resolveNousRuntimeCredentials(env, false, "")
	if err != nil {
		t.Fatal(err)
	}
	if got := portal.paths(); !reflect.DeepEqual(got, []string{"/api/anonymous/token"}) {
		t.Fatalf("paths %v", got)
	}
	if strings.TrimRight(anyString(creds["base_url"]), "/") != anonWelcome {
		t.Fatalf("base_url %v", creds["base_url"])
	}
	data, _ := env.Store.Load()
	raw, _ := json.Marshal(data)
	if strings.Contains(string(raw), "quarantine") {
		t.Fatal("a guest exchange must never quarantine")
	}
}

func TestAnonDeadCredentialIsRetiredAndNeverReplaced(t *testing.T) {
	portal, env := installAnonPortal(t)
	first := anonClaimedTrial(t, env)
	portal.set(func(p *anonFakePortal) { p.deadTokens[anyString(first["anon_token"])] = true })
	_, err := resolveNousRuntimeCredentials(env, true, "")
	if _, dead := anonCredentialDeadCode(err); !dead {
		t.Fatalf("want a dead credential error, got %T %v", err, err)
	}
	if _, ok := anonProviders(t, env)["nous"]; ok {
		t.Fatal("the dead guest must be cleared")
	}
	if portal.minted != 1 {
		t.Fatalf("minted %d", portal.minted)
	}
}

func TestAnonMintPersistsBeforeAnyExchangeAndFirstUseExchangesOnce(t *testing.T) {
	portal, env := installAnonPortal(t)
	first := anonClaimedTrial(t, env)
	if _, has := first["access_token"]; !isGuestState(first) || has {
		t.Fatalf("first %v", first)
	}
	if got := portal.paths(); !reflect.DeepEqual(got, []string{"/api/anonymous/create"}) {
		t.Fatalf("mint alone, exchange is lazy: %v", got)
	}
	creds, err := resolveNousRuntimeCredentials(env, false, "")
	if err != nil || anyString(creds["api_key"]) == "" {
		t.Fatalf("%v %v", creds, err)
	}
	if portal.minted != 1 {
		t.Fatal("a stored credential is exchanged, never re-minted")
	}
	if portal.count("/api/anonymous/token") != 1 {
		t.Fatalf("paths %v", portal.paths())
	}
}

func TestAnonMintMemoIsKeyedPerProfileHome(t *testing.T) {
	_, env := installAnonPortal(t)
	scoped := env
	scoped.Ctx = home.WithOverride(context.Background(), t.TempDir())
	if mintMemoKey(env) != "" || mintMemoKey(scoped) == "" {
		t.Fatalf("keys %q %q", mintMemoKey(env), mintMemoKey(scoped))
	}
}

func TestTrialModelExportReadsTheHomeIdentity(t *testing.T) {
	h := runtimeConfigHome(t, "model:\n  default: m\n")
	ctx := context.Background()
	if got, want := TrialModel(ctx, h), trialModel(modelsPoolEnv(ctxForHome(ctx, h))); got != want || got != "" {
		t.Fatalf("trial model %q want empty %q", got, want)
	}
}

func TestTrialModelExportCarriesTheClaimedWelcomeTrial(t *testing.T) {
	_, env := installAnonPortal(t)
	anonClaimedTrial(t, env)
	if got := TrialModel(context.Background(), os.Getenv("ICEBATH_HOME")); got != GuestModel {
		t.Fatalf("claimed trial model %q want %q", got, GuestModel)
	}
}

func TestAnonAuthExportsAnswerLikeTheirOriginals(t *testing.T) {
	env := anonPresentEnv(t)
	if HasGuest(env) != hasGuest(env) || HasFreeTierAccount(env) != hasFreeTierAccount(env) || GuestNoticePending(env) != guestNoticePending(env) {
		t.Fatal("an anon export disagrees with its original")
	}
	for _, token := range []string{"", "not-a-jwt"} {
		if IsConnectorsOnly(token) != isConnectorsOnly(token) {
			t.Fatalf("IsConnectorsOnly(%q) disagrees", token)
		}
	}
}

func TestRouteCanServeModelGatesOnlyTheWelcomeHost(t *testing.T) {
	var welcome string
	for host := range WelcomeHosts() {
		welcome = "https://" + host + "/v1"
		break
	}
	cases := []struct {
		provider, baseURL, model string
		want                     bool
	}{
		{"openrouter", welcome, "anthropic/claude", true},
		{"nous", "https://inference-api.nousresearch.com/v1", "Hermes-4", true},
		{"nous", welcome, "", true},
		{"nous", welcome, GuestModel, true},
		{"nous", welcome, "Hermes-4", false},
	}
	for _, c := range cases {
		if got := RouteCanServeModel(c.provider, c.baseURL, c.model); got != c.want {
			t.Errorf("RouteCanServeModel(%q, %q, %q) = %v, want %v", c.provider, c.baseURL, c.model, got, c.want)
		}
	}
}
