package provider

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/tools"
)

// ---- tests/hermes_cli/test_anon_surfaces.py (icebath status surface) ----

// Status sink printing the hermes status row, detail and section layout without colour
type statusTestSink struct {
	buf      bytes.Buffer
	env      map[string]string
	provider string
}

func (s *statusTestSink) Section(title string) { fmt.Fprintf(&s.buf, "\n◆ %s\n", title) }

func (s *statusTestSink) Row(name string, ok bool, text string, width int, sep string) {
	mark := "✗"
	if ok {
		mark = "✓"
	}
	fmt.Fprintf(&s.buf, "  %-*s%s%s %s\n", width, name, sep, mark, text)
}

func (s *statusTestSink) Detail(label string, value any) {
	text := "None"
	if value != nil {
		text = pyStr(value)
	}
	fmt.Fprintf(&s.buf, "    %-12s%s\n", label, text)
}

func (s *statusTestSink) Print(line string) { s.buf.WriteString(line + "\n") }

func (s *statusTestSink) EnvValue(name string) string {
	if v, ok := s.env[name]; ok {
		return v
	}
	return os.Getenv(name)
}

func (s *statusTestSink) FirstEnvValue(names ...string) string {
	for _, n := range names {
		if v := s.EnvValue(n); v != "" {
			return v
		}
	}
	return ""
}

func (s *statusTestSink) EffectiveProviderLabel() string { return s.provider }

var anonSurfaceForbidden = regexp.MustCompile(`(?i)guest|anonymous|user id|org id|nas_user|nas_organisation`)

// Temp home on the local desktop backend with no network account lookup and no ambient provider keys
func anonSurfaceStore(t *testing.T) (string, context.Context) {
	t.Helper()
	h := t.TempDir()
	t.Setenv("ICEBATH_HOME", h)
	t.Setenv("ICEBATH_SHARED_AUTH_DIR", filepath.Join(t.TempDir(), "shared-store"))
	for _, v := range []string{"OPENROUTER_API_KEY", "OPENAI_API_KEY", "ANTHROPIC_API_KEY", "NOUS_API_KEY"} {
		t.Setenv(v, "")
		os.Unsetenv(v)
	}
	prev := fetchNousAccountInfoFn
	fetchNousAccountInfoFn = func(PoolEnv, string, string) (map[string]any, error) {
		t.Error("portal fetch must not happen on a read-only surface")
		return nil, errors.New("no network")
	}
	ResetNousPortalAccountInfoCache()
	InvalidateNousAuthStatusCache()
	t.Cleanup(func() {
		fetchNousAccountInfoFn = prev
		ResetNousPortalAccountInfoCache()
		InvalidateNousAuthStatusCache()
	})
	return h, tools.WithServingLocalDesktop(context.Background(), true)
}

func anonSurfaceWriteAuth(t *testing.T, h string, state map[string]any) {
	t.Helper()
	raw, _ := json.Marshal(map[string]any{"active_provider": "nous", "providers": map[string]any{"nous": state}})
	if err := os.WriteFile(filepath.Join(h, "auth.json"), raw, 0o600); err != nil {
		t.Fatal(err)
	}
}

func anonSurfaceJWT(claims map[string]any) string {
	payload := map[string]any{"sub": "nas_user:abc", "client_id": "nas-anonymous", "account_tier": "anonymous",
		"scope": "inference:invoke", "exp": time.Now().Unix() + 100_000_000}
	for k, v := range claims {
		payload[k] = v
	}
	return jwtWithClaims(payload)
}

func renderStatusAuthProviders(t *testing.T, ctx context.Context) string {
	t.Helper()
	sink := &statusTestSink{}
	c := &StatusContext{Ctx: ctx, Out: sink, Config: map[string]any{}}
	if err := StatusRenderAuthProviders(c); err != nil {
		t.Fatal(err)
	}
	return sink.buf.String()
}

