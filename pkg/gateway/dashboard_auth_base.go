package gateway

import (
	"context"
	"errors"
	"fmt"
	"reflect"

	"github.com/nickheyer/icebath/pkg/internal/pyjwt"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
)

// Verified interactive identity a dashboard auth provider mints, tokens opaque to Icebath
type DashboardSession struct {
	UserID       string
	Email        string
	DisplayName  string
	OrgID        string
	Provider     string
	ExpiresAt    int64
	AccessToken  string
	RefreshToken string
	Issuer       string
}

// Verified non interactive caller of one bearer token, empty scopes meaning unscoped
type TokenPrincipal struct {
	Principal string
	Provider  string
	Scopes    []string
}

// First leg of the OAuth round trip, the IDP authorize URL and the PKCE and CSRF cookies to set
type LoginStart struct {
	RedirectURL   string
	CookiePayload map[string]string
}

// IDP unreachable or transiently failing, answered with HTTP 503
type ProviderError struct{ Msg string }

func (e *ProviderError) Error() string      { return e.Msg }
func (e *ProviderError) PyTypeName() string { return "ProviderError" }

// OAuth callback code or state failed validation, answered with HTTP 400
type InvalidCodeError struct{ Msg string }

func (e *InvalidCodeError) Error() string      { return e.Msg }
func (e *InvalidCodeError) PyTypeName() string { return "InvalidCodeError" }

// Username and password rejected, answered with a generic 401
type InvalidCredentialsError struct{ Msg string }

func (e *InvalidCredentialsError) Error() string      { return e.Msg }
func (e *InvalidCredentialsError) PyTypeName() string { return "InvalidCredentialsError" }

// This provider rejects the refresh token, the middleware trying every other provider first
type RefreshExpiredError struct{ Msg string }

func (e *RefreshExpiredError) Error() string      { return e.Msg }
func (e *RefreshExpiredError) PyTypeName() string { return "RefreshExpiredError" }

// Default password or token method of a provider that never overrode it
type DashboardAuthNotImplementedError struct{ Msg string }

func (e *DashboardAuthNotImplementedError) Error() string      { return e.Msg }
func (e *DashboardAuthNotImplementedError) PyTypeName() string { return "NotImplementedError" }

// Duplicate dashboard auth provider name in the effective registry
type DashboardAuthValueError struct{ Msg string }

func (e *DashboardAuthValueError) Error() string      { return e.Msg }
func (e *DashboardAuthValueError) PyTypeName() string { return "ValueError" }

// Dashboard auth protocol violation of a provider class
type DashboardAuthTypeError struct{ Msg string }

func (e *DashboardAuthTypeError) Error() string      { return e.Msg }
func (e *DashboardAuthTypeError) PyTypeName() string { return "TypeError" }

// Whether a PyJWT exception class is the named class or one of its subclasses
func pyjwtIsInstance(class, base string) bool { return pyjwt.ClassIs(class, base) }

// Python repr of an exception, its class name around the quoted message
func exceptionRepr(err error) string {
	name := "Exception"
	var jwtErr *pyjwt.Error
	var named pyrepr.TypeNamer
	switch {
	case errors.As(err, &jwtErr):
		name = jwtErr.Class
	case errors.As(err, &named):
		name = named.PyTypeName()
	}
	return name + "(" + pyrepr.Repr(err.Error()) + ")"
}

// Maps a JWKS signing key lookup failure to ProviderError for transport faults and InvalidCodeError otherwise
func ClassifyJWKSLookupError(err error) error {
	var jwtErr *pyjwt.Error
	if !errors.As(err, &jwtErr) {
		return &ProviderError{Msg: "JWKS lookup failed: " + exceptionRepr(err)}
	}
	switch c := jwtErr.Class; {
	case pyjwtIsInstance(c, "PyJWKClientConnectionError"):
		return &ProviderError{Msg: "JWKS lookup failed: " + err.Error()}
	case pyjwtIsInstance(c, "DecodeError"), pyjwtIsInstance(c, "PyJWKSetError"):
		return &InvalidCodeError{Msg: "token not verifiable by this provider: " + err.Error()}
	case pyjwtIsInstance(c, "PyJWKClientError"):
		return &ProviderError{Msg: "JWKS lookup failed: " + err.Error()}
	case pyjwtIsInstance(c, "InvalidTokenError"):
		return &InvalidCodeError{Msg: "token not verifiable by this provider: " + err.Error()}
	}
	return &ProviderError{Msg: "JWKS lookup failed: " + exceptionRepr(err)}
}

