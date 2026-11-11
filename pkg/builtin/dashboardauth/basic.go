package dashboardauth

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"fmt"
	"log/slog"
	"math"
	"math/big"
	"os"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/scrypt"

	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/internal/pybase64"
	"github.com/nickheyer/icebath/pkg/internal/pyint"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/plugins"
)

const (
	basicTag               = "dashboard-auth-basic"
	basicDefaultTTLSeconds = 12 * 60 * 60
	basicRefreshTTLSeconds = 30 * 24 * 60 * 60
	basicScryptN           = 1 << 14
	basicScryptR           = 8
	basicScryptP           = 1
	basicScryptDKLen       = 32
	basicScryptSaltBytes   = 16
	basicSigLen            = sha256.Size
	basicNotInteractive    = "BasicAuthProvider is password-only; use complete_password_login."
	basicNoStartLogin      = "BasicAuthProvider is password-only; there is no OAuth redirect flow. " +
		"The login page POSTs to /auth/password-login instead."
	opensslScryptMaxMem = 1024 * 1024 * 32
	opensslScryptPRMax  = 1<<30 - 1
)

var (
	basicSkipMu                 sync.Mutex
	basicLastSkip               string
	basicShellTextSecretMu      sync.Mutex
	basicShellTextSecretShown   bool
	basicUnexpandedShellMarkers = []string{"$(", "`", "openssl rand"}
	basicDummyHash              = HashPassword("dummy-password-for-constant-time-verify")
	basicClock                  = time.Now
)

func basicLog() *slog.Logger { return slog.Default().With("logger", "plugins.dashboard_auth.basic") }

// Reason the basic provider last declined to register, empty after a registration
func BasicLastSkipReason() string {
	basicSkipMu.Lock()
	defer basicSkipMu.Unlock()
	return basicLastSkip
}

func setBasicLastSkip(reason string) {
	basicSkipMu.Lock()
	basicLastSkip = reason
	basicSkipMu.Unlock()
}

// Scrypt hash in the scrypt$n$r$p$salt$key text form with a fresh random salt
func HashPassword(password string) string {
	salt := tokenBytes(basicScryptSaltBytes)
	dk, err := scrypt.Key([]byte(password), salt, basicScryptN, basicScryptR, basicScryptP, basicScryptDKLen)
	if err != nil {
		panic(err)
	}
	return fmt.Sprintf("scrypt$%d$%d$%d$%s$%s", basicScryptN, basicScryptR, basicScryptP,
		base64.StdEncoding.EncodeToString(salt), base64.StdEncoding.EncodeToString(dk))
}

// Unsigned long of a hashlib scrypt cost argument, false for a negative value and an OverflowError past 64 bits
func scryptULong(n *big.Int) (uint64, bool, error) {
	if n.Sign() < 0 {
		return 0, false, nil
	}
	if !n.IsUint64() {
		return 0, false, &pyint.Error{Kind: "OverflowError", Msg: "Python int too large for C unsigned long"}
	}
	return n.Uint64(), true, nil
}

// Whether hashlib scrypt with the default OpenSSL memory cap accepts n, r, p and the key length
func scryptParamsAccepted(n, r, p uint64, passLen, saltLen, dkLen int) bool {
	if passLen > math.MaxInt32 || saltLen > math.MaxInt32 || dkLen < 1 || dkLen > math.MaxInt32 {
		return false
	}
	if n < 2 || n&(n-1) != 0 || r == 0 || p == 0 || r > math.MaxUint32 || p > math.MaxUint32 {
		return false
	}
	if p > opensslScryptPRMax/r {
		return false
	}
	if 16*r <= 63 && n >= uint64(1)<<(16*r) {
		return false
	}
	bLen := p * 128 * r
	if bLen > math.MaxInt32 {
		return false
	}
	if n+2 > (math.MaxUint64/128)/r {
		return false
	}
	vLen := 128 * r * (n + 2)
	if bLen > math.MaxUint64-vLen {
		return false
	}
	return bLen+vLen <= opensslScryptMaxMem
}