func TestStatusAuthProvidersRenderFreeTierCopy(t *testing.T) {
	h, ctx := anonSurfaceStore(t)
	anonSurfaceWriteAuth(t, h, map[string]any{
		"auth_method": "anonymous", "account_tier": "anonymous", "anon_token": "anon_t",
		"access_token": anonSurfaceJWT(nil), "expires_at": "2030-01-01T00:00:00+00:00",
		"inference_base_url": anonWelcome, "user_id": "nas_user:abc", "org_id": "nas_organisation:def",
		WelcomeClaimFlag: true,
	})
	text := renderStatusAuthProviders(t, ctx)
	if !strings.Contains(strings.ToLower(text), "free tier") || !strings.Contains(text, FreeTierLabel) || !strings.Contains(text, GuestModel) {
		t.Fatalf("status did not name the free tier:\n%s", text)
	}
	if !strings.Contains(text, UpgradeHint) {
		t.Fatalf("status lacks the upgrade hint:\n%s", text)
	}
	if leaked := anonSurfaceForbidden.FindString(text); leaked != "" {
		t.Fatalf("status leaked %q:\n%s", leaked, text)
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

func TestStatusAuthProvidersKeepAccountRendering(t *testing.T) {
	h, ctx := anonSurfaceStore(t)
	anonSurfaceWriteAuth(t, h, map[string]any{
		"auth_method": "oauth_device_code", "client_id": "hermes-cli",
		"access_token":  anonSurfaceJWT(map[string]any{"sub": "nas_user:real", "client_id": "hermes-cli", "account_tier": "standard", "paid_access": true}),
		"refresh_token": "rt_live", "expires_at": "2030-01-01T00:00:00+00:00",
		"portal_base_url": "https://portal.nousresearch.com", "inference_base_url": "https://inference-api.nousresearch.com/v1",
	})
	text := renderStatusAuthProviders(t, ctx)
	if strings.Contains(strings.ToLower(text), "free tier") || strings.Contains(text, UpgradeHint) {
		t.Fatalf("status mislabelled a real account:\n%s", text)
	}
	info := GetNousPortalAccountInfo(ctx, false)
	if info == nil || info.IsAnonymousTier() || FormatNousPortalEntitlementMessage(info, "", false) != "" {
		t.Fatalf("info %+v", info)
	}
}

func TestStatusAuthFormattersAndOAuthBlockGates(t *testing.T) {
	if statusFormatISOTimestamp(nil) != "(unknown)" || statusFormatISOTimestamp("  ") != "(unknown)" || statusFormatISOTimestamp(" not a date") != " not a date" {
		t.Fatal("timestamp fallbacks")
	}
	want := time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC).Local().Format("2006-01-02 15:04:05 MST")
	if got := statusFormatISOTimestamp("2030-01-01T00:00:00Z"); got != want {
		t.Fatalf("timestamp %v want %s", got, want)
	}
	if got, err := statusQwenExpiry(int64(1893456000123)); err != nil || got != "2030-01-01T00:00:00.123000+00:00" {
		t.Fatalf("qwen expiry %q %v", got, err)
	}
	if got, err := statusQwenExpiry("1893456000000"); err != nil || got != "2030-01-01T00:00:00+00:00" {
		t.Fatalf("qwen expiry %q %v", got, err)
	}
	if _, err := statusQwenExpiry("soon"); err == nil {
		t.Fatal("a non integer expiry must raise")
	}
	sink := &statusTestSink{}
	block := statusOAuthBlocks[2]
	if err := statusOAuthBlockRender(sink, block, map[string]any{"logged_in": false, "region": "cn", "expires_at": "x", "error": "boom"}); err != nil {
		t.Fatal(err)
	}
	out := sink.buf.String()
	if !strings.Contains(out, "not logged in (run: icebath auth add minimax-oauth)") || strings.Contains(out, "Region:") ||
		!strings.Contains(out, "    Access exp: x") || !strings.Contains(out, "    Error:      boom") {
		t.Fatalf("logged out block:\n%s", out)
	}
	sink.buf.Reset()
	_ = statusOAuthBlockRender(sink, block, map[string]any{"logged_in": true, "region": "cn", "error": "boom"})
	if out := sink.buf.String(); !strings.Contains(out, "✓ logged in") || !strings.Contains(out, "Region:     cn") || strings.Contains(out, "Error:") {
		t.Fatalf("logged in block:\n%s", out)
	}
	if statusFeatureState(NousFeatureState{Active: true}, false) != "active via configured provider" ||
		statusFeatureState(NousFeatureState{Key: "modal"}, true) != "available via subscription (optional)" ||
		statusFeatureState(NousFeatureState{IncludedByDefault: true}, false) != "not configured" {
		t.Fatal("feature states")
	}
}
