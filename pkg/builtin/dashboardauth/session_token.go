package dashboardauth

import (
	"context"
	"crypto/subtle"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/plugins"
)

const (
	sessionTokenTag            = "dashboard-auth-session-token"
	sessionTokenHorizonSeconds = 24 * 60 * 60
	sessionTokenNotInteractive = "SessionTokenProvider has no login flow: clients present the provisioned session " +
		"token directly (Authorization: Bearer, or ?token= on the /api/ws upgrade)."
)

var (
	sessionTokenSkipMu   sync.Mutex
	sessionTokenLastSkip string
)

// Reason the session token provider last declined to register, empty after a registration
func SessionTokenLastSkipReason() string {
	sessionTokenSkipMu.Lock()
	defer sessionTokenSkipMu.Unlock()
	return sessionTokenLastSkip
}

func setSessionTokenLastSkip(reason string) {
	sessionTokenSkipMu.Lock()
	sessionTokenLastSkip = reason
	sessionTokenSkipMu.Unlock()
}

// Whether dashboard.session_token_auth.enabled turns the provider off
func sessionTokenOptOut(ctx context.Context) bool {
	section := loadConfigSection(ctx, sessionTokenLog(), sessionTokenTag, "dashboard", "session_token_auth")
	switch strings.ToLower(pystr.Strip(pyrepr.Str(sectionGet(section, "enabled", true)))) {
	case "0", "false", "no", "off":
		return true
	}
	return false
}

// Provisioned deployment token, a skip when it is unset, disabled or its file unreadable
func sessionTokenSettings(ctx context.Context) (string, error) {
	if pystr.Strip(os.Getenv("ICEBATH_DASHBOARD_SESSION_TOKEN")) == "" {
		return "", skipDebug("ICEBATH_DASHBOARD_SESSION_TOKEN is not set. Provision it (a literal token, or " +
			"'file:<path>' pointing at a secret-manager-decrypted token file) to let the " +
			"deployment session token authorize the gated dashboard.")
	}
	if sessionTokenOptOut(ctx) {
		return "", skipDebug("dashboard.session_token_auth.enabled is false; the deployment session token " +
			"does not authenticate the gated dashboard.")
	}
	token, err := gateway.ResolveSessionToken()
	var exit *gateway.DashboardExit
	if errors.As(err, &exit) {
		return "", skipAt("error", exit.Error())
	}
	if err != nil {
		return "", err
	}
	if n := utf8.RuneCountInString(token); n < 32 {
		sessionTokenLog().Warn(fmt.Sprintf("%s: the provisioned session token is short (%d chars); use a "+
			"`secrets.token_urlsafe(32)`-shaped value for a deployment credential.", sessionTokenTag, n))
	}
	return token, nil
}

// Vouches for the provisioned deployment session token presented as a bearer or ws token
type SessionTokenProvider struct {
	nonInteractive
	token string
}

// Provider for a non empty token, a ValueError otherwise
func NewSessionTokenProvider(token string) (*SessionTokenProvider, error) {
	if token == "" {
		return nil, &gateway.DashboardAuthValueError{Msg: "token must be non-empty"}
	}
	return &SessionTokenProvider{nonInteractive: nonInteractive{notInteractive: sessionTokenNotInteractive,
		noStartLogin: sessionTokenNotInteractive}, token: token}, nil
}

func (p *SessionTokenProvider) Name() string { return "session-token" }

func (p *SessionTokenProvider) DisplayName() string { return "Deployment Session Token" }

func (p *SessionTokenProvider) SupportsPassword() bool { return false }

func (p *SessionTokenProvider) SupportsSession() bool { return true }

func (p *SessionTokenProvider) InteractiveLogin() bool { return false }

// Session of the deployment token for a constant time match, nil for anything else
func (p *SessionTokenProvider) VerifySession(_ context.Context, accessToken string) (*gateway.DashboardSession, error) {
	if accessToken == "" || subtle.ConstantTimeCompare([]byte(accessToken), []byte(p.token)) != 1 {
		return nil, nil
	}
	return &gateway.DashboardSession{UserID: "deployment-token", DisplayName: "Deployment session token", Provider: p.Name(),
		ExpiresAt: time.Now().Unix() + sessionTokenHorizonSeconds, AccessToken: accessToken}, nil
}

func (p *SessionTokenProvider) RefreshSession(context.Context, string) (*gateway.DashboardSession, error) {
	return nil, &gateway.RefreshExpiredError{Msg: "the deployment session token has no refresh token"}
}

func (p *SessionTokenProvider) RevokeSession(context.Context, string) error { return nil }

// Bundled session token dashboard auth plugin
type SessionTokenPlugin struct{}

func (SessionTokenPlugin) Manifest() plugins.Manifest {
	return plugins.Manifest{
		Name: "session-token", Key: "dashboard_auth/session_token", Version: "1.0.0", Author: "NousResearch", Kind: "backend",
		Description: "Dashboard auth provider — the deployment session token (ICEBATH_DASHBOARD_SESSION_TOKEN, including the file:<path> form the Nix modules render) as a first-class gated-mode credential. verify_session constant-time-compares the presented Authorization: Bearer / /api/ws ?token= credential against the provisioned token, so a token-only deployment satisfies the auth gate without basic-auth or OAuth — the shape the Desktop's 'Session token' remote-gateway field always promised. Not an interactive login: no login-page button, no cookies, no refresh (rotation is redeploying the secret). Disable with dashboard.session_token_auth.enabled: false.",
		RequiresEnv: []plugins.EnvSpec{{Name: "ICEBATH_DASHBOARD_SESSION_TOKEN"}},
	}
}

// Registers the session token provider when the deployment token is provisioned and enabled
func (SessionTokenPlugin) Register(pctx *plugins.Context) error {
	ctx := pctx.HomeContext()
	setSessionTokenLastSkip("")
	kwargs, reason, err := registerProvider(pctx, sessionTokenLog(), sessionTokenTag, "SessionTokenProvider",
		func() (string, error) { return sessionTokenSettings(ctx) },
		func(token string) (gateway.DashboardAuthProvider, error) { return NewSessionTokenProvider(token) })
	setSessionTokenLastSkip(reason)
	if err != nil {
		return err
	}
	if kwargs != nil {
		sessionTokenLog().Info(fmt.Sprintf("%s: registered deployment session-token provider — the provisioned "+
			"ICEBATH_DASHBOARD_SESSION_TOKEN authorizes the gated dashboard "+
			"(bearer REST + /api/ws ?token=).", sessionTokenTag))
	}
	return nil
}

func sessionTokenLog() *slog.Logger {
	return slog.Default().With("logger", "plugins.dashboard_auth.session_token")
}

func init() { plugins.RegisterBundled(SessionTokenPlugin{}) }
