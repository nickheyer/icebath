package dashboardauth

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/gateway"
)

// Isolated home and registry with the deployment token unset and config.yaml when given
func sessionTokenCase(t *testing.T, configYAML string) {
	t.Helper()
	drainCase(t, configYAML)
	t.Setenv("ICEBATH_DASHBOARD_SESSION_TOKEN", "")
}

func newSessionToken(t *testing.T, token string) *SessionTokenProvider {
	t.Helper()
	p, err := NewSessionTokenProvider(token)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func registerSessionToken(t *testing.T) *SessionTokenProvider {
	t.Helper()
	if err := (SessionTokenPlugin{}).Register(pluginContextFor("session-token", "dashboard_auth/session_token")); err != nil {
		t.Fatal(err)
	}
	p, _ := gateway.SnapshotDashboardAuthRegistration("session-token", "").(*SessionTokenProvider)
	return p
}

// test_session_token_provider.py TestProvider test_protocol_compliance
func TestProviderProtocolCompliance(t *testing.T) {
	sessionTokenCase(t, "")
	if err := gateway.AssertDashboardAuthProtocol(newSessionToken(t, tokenURLSafeForTest())); err != nil {
		t.Fatal(err)
	}
}

// test_session_token_provider.py TestProvider test_is_a_session_credential_but_not_an_interactive_login
func TestProviderIsASessionCredentialButNotAnInteractiveLogin(t *testing.T) {
	sessionTokenCase(t, "")
	p := newSessionToken(t, tokenURLSafeForTest())
	if !gateway.DashboardAuthSupportsSession(p) || gateway.DashboardAuthSupportsPassword(p) || gateway.DashboardAuthInteractiveLogin(p) {
		t.Fatal("capability flags")
	}
}

// test_session_token_provider.py TestProvider test_verify_session_accepts_the_provisioned_token
func TestProviderVerifySessionAcceptsTheProvisionedToken(t *testing.T) {
	sessionTokenCase(t, "")
	token := tokenURLSafeForTest()
	s, err := newSessionToken(t, token).VerifySession(context.Background(), token)
	if err != nil || s == nil || s.Provider != "session-token" || s.UserID != "deployment-token" || s.AccessToken != token || s.ExpiresAt <= 0 {
		t.Fatalf("%+v %v", s, err)
	}
}

// test_session_token_provider.py TestProvider test_verify_session_rejects_foreign_and_empty
func TestProviderVerifySessionRejectsForeignAndEmpty(t *testing.T) {
	sessionTokenCase(t, "")
	p := newSessionToken(t, tokenURLSafeForTest())
	for _, presented := range []string{tokenURLSafeForTest(), ""} {
		if s, err := p.VerifySession(context.Background(), presented); s != nil || err != nil {
			t.Fatalf("%q: %+v %v", presented, s, err)
		}
	}
}

// test_session_token_provider.py TestProvider test_refresh_raises_expired_and_revoke_is_silent
func TestProviderRefreshRaisesExpiredAndRevokeIsSilent(t *testing.T) {
	sessionTokenCase(t, "")
	p := newSessionToken(t, tokenURLSafeForTest())
	_, err := p.RefreshSession(context.Background(), "anything")
	var expired *gateway.RefreshExpiredError
	if !errors.As(err, &expired) {
		t.Fatal(err)
	}
	if err := p.RevokeSession(context.Background(), "anything"); err != nil {
		t.Fatal(err)
	}
}

// test_session_token_provider.py TestProvider test_interactive_methods_raise
func TestProviderInteractiveMethodsRaise(t *testing.T) {
	sessionTokenCase(t, "")
	p := newSessionToken(t, tokenURLSafeForTest())
	var notImpl *gateway.DashboardAuthNotImplementedError
	if _, err := p.StartLogin(context.Background(), "r"); !errors.As(err, &notImpl) {
		t.Fatal(err)
	}
	if _, err := p.CompleteLogin(context.Background(), "c", "s", "v", "r"); !errors.As(err, &notImpl) {
		t.Fatal(err)
	}
}

// test_session_token_provider.py TestProvider test_construction_rejects_empty_token
func TestProviderConstructionRejectsEmptyToken(t *testing.T) {
	_, err := NewSessionTokenProvider("")
	var valueErr *gateway.DashboardAuthValueError
	if !errors.As(err, &valueErr) {
		t.Fatal(err)
	}
}

// test_session_token_provider.py TestRegister test_skips_when_no_token_provisioned
func TestRegisterSkipsWhenNoTokenProvisioned(t *testing.T) {
	sessionTokenCase(t, "")
	if registerSessionToken(t) != nil {
		t.Fatal("registered")
	}
	if !strings.Contains(SessionTokenLastSkipReason(), "ICEBATH_DASHBOARD_SESSION_TOKEN") {
		t.Fatal(SessionTokenLastSkipReason())
	}
}

// test_session_token_provider.py TestRegister test_registers_with_env_token
func TestRegisterRegistersWithEnvToken(t *testing.T) {
	sessionTokenCase(t, "")
	token := tokenURLSafeForTest()
	t.Setenv("ICEBATH_DASHBOARD_SESSION_TOKEN", token)
	p := registerSessionToken(t)
	if p == nil {
		t.Fatal("not registered")
	}
	if s, err := p.VerifySession(context.Background(), token); s == nil || err != nil {
		t.Fatalf("%+v %v", s, err)
	}
	if SessionTokenLastSkipReason() != "" {
		t.Fatal(SessionTokenLastSkipReason())
	}
}

// test_session_token_provider.py TestRegister test_resolves_file_prefix
func TestRegisterResolvesFilePrefix(t *testing.T) {
	sessionTokenCase(t, "")
	token := tokenURLSafeForTest()
	tokenFile := filepath.Join(t.TempDir(), "icebath-gateway-token")
	if err := os.WriteFile(tokenFile, []byte("\ufeff"+token+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("ICEBATH_DASHBOARD_SESSION_TOKEN", "file:"+tokenFile)
	p := registerSessionToken(t)
	if p == nil {
		t.Fatal("not registered")
	}
	if s, err := p.VerifySession(context.Background(), token); s == nil || err != nil {
		t.Fatalf("%+v %v", s, err)
	}
}

// test_session_token_provider.py TestRegister test_skip_on_unreadable_token_file
func TestRegisterSkipOnUnreadableTokenFile(t *testing.T) {
	sessionTokenCase(t, "")
	t.Setenv("ICEBATH_DASHBOARD_SESSION_TOKEN", "file:"+filepath.Join(t.TempDir(), "missing"))
	captureSlog(t)
	if registerSessionToken(t) != nil {
		t.Fatal("registered")
	}
	if SessionTokenLastSkipReason() == "" {
		t.Fatal("no skip reason")
	}
}

// test_session_token_provider.py TestRegister test_config_opt_out
func TestRegisterConfigOptOut(t *testing.T) {
	sessionTokenCase(t, "dashboard:\n  session_token_auth:\n    enabled: false\n")
	t.Setenv("ICEBATH_DASHBOARD_SESSION_TOKEN", tokenURLSafeForTest())
	if registerSessionToken(t) != nil {
		t.Fatal("registered")
	}
	if !strings.Contains(SessionTokenLastSkipReason(), "enabled") {
		t.Fatal(SessionTokenLastSkipReason())
	}
}

// test_session_token_provider.py TestRegister test_short_token_still_registers_with_warning
func TestRegisterShortTokenStillRegistersWithWarning(t *testing.T) {
	sessionTokenCase(t, "")
	t.Setenv("ICEBATH_DASHBOARD_SESSION_TOKEN", "short-but-mine")
	records := captureSlog(t)
	if registerSessionToken(t) == nil {
		t.Fatal("not registered")
	}
	for _, r := range records() {
		if strings.Contains(r.Message, "short") {
			return
		}
	}
	t.Fatal("no short token warning")
}

func realLoaderCase(t *testing.T, configYAML string) {
	t.Helper()
	sessionTokenCase(t, configYAML)
	t.Setenv("ICEBATH_DASHBOARD_SESSION_TOKEN", tokenURLSafeForTest())
}

// test_session_token_provider.py TestRealConfigLoader test_enabled_false_in_config_yaml_skips_registration
func TestRealConfigLoaderEnabledFalseInConfigYAMLSkipsRegistration(t *testing.T) {
	realLoaderCase(t, "dashboard:\n  session_token_auth:\n    enabled: false\n")
	if registerSessionToken(t) != nil {
		t.Fatal("registered")
	}
	if !strings.Contains(SessionTokenLastSkipReason(), "enabled is false") {
		t.Fatal(SessionTokenLastSkipReason())
	}
}

// test_session_token_provider.py TestRealConfigLoader test_enabled_true_in_config_yaml_registers
func TestRealConfigLoaderEnabledTrueInConfigYAMLRegisters(t *testing.T) {
	realLoaderCase(t, "dashboard:\n  session_token_auth:\n    enabled: true\n")
	if registerSessionToken(t) == nil {
		t.Fatal("not registered")
	}
}

// test_session_token_provider.py TestRealConfigLoader test_absent_section_defaults_to_enabled
func TestRealConfigLoaderAbsentSectionDefaultsToEnabled(t *testing.T) {
	realLoaderCase(t, "dashboard:\n  theme: default\n")
	if registerSessionToken(t) == nil {
		t.Fatal("not registered")
	}
}
