package plugins_test

import ()

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
