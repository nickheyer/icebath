package dashboard

import (
	"context"
	"errors"
	"maps"
	"strings"

	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pypath"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
)

const nativeHTTPHeader = "x-icebath-gateway-ticket"

// Scope extra key the gateway listener stamps with the socket peer before the proxy middleware rewrites the client
const GatewaySocketPeerKey = "hermes.gateway_socket_peer"

// Collection routes under the profiles path by method, every other method naming a profile
var profileCollectionRoutes = map[string]map[string]bool{
	"active": {"GET": true, "POST": true}, "import": {"POST": true},
	"projects/tree": {"GET": true}, "sessions": {"GET": true},
	"sessions/pull-requests": {"POST": true}, "sessions/sidebar": {"GET": true},
}

// Audio stream socket paths a local Desktop interactive grant may open
var audioStreamPaths = map[string]bool{"/api/audio/speak-stream": true, "/api/audio/transcribe-stream": true}

func profileScopeMismatch() error {
	return &gateway.PyError{Kind: "PermissionError", Msg: "profile_scope_mismatch"}
}

// Whether err is the PermissionError a grant check or a ticket redemption raises
func isPermissionError(err error) bool {
	var pe *gateway.PyError
	return errors.As(err, &pe) && pe.Kind == "PermissionError"
}

// Native principal stamped on the connection state, nil when none
func nativePrincipal(conn *HTTPConnection) map[string]any {
	grant, _ := conn.State().Get("native_http_principal").(map[string]any)
	return grant
}

// Context resolving implicit profile selectors in the home of the request ticket
func nativeProfileScope(ctx context.Context, conn *HTTPConnection) context.Context {
	return grantHomeScope(ctx, nativePrincipal(conn))
}

// Context with the grant home bound as the home override, unchanged without a grant
func grantHomeScope(ctx context.Context, grant map[string]any) context.Context {
	if grant == nil {
		return ctx
	}
	return home.WithOverride(ctx, pypath.PathStr(pyrepr.Str(grant["profile_id"])))
}

// Refuses a profile selector naming another home than the grant, implicit selectors passing when current
func ownProfile(value any, profileID string, current bool) error {
	if value == nil {
		return nil
	}
	text, ok := value.(string)
	if ok && current && (pystr.Strip(text) == "" || pystr.Lower(pystr.Strip(text)) == "current") {
		return nil
	}
	if !ok {
		return profileScopeMismatch()
	}
	dir, err := resolveProfileDir(pystr.Strip(text))
	if err != nil {
		var he *HTTPException
		if errors.As(err, &he) {
			return profileScopeMismatch()
		}
		return err
	}
	target, err := pypath.Resolve(dir)
	if err != nil {
		return err
	}
	if pypath.Normcase(pypath.PathStr(target)) != pypath.Normcase(pypath.PathStr(profileID)) {
		return profileScopeMismatch()
	}
	return nil
}