// Constant time scrypt verify, false on any malformed hash and an error for a cost past 64 bits
func verifyPassword(password, encoded string) (bool, error) {
	parts := strings.Split(encoded, "$")
	if len(parts) != 6 || parts[0] != "scrypt" {
		return false, nil
	}
	var costs [3]*big.Int
	for i, text := range parts[1:4] {
		v, ok := pyint.Parse(text)
		if !ok {
			return false, nil
		}
		costs[i] = v
	}
	salt, err := pybase64.B64Decode(parts[4])
	if err != nil {
		return false, nil
	}
	expected, err := pybase64.B64Decode(parts[5])
	if err != nil {
		return false, nil
	}
	var params [3]uint64
	for i, cost := range costs {
		v, ok, err := scryptULong(cost)
		if err != nil {
			return false, err
		}
		if !ok {
			return false, nil
		}
		params[i] = v
	}
	if !scryptParamsAccepted(params[0], params[1], params[2], len(password), len(salt), len(expected)) {
		return false, nil
	}
	actual, err := scrypt.Key([]byte(password), salt, int(params[0]), int(params[1]), int(params[2]), len(expected))
	if err != nil {
		return false, nil
	}
	return hmac.Equal(actual, expected), nil
}

// Url safe base64 of the compact JSON payload followed by its HMAC SHA256
func basicSign(payload *pyjson.Object, secret []byte) string {
	raw, err := pyjson.Dumps(payload, pyjson.Separators(",", ":"))
	if err != nil {
		panic(err)
	}
	mac := hmac.New(sha256.New, secret)
	mac.Write([]byte(raw))
	return base64.URLEncoding.EncodeToString(append([]byte(raw), mac.Sum(nil)...))
}

// Signed payload of kind that is unexpired, nil for anything else, an error when a signed payload is not comparable
func basicUnsign(token string, secret []byte, kind string) (*pyjson.Object, error) {
	ascii := make([]byte, 0, len(token))
	for i := range len(token) {
		if token[i] < 0x80 {
			ascii = append(ascii, token[i])
		}
	}
	blob, err := pybase64.URLSafeB64Decode(string(ascii))
	if err != nil || len(blob) <= basicSigLen {
		return nil, nil
	}
	raw, sig := blob[:len(blob)-basicSigLen], blob[len(blob)-basicSigLen:]
	mac := hmac.New(sha256.New, secret)
	mac.Write(raw)
	if !hmac.Equal(sig, mac.Sum(nil)) {
		return nil, nil
	}
	decoded, err := pyjson.LoadsBytes(raw)
	if err != nil {
		return nil, nil
	}
	payload, isDict := decoded.(*pyjson.Object)
	if !isDict {
		return nil, fmt.Errorf("'%s' object has no attribute 'get'", pyrepr.TypeName(decoded))
	}
	if got, ok := payload.GetOr("kind", nil).(string); !ok || got != kind {
		return nil, nil
	}
	expired, err := payloadExpired(payload.GetOr("exp", int64(0)), basicClock().Unix())
	if err != nil || expired {
		return nil, err
	}
	return payload, nil
}

// Python exp <= now for a JSON value, the TypeError of an incomparable one
func payloadExpired(exp any, now int64) (bool, error) {
	switch v := exp.(type) {
	case int64:
		return v <= now, nil
	case *big.Int:
		return v.Cmp(big.NewInt(now)) <= 0, nil
	case float64:
		return v <= float64(now), nil
	case bool:
		if v {
			return 1 <= now, nil
		}
		return 0 <= now, nil
	}
	return false, fmt.Errorf("'<=' not supported between instances of '%s' and 'int'", pyrepr.TypeName(exp))
}

