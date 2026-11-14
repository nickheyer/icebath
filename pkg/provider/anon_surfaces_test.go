package provider

import (
	"bytes"
	"context"
	"strings"
	"testing"
)

// Guest auth state holding the welcome trial claim
func anonSurfaceGuestState() map[string]any {
	return map[string]any{
		"auth_method": "anonymous", "account_tier": "anonymous", "anon_token": "anon_t",
		"access_token": anonSurfaceJWT(nil), "expires_at": "2030-01-01T00:00:00+00:00",
		"inference_base_url": anonWelcome, "user_id": "nas_user:abc", "org_id": "nas_organisation:def",
		WelcomeClaimFlag: true,
	}
}

// Signed in account auth state with paid access
func anonSurfaceAccountState() map[string]any {
	return map[string]any{
		"auth_method": "oauth_device_code", "client_id": "hermes-cli",
		"access_token":  anonSurfaceJWT(map[string]any{"sub": "nas_user:real", "client_id": "hermes-cli", "account_tier": "standard", "paid_access": true}),
		"refresh_token": "rt_live", "expires_at": "2030-01-01T00:00:00+00:00",
		"portal_base_url": "https://portal.nousresearch.com", "inference_base_url": "https://inference-api.nousresearch.com/v1",
	}
}

// Output of icebath auth status or auth list for the nous provider
func anonSurfaceAuth(t *testing.T, ctx context.Context, action string) string {
	t.Helper()
	var out, errw bytes.Buffer
	if code, err := AuthCommand(ctx, modelsPoolEnv(ctx), authParseArgs(t, []string{action, "nous"}), &out, &errw); err != nil || code != 0 {
		t.Fatalf("auth %s = %d %v %s", action, code, err, errw.String())
	}
	return out.String()
}

// Every read only display surface rendered over the current auth store
func anonSurfaceRenderAll(t *testing.T, ctx context.Context) map[string]string {
	t.Helper()
	out := map[string]string{"auth status": anonSurfaceAuth(t, ctx, "status"), "auth list": anonSurfaceAuth(t, ctx, "list"),
		"icebath status": renderStatusAuthProviders(t, ctx)}
	var portal bytes.Buffer
	if code, err := portalCmdStatus(ctx, modelsPoolEnv(ctx), &portal); err != nil || code != 0 {
		t.Fatalf("portal info = %d %v", code, err)
	}
	out["portal info"] = portal.String()
	return out
}

func TestFreeTierRendersFreeTierCopyOnEverySurface(t *testing.T) {
	h, ctx := anonSurfaceStore(t)
	anonSurfaceWriteAuth(t, h, anonSurfaceGuestState())
	for surface, text := range anonSurfaceRenderAll(t, ctx) {
		if !strings.Contains(strings.ToLower(text), "free tier") {
			t.Fatalf("%s did not name the free tier:\n%s", surface, text)
		}
		if !strings.Contains(text, FreeTierLabel) || !strings.Contains(text, GuestModel) {
			t.Fatalf("%s lacks the free tier label or model:\n%s", surface, text)
		}
		if !strings.Contains(text, UpgradeHint) {
			t.Fatalf("%s lacks the upgrade hint:\n%s", surface, text)
		}
		if leaked := anonSurfaceForbidden.FindString(text); leaked != "" {
			t.Fatalf("%s leaked %q:\n%s", surface, leaked, text)
		}
	}
	info := GetNousPortalAccountInfo(ctx, false)
	if info == nil || !info.IsAnonymousTier() {
		t.Fatalf("info %+v", info)
	}
	message := FormatNousPortalEntitlementMessage(info, "managed web tools", false)
	if message != FreeTierNeedsAccount || strings.Contains(strings.ToLower(message), "billing") || anonSurfaceForbidden.MatchString(message) {
		t.Fatalf("entitlement message %q", message)
	}
}

func TestRealAccountKeepsAccountRendering(t *testing.T) {
	h, ctx := anonSurfaceStore(t)
	anonSurfaceWriteAuth(t, h, anonSurfaceAccountState())
	rendered := anonSurfaceRenderAll(t, ctx)
	for surface, text := range rendered {
		if strings.Contains(strings.ToLower(text), "free tier") || strings.Contains(text, UpgradeHint) {
			t.Fatalf("%s mislabelled a real account:\n%s", surface, text)
		}
	}
	if !strings.Contains(rendered["auth status"], "logged in") || !strings.Contains(rendered["auth list"], "credentials") {
		t.Fatalf("auth status %q auth list %q", rendered["auth status"], rendered["auth list"])
	}
	info := GetNousPortalAccountInfo(ctx, false)
	if info == nil || info.IsAnonymousTier() || FormatNousPortalEntitlementMessage(info, "", false) != "" {
		t.Fatalf("info %+v", info)
	}
}

func TestAClaimedGuestReadsAsTheFreeTierOnTheCliToo(t *testing.T) {
	h, _ := anonSurfaceStore(t)
	anonSurfaceWriteAuth(t, h, anonSurfaceGuestState())
	text := anonSurfaceAuth(t, context.Background(), "status")
	if !strings.Contains(strings.ToLower(text), "free tier") {
		t.Fatalf("auth status off the desktop did not name the free tier:\n%s", text)
	}
}
