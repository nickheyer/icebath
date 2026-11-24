package gatewayapi

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/nickheyer/icebath/pkg/gateway"
	anyutil "github.com/nickheyer/icebath/pkg/utils/any"
	jsonutil "github.com/nickheyer/icebath/pkg/utils/json"
)

// RoomGrantReauthorizationRequired is a validly signed room grant that was revoked or superseded
type RoomGrantReauthorizationRequired struct{ Msg string }

func (e *RoomGrantReauthorizationRequired) Error() string { return e.Msg }

// JSON error response with the OpenAI envelope, code and type optional
func jsonError(message, code string, status int) *gateway.WebResponse {
	var codeValue any
	if code != "" {
		codeValue = code
	}
	return errorResponse(message, status, errOpts{Code: codeValue})
}

// 401 for an invalid grant, 403 reauthorization for a revoked or superseded one
func roomGrantErrorResponse(err error) *gateway.WebResponse {
	message, code, status := "Room authorization is invalid or expired.", "invalid_room_grant", 401
	var reauth *RoomGrantReauthorizationRequired
	if errors.As(err, &reauth) {
		message, code, status = "Room authorization needs to be renewed.", "room_reauthorization_required", 403
	}
	return errorResponse(message, status, errOpts{Type: "gateway_auth_error", Code: code})
}

// Room grant bearer token of a request, empty without the IcebathRoom scheme or the old one an older peer sends
func roomGrantToken(request *gateway.WebRequest) string {
	scheme, token, found := strings.Cut(request.Header.Get("Authorization"), " ")
	if lower := strings.ToLower(scheme); !found || (lower != "icebathroom" && lower != "hermesroom") {
		return ""
	}
	return strings.TrimSpace(token)
}

// Room identity fields every grant and invitation carries
var roomIdentityFields = []string{"room_id", "home_install_id", "authority_gateway_id", "authority_epoch", "member_id"}

// Refuses a renewal whose live execution policy digest differs from the one the grant was issued for
func requireUnchangedExecutionPolicy(claims, executionPolicy *jsonutil.Object) error {
	digest := func(value any) string {
		if !anyutil.IsTrue(value) {
			return ""
		}
		return anyutil.String(value)
	}
	if digest(executionPolicy.ValueOr("policy_digest", nil)) != digest(claims.ValueOr("execution_policy_digest", nil)) {
		return &RoomGrantReauthorizationRequired{"room execution policy changed"}
	}
	return nil
}

// Status horizon of a grant, its dispatch expiry when it carries none
func hardExpiry(claims *jsonutil.Object) (float64, error) {
	value, ok := claims.Get("status_expires_at")
	if !ok {
		if value, ok = claims.Get("expires_at"); !ok {
			return 0, &gateway.PyError{Kind: "KeyError", Msg: "'expires_at'"}
		}
	}
	return anyutil.ToFloat(value)
}

// Room authority values of a mapping for an issued grant, body values coerced to text and int
func roomIdentity(source *jsonutil.Object, coerce bool) (map[string]any, error) {
	out := map[string]any{}
	for _, key := range roomIdentityFields {
		value, ok := source.Get(key)
		if !ok {
			return nil, &gateway.PyError{Kind: "KeyError", Msg: anyutil.Quote(key)}
		}
		switch {
		case key == "authority_epoch":
			n, err := anyutil.ToInt(value)
			if err != nil {
				return nil, err
			}
			out[key] = n
		case coerce:
			out[key] = anyutil.String(value)
		default:
			out[key] = value
		}
	}
	return out, nil
}

// Profile and installation this gateway serves the request for, refused when the claims target another
func localTarget(ctx context.Context, claims *jsonutil.Object) (string, string, error) {
	profile := requestProfile(ctx)
	if profile == "" {
		profile = "default"
	}
	installationID, err := gateway.LocalAuthorityGatewayID(ctx)
	if err != nil {
		return "", "", err
	}
	if claims != nil && (!anyutil.Equal(claims.ValueOr("target_profile", nil), profile) ||
		!anyutil.Equal(claims.ValueOr("target_install_id", nil), installationID)) {
		return "", "", &gateway.PyError{Kind: "ValueError", Msg: "room grant target does not match this profile"}
	}
	return profile, installationID, nil
}

