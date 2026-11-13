package dashboardauth

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"io"
	"log/slog"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/plugins"
)

// Isolated home with the basic auth env cleared, the registry reset, and config.yaml when given
func basicCase(t *testing.T, configYAML string) {
	t.Helper()
	drainCase(t, configYAML)
	for _, name := range []string{"ICEBATH_DASHBOARD_BASIC_AUTH_USERNAME", "ICEBATH_DASHBOARD_BASIC_AUTH_PASSWORD",
		"ICEBATH_DASHBOARD_BASIC_AUTH_PASSWORD_HASH", "ICEBATH_DASHBOARD_BASIC_AUTH_SECRET", "ICEBATH_DASHBOARD_BASIC_AUTH_TTL_SECONDS"} {
		t.Setenv(name, "")
	}
}

func randomBytes(n int) []byte {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return b
}

func makeBasic(t *testing.T) *BasicAuthProvider {
	t.Helper()
	p, err := NewBasicAuthProvider("admin", HashPassword("hunter2"), randomBytes(32), basicDefaultTTLSeconds)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func mustVerify(t *testing.T, ok bool, err error) bool {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
	return ok
}

func mustLogin(t *testing.T, p *BasicAuthProvider, username, password string) *gateway.DashboardSession {
	t.Helper()
	s, err := p.CompletePasswordLogin(context.Background(), username, password)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func verified(t *testing.T, p *BasicAuthProvider, token string) *gateway.DashboardSession {
	t.Helper()
	s, err := p.VerifySession(context.Background(), token)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// Plugin context of a bundled auth plugin on the launch profile manager
func pluginContextFor(name, key string) *plugins.Context {
	return plugins.NewContext(&plugins.Manifest{Name: name, Key: key}, plugins.NewManager(home.Key(home.Process())))
}

// Runs register on a fresh basic plugin context and returns what the registry holds afterwards
func registerBasic(t *testing.T) *BasicAuthProvider {
	t.Helper()
	pctx := pluginContextFor("basic", "dashboard_auth/basic")
	if err := (BasicPlugin{}).Register(pctx); err != nil {
		t.Fatal(err)
	}
	p, _ := gateway.SnapshotDashboardAuthRegistration("basic", "").(*BasicAuthProvider)
	return p
}

// Records every slog record emitted until cleanup
func captureSlog(t *testing.T) func() []slog.Record {
	t.Helper()
	var mu sync.Mutex
	var records []slog.Record
	prev := slog.Default()
	slog.SetDefault(slog.New(recordHandler{add: func(r slog.Record) { mu.Lock(); records = append(records, r); mu.Unlock() }}))
	t.Cleanup(func() { slog.SetDefault(prev) })
	return func() []slog.Record { mu.Lock(); defer mu.Unlock(); return append([]slog.Record(nil), records...) }
}

type recordHandler struct{ add func(slog.Record) }

func (h recordHandler) Enabled(context.Context, slog.Level) bool      { return true }
func (h recordHandler) Handle(_ context.Context, r slog.Record) error { h.add(r.Clone()); return nil }
func (h recordHandler) WithAttrs([]slog.Attr) slog.Handler            { return h }
func (h recordHandler) WithGroup(string) slog.Handler                 { return h }

// Text written to stderr while fn runs
func captureStderr(t *testing.T, fn func()) string {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	prev := os.Stderr
	os.Stderr = w
	done := make(chan string)
	go func() { b, _ := io.ReadAll(r); done <- string(b) }()
	fn()
	os.Stderr = prev
	_ = w.Close()
	return <-done
}

// test_basic_provider.py TestPasswordHashing test_hash_then_verify_round_trips
func TestPasswordHashingHashThenVerifyRoundTrips(t *testing.T) {
	basicCase(t, "")
	h := HashPassword("hunter2")
	if !strings.HasPrefix(h, "scrypt$") {
		t.Fatal(h)
	}
	if ok, err := verifyPassword("hunter2", h); !mustVerify(t, ok, err) {
		t.Fatal("round trip failed")
	}
}

// test_basic_provider.py TestPasswordHashing test_wrong_password_fails
func TestPasswordHashingWrongPasswordFails(t *testing.T) {
	basicCase(t, "")
	if ok, err := verifyPassword("wrong", HashPassword("hunter2")); mustVerify(t, ok, err) {
		t.Fatal("wrong password verified")
	}
}

// test_basic_provider.py TestPasswordHashing test_malformed_hash_returns_false
func TestPasswordHashingMalformedHashReturnsFalse(t *testing.T) {
	basicCase(t, "")
	for _, encoded := range []string{"not-a-valid-hash", "bcrypt$wrong$scheme"} {
		if ok, err := verifyPassword("x", encoded); mustVerify(t, ok, err) {
			t.Fatal(encoded)
		}
	}
}

// test_basic_provider.py TestPasswordHashing test_two_hashes_of_same_password_differ
func TestPasswordHashingTwoHashesOfSamePasswordDiffer(t *testing.T) {
	basicCase(t, "")
	if HashPassword("pw") == HashPassword("pw") {
		t.Fatal("hashes equal")
	}
}

// test_basic_provider.py TestProvider test_protocol_compliant
func TestProtocolCompliant(t *testing.T) {
	basicCase(t, "")
	if err := gateway.AssertDashboardAuthProtocol(makeBasic(t)); err != nil {
		t.Fatal(err)
	}
}

// test_basic_provider.py TestProvider test_login_mints_session
func TestLoginMintsSession(t *testing.T) {
	basicCase(t, "")
	s := mustLogin(t, makeBasic(t), "admin", "hunter2")
	if s.UserID != "admin" || s.Provider != "basic" || s.AccessToken == "" || s.RefreshToken == "" {
		t.Fatalf("%+v", s)
	}
}

// test_basic_provider.py TestProvider test_bad_credentials_raise
func TestBadCredentialsRaise(t *testing.T) {
	basicCase(t, "")
	p := makeBasic(t)
	for _, pair := range [][2]string{{"admin", "wrong"}, {"ghost", "hunter2"}, {"", ""}} {
		_, err := p.CompletePasswordLogin(context.Background(), pair[0], pair[1])
		var invalid *gateway.InvalidCredentialsError
		if !errors.As(err, &invalid) {
			t.Fatalf("%v: %v", pair, err)
		}
	}
}

// test_basic_provider.py TestProvider test_verify_round_trips_and_rejects_tamper
func TestVerifyRoundTripsAndRejectsTamper(t *testing.T) {
	basicCase(t, "")
	p := makeBasic(t)
	s := mustLogin(t, p, "admin", "hunter2")
	if verified(t, p, s.AccessToken) == nil {
		t.Fatal("access token rejected")
	}
	if verified(t, p, "garbage") != nil {
		t.Fatal("garbage verified")
	}
}

// test_basic_provider.py TestProvider test_access_token_not_accepted_as_refresh
func TestAccessTokenNotAcceptedAsRefresh(t *testing.T) {
	basicCase(t, "")
	p := makeBasic(t)
	s := mustLogin(t, p, "admin", "hunter2")
	if verified(t, p, s.RefreshToken) != nil {
		t.Fatal("refresh token verified as access")
	}
	_, err := p.RefreshSession(context.Background(), s.AccessToken)
	var expired *gateway.RefreshExpiredError
	if !errors.As(err, &expired) {
		t.Fatal(err)
	}
}

// test_basic_provider.py TestProvider test_refresh_round_trips
func TestRefreshRoundTrips(t *testing.T) {
	basicCase(t, "")
	p := makeBasic(t)
	s := mustLogin(t, p, "admin", "hunter2")
	r, err := p.RefreshSession(context.Background(), s.RefreshToken)
	if err != nil || r.UserID != "admin" {
		t.Fatalf("%+v %v", r, err)
	}
	if verified(t, p, r.AccessToken) == nil {
		t.Fatal("refreshed access token rejected")
	}
}

// test_basic_provider.py TestProvider test_sessions_minted_in_one_second_get_their_own_tokens
func TestSessionsMintedInOneSecondGetTheirOwnTokens(t *testing.T) {
	basicCase(t, "")
	prev := basicClock
	basicClock = func() time.Time { return time.Unix(1_700_000_000, 0) }
	t.Cleanup(func() { basicClock = prev })
	p := makeBasic(t)
	first := mustLogin(t, p, "admin", "hunter2")
	second := mustLogin(t, p, "admin", "hunter2")
	renewed, err := p.RefreshSession(context.Background(), first.RefreshToken)
	if err != nil {
		t.Fatal(err)
	}
	access, refresh := map[string]bool{}, map[string]bool{}
	for _, s := range []*gateway.DashboardSession{first, second, renewed} {
		access[s.AccessToken], refresh[s.RefreshToken] = true, true
		if verified(t, p, s.AccessToken) == nil {
			t.Fatal("session rejected")
		}
	}
	if len(access) != 3 || len(refresh) != 3 {
		t.Fatalf("access %d refresh %d", len(access), len(refresh))
	}
}

// test_basic_provider.py TestProvider test_cross_secret_token_does_not_verify
func TestCrossSecretTokenDoesNotVerify(t *testing.T) {
	basicCase(t, "")
	p1, p2 := makeBasic(t), makeBasic(t)
	s := mustLogin(t, p1, "admin", "hunter2")
	if verified(t, p2, s.AccessToken) != nil {
		t.Fatal("token of another secret verified")
	}
}

// test_basic_provider.py TestProvider test_oauth_methods_raise_not_implemented
func TestOAuthMethodsRaiseNotImplemented(t *testing.T) {
	basicCase(t, "")
	p := makeBasic(t)
	var notImpl *gateway.DashboardAuthNotImplementedError
	if _, err := p.StartLogin(context.Background(), "https://x/auth/callback"); !errors.As(err, &notImpl) {
		t.Fatal(err)
	}
	if _, err := p.CompleteLogin(context.Background(), "c", "s", "v", "r"); !errors.As(err, &notImpl) {
		t.Fatal(err)
	}
}

// test_basic_provider.py TestProvider test_construction_validates_inputs
func TestConstructionValidatesInputs(t *testing.T) {
	basicCase(t, "")
	good := HashPassword("pw")
	long := []byte(strings.Repeat("x", 32))
	for _, c := range []struct {
		user, hash string
		secret     []byte
	}{{"", good, long}, {"admin", "", long}, {"admin", good, []byte("short")}} {
		_, err := NewBasicAuthProvider(c.user, c.hash, c.secret, basicDefaultTTLSeconds)
		var valueErr *gateway.DashboardAuthValueError
		if !errors.As(err, &valueErr) {
			t.Fatalf("%q %q: %v", c.user, c.hash, err)
		}
	}
}

// A skip carries its reason as the message and is logged with the tag at the level it names
func TestSkipRegistrationReasonAndLevel(t *testing.T) {
	var skip *skipRegistration
	if err := skipDebug("why"); !errors.As(err, &skip) || skip.reason != "why" || skip.level != "debug" || err.Error() != "why" {
		t.Fatalf("debug skip %+v", skip)
	}
	for cfg, want := range map[string]slog.Level{"": slog.LevelDebug, "dashboard:\n  basic_auth:\n    username: admin\n": slog.LevelWarn} {
		basicCase(t, cfg)
		records := captureSlog(t)
		registerBasic(t)
		found := false
		for _, r := range records() {
			found = found || (r.Level == want && r.Message == "dashboard-auth-basic: "+BasicLastSkipReason())
		}
		if !found || BasicLastSkipReason() == "" {
			t.Fatalf("%q no %v record of %q", cfg, want, BasicLastSkipReason())
		}
	}
}

// test_basic_provider.py TestRegister test_skips_when_no_username
func TestSkipsWhenNoUsername(t *testing.T) {
	basicCase(t, "")
	if registerBasic(t) != nil {
		t.Fatal("provider registered")
	}
	if !strings.Contains(BasicLastSkipReason(), "username") {
		t.Fatal(BasicLastSkipReason())
	}
}

// Register resolves every settings branch, password forms, ttl parsing and floor, and a too short secret
func TestBasicSettingsBranchesThroughRegister(t *testing.T) {
	basicCase(t, "dashboard:\n  basic_auth:\n    username: admin\n")
	if registerBasic(t) != nil || !strings.HasPrefix(BasicLastSkipReason(), "dashboard.basic_auth.username is set but neither password_hash nor password") {
		t.Fatalf("no password %q", BasicLastSkipReason())
	}
	secret := base64.StdEncoding.EncodeToString(randomBytes(32))
	basicCase(t, "dashboard:\n  basic_auth:\n    username: admin\n    password: plain-pw\n    secret: "+secret+"\n    session_ttl_seconds: 30\n")
	p := registerBasic(t)
	if p == nil {
		t.Fatalf("config plaintext %q", BasicLastSkipReason())
	}
	if s := mustLogin(t, p, "admin", "plain-pw"); s.ExpiresAt-time.Now().Unix() > 60 || p.ttl != 60 {
		t.Fatalf("ttl floor %d", p.ttl)
	}
	basicCase(t, "dashboard:\n  basic_auth:\n    username: admin\n    password: plain-pw\n    password_hash: '"+HashPassword("hash-pw")+"'\n    secret: "+secret+"\n    session_ttl_seconds: soon\n")
	p = registerBasic(t)
	if p == nil || p.ttl != basicDefaultTTLSeconds {
		t.Fatalf("config hash %v", p)
	}
	mustLogin(t, p, "admin", "hash-pw")
	if _, err := p.CompletePasswordLogin(context.Background(), "admin", "plain-pw"); err == nil {
		t.Fatal("config plaintext beat the config hash")
	}
	basicCase(t, "dashboard:\n  basic_auth:\n    username: admin\n    password: pw\n    secret: short\n")
	if registerBasic(t) != nil || BasicLastSkipReason() != "BasicAuthProvider construction failed: secret must be at least 16 bytes" {
		t.Fatalf("short secret %q", BasicLastSkipReason())
	}
}

// test_basic_provider.py TestRegister test_registers_with_env_plaintext_password
func TestRegistersWithEnvPlaintextPassword(t *testing.T) {
	basicCase(t, "")
	t.Setenv("ICEBATH_DASHBOARD_BASIC_AUTH_USERNAME", "admin")
	t.Setenv("ICEBATH_DASHBOARD_BASIC_AUTH_PASSWORD", "hunter2")
	p := registerBasic(t)
	if p == nil {
		t.Fatal("provider not registered")
	}
	if s := mustLogin(t, p, "admin", "hunter2"); s.UserID != "admin" {
		t.Fatal(s.UserID)
	}
	if BasicLastSkipReason() != "" {
		t.Fatal(BasicLastSkipReason())
	}
}

// test_basic_provider.py TestRegister test_env_password_overrides_config
func TestEnvPasswordOverridesConfig(t *testing.T) {
	basicCase(t, "dashboard:\n  basic_auth:\n    username: admin\n    password_hash: '"+HashPassword("config-pw")+"'\n")
	t.Setenv("ICEBATH_DASHBOARD_BASIC_AUTH_PASSWORD", "env-pw")
	p := registerBasic(t)
	if p == nil {
		t.Fatal("provider not registered")
	}
	mustLogin(t, p, "admin", "env-pw")
	_, err := p.CompletePasswordLogin(context.Background(), "admin", "config-pw")
	var invalid *gateway.InvalidCredentialsError
	if !errors.As(err, &invalid) {
		t.Fatal(err)
	}
}

// test_basic_provider.py TestRegister test_explicit_secret_makes_sessions_portable
func TestExplicitSecretMakesSessionsPortable(t *testing.T) {
	basicCase(t, "")
	t.Setenv("ICEBATH_DASHBOARD_BASIC_AUTH_USERNAME", "admin")
	t.Setenv("ICEBATH_DASHBOARD_BASIC_AUTH_PASSWORD", "hunter2")
	t.Setenv("ICEBATH_DASHBOARD_BASIC_AUTH_SECRET", hex.EncodeToString(randomBytes(32)))
	p1 := registerBasic(t)
	p2 := registerBasic(t)
	if p1 == nil || p2 == nil || p1 == p2 {
		t.Fatalf("%p %p", p1, p2)
	}
	s := mustLogin(t, p1, "admin", "hunter2")
	if verified(t, p2, s.AccessToken) == nil {
		t.Fatal("session not portable across providers sharing the secret")
	}
}

const basicLiteralSecret = "$(openssl rand -base64 32)"

// test_basic_secret_shell_text.py test_env_secret_with_shell_text_is_not_used
func TestEnvSecretWithShellTextIsNotUsed(t *testing.T) {
	for _, value := range []string{basicLiteralSecret, "`openssl rand -base64 32`", "prefix-$(head -c 32 /dev/urandom)",
		"<32+ random bytes; openssl rand -base64 32>", "$ (openssl rand -base64 32)", "openssl rand -hex 32"} {
		t.Run(value, func(t *testing.T) {
			basicCase(t, "")
			t.Setenv("ICEBATH_DASHBOARD_BASIC_AUTH_SECRET", value)
			records := captureSlog(t)
			var first, second []byte
			captureStderr(t, func() {
				first = basicResolveSecret(map[string]any{})
				second = basicResolveSecret(map[string]any{})
			})
			if string(first) == value || len(first) != 32 || string(first) == string(second) {
				t.Fatalf("first %x second %x", first, second)
			}
			var errs []string
			for _, r := range records() {
				if r.Level == slog.LevelError {
					errs = append(errs, r.Message)
				}
			}
			if len(errs) == 0 || !strings.Contains(errs[0], "unexpanded shell") {
				t.Fatal(errs)
			}
		})
	}
}

// test_basic_secret_shell_text.py test_shell_text_secret_is_reported_once_on_stderr
func TestShellTextSecretIsReportedOnceOnStderr(t *testing.T) {
	basicCase(t, "")
	basicShellTextSecretMu.Lock()
	prev := basicShellTextSecretShown
	basicShellTextSecretShown = false
	basicShellTextSecretMu.Unlock()
	t.Cleanup(func() {
		basicShellTextSecretMu.Lock()
		basicShellTextSecretShown = prev
		basicShellTextSecretMu.Unlock()
	})
	t.Setenv("ICEBATH_DASHBOARD_BASIC_AUTH_SECRET", "$(openssl rand -hex 32)")
	captureSlog(t)
	out := captureStderr(t, func() {
		basicResolveSecret(map[string]any{})
		basicResolveSecret(map[string]any{})
	})
	if strings.Count(out, "ICEBATH_DASHBOARD_BASIC_AUTH_SECRET") != 1 || len(strings.Split(strings.TrimSpace(out), "\n")) != 1 ||
		strings.Contains(out, "$(openssl rand -hex 32)") {
		t.Fatalf("%q", out)
	}
}

// test_basic_secret_shell_text.py test_config_secret_with_shell_text_is_not_used
func TestConfigSecretWithShellTextIsNotUsed(t *testing.T) {
	basicCase(t, "")
	captureSlog(t)
	var got []byte
	captureStderr(t, func() { got = basicResolveSecret(map[string]any{"secret": basicLiteralSecret}) })
	if string(got) == basicLiteralSecret {
		t.Fatal("literal used")
	}
}

// test_basic_secret_shell_text.py test_generated_secret_still_resolves
func TestGeneratedSecretStillResolves(t *testing.T) {
	basicCase(t, "")
	key := randomBytes(32)
	t.Setenv("ICEBATH_DASHBOARD_BASIC_AUTH_SECRET", base64.StdEncoding.EncodeToString(key))
	if got := basicResolveSecret(map[string]any{}); string(got) != string(key) {
		t.Fatalf("%x", got)
	}
}

// test_basic_secret_shell_text.py test_token_signed_with_the_literal_text_is_rejected
func TestTokenSignedWithTheLiteralTextIsRejected(t *testing.T) {
	basicCase(t, "")
	t.Setenv("ICEBATH_DASHBOARD_BASIC_AUTH_USERNAME", "admin")
	t.Setenv("ICEBATH_DASHBOARD_BASIC_AUTH_PASSWORD", "choose-a-strong-password")
	t.Setenv("ICEBATH_DASHBOARD_BASIC_AUTH_SECRET", basicLiteralSecret)
	captureSlog(t)
	var s basicSettings
	var err error
	captureStderr(t, func() { s, err = basicResolveSettings(context.Background()) })
	if err != nil {
		t.Fatal(err)
	}
	p, err := NewBasicAuthProvider(s.username, s.passwordHash, s.secret, s.ttlSeconds)
	if err != nil {
		t.Fatal(err)
	}
	claims := pyjson.NewObject()
	claims.Set("sub", "admin")
	claims.Set("kind", "access")
	claims.Set("exp", int64(1<<31-1))
	claims.Set("jti", "x")
	if verified(t, p, basicSign(claims, []byte(basicLiteralSecret))) != nil {
		t.Fatal("forged token verified")
	}
	session := mustLogin(t, p, "admin", "choose-a-strong-password")
	if verified(t, p, session.AccessToken) == nil {
		t.Fatal("own session rejected")
	}
}