// Python int of an unexpired exp, saturated to the session field and the error of a NaN or infinite float
func payloadExpInt(exp any) (int64, error) {
	switch v := exp.(type) {
	case int64:
		return v, nil
	case *big.Int:
		if v.IsInt64() {
			return v.Int64(), nil
		}
		return math.MaxInt64, nil
	case float64:
		if math.IsNaN(v) {
			return 0, &pyint.Error{Kind: "ValueError", Msg: "cannot convert float NaN to integer"}
		}
		if math.IsInf(v, 0) {
			return 0, &pyint.Error{Kind: "OverflowError", Msg: "cannot convert float infinity to integer"}
		}
		if v >= math.MaxInt64 {
			return math.MaxInt64, nil
		}
		return int64(v), nil
	case bool:
		if v {
			return 1, nil
		}
		return 0, nil
	}
	return 0, fmt.Errorf("int() argument must be a string, a bytes-like object or a real number, not '%s'", pyrepr.TypeName(exp))
}

// Username and password provider with stateless HMAC signed sessions
type BasicAuthProvider struct {
	nonInteractive
	username     string
	passwordHash string
	secret       []byte
	ttl          int64
}

// Provider for a username, password hash and a secret of at least 16 bytes, the ttl floored at a minute
func NewBasicAuthProvider(username, passwordHash string, secret []byte, ttlSeconds int64) (*BasicAuthProvider, error) {
	if username == "" {
		return nil, &gateway.DashboardAuthValueError{Msg: "username must be non-empty"}
	}
	if passwordHash == "" {
		return nil, &gateway.DashboardAuthValueError{Msg: "password_hash must be non-empty"}
	}
	if len(secret) < 16 {
		return nil, &gateway.DashboardAuthValueError{Msg: "secret must be at least 16 bytes"}
	}
	return &BasicAuthProvider{nonInteractive: nonInteractive{notInteractive: basicNotInteractive, noStartLogin: basicNoStartLogin},
		username: username, passwordHash: passwordHash, secret: secret, ttl: max(60, ttlSeconds)}, nil
}

func (p *BasicAuthProvider) Name() string { return "basic" }

func (p *BasicAuthProvider) DisplayName() string { return "Username & Password" }

func (p *BasicAuthProvider) SupportsPassword() bool { return true }

// Session for the configured credentials after a scrypt verify that runs for unknown usernames too
func (p *BasicAuthProvider) CompletePasswordLogin(_ context.Context, username, password string) (*gateway.DashboardSession, error) {
	usernameOK := subtle.ConstantTimeCompare([]byte(username), []byte(p.username)) == 1
	encoded := basicDummyHash
	if usernameOK {
		encoded = p.passwordHash
	}
	passwordOK, err := verifyPassword(password, encoded)
	if err != nil {
		return nil, err
	}
	if !usernameOK || !passwordOK {
		return nil, &gateway.InvalidCredentialsError{Msg: "invalid username or password"}
	}
	return p.mintSession(p.username), nil
}

// Session of a valid unexpired access token, nil otherwise
func (p *BasicAuthProvider) VerifySession(_ context.Context, accessToken string) (*gateway.DashboardSession, error) {
	payload, err := basicUnsign(accessToken, p.secret, "access")
	if err != nil || payload == nil {
		return nil, err
	}
	exp, err := payloadExpInt(payload.GetOr("exp", nil))
	if err != nil {
		return nil, err
	}
	return p.session(pyrepr.Str(payload.GetOr("sub", "")), exp, accessToken, ""), nil
}

// Fresh session for a valid refresh token, RefreshExpiredError for a missing or invalid one
func (p *BasicAuthProvider) RefreshSession(_ context.Context, refreshToken string) (*gateway.DashboardSession, error) {
	if refreshToken == "" {
		return nil, &gateway.RefreshExpiredError{Msg: "no refresh token present in session"}
	}
	payload, err := basicUnsign(refreshToken, p.secret, "refresh")
	if err != nil {
		return nil, err
	}
	if payload == nil {
		return nil, &gateway.RefreshExpiredError{Msg: "refresh token expired or invalid"}
	}
	return p.mintSession(pyrepr.Str(payload.GetOr("sub", p.username))), nil
}

func (p *BasicAuthProvider) RevokeSession(context.Context, string) error { return nil }