// Gateway owned RoomLink signing secret, never derived from a client known API key
func (a *APIServerAdapter) roomGrantSecret(ctx context.Context) ([]byte, error) {
	return gateway.GatewayRoomGrantSecret(ctx, "")
}

// Grant claims of a request after signature, scope and horizon checks, without the revocation lookup
func (a *APIServerAdapter) decodeRequestGrant(ctx context.Context, request *gateway.WebRequest, permission string) (*jsonutil.Object, error) {
	token := roomGrantToken(request)
	if token == "" {
		return nil, &gateway.PyError{Kind: "ValueError", Msg: "room grant is missing"}
	}
	secret, err := a.roomGrantSecret(ctx)
	if err != nil {
		return nil, err
	}
	return gateway.DecodeRoomGrant(secret, token, permission, nil)
}

// Grant claims of a request that are neither revoked nor superseded by the target's live reservation
func (a *APIServerAdapter) roomGrantClaims(ctx context.Context, request *gateway.WebRequest, permission string) (*jsonutil.Object, error) {
	claims, err := a.decodeRequestGrant(ctx, request, permission)
	if err != nil {
		return nil, err
	}
	dbPath := gateway.HostedRoomsDefaultDBPath(ctx)
	revoked, err := gateway.RoomGrantIsRevoked(ctx, dbPath, claims, nil)
	if err != nil {
		return nil, err
	}
	if revoked {
		return nil, &RoomGrantReauthorizationRequired{"room grant is revoked"}
	}
	current, err := gateway.PeerRoomGrantIsCurrent(ctx, dbPath, claims, nil)
	if err != nil {
		return nil, err
	}
	if !current {
		return nil, &RoomGrantReauthorizationRequired{"room grant is no longer current"}
	}
	return claims, nil
}

// Room member routes the API server serves, its file routes included
func (a *APIServerAdapter) roomGrantHTTPRoutes() []httpRoute {
	return append([]httpRoute{
		{"POST", "/v1/room-members/invitations", a.handleRoomMemberInvitation},
		{"GET", "/v1/room-members/capabilities", a.handleRoomMemberCapabilities},
		{"POST", "/v1/room-members/grants/refresh", a.handleRoomMemberGrantRefresh},
		{"POST", "/v1/room-members/grants/revoke", a.handleRoomMemberGrantRevoke}}, a.roomFileHTTPRoutes()...)
}

// Wall clock seconds
func wallClock() float64 { return float64(time.Now().UnixNano()) / 1e9 }

