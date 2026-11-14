package provider

import (
	"bytes"
	"strings"
	"testing"
)

func TestPortalInfoRendersFreeTierCopy(t *testing.T) {
	h, ctx := anonSurfaceStore(t)
	anonSurfaceWriteAuth(t, h, map[string]any{
		"auth_method": "anonymous", "account_tier": "anonymous", "anon_token": "anon_t",
		"access_token": anonSurfaceJWT(nil), "expires_at": "2030-01-01T00:00:00+00:00",
		"inference_base_url": anonWelcome, "user_id": "nas_user:abc", "org_id": "nas_organisation:def",
		WelcomeClaimFlag: true,
	})
	var out bytes.Buffer
	if code, err := portalCmdStatus(ctx, modelsPoolEnv(ctx), &out); err != nil || code != 0 {
		t.Fatalf("portal info = %d %v", code, err)
	}
	text := out.String()
	if !strings.Contains(strings.ToLower(text), "free tier") || !strings.Contains(text, FreeTierLabel) || !strings.Contains(text, GuestModel) {
		t.Fatalf("portal info did not name the free tier:\n%s", text)
	}
	if !strings.Contains(text, UpgradeHint) || !strings.Contains(text, "API:     "+anonWelcome) {
		t.Fatalf("portal info lacks the upgrade hint or the API line:\n%s", text)
	}
	if leaked := anonSurfaceForbidden.FindString(text); leaked != "" {
		t.Fatalf("portal info leaked %q:\n%s", leaked, text)
	}
	if !strings.Contains(text, "Tool Gateway") {
		t.Fatalf("portal info lacks the Tool Gateway section:\n%s", text)
	}
}

func TestPortalInfoKeepsAccountRendering(t *testing.T) {
	h, ctx := anonSurfaceStore(t)
	anonSurfaceWriteAuth(t, h, map[string]any{
		"auth_method": "oauth_device_code", "client_id": "hermes-cli",
		"access_token":  anonSurfaceJWT(map[string]any{"sub": "nas_user:real", "client_id": "hermes-cli", "account_tier": "standard", "paid_access": true}),
		"refresh_token": "rt_live", "expires_at": "2030-01-01T00:00:00+00:00",
		"portal_base_url": "https://portal.nousresearch.com", "inference_base_url": "https://inference-api.nousresearch.com/v1",
	})
	var out bytes.Buffer
	if code, err := portalCmdStatus(ctx, modelsPoolEnv(ctx), &out); err != nil || code != 0 {
		t.Fatalf("portal info = %d %v", code, err)
	}
	text := out.String()
	if strings.Contains(strings.ToLower(text), "free tier") || strings.Contains(text, UpgradeHint) {
		t.Fatalf("portal info mislabelled a real account:\n%s", text)
	}
	if !strings.Contains(text, "logged in") || !strings.Contains(text, "Portal:  https://portal.nousresearch.com") {
		t.Fatalf("portal info lacks the account lines:\n%s", text)
	}
}

func TestPortalCommandDispatch(t *testing.T) {
	_, ctx := anonSurfaceStore(t)
	var out, errOut bytes.Buffer
	code, err := PortalCommand(ctx, modelsPoolEnv(ctx), map[string]any{"portal_command": "bogus"}, &out, &errOut)
	if err != nil || code != 1 || errOut.String() != "Unknown portal subcommand: bogus\nRun `icebath portal -h` for usage.\n" {
		t.Fatalf("unknown subcommand = %d %v %q", code, err, errOut.String())
	}
	out.Reset()
	errOut.Reset()
	code, err = PortalCommand(ctx, modelsPoolEnv(ctx), map[string]any{"portal_command": "tools"}, &out, &errOut)
	if err != nil || code != 0 {
		t.Fatalf("tools = %d %v %s", code, err, errOut.String())
	}
	for _, want := range []string{"Tool Gateway catalog", "Web search & extract", "partner: FAL", "Cloud terminal", "Manage your subscription: " + PortalSubscriptionURL} {
		if !strings.Contains(out.String(), want) {
			t.Fatalf("tools output lacks %q:\n%s", want, out.String())
		}
	}
}
