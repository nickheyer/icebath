package portal

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"strings"

	"github.com/google/uuid"
	"github.com/nickheyer/icebath/internal/model/auth"
)

const (
	EnvToolGatewayScheme    = "TOOL_GATEWAY_SCHEME"
	EnvToolGatewayDomain    = "TOOL_GATEWAY_DOMAIN"
	EnvToolGatewayUserToken = "TOOL_GATEWAY_USER_TOKEN"

	SessionHeader   = "x-portal-icebath-session"
	TurnHeader      = "x-portal-icebath-turn"
	ToolCallHeader  = "x-portal-icebath-tool-call"
	RequestIDHeader = "x-request-id"

	defaultToolGatewayDomain = "icebath.app"
	defaultToolGatewayScheme = "https"
)

type CallerIdentity interface {
	RequestSurface(ctx context.Context) string
	SendConsented(ctx context.Context) bool
	ConversationID(ctx context.Context) string
}

type ManagedToolGateway struct {
	Vendor        string
	GatewayOrigin string
	UserToken     auth.Secret
	ManagedMode   bool
}

type CallRef struct {
	TurnID     string
	ToolCallID string
}

func (a *Account) gatewayState(ctx context.Context) *object {
	state := a.quietState(ctx)
	if state == nil || (isGuestState(state) && !a.guestAllowed(ctx)) {
		return nil
	}
	return state
}

func (a *Account) userTokenOverride(ctx context.Context) string {
	value, _, err := a.secrets.Lookup(ctx, EnvToolGatewayUserToken)
	if err != nil {
		value = os.Getenv(EnvToolGatewayUserToken)
	}
	return strings.TrimSpace(value)
}

func (a *Account) PeekAccessToken(ctx context.Context) auth.Secret {
	ctx = a.bind(ctx)
	if explicit := a.userTokenOverride(ctx); explicit != "" {
		return auth.NewSecret(explicit)
	}
	return auth.NewSecret(strings.TrimSpace(heldConnectorsToken(a.gatewayState(ctx), nil)))
}

func (a *Account) ReadAccessToken(ctx context.Context) (auth.Secret, error) {
	ctx = a.bind(ctx)
	if explicit := a.userTokenOverride(ctx); explicit != "" {
		return auth.NewSecret(explicit), nil
	}
	state := a.gatewayState(ctx)
	if state.size() == 0 {
		return auth.Secret{}, nil
	}
	skew := auth.AccessTokenRefreshSkew
	if fresh := strings.TrimSpace(heldConnectorsToken(state, &skew)); fresh != "" {
		return auth.NewSecret(fresh), nil
	}
	cached := a.PeekAccessToken(ctx)
	refreshed, err := a.AccessToken(ctx, AccessTokenOptions{RefreshSkew: skew})
	if err != nil {
		var dead *CredentialDeadError
		if errors.As(err, &dead) {
			code := dead.Err.Code
			if code == "" {
				code = AnonCredentialDead
			}
			return a.replaceDeadGuestToken(ctx, state, code)
		}
		slog.Debug("Icebath Portal access token refresh failed", "error", fmt.Sprintf("%T", err))
		return cached, nil
	}
	if token := strings.TrimSpace(refreshed.Reveal()); token != "" {
		return auth.NewSecret(token), nil
	}
	return cached, nil
}

func (a *Account) replaceDeadGuestToken(ctx context.Context, dead *object, code string) (auth.Secret, error) {
	if err := a.clearDeadGuest(ctx, code, dead.str("anon_token")); err != nil {
		return auth.Secret{}, err
	}
	if code == AnonAccountLocked {
		return auth.Secret{}, nil
	}
	state, err := a.EnsureIdentity(ctx, EnsureOptions{})
	if err == nil && state != nil {
		var token auth.Secret
		if token, err = a.AccessToken(ctx, AccessTokenOptions{RefreshSkew: auth.AccessTokenRefreshSkew}); err == nil {
			return auth.NewSecret(strings.TrimSpace(token.Reveal())), nil
		}
	}
	if err != nil {
		slog.Debug("Icebath Portal free tier replacement after a retired credential failed", "error", fmt.Sprintf("%T", err))
	}
	return auth.Secret{}, nil
}

func ToolGatewayScheme() (string, error) {
	scheme := strings.ToLower(strings.TrimSpace(os.Getenv(EnvToolGatewayScheme)))
	if scheme == "" {
		scheme = defaultToolGatewayScheme
	}
	if scheme != "http" && scheme != "https" {
		return "", errors.New(EnvToolGatewayScheme + " must be 'http' or 'https'")
	}
	return scheme, nil
}

func VendorGatewayURL(vendor string) (string, error) {
	name := strings.ReplaceAll(strings.ToUpper(vendor), "-", "_") + "_GATEWAY_URL"
	if explicit := strings.TrimRight(strings.TrimSpace(os.Getenv(name)), "/"); explicit != "" {
		return explicit, nil
	}
	domain := strings.Trim(strings.TrimSpace(os.Getenv(EnvToolGatewayDomain)), "/")
	if domain == "" {
		domain = defaultToolGatewayDomain
	}
	scheme, err := ToolGatewayScheme()
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("%s://%s-gateway.%s", scheme, vendor, domain), nil
}

func (a *Account) vendorGateway(ctx context.Context, vendor string, peek bool) (*ManagedToolGateway, error) {
	origin, err := VendorGatewayURL(vendor)
	if err != nil {
		return nil, err
	}
	var token auth.Secret
	if peek {
		token = a.PeekAccessToken(ctx)
	} else if token, err = a.ReadAccessToken(ctx); err != nil {
		return nil, err
	}
	if origin == "" || token.Empty() {
		return nil, nil
	}
	return &ManagedToolGateway{Vendor: vendor, GatewayOrigin: origin, UserToken: token, ManagedMode: true}, nil
}