// Mints a short lived room and profile grant for a trusted home gateway and reserves the peer room
func (a *APIServerAdapter) handleRoomMemberInvitation(ctx context.Context, request *gateway.WebRequest) (*gateway.WebResponse, error) {
	if authErr := a.checkAuth(ctx, request); authErr != nil {
		return authErr, nil
	}
	body, errResp := readJSONBody(request)
	if errResp != nil {
		return errResp, nil
	}
	allowed := map[string]bool{"grant_id": true, "ttl_seconds": true, "status_ttl_seconds": true}
	for _, field := range roomIdentityFields {
		allowed[field] = true
	}
	present := map[string]bool{}
	for _, key := range body.Keys() {
		present[key] = true
		if !allowed[key] {
			return jsonError("Invitation is missing required room authority fields.", "invalid_room_invitation", http.StatusBadRequest), nil
		}
	}
	for _, field := range roomIdentityFields {
		if !present[field] {
			return jsonError("Invitation is missing required room authority fields.", "invalid_room_invitation", http.StatusBadRequest), nil
		}
	}
	var token, profile string
	var catalog, claims *jsonutil.Object
	err := func() error {
		var targetInstallID string
		var err error
		if profile, targetInstallID, err = localTarget(ctx, nil); err != nil {
			return err
		}
		ttl, err := anyutil.ToFloat(body.ValueOr("ttl_seconds", int64(3600)))
		if err != nil {
			return err
		}
		if !(60 <= ttl && ttl <= 24*60*60) {
			return &gateway.PyError{Kind: "ValueError", Msg: "ttl_seconds must be between 60 and 86400"}
		}
		statusTTL, err := anyutil.ToFloat(body.ValueOr("status_ttl_seconds", ttl))
		if err != nil {
			return err
		}
		if !(ttl <= statusTTL && statusTTL <= 30*24*60*60) {
			return &gateway.PyError{Kind: "ValueError", Msg: "status_ttl_seconds must be at least ttl_seconds and no more than 2592000"}
		}
		var executionPolicy *jsonutil.Object
		if executionPolicy, catalog, err = gateway.APILocalRoomCatalog(ctx, a, profile, targetInstallID); err != nil {
			return err
		}
		secret, err := a.roomGrantSecret(ctx)
		if err != nil {
			return err
		}
		identity, err := roomIdentity(body, true)
		if err != nil {
			return err
		}
		grantID := body.ValueOr("grant_id", nil)
		if anyutil.IsTrue(grantID) {
			grantID = anyutil.String(grantID)
		} else {
			grantID = "grant-" + hexUUID()
		}
		issuedAt := wallClock()
		digest, _ := executionPolicy.ValueOr("policy_digest", "").(string)
		if token, err = gateway.IssueRoomGrant(ctx, secret, gateway.RoomGrantIssue{GrantID: grantID, RoomID: identity["room_id"],
			HomeInstallID: identity["home_install_id"], AuthorityGatewayID: identity["authority_gateway_id"],
			AuthorityEpoch: identity["authority_epoch"], MemberID: identity["member_id"], TargetInstallID: targetInstallID,
			TargetProfile: profile, ExecutionPolicyDigest: digest, IssuedAt: &issuedAt, TTLSeconds: &ttl,
			StatusTTLSeconds: &statusTTL}); err != nil {
			return err
		}
		if claims, err = gateway.DecodeRoomGrant(secret, token, "status", nil); err != nil {
			return err
		}
		expiry, err := hardExpiry(claims)
		if err != nil {
			return err
		}
		return gateway.ReservePeerRoom(ctx, gateway.HostedRoomsDefaultDBPath(ctx), claims, expiry, nil)
	}()
	if err != nil {
		return jsonError(err.Error(), "invalid_room_invitation", http.StatusBadRequest), nil
	}
	expiresAt, err := anyutil.ToFloat(claims.ValueOr("expires_at", nil))
	if err != nil {
		return nil, err
	}
	statusExpiresAt, err := anyutil.ToFloat(claims.ValueOr("status_expires_at", nil))
	if err != nil {
		return nil, err
	}
	return jsonResponse(jsonutil.ObjectOf("object", "icebath.room_member.invitation", "grant", token, "target_profile", profile,
		"catalog", catalog, "expires_at", expiresAt, "status_expires_at", statusExpiresAt), http.StatusCreated, nil), nil
}

// Verifies a scoped grant and answers this target's live room catalog
func (a *APIServerAdapter) handleRoomMemberCapabilities(ctx context.Context, request *gateway.WebRequest) (*gateway.WebResponse, error) {
	var claims, catalog *jsonutil.Object
	var profile string
	err := func() error {
		var installationID string
		var err error
		if claims, err = a.roomGrantClaims(ctx, request, "status"); err != nil {
			return err
		}
		if profile, installationID, err = localTarget(ctx, claims); err != nil {
			return err
		}
		_, catalog, err = gateway.APILocalRoomCatalog(ctx, a, profile, installationID)
		return err
	}()
	if err != nil {
		return roomGrantErrorResponse(err), nil
	}
	out := jsonutil.ObjectOf("object", "icebath.room_member.capabilities")
	for _, field := range roomIdentityFields {
		value, ok := claims.Get(field)
		if !ok {
			return nil, &gateway.PyError{Kind: "KeyError", Msg: anyutil.Quote(field)}
		}
		out.Set(field, value)
	}
	out.Set("target_profile", profile)
	out.Set("catalog", catalog)
	return jsonResponse(out, http.StatusOK, nil), nil
}