// Protocol every dashboard auth provider implements, a nil session from verify meaning expired or unknown
type DashboardAuthProvider interface {
	Name() string
	DisplayName() string
	StartLogin(ctx context.Context, redirectURI string) (LoginStart, error)
	CompleteLogin(ctx context.Context, code, state, codeVerifier, redirectURI string) (*DashboardSession, error)
	VerifySession(ctx context.Context, accessToken string) (*DashboardSession, error)
	RefreshSession(ctx context.Context, refreshToken string) (*DashboardSession, error)
	RevokeSession(ctx context.Context, refreshToken string) error
}

// Capability flags and methods a provider overrides, the protocol defaults applying otherwise
type (
	dashboardAuthPassword interface{ SupportsPassword() bool }
	dashboardAuthToken    interface{ SupportsToken() bool }
	dashboardAuthSession  interface{ SupportsSession() bool }
	dashboardAuthLogin    interface{ InteractiveLogin() bool }
	dashboardAuthPassLog  interface {
		CompletePasswordLogin(ctx context.Context, username, password string) (*DashboardSession, error)
	}
	dashboardAuthTokenVerify interface {
		VerifyToken(ctx context.Context, token string) (*TokenPrincipal, error)
	}
)

// Whether the provider serves the username and password form, false by default
func DashboardAuthSupportsPassword(p any) bool {
	f, ok := p.(dashboardAuthPassword)
	return ok && f.SupportsPassword()
}

// Whether the token auth seam may ask the provider to verify a bearer, false by default
func DashboardAuthSupportsToken(p any) bool {
	f, ok := p.(dashboardAuthToken)
	return ok && f.SupportsToken()
}

// Whether the provider backs interactive cookie sessions, true by default
func DashboardAuthSupportsSession(p any) bool {
	f, ok := p.(dashboardAuthSession)
	return !ok || f.SupportsSession()
}

// Whether the provider offers a browser sign in flow, true by default
func DashboardAuthInteractiveLogin(p any) bool {
	f, ok := p.(dashboardAuthLogin)
	return !ok || f.InteractiveLogin()
}

// Python class name of a provider value
func providerClassName(p any) string {
	t := reflect.TypeOf(p)
	if t == nil {
		return "NoneType"
	}
	for t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	if t.Kind() == reflect.Struct && t.Name() != "" {
		return t.Name()
	}
	return pyrepr.TypeName(p)
}

// Verifies a username and password through the provider, NotImplementedError when it never overrode the method
func DashboardAuthCompletePasswordLogin(ctx context.Context, p DashboardAuthProvider, username, password string) (*DashboardSession, error) {
	if f, ok := p.(dashboardAuthPassLog); ok {
		return f.CompletePasswordLogin(ctx, username, password)
	}
	return nil, &DashboardAuthNotImplementedError{Msg: providerClassName(p) + " does not support password login " +
		"(set supports_password = True and override complete_password_login)"}
}

// Verifies a non interactive bearer through the provider, NotImplementedError when it never overrode the method
func DashboardAuthVerifyToken(ctx context.Context, p DashboardAuthProvider, token string) (*TokenPrincipal, error) {
	if f, ok := p.(dashboardAuthTokenVerify); ok {
		return f.VerifyToken(ctx, token)
	}
	return nil, &DashboardAuthNotImplementedError{Msg: providerClassName(p) + " does not support token auth " +
		"(set supports_token = True and override verify_token)"}
}

// TypeError naming the first missing attribute or method of a value that does not fully implement the protocol
func AssertDashboardAuthProtocol(p any) error {
	cls := providerClassName(p)
	named, ok := p.(interface{ Name() string })
	if !ok || named.Name() == "" {
		return &DashboardAuthTypeError{Msg: fmt.Sprintf("%s missing or empty attribute: 'name'", cls)}
	}
	display, ok := p.(interface{ DisplayName() string })
	if !ok || display.DisplayName() == "" {
		return &DashboardAuthTypeError{Msg: fmt.Sprintf("%s missing or empty attribute: 'display_name'", cls)}
	}
	checks := []struct {
		method string
		ok     bool
	}{
		{"start_login", implements[interface {
			StartLogin(context.Context, string) (LoginStart, error)
		}](p)},
		{"complete_login", implements[interface {
			CompleteLogin(context.Context, string, string, string, string) (*DashboardSession, error)
		}](p)},
		{"verify_session", implements[interface {
			VerifySession(context.Context, string) (*DashboardSession, error)
		}](p)},
		{"refresh_session", implements[interface {
			RefreshSession(context.Context, string) (*DashboardSession, error)
		}](p)},
		{"revoke_session", implements[interface {
			RevokeSession(context.Context, string) error
		}](p)},
	}
	for _, c := range checks {
		if !c.ok {
			return &DashboardAuthTypeError{Msg: fmt.Sprintf("%s missing method: %s", cls, c.method)}
		}
	}
	return nil
}

// Whether a value implements the interface T
func implements[T any](v any) bool {
	_, ok := v.(T)
	return ok
}
