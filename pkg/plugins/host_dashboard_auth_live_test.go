package plugins_test

import (
	"context"
	"errors"
	"testing"

	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/plugins"
)

const dashboardAuthPlugin = `
import dataclasses, sys, types

# Stands in for the upstream hermes_cli.dashboard_auth module the way a hermes test installs a module double
sys.modules.setdefault("hermes_cli", types.ModuleType("hermes_cli"))
_module = types.ModuleType("hermes_cli.dashboard_auth")
class DashboardAuthProvider:
    name = ""
    display_name = ""
    supports_password = False
    supports_token = False
    supports_session = True
    interactive_login = True
@dataclasses.dataclass(frozen=True)
class Session:
    user_id: str
    email: str
    display_name: str
    org_id: str
    provider: str
    expires_at: int
    access_token: str
    refresh_token: str
    issuer: str = ""
@dataclasses.dataclass(frozen=True)
class LoginStart:
    redirect_url: str
    cookie_payload: dict
@dataclasses.dataclass(frozen=True)
class TokenPrincipal:
    principal: str
    provider: str
    scopes: tuple = ()
class ProviderError(Exception):
    pass
class InvalidCredentialsError(Exception):
    pass
_module.DashboardAuthProvider = DashboardAuthProvider
sys.modules["hermes_cli.dashboard_auth"] = _module

REVOKED = []

def _session(user):
    return Session(user_id=user, email=user + "@acme", display_name=user.title(), org_id="acme", provider="acme-sso",
                   expires_at=1700000000, access_token="at-" + user, refresh_token="rt-" + user, issuer="https://idp")

class Acme(DashboardAuthProvider):
    name = "acme-sso"
    display_name = "Acme SSO"
    supports_password = True
    supports_token = True
    def start_login(self, *, redirect_uri):
        return LoginStart(redirect_url="https://idp/authorize?r=" + redirect_uri, cookie_payload={"state": "s1"})
    def complete_login(self, *, code, state, code_verifier, redirect_uri):
        return _session(code + state + code_verifier)
    def verify_session(self, *, access_token):
        return None if access_token == "bad" else _session(access_token)
    def refresh_session(self, *, refresh_token):
        raise ProviderError("idp down")
    def revoke_session(self, *, refresh_token):
        REVOKED.append(refresh_token)
        if len(REVOKED) > 1:
            raise RuntimeError("revoked twice")
    def complete_password_login(self, *, username, password):
        if password != "pw":
            raise InvalidCredentialsError("wrong password")
        return _session(username)
    def verify_token(self, *, token):
        return TokenPrincipal(principal="bot", provider=self.name, scopes=("read", "write"))

def register(ctx):
    ctx.register_dashboard_auth_provider(Acme())
`

func TestLiveInProcessPluginDashboardAuthProviderServesTheAuthGate(t *testing.T) {
	world := plugins.NewLoginWorld(t, "acme-auth", dashboardAuthPlugin)
	world.SetTier("verified")
	previous := plugins.InstalledServices()
	next := plugins.Services{}
	if previous != nil {
		next = *previous
	}
	next.DashboardAuth = gateway.DashboardAuthRegistryService()
	next.HostObject = func(kind string, obj any) (string, bool) {
		if kind == "dashboard_auth_provider" {
			return gateway.HostedDashboardAuthProviderName(obj)
		}
		return "", false
	}
	plugins.InstallServices(&next)
	t.Cleanup(func() {
		plugins.InstallServices(previous)
		gateway.ClearDashboardAuthProviders()
	})
	if err := world.InstallCatalogEntry("", false); err != nil {
		t.Fatal(err)
	}
	t.Setenv("ICEBATH_HOME", world.Home())
	world.Load(t, "plugins:\n  enabled: [acme-auth]\n")
	ctx := context.Background()
	p := gateway.GetDashboardAuthProvider(ctx, "acme-sso", "")
	if p == nil {
		t.Fatal("the hosted provider did not register")
	}
	if p.Name() != "acme-sso" || p.DisplayName() != "Acme SSO" || !gateway.DashboardAuthSupportsPassword(p) ||
		!gateway.DashboardAuthSupportsToken(p) || !gateway.DashboardAuthSupportsSession(p) || !gateway.DashboardAuthInteractiveLogin(p) {
		t.Fatalf("capabilities of %s", p.Name())
	}
	start, err := p.StartLogin(ctx, "https://dash/cb")
	if err != nil || start.RedirectURL != "https://idp/authorize?r=https://dash/cb" || start.CookiePayload["state"] != "s1" {
		t.Fatalf("start_login = %+v %v", start, err)
	}
	s, err := p.CompleteLogin(ctx, "c", "s", "v", "https://dash/cb")
	if err != nil || s.UserID != "csv" || s.Email != "csv@acme" || s.ExpiresAt != 1700000000 || s.AccessToken != "at-csv" || s.Issuer != "https://idp" {
		t.Fatalf("complete_login = %+v %v", s, err)
	}
	if s, err := p.VerifySession(ctx, "bad"); s != nil || err != nil {
		t.Fatalf("verify_session of an unknown token = %+v %v", s, err)
	}
	var down *gateway.ProviderError
	if _, err := p.RefreshSession(ctx, "rt"); !errors.As(err, &down) || down.Msg != "idp down" {
		t.Fatalf("refresh_session error = %#v", err)
	}
	if err := p.RevokeSession(ctx, "rt-1"); err != nil {
		t.Fatal(err)
	}
	if err := p.RevokeSession(ctx, "rt-2"); err == nil {
		t.Fatal("a plain plugin exception did not surface")
	}
	var wrong *gateway.InvalidCredentialsError
	if _, err := p.(interface {
		CompletePasswordLogin(context.Context, string, string) (*gateway.DashboardSession, error)
	}).CompletePasswordLogin(ctx, "amy", "nope"); !errors.As(err, &wrong) {
		t.Fatalf("password rejection = %#v", err)
	}
	principal, err := p.(interface {
		VerifyToken(context.Context, string) (*gateway.TokenPrincipal, error)
	}).VerifyToken(ctx, "tok")
	if err != nil || principal.Principal != "bot" || principal.Provider != "acme-sso" || len(principal.Scopes) != 2 || principal.Scopes[1] != "write" {
		t.Fatalf("verify_token = %+v %v", principal, err)
	}
}