func (a *Account) ManagedToolsEnabled(ctx context.Context, forceFresh bool) bool {
	info := a.AccountInfo(ctx, AccountOptions{ForceFresh: forceFresh})
	return info.LoggedIn && info.ToolGatewayEntitled()
}

func (a *Account) ToolGatewayUnavailableMessage(ctx context.Context, capability string, forceFresh bool) string {
	if capability == "" {
		capability = "the Icebath Portal Tool Gateway"
	}
	info := a.AccountInfo(ctx, AccountOptions{ForceFresh: forceFresh})
	if message := EntitlementMessage(info, EntitlementOptions{Capability: capability, InChat: true}); message != "" {
		return message
	}
	return fmt.Sprintf("%s is unavailable. Run `icebath model` to refresh your Icebath Portal login and billing status.", capability)
}

type ToolGateway struct {
	Account *Account
}

func (g ToolGateway) Enabled(ctx context.Context, fresh bool) bool {
	return g.Account.ManagedToolsEnabled(ctx, fresh)
}

func (g ToolGateway) UnavailableMessage(ctx context.Context, capability string, fresh bool) string {
	return g.Account.ToolGatewayUnavailableMessage(ctx, capability, fresh)
}

func (a *Account) ResolveManagedToolGateway(ctx context.Context, vendor string) (*ManagedToolGateway, error) {
	if !a.ManagedToolsEnabled(ctx, false) {
		return nil, nil
	}
	return a.vendorGateway(a.bind(ctx), vendor, false)
}

func (a *Account) ResolveFreeSearchGateway(ctx context.Context) (*ManagedToolGateway, error) {
	return a.vendorGateway(a.bind(ctx), "perplexity", false)
}

func (a *Account) ManagedToolGatewayReady(ctx context.Context, vendor string) (bool, error) {
	if !a.ManagedToolsEnabled(ctx, false) {
		return false, nil
	}
	gateway, err := a.vendorGateway(a.bind(ctx), vendor, true)
	return gateway != nil, err
}

func ManagedGatewayOrigin() (string, error) { return VendorGatewayURL("tool") }

func ConnectorGatewayOrigin() (string, error) { return VendorGatewayURL("connector") }

func IsManagedGatewayURL(target string) bool {
	trimmed := strings.TrimSpace(target)
	if trimmed == "" {
		return false
	}
	scheme, netloc := splitOrigin(trimmed)
	if scheme == "" {
		return false
	}
	for _, label := range []string{"tool", "connector"} {
		origin, err := VendorGatewayURL(label)
		if err != nil {
			return false
		}
		if expectedScheme, expectedNetloc := splitOrigin(origin); scheme == expectedScheme && netloc == expectedNetloc {
			return true
		}
	}
	return false
}

func (a *Account) ManagedGatewayAuthHeaders(ctx context.Context, target string) http.Header {
	headers := http.Header{}
	if !IsManagedGatewayURL(target) {
		return headers
	}
	token, err := a.ReadAccessToken(ctx)
	if err != nil {
		slog.Debug("Managed gateway token read failed", "url", target, "error", fmt.Sprintf("%T", err))
		return headers
	}
	if strings.TrimSpace(token.Reveal()) == "" {
		return headers
	}
	headers.Set("Authorization", "Bearer "+strings.TrimSpace(token.Reveal()))
	headers.Set("User-Agent", a.GatewayUserAgent(ctx))
	return headers
}

func (a *Account) RequestSurface(ctx context.Context) string {
	surface := a.identity.RequestSurface(a.bind(ctx))
	if surface == "unknown" {
		return "other"
	}
	return surface
}

func osFamilyName() string {
	switch platformName() {
	case "darwin":
		return "macos"
	case "linux":
		return "linux"
	case "win32":
		return "windows"
	}
	return "unknown"
}

func (a *Account) GatewayUserAgent(ctx context.Context) string {
	return fmt.Sprintf("%s/%s (%s; %s)", clientName, a.version, a.RequestSurface(ctx), osFamilyName())
}

func (a *Account) CorrelationHeaders(ctx context.Context, call *CallRef) http.Header {
	headers := http.Header{}
	ctx = a.bind(ctx)
	if !a.identity.SendConsented(ctx) {
		return headers
	}
	headers.Set(RequestIDHeader, uuid.NewString())
	if id := a.identity.ConversationID(ctx); id != "" {
		headers.Set(SessionHeader, id)
	}
	if call != nil && call.TurnID != "" {
		headers.Set(TurnHeader, call.TurnID)
	}
	if call != nil && call.ToolCallID != "" {
		headers.Set(ToolCallHeader, call.ToolCallID)
	}
	return headers
}

func (a *Account) ConnectorHeaders(ctx context.Context, call *CallRef) http.Header {
	headers := a.CorrelationHeaders(ctx, call)
	headers.Set("User-Agent", a.GatewayUserAgent(ctx))
	return headers
}

func ResponseRequestID(header http.Header) string {
	return header.Get(RequestIDHeader)
}

func LogConnectorOutcome(route string, status int, code, requestID string, call *CallRef) {
	shown := "transport"
	if status != 0 {
		shown = fmt.Sprint(status)
	}
	dispatch := "-"
	if call != nil && call.ToolCallID != "" {
		dispatch = call.ToolCallID
	}
	line := fmt.Sprintf("connector %s %s %s request_id=%s dispatch=%s", route, shown, firstText(code, "-"), firstText(requestID, "-"), dispatch)
	if code != "" && code != "connection_not_found" {
		slog.Warn(line)
		return
	}
	slog.Debug(line)
}