// Renews dispatch access within the grant's status horizon without a broad gateway key
func (a *APIServerAdapter) handleRoomMemberGrantRefresh(ctx context.Context, request *gateway.WebRequest) (*gateway.WebResponse, error) {
	body, errResp := readJSONBody(request)
	if errResp != nil {
		return errResp, nil
	}
	for _, key := range body.Keys() {
		if key != "ttl_seconds" {
			return jsonError("Grant refresh accepts only ttl_seconds.", "invalid_room_grant_refresh", http.StatusBadRequest), nil
		}
	}
	var token string
	var now, dispatchTTL, expiry float64
	var executionPolicy *jsonutil.Object
	err := func() error {
		claims, err := a.roomGrantClaims(ctx, request, "dispatch")
		if err != nil {
			return err
		}
		profile, installationID, err := localTarget(ctx, claims)
		if err != nil {
			return err
		}
		now = wallClock()
		if expiry, err = hardExpiry(claims); err != nil {
			return err
		}
		remaining := expiry - now
		requested, err := anyutil.ToFloat(body.ValueOr("ttl_seconds", int64(gateway.MaxDispatchGrantTTLSeconds)))
		if err != nil {
			return err
		}
		if remaining <= 0 || requested <= 0 {
			return &gateway.PyError{Kind: "ValueError", Msg: "room grant renewal horizon expired"}
		}
		dispatchTTL = min(requested, gateway.MaxDispatchGrantTTLSeconds, remaining)
		scoped, err := profileScope(ctx, profile)
		if err != nil {
			return err
		}
		if executionPolicy, err = gateway.ExecutionPolicyMapping(scoped, profile, nil); err != nil {
			return err
		}
		if err := requireUnchangedExecutionPolicy(claims, executionPolicy); err != nil {
			return err
		}
		identity, err := roomIdentity(claims, false)
		if err != nil {
			return err
		}
		permissions, ok := claims.Get("permissions")
		if !ok {
			return &gateway.PyError{Kind: "KeyError", Msg: "'permissions'"}
		}
		granted, _ := permissions.([]any)
		secret, err := a.roomGrantSecret(ctx)
		if err != nil {
			return err
		}
		digest, _ := executionPolicy.ValueOr("policy_digest", "").(string)
		token, err = gateway.IssueRoomGrant(ctx, secret, gateway.RoomGrantIssue{GrantID: "grant-refresh-" + hexUUID(),
			RoomID: identity["room_id"], HomeInstallID: identity["home_install_id"], AuthorityGatewayID: identity["authority_gateway_id"],
			AuthorityEpoch: identity["authority_epoch"], MemberID: identity["member_id"], TargetInstallID: installationID,
			TargetProfile: profile, ExecutionPolicyDigest: digest, Permissions: granted, IssuedAt: &now, TTLSeconds: &dispatchTTL,
			StatusExpiresAt: &expiry})
		return err
	}()
	if err != nil {
		return roomGrantErrorResponse(err), nil
	}
	return jsonResponse(jsonutil.ObjectOf("object", "icebath.room_member.grant", "grant", token, "expires_at", now+dispatchTTL,
		"status_expires_at", expiry, "execution_policy", executionPolicy), http.StatusOK, nil), nil
}

// Revokes exactly the scoped grant authenticating the request, idempotent for a retried revoke
func (a *APIServerAdapter) handleRoomMemberGrantRevoke(ctx context.Context, request *gateway.WebRequest) (*gateway.WebResponse, error) {
	body, errResp := readJSONBody(request)
	if errResp != nil {
		return errResp, nil
	}
	if body.Len() > 0 {
		return jsonError("Grant revoke accepts no fields.", "invalid_room_grant_revoke", http.StatusBadRequest), nil
	}
	err := func() error {
		claims, err := a.decodeRequestGrant(ctx, request, "status")
		if err != nil {
			return err
		}
		if _, _, err := localTarget(ctx, claims); err != nil {
			return err
		}
		expiry, err := hardExpiry(claims)
		if err != nil {
			return err
		}
		return gateway.RevokeRoomGrantScope(ctx, gateway.HostedRoomsDefaultDBPath(ctx), claims, expiry, nil)
	}()
	if err != nil {
		return roomGrantErrorResponse(nil), nil
	}
	return jsonResponse(jsonutil.ObjectOf("object", "icebath.room_member.grant.revocation", "revoked", true), http.StatusOK, nil), nil
}