// Access and refresh tokens sharing a fresh jti so two sessions minted in one second differ
func (p *BasicAuthProvider) mintSession(userID string) *gateway.DashboardSession {
	now := basicClock().Unix()
	exp := now + p.ttl
	if exp < now {
		exp = math.MaxInt64
	}
	jti := b64urlNoPad(tokenBytes(16))
	claims := func(kind string, exp int64) *pyjson.Object {
		o := pyjson.NewObject()
		o.Set("sub", userID)
		o.Set("kind", kind)
		o.Set("exp", exp)
		o.Set("jti", jti)
		return o
	}
	return p.session(userID, exp, basicSign(claims("access", exp), p.secret),
		basicSign(claims("refresh", now+basicRefreshTTLSeconds), p.secret))
}

func (p *BasicAuthProvider) session(userID string, exp int64, accessToken, refreshToken string) *gateway.DashboardSession {
	return &gateway.DashboardSession{UserID: userID, DisplayName: userID, Provider: p.Name(), ExpiresAt: exp,
		AccessToken: accessToken, RefreshToken: refreshToken}
}

// Mapping under dashboard.basic_auth of the active config, empty when absent or unloadable
func loadConfigBasicAuthSection(ctx context.Context) map[string]any {
	return loadConfigSection(ctx, basicLog(), basicTag, "dashboard", "basic_auth")
}

// Token signing secret from base64, hex or raw text, a random per process key when unset or holding shell text
func basicResolveSecret(section map[string]any) []byte {
	raw := resolveEnvOrCfg("ICEBATH_DASHBOARD_BASIC_AUTH_SECRET", sectionGet(section, "secret", nil))
	if raw == "" {
		basicLog().Info("dashboard-auth-basic: no 'secret' configured; generating a random " +
			"per-process signing key. Sessions will not survive a restart or span " +
			"multiple workers. Set dashboard.basic_auth.secret (or " +
			"ICEBATH_DASHBOARD_BASIC_AUTH_SECRET) for stable sessions.")
		return tokenBytes(32)
	}
	for _, marker := range basicUnexpandedShellMarkers {
		if !strings.Contains(raw, marker) {
			continue
		}
		basicLog().Error("dashboard-auth-basic: the configured signing secret contains unexpanded shell " +
			"text (such as '$(openssl rand -base64 32)') instead of a random value, so it " +
			"is ignored and a random per-process signing key is used. Set " +
			"ICEBATH_DASHBOARD_BASIC_AUTH_SECRET (or dashboard.basic_auth.secret) to the " +
			"output of 'openssl rand -base64 32' to keep sessions across restarts.")
		basicShellTextSecretMu.Lock()
		if !basicShellTextSecretShown {
			basicShellTextSecretShown = true
			fmt.Fprintln(os.Stderr, "icebath: dashboard login: ICEBATH_DASHBOARD_BASIC_AUTH_SECRET (or "+
				"dashboard.basic_auth.secret) holds shell text instead of a random value; "+
				"using a per-process key, so sessions end on restart. Replace it with the "+
				"output of: openssl rand -base64 32")
		}
		basicShellTextSecretMu.Unlock()
		return tokenBytes(32)
	}
	for _, decode := range []func(string) ([]byte, error){pybase64.B64Decode, pystr.BytesFromHex} {
		if decoded, err := decode(raw); err == nil && len(decoded) >= 16 {
			return decoded
		}
	}
	return []byte(raw)
}

// Constructor arguments of the basic provider resolved from env and config
type basicSettings struct {
	username     string
	passwordHash string
	secret       []byte
	ttlSeconds   int64
}

