package gateway

import (
	"context"
	"errors"
	"fmt"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/plugins"
)

// Base reference of the objects register_dashboard_auth_provider receives from an in process plugin in the host
var hostedDashboardAuthBase = plugins.HostInProcessObjectBases["register_dashboard_auth_provider"]

// Python dashboard auth provider an in process plugin registered, living in the plugin host and called by reference
type hostedDashboardAuthProvider struct{ obj *plugins.HostedObject }

// Plugin host object subclassing DashboardAuthProvider as a provider, any other value unchanged
func adaptHostedDashboardAuthProvider(p any) any {
	obj, ok := p.(*plugins.HostedObject)
	if !ok || obj == nil || obj.Base != hostedDashboardAuthBase || obj.Identity["is_base"] != true {
		return p
	}
	return hostedDashboardAuthProvider{obj: obj}
}

// Attribute read in the host, a failing read raising like the Python attribute access
func (h hostedDashboardAuthProvider) mustGet(name string) any {
	v, err := h.obj.Get(context.Background(), name)
	if err != nil {
		panic(err)
	}
	return v
}

func (h hostedDashboardAuthProvider) Name() string { return config.PyStr(h.mustGet("name")) }
func (h hostedDashboardAuthProvider) DisplayName() string {
	return config.PyStr(h.mustGet("display_name"))
}
func (h hostedDashboardAuthProvider) SupportsPassword() bool {
	return config.PyTruthy(h.mustGet("supports_password"))
}
func (h hostedDashboardAuthProvider) SupportsToken() bool {
	return config.PyTruthy(h.mustGet("supports_token"))
}
func (h hostedDashboardAuthProvider) SupportsSession() bool {
	return config.PyTruthy(h.mustGet("supports_session"))
}
func (h hostedDashboardAuthProvider) InteractiveLogin() bool {
	return config.PyTruthy(h.mustGet("interactive_login"))
}

// Protocol exception a provider raised in the host, as the Go error the auth routes answer for it
func hostedAuthError(err error) error {
	var remote *plugins.PluginHostError
	if !errors.As(err, &remote) {
		return err
	}
	switch remote.TypeName {
	case "ProviderError":
		return &ProviderError{Msg: remote.RemoteMessage}
	case "InvalidCodeError":
		return &InvalidCodeError{Msg: remote.RemoteMessage}
	case "InvalidCredentialsError":
		return &InvalidCredentialsError{Msg: remote.RemoteMessage}
	case "RefreshExpiredError":
		return &RefreshExpiredError{Msg: remote.RemoteMessage}
	}
	return err
}

// Calls one keyword only protocol method in the host
func (h hostedDashboardAuthProvider) call(ctx context.Context, method string, kwargs map[string]any) (any, error) {
	v, err := h.obj.Call(ctx, method, nil, kwargs)
	return v, hostedAuthError(err)
}

// Fields of the dataclass the host returned, the attribute error Python raises reading one off anything else
func authRecord(v any, want string) (map[string]any, error) {
	if rec, ok := v.(plugins.Record); ok {
		return rec, nil
	}
	return nil, &PyError{"AttributeError", fmt.Sprintf("'%s' object is not a %s", pyrepr.TypeName(v), want)}
}

// Session a hosted method returned, nil for None
func hostedSession(v any, err error) (*DashboardSession, error) {
	if err != nil || v == nil {
		return nil, err
	}
	f, err := authRecord(v, "Session")
	if err != nil {
		return nil, err
	}
	expires, err := sessionExpiry(f["expires_at"])
	if err != nil {
		return nil, err
	}
	return &DashboardSession{UserID: config.PyStr(f["user_id"]), Email: config.PyStr(f["email"]), DisplayName: config.PyStr(f["display_name"]),
		OrgID: config.PyStr(f["org_id"]), Provider: config.PyStr(f["provider"]), ExpiresAt: expires, AccessToken: config.PyStr(f["access_token"]),
		RefreshToken: config.PyStr(f["refresh_token"]), Issuer: config.PyStr(f["issuer"])}, nil
}

func (h hostedDashboardAuthProvider) StartLogin(ctx context.Context, redirectURI string) (LoginStart, error) {
	v, err := h.call(ctx, "start_login", map[string]any{"redirect_uri": redirectURI})
	if err != nil {
		return LoginStart{}, err
	}
	f, err := authRecord(v, "LoginStart")
	if err != nil {
		return LoginStart{}, err
	}
	cookies := map[string]string{}
	if m, ok := f["cookie_payload"].(map[string]any); ok {
		for k, val := range m {
			cookies[k] = config.PyStr(val)
		}
	}
	return LoginStart{RedirectURL: config.PyStr(f["redirect_url"]), CookiePayload: cookies}, nil
}

func (h hostedDashboardAuthProvider) CompleteLogin(ctx context.Context, code, state, codeVerifier, redirectURI string) (*DashboardSession, error) {
	return hostedSession(h.call(ctx, "complete_login", map[string]any{"code": code, "state": state, "code_verifier": codeVerifier, "redirect_uri": redirectURI}))
}

func (h hostedDashboardAuthProvider) VerifySession(ctx context.Context, accessToken string) (*DashboardSession, error) {
	return hostedSession(h.call(ctx, "verify_session", map[string]any{"access_token": accessToken}))
}

func (h hostedDashboardAuthProvider) RefreshSession(ctx context.Context, refreshToken string) (*DashboardSession, error) {
	return hostedSession(h.call(ctx, "refresh_session", map[string]any{"refresh_token": refreshToken}))
}

func (h hostedDashboardAuthProvider) RevokeSession(ctx context.Context, refreshToken string) error {
	_, err := h.call(ctx, "revoke_session", map[string]any{"refresh_token": refreshToken})
	return err
}

func (h hostedDashboardAuthProvider) CompletePasswordLogin(ctx context.Context, username, password string) (*DashboardSession, error) {
	return hostedSession(h.call(ctx, "complete_password_login", map[string]any{"username": username, "password": password}))
}

func (h hostedDashboardAuthProvider) VerifyToken(ctx context.Context, token string) (*TokenPrincipal, error) {
	v, err := h.call(ctx, "verify_token", map[string]any{"token": token})
	if err != nil || v == nil {
		return nil, err
	}
	f, err := authRecord(v, "TokenPrincipal")
	if err != nil {
		return nil, err
	}
	scopes := []string{}
	if items, ok := f["scopes"].([]any); ok {
		for _, s := range items {
			scopes = append(scopes, config.PyStr(s))
		}
	}
	return &TokenPrincipal{Principal: config.PyStr(f["principal"]), Provider: config.PyStr(f["provider"]), Scopes: scopes}, nil
}

// Unix expiry of a hosted Session, the int the dataclass declares or a float the plugin computed
func sessionExpiry(v any) (int64, error) {
	switch n := v.(type) {
	case int64:
		return n, nil
	case float64:
		return int64(n), nil
	case bool:
		if n {
			return 1, nil
		}
		return 0, nil
	}
	return 0, &PyError{"TypeError", fmt.Sprintf("Session.expires_at must be a number, not %s", pyrepr.TypeName(v))}
}