// Refuses a request whose query or JSON body profile selectors leave the grant home
func checkProfileScope(ctx context.Context, req *Request, profileID string) error {
	path := strings.TrimRight(req.URL().Path(), "/")
	current := true
	for _, prefix := range []string{"/api/profiles/", "/api/cron/jobs", "/api/sessions", "/api/fs/download", "/api/fs/read-data-url"} {
		if strings.HasPrefix(path, prefix) {
			current = false
		}
	}
	for _, value := range req.QueryParams().GetList("profile") {
		if err := ownProfile(value, profileID, current); err != nil {
			return err
		}
	}
	contentType := pystr.Lower(pystr.Strip(strings.SplitN(req.Headers().Get("content-type"), ";", 2)[0]))
	if contentType == "" || contentType == "application/json" ||
		(strings.HasPrefix(contentType, "application/") && strings.HasSuffix(contentType, "+json")) {
		if _, err := req.Body(ctx); err != nil {
			return err
		}
		body, err := req.JSON(ctx)
		if err != nil {
			body = nil
		}
		if obj, ok := body.(*pyjson.Object); ok {
			if v, has := obj.Get("profile"); has {
				if err := ownProfile(v, profileID, current); err != nil {
					return err
				}
			}
		}
	}
	if strings.HasPrefix(path, "/api/cron/jobs") && req.QueryParams().Get("profile") == "" {
		return profileScopeMismatch()
	}
	if path == "/api/profiles" {
		if req.Method() != "GET" {
			return profileScopeMismatch()
		}
		return nil
	}
	if !strings.HasPrefix(path, "/api/profiles/") {
		return nil
	}
	suffix := strings.TrimPrefix(path, "/api/profiles/")
	collection, _ := profileCollectionRoute(path, req.Method())
	aggregate := map[string]string{"sessions": "profile", "sessions/sidebar": "recents_profile",
		"projects/tree": "profile", "sessions/pull-requests": "profile"}[collection]
	switch {
	case aggregate != "":
		values := req.QueryParams().GetList(aggregate)
		if len(values) == 0 {
			return profileScopeMismatch()
		}
		for _, value := range values {
			if err := ownProfile(value, profileID, false); err != nil {
				return err
			}
		}
	case collection == "active" && req.Method() == "GET":
		return nil
	case collection == "active" || collection == "import":
		return profileScopeMismatch()
	default:
		if err := ownProfile(strings.SplitN(suffix, "/", 2)[0], profileID, false); err != nil {
			return err
		}
		if req.Method() == "PATCH" || req.Method() == "DELETE" {
			return profileScopeMismatch()
		}
	}
	return nil
}

// Collection suffix the method and path serve, false when the path names a profile
func profileCollectionRoute(path, method string) (string, bool) {
	trimmed := strings.TrimRight(path, "/")
	suffix := strings.TrimPrefix(trimmed, "/api/profiles/")
	if suffix == trimmed {
		return "", false
	}
	if profileCollectionRoutes[suffix][strings.ToUpper(method)] {
		return suffix, true
	}
	return "", false
}

// Profile a PATCH or DELETE of one profile names, false for any other route
func registryTarget(req *HTTPConnection) (string, bool) {
	if req.Scope.Method != "PATCH" && req.Scope.Method != "DELETE" {
		return "", false
	}
	trimmed := strings.TrimRight(req.URL().Path(), "/")
	suffix := strings.TrimPrefix(trimmed, "/api/profiles/")
	if suffix == trimmed || suffix == "" || strings.Contains(suffix, "/") {
		return "", false
	}
	return suffix, true
}

// Refuses a registry grant aimed at another profile or at its running launch home, a default root rename allowed
func checkRegistryScope(req *HTTPConnection, profileID string, launchHome *string) error {
	for _, value := range req.QueryParams().GetList("profile") {
		if err := ownProfile(value, profileID, false); err != nil {
			return err
		}
	}
	if target, ok := registryTarget(req); ok {
		if err := ownProfile(target, profileID, false); err != nil {
			return err
		}
	}
	if launchHome == nil {
		return profileScopeMismatch()
	}
	launch := home.Key(*launchHome)
	if home.Key(profileID) == launch && !(req.Scope.Method == "PATCH" && launch == home.Key(home.Root())) {
		return profileScopeMismatch()
	}
	return nil
}

// Redeems a native http grant, the profile create or registry purpose for its own host route
func redeemHTTPTicket(store *gateway.TicketStore, ticket string, req *Request) (map[string]any, error) {
	grant, err := store.Redeem(ticket, nil, "native-http")
	if err != nil {
		if !isPermissionError(err) {
			return nil, err
		}
		if _, target := registryTarget(&req.HTTPConnection); req.Method() == "POST" && strings.TrimRight(req.URL().Path(), "/") == "/api/profiles" {
			grant, err = store.Redeem(ticket, nil, "native-profile-create")
		} else if target {
			grant, err = store.Redeem(ticket, nil, "native-profile-registry")
		}
		if err != nil {
			return nil, err
		}
	}
	capabilities, _ := grant["capabilities"].(map[string]bool)
	if !maps.Equal(capabilities, gateway.PurposeCapabilities(pyrepr.Str(grant["purpose"]))) {
		return nil, &gateway.PyError{Kind: "PermissionError", Msg: "invalid native grant"}
	}
	return grant, nil
}