// Provider settings from env over config, a skip when the username or every password form is missing
func basicResolveSettings(ctx context.Context) (basicSettings, error) {
	section := loadConfigBasicAuthSection(ctx)
	setting := func(envName, cfgKey string) string { return resolveEnvOrCfg(envName, sectionGet(section, cfgKey, "")) }
	username := setting("ICEBATH_DASHBOARD_BASIC_AUTH_USERNAME", "username")
	passwordHash := setting("ICEBATH_DASHBOARD_BASIC_AUTH_PASSWORD_HASH", "password_hash")
	plaintext := setting("ICEBATH_DASHBOARD_BASIC_AUTH_PASSWORD", "password")
	ttlRaw := setting("ICEBATH_DASHBOARD_BASIC_AUTH_TTL_SECONDS", "session_ttl_seconds")
	if username == "" {
		return basicSettings{}, skipDebug("dashboard.basic_auth.username is not set (and ICEBATH_DASHBOARD_BASIC_AUTH_USERNAME " +
			"is empty). Set a username and a password (or password_hash) under " +
			"dashboard.basic_auth in config.yaml to enable username/password dashboard " +
			"login, or use the OAuth provider, or pass --insecure to skip the auth gate.")
	}
	if passwordHash == "" && plaintext == "" {
		return basicSettings{}, skipAt("warning", "dashboard.basic_auth.username is set but neither password_hash nor password "+
			"is configured. Provide one of them (password_hash is preferred — compute it "+
			"with plugins.dashboard_auth.basic.hash_password).")
	}
	if fromEnv := pystr.Strip(os.Getenv("ICEBATH_DASHBOARD_BASIC_AUTH_PASSWORD")); fromEnv != "" {
		passwordHash = HashPassword(fromEnv)
		basicLog().Info("dashboard-auth-basic: hashed env-supplied password in-memory (overrides any config password_hash).")
	} else if passwordHash == "" {
		passwordHash = HashPassword(plaintext)
		basicLog().Info("dashboard-auth-basic: hashed plaintext password in-memory. " +
			"For production, precompute dashboard.basic_auth.password_hash " +
			"and remove the plaintext password from config.")
	}
	ttl := int64(basicDefaultTTLSeconds)
	if ttlRaw != "" {
		if v, ok := pyint.Parse(ttlRaw); ok {
			switch {
			case v.IsInt64():
				ttl = v.Int64()
			case v.Sign() > 0:
				ttl = math.MaxInt64
			default:
				ttl = math.MinInt64
			}
		}
	}
	return basicSettings{username: username, passwordHash: passwordHash, secret: basicResolveSecret(section), ttlSeconds: ttl}, nil
}

// Bundled username and password dashboard auth plugin
type BasicPlugin struct{}

func (BasicPlugin) Manifest() plugins.Manifest {
	return plugins.Manifest{
		Name: "basic", Key: "dashboard_auth/basic", Version: "1.0.0", Author: "NousResearch", Kind: "backend",
		Description: "Dashboard auth provider — username/password (no OAuth IDP). A self-hosted 'just put a password on my dashboard' provider. Activates when dashboard.basic_auth.username plus a password (or password_hash) are configured via config.yaml (canonical surface) or the ICEBATH_DASHBOARD_BASIC_AUTH_* env vars. Sessions are stateless HMAC-signed tokens minted by the provider; password hashing uses stdlib scrypt (no third-party dependency). Set dashboard.basic_auth.secret for restart-surviving / multi-worker sessions.",
		RequiresEnv: []plugins.EnvSpec{{Name: "ICEBATH_DASHBOARD_BASIC_AUTH_USERNAME"}},
	}
}

// Registers the basic provider when a username and a password or password hash are configured
func (BasicPlugin) Register(pctx *plugins.Context) error {
	ctx := pctx.HomeContext()
	setBasicLastSkip("")
	kwargs, reason, err := registerProvider(pctx, basicLog(), basicTag, "BasicAuthProvider",
		func() (basicSettings, error) { return basicResolveSettings(ctx) },
		func(s basicSettings) (gateway.DashboardAuthProvider, error) {
			return NewBasicAuthProvider(s.username, s.passwordHash, s.secret, s.ttlSeconds)
		})
	setBasicLastSkip(reason)
	if err != nil {
		return err
	}
	if kwargs != nil {
		basicLog().Info(fmt.Sprintf("dashboard-auth-basic: registered password provider (username=%s)", kwargs.username))
	}
	return nil
}

func init() { plugins.RegisterBundled(BasicPlugin{}) }