// Runner that keeps a registry of served profile authorities
type nativeRegistryRunner interface {
	SessionAuthorities() *gateway.SessionAuthorities
}

// Runner holding the private bootstrap ticket store
type nativeTicketRunner interface {
	SessionTicketStore() *gateway.TicketStore
}

// Runner exposing its session runtime descriptor
type nativeDescriptorRunner interface {
	SessionRuntimeDescriptor() map[string]any
}

// Runner exposing whether it drains
type nativeDrainingRunner interface {
	Draining() bool
}

// Profile id of the runner launch authority, nil when no launch authority is built
func launchHome(runner any) (*string, error) {
	var launch *gateway.SessionAuthority
	if r, ok := runner.(nativeRegistryRunner); ok && r.SessionAuthorities() != nil {
		var err error
		if launch, err = r.SessionAuthorities().Launch(); err != nil {
			return nil, err
		}
	} else if all := gateway.AllAuthorities(runner); len(all) > 0 {
		launch = all[0]
	}
	if launch == nil {
		return nil, nil
	}
	return &launch.ProfileID, nil
}

// Served profile list of a descriptor as a list of dicts
func servedProfilesList(v any) any {
	if list, ok := v.([]map[string]any); ok {
		out := make([]any, len(list))
		for i, m := range list {
			out[i] = m
		}
		return out
	}
	return v
}

// Runner and ticket store when the connection may redeem a private grant here, false otherwise
func nativeStore(conn *HTTPConnection, tickets []string) (any, *gateway.TicketStore, bool) {
	runner := conn.App().State.Get("gateway_runner")
	descriptor := map[string]any{}
	if r, ok := runner.(nativeDescriptorRunner); ok && runner != nil {
		descriptor = r.SessionRuntimeDescriptor()
	}
	authorities := gateway.AllAuthorities(runner)
	var store *gateway.TicketStore
	if r, ok := runner.(nativeTicketRunner); ok && runner != nil {
		store = r.SessionTicketStore()
	}
	peer, _ := conn.Scope.Extra[GatewaySocketPeerKey].(*Address)
	draining := true
	if r, ok := runner.(nativeDrainingRunner); ok && runner != nil {
		draining = r.Draining()
	}
	restartRead := false
	if r, ok := runner.(gateway.DrainRunner); ok && runner != nil {
		restartRead = gateway.RestartWaitRead(r, conn.Scope.Method, conn.URL().Path())
	}
	if len(tickets) != 1 || conn.Headers().Contains("origin") || peer == nil ||
		(peer.Host != "127.0.0.1" && peer.Host != "::1") || !pyrepr.Equal(descriptor["state"], "ready") ||
		(draining && !restartRead) || len(authorities) == 0 || store == nil ||
		!pyrepr.Equal(store.InstanceID, descriptor["instance_id"]) {
		return nil, nil, false
	}
	served := store.ProfileIDs()
	expected := []any{}
	for _, a := range authorities {
		if a.InstanceID != store.InstanceID {
			return nil, nil, false
		}
		if !a.Retiring && served[a.ProfileID] {
			expected = append(expected, map[string]any{"profile_id": a.ProfileID, "home": a.ProfileID})
		}
	}
	if !pyrepr.Equal(servedProfilesList(descriptor["served_profiles"]), expected) {
		return nil, nil, false
	}
	return runner, store, true
}

// Native http grant a Desktop plugin socket upgrade presents, nil when refused
func redeemNativeWS(conn *HTTPConnection) (map[string]any, error) {
	tickets := conn.Headers().GetList(nativeHTTPHeader)
	if !strings.HasPrefix(conn.URL().Path(), "/api/plugins/") {
		return nil, nil
	}
	runner, store, ok := nativeStore(conn, tickets)
	if !ok {
		return nil, nil
	}
	grant, err := store.Redeem(tickets[0], nil, "native-http")
	if err != nil {
		return nil, permissionAsRefusal(err)
	}
	capabilities, _ := grant["capabilities"].(map[string]bool)
	if !maps.Equal(capabilities, map[string]bool{"http:owner": true}) {
		return nil, nil
	}
	profileID := pyrepr.Str(grant["profile_id"])
	authority, err := gateway.AuthorityForProfileID(runner, profileID)
	if err != nil {
		return nil, permissionAsRefusal(err)
	}
	if authority == nil {
		return nil, nil
	}
	for _, value := range conn.QueryParams().GetList("profile") {
		if err := ownProfile(value, profileID, true); err != nil {
			return nil, permissionAsRefusal(err)
		}
	}
	return grant, nil
}

// Nil for a PermissionError refusal, the error itself otherwise
func permissionAsRefusal(err error) error {
	if isPermissionError(err) {
		return nil
	}
	return err
}

// Header lookup of the connection for the gateway boundary helpers
func (c *HTTPConnection) BoundaryHeader(name string) string { return c.Headers().Get(name) }

// Client host of the connection, false when the scope carries none
func (c *HTTPConnection) BoundaryClient() (string, bool) {
	if c.Scope.Client == nil {
		return "", false
	}
	return c.Scope.Client.Host, true
}

// Interactive grant a local Desktop audio stream upgrade presents, nil when refused
func redeemAudioWS(ws *WebSocket) (map[string]any, error) {
	ticket, reason := gateway.GatewayWSTicketFromSubprotocol(&ws.HTTPConnection)
	if reason != "ok" || !audioStreamPaths[ws.URL().Path()] {
		return nil, nil
	}
	runner, store, ok := nativeStore(&ws.HTTPConnection, []string{ticket})
	if !ok {
		return nil, nil
	}
	grant, err := store.Redeem(ticket, nil, "interactive")
	if err != nil {
		return nil, permissionAsRefusal(err)
	}
	profileID := pyrepr.Str(grant["profile_id"])
	authority, err := gateway.AuthorityForProfileID(runner, profileID)
	if err != nil {
		return nil, permissionAsRefusal(err)
	}
	if authority == nil {
		return nil, nil
	}
	for _, value := range ws.QueryParams().GetList("profile") {
		if err := ownProfile(value, profileID, true); err != nil {
			return nil, permissionAsRefusal(err)
		}
	}
	return grant, nil
}

// Grant the plugin socket gate admitted for this socket, nil when refused or not a plugin socket
func authenticateNativeWS(ws *WebSocket) map[string]any {
	if !strings.HasPrefix(ws.URL().Path(), "/api/plugins/") {
		return nil
	}
	return nativePrincipal(&ws.HTTPConnection)
}

// Rejection for a refused native ticket or scope, else stamps the one request principal, inert without the header
func authenticateNativeHTTP(ctx context.Context, req *Request) (ASGIResponse, error) {
	tickets := req.Headers().GetList(nativeHTTPHeader)
	if len(tickets) == 0 {
		return nil, nil
	}
	runner, store, ok := nativeStore(&req.HTTPConnection, tickets)
	if !ok {
		return MustJSONResponse(map[string]any{"detail": "Unauthorized"}, 401), nil
	}
	grant, err := redeemHTTPTicket(store, tickets[0], req)
	if err == nil {
		var authority *gateway.SessionAuthority
		if authority, err = gateway.AuthorityForProfileID(runner, pyrepr.Str(grant["profile_id"])); err == nil && authority == nil {
			err = &gateway.PyError{Kind: "PermissionError", Msg: "unserved native grant"}
		}
	}
	if err != nil {
		if isPermissionError(err) {
			return MustJSONResponse(map[string]any{"detail": "Unauthorized"}, 401), nil
		}
		return nil, err
	}
	profileID := pyrepr.Str(grant["profile_id"])
	switch purpose := pyrepr.Str(grant["purpose"]); {
	case purpose == "native-profile-registry":
		launch, lerr := launchHome(runner)
		if lerr != nil {
			return nil, lerr
		}
		err = checkRegistryScope(&req.HTTPConnection, profileID, launch)
	case purpose != "native-profile-create":
		err = checkProfileScope(ctx, req, profileID)
	}
	if err != nil {
		if isPermissionError(err) {
			return MustJSONResponse(map[string]any{"detail": "profile_scope_mismatch"}, 403), nil
		}
		return nil, err
	}
	req.State().Set("native_http_principal", grant)
	return nil, nil
}
