package gatewayapi

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/state"
)

type requestProfileKey struct{}

// Profile the prefix middleware selected for this request, empty for none
func requestProfile(ctx context.Context) string {
	p, _ := ctx.Value(requestProfileKey{}).(string)
	return p
}

// Context carrying the request profile
func withRequestProfile(ctx context.Context, profile string) context.Context {
	return context.WithValue(ctx, requestProfileKey{}, profile)
}

// Platform adapter answering HTTP events with its own verifier and dispatcher
type HTTPEventAdapter interface {
	VerifyHTTPEventRequest(ctx context.Context, authHeader string) (bool, string, error)
	DispatchHTTPEvent(ctx context.Context, payload *pyjson.Object) (any, error)
}

// Adapter a platform event callback dispatches to, the routed profile's own and never the primary's
func (a *APIServerAdapter) getPlatformCallbackAdapter(ctx context.Context, request *gateway.WebRequest, platformName string) any {
	app := a.currentApp(request)
	if app != nil {
		if injected, ok := app.State["platform_event_adapters"].(map[string]any); ok {
			if adapter := injected[platformName]; adapter != nil {
				return adapter
			}
		}
		if adapter := app.State[platformName+"_adapter"]; adapter != nil {
			return adapter
		}
	}
	runner := a.GatewayRunner
	if runner == nil && app != nil {
		runner = app.State["gateway_runner"]
	}
	if runner == nil {
		return nil
	}
	platform, err := gateway.ParsePlatform(ctx, platformName)
	if err != nil {
		return nil
	}
	resolver, ok := runner.(gateway.APIServerRunner)
	if !ok {
		return nil
	}
	var profile *string
	if p := requestProfile(ctx); p != "" {
		profile = &p
	}
	adapter := resolver.AuthorizationAdapter(ctx, platform, profile)
	if adapter == nil {
		return nil
	}
	return adapter
}

// Application serving a request, the adapter's own
func (a *APIServerAdapter) currentApp(request *gateway.WebRequest) *gateway.WebApplication {
	return a.app
}

// Platform event ingress authenticated by the target adapter's own verifier
func (a *APIServerAdapter) handlePlatformEventCallback(ctx context.Context, request *gateway.WebRequest) (*gateway.WebResponse, error) {
	platformName := normalizeCallbackPlatform(request.MatchInfo["platform"])
	if platformName == "" {
		return errorResponse("Invalid platform name", http.StatusBadRequest, errOpts{Code: "invalid_platform"}), nil
	}
	adapter := a.getPlatformCallbackAdapter(ctx, request, platformName)
	if adapter == nil {
		return errorResponse("Platform adapter is not connected", http.StatusServiceUnavailable, errOpts{Code: "platform_unavailable"}), nil
	}
	events, ok := adapter.(HTTPEventAdapter)
	if !ok {
		return errorResponse("Platform adapter does not support HTTP events", http.StatusServiceUnavailable,
			errOpts{Code: "platform_http_events_unsupported"}), nil
	}
	authHeader := request.Header.Get("Authorization")
	verified, code := func() (ok bool, code string) {
		defer func() {
			if rec := recover(); rec != nil {
				apiLog().Error(fmt.Sprintf("Platform HTTP event verifier failed for %s", platformName), "error", rec)
				ok, code = false, "platform_event_verifier_error"
			}
		}()
		ok, code, err := events.VerifyHTTPEventRequest(ctx, authHeader)
		if err != nil {
			apiLog().Error(fmt.Sprintf("Platform HTTP event verifier failed for %s", platformName), "error", err)
			return false, "platform_event_verifier_error"
		}
		return ok, code
	}()
	if !verified {
		if code == "" {
			code = "invalid_platform_event_authorization"
		}
		return errorResponse("Invalid platform event authorization", http.StatusUnauthorized, errOpts{Code: code}), nil
	}
	payload, err := requestJSON(request)
	if err != nil {
		return errorResponse("Invalid JSON in platform event", http.StatusBadRequest, errOpts{Code: "invalid_json"}), nil
	}
	obj, ok := payload.(*pyjson.Object)
	if !ok {
		return errorResponse("Platform event must be a JSON object", http.StatusBadRequest, errOpts{Code: "invalid_request"}), nil
	}
	result, err := func() (result any, err error) {
		defer func() {
			if rec := recover(); rec != nil {
				err = fmt.Errorf("%v", rec)
			}
		}()
		return events.DispatchHTTPEvent(ctx, obj)
	}()
	if err != nil {
		var parked *gateway.ParkedRouteRefused
		if errors.As(err, &parked) {
			return errorResponse(parked.Error(), gateway.ParkedRouteRefusedStatus, errOpts{Type: "server_error",
				Code: gateway.ParkedRouteRefusedCode, Headers: gateway.ParkedRouteRefusedHeaders}), nil
		}
		apiLog().Error(fmt.Sprintf("Platform HTTP event dispatch failed for %s", platformName), "error", err)
		return errorResponse("Platform event dispatch failed", http.StatusInternalServerError, errOpts{Type: "server_error",
			Code: "platform_event_dispatch_failed"}), nil
	}
	if resultObj, ok := result.(*pyjson.Object); ok {
		return jsonResponse(resultObj, http.StatusOK, nil), nil
	}
	return jsonResponse(pyjson.NewObject(), http.StatusOK, nil), nil
}

// Profile a request prefix routes to, rejected when this gateway does not serve it
func (a *APIServerAdapter) resolveRequestProfile(ctx context.Context, request *gateway.WebRequest) (string, bool, error) {
	profile, rejected, err := gateway.APIResolveRequestProfile(ctx, a.GatewayRunner, request)
	if err != nil || rejected || profile == nil {
		return "", rejected, err
	}
	return *profile, false, nil
}

// Multiplex runtime scope of a profile, the default's under multiplex when unset
func profileScope(ctx context.Context, profile string) (context.Context, error) {
	return gateway.APIProfileScope(ctx, profile)
}

// Profile prefixed path served by that profile's shared listener adapter
func (a *APIServerAdapter) handleProfileIngress(ctx context.Context, request *gateway.WebRequest) (*gateway.WebResponse, error) {
	return gateway.DispatchProfileIngress(ctx, a.GatewayRunner, requestProfile(ctx), request.MatchInfo["tail"], request, true)
}

// Middleware rejecting unknown profile prefixes and scoping the request home
func (a *APIServerAdapter) profilePrefixMiddleware(ctx context.Context, request *gateway.WebRequest, handler gateway.WebHandler) (*gateway.WebResponse, error) {
	profile, rejected, err := a.resolveRequestProfile(ctx, request)
	if err != nil {
		return nil, err
	}
	if rejected {
		return gateway.APIProfileRejectionResponse(request)
	}
	ctx = withRequestProfile(ctx, profile)
	scoped, err := profileScope(ctx, profile)
	if err != nil {
		return nil, err
	}
	return handler(scoped, request)
}

// Route row registered by connect
type httpRoute struct {
	method, path string
	handler      gateway.WebHandler
}

// Method, path and handler rows connect registers, plus the profile prefixed mirrors
func (a *APIServerAdapter) httpRouteTable() []httpRoute {
	auth := a.requireAuth
	admit := a.admitAPIAgentRequest
	routes := []httpRoute{
		{"GET", "/health", a.handleHealth},
		{"GET", "/health/detailed", auth(a.handleHealthDetailed)},
		{"GET", "/v1/health", a.handleHealth},
		{"GET", "/v1/models", auth(a.handleModels)},
		{"GET", "/api/model/options", auth(a.handleModelOptions)},
		{"GET", "/v1/capabilities", auth(a.handleCapabilities)},
		{"GET", "/v1/skills", auth(a.handleSkills)},
		{"GET", "/v1/toolsets", auth(a.handleToolsets)},
		{"GET", "/api/sessions", auth(a.handleListSessions)},
		{"POST", "/api/sessions", auth(a.handleCreateSession)},
		{"GET", "/api/sessions/{session_id}", auth(a.handleGetSession)},
		{"PATCH", "/api/sessions/{session_id}", auth(a.handlePatchSession)},
		{"DELETE", "/api/sessions/{session_id}", auth(a.handleDeleteSession)},
		{"GET", "/api/sessions/{session_id}/messages", auth(a.handleSessionMessages)},
		{"POST", "/api/sessions/{session_id}/fork", auth(a.handleForkSession)},
		{"POST", "/api/sessions/{session_id}/chat", admit(a.handleSessionChat)},
		{"POST", "/api/sessions/{session_id}/chat/stream", admit(a.handleSessionChatStream)},
		{"POST", "/api/sessions/{session_id}/model", auth(a.handleSessionModelLock)},
		{"POST", "/v1/chat/completions", admit(a.handleChatCompletions)},
		{"POST", "/v1/responses", admit(a.handleResponses)},
		{"GET", "/v1/responses/{response_id}", a.handleGetResponse},
		{"DELETE", "/v1/responses/{response_id}", a.handleDeleteResponse},
		{"POST", "/api/platforms/{platform}/events", a.handlePlatformEventCallback},
		{"GET", "/api/jobs", a.handleListJobs},
		{"POST", "/api/jobs", a.handleCreateJob},
		{"GET", "/api/jobs/{job_id}", a.handleGetJob},
		{"PATCH", "/api/jobs/{job_id}", a.handleUpdateJob},
		{"DELETE", "/api/jobs/{job_id}", a.handleDeleteJob},
		{"POST", "/api/jobs/{job_id}/pause", a.handlePauseJob},
		{"POST", "/api/jobs/{job_id}/resume", a.handleResumeJob},
		{"POST", "/api/jobs/{job_id}/run", a.handleRunJob},
	}
	routes = append(routes, a.roomGrantHTTPRoutes()...)
	routes = append(routes, a.runHTTPRoutes()...)
	routes = append(routes, httpRoute{"POST", "/api/cron/fire", a.handleCronFire})
	return routes
}

const (
	// Cap on session headers, above any realistic channel id
	maxSessionHeaderLen = 256
	// Source stamped on every session row this platform owns
	sessionSource = "api_server"
)

// Live session a client declared with the session key header, empty when undeclared or unknown
func (a *APIServerAdapter) declaredConversationSession(ctx context.Context, gatewaySessionKey string) string {
	key := strings.TrimSpace(gatewaySessionKey)
	if key == "" {
		return ""
	}
	db := a.ensureSessionDB(ctx)
	if db == nil {
		return ""
	}
	if holder, ok := a.GatewayRunner.(interface {
		SessionAuthority() *gateway.SessionAuthority
	}); ok && holder.SessionAuthority() != nil {
		id, ok, err := gateway.APIDeclaredSession(ctx, db, key)
		if err != nil || !ok {
			return ""
		}
		return id
	}
	row, err := db.FindLatestGatewaySessionForPeer(ctx, state.GatewayPeerQuery{Source: sessionSource, SessionKey: &key})
	if err != nil {
		apiLog().Debug(fmt.Sprintf("[%s] declared-conversation lookup failed", a.Name()), "error", err)
		return ""
	}
	if row == nil {
		return ""
	}
	id, _ := row.Get("id")
	if id == nil || id == "" {
		return ""
	}
	return fmt.Sprint(id)
}

// Records the declared conversation key on the session row and its compression ancestors
func (a *APIServerAdapter) bindDeclaredConversation(ctx context.Context, sessionID, gatewaySessionKey string) {
	key := strings.TrimSpace(gatewaySessionKey)
	sid := strings.TrimSpace(sessionID)
	if key == "" || sid == "" {
		return
	}
	db := a.ensureSessionDB(ctx)
	if db == nil {
		return
	}
	existing, err := db.GetSession(ctx, sid)
	if err != nil {
		apiLog().Debug(fmt.Sprintf("[%s] declared-conversation bind failed for %s", a.Name(), sid), "error", err)
		return
	}
	current := ""
	if existing != nil {
		if v, _ := existing.Get("session_key"); v != nil {
			current = strings.TrimSpace(fmt.Sprint(v))
		}
	}
	if current != "" && current != key {
		apiLog().Debug(fmt.Sprintf("[%s] refusing to rebind session %s from a different declared conversation", a.Name(), sid))
		return
	}
	if err := db.RecordGatewaySessionPeer(ctx, sid, state.GatewayPeer{Source: sessionSource, SessionKey: &key,
		IncludeCompressionAncestors: true}); err != nil {
		apiLog().Debug(fmt.Sprintf("[%s] declared-conversation bind failed for %s", a.Name(), sid), "error", err)
	}
}

var sessionHeaderUnsafeRE = regexp.MustCompile("[\r\n\x00]")

// Validated session key header, a 403 without an API key and a 400 for unsafe or long values
func (a *APIServerAdapter) parseSessionKeyHeader(request *gateway.WebRequest) (string, *gateway.WebResponse) {
	raw := strings.TrimSpace(request.Header.Get("X-Hermes-Session-Key"))
	if raw == "" {
		return "", nil
	}
	if a.apiKey == "" {
		apiLog().Warn("X-Hermes-Session-Key rejected: no API key configured. Set API_SERVER_KEY to enable long-term memory scoping.")
		return "", errorResponse("X-Hermes-Session-Key requires API key authentication. "+
			"Configure API_SERVER_KEY to enable this feature.", http.StatusForbidden, errOpts{})
	}
	if sessionHeaderUnsafeRE.MatchString(raw) {
		return "", invalidRequest("Invalid session key")
	}
	if len([]rune(raw)) > maxSessionHeaderLen {
		return "", invalidRequest("Session key too long")
	}
	return raw, nil
}

// Responses state of the routed profile's home, each home with its own store
func (a *APIServerAdapter) currentResponseStore(ctx context.Context) (*gateway.ResponseStore, error) {
	h := home.Dir(ctx)
	if h == a.responseStoreHome {
		return a.responseStore, nil
	}
	a.responseStoreMu.Lock()
	defer a.responseStoreMu.Unlock()
	if store := a.responseStores[h]; store != nil {
		return store, nil
	}
	store, err := gateway.NewResponseStore(ctx, gateway.APIMaxStoredResponses, filepath.Join(h, "response_store.db"),
		gateway.APIMaxStoredResponses*gateway.APIIdentityRetentionFactor)
	if err != nil {
		return nil, err
	}
	a.responseStores[h] = store
	return store, nil
}

// Session store of a home through the runtime authority or this adapter's own cache, nil once closed
func (a *APIServerAdapter) openAndCacheSessionDB(ctx context.Context, h string) (*state.SessionDB, error) {
	if a.GatewayRunner != nil {
		return a.selectedSessionDB(ctx, h), nil
	}
	a.sessionDBCacheMu.Lock()
	defer a.sessionDBCacheMu.Unlock()
	if a.sessionDBCacheClosed {
		return nil, nil
	}
	db := a.cachedSessionDBLocked(h)
	if db == nil {
		opened, err := state.Acquire(ctx, filepath.Join(h, "state.db"))
		if err != nil {
			return nil, err
		}
		db = opened
		a.sessionDBs[h] = db
	}
	return db, nil
}

// Cached store of a home, dropped when its registry generation was torn down, caller holds the cache lock
func (a *APIServerAdapter) cachedSessionDBLocked(key string) *state.SessionDB {
	db := a.sessionDBs[key]
	if db != nil && !db.SharedRegistryOwned() {
		delete(a.sessionDBs, key)
		return nil
	}
	return db
}

// Closes the session stores this adapter's profile cache owns
func (a *APIServerAdapter) closeCachedSessionDBs() {
	a.sessionDBCacheMu.Lock()
	a.sessionDBCacheClosed = true
	cached := make([]*state.SessionDB, 0, len(a.sessionDBs))
	for _, db := range a.sessionDBs {
		cached = append(cached, db)
	}
	a.sessionDBs = map[string]*state.SessionDB{}
	a.sessionDBCacheMu.Unlock()
	for _, db := range cached {
		if db == a.sessionDB {
			continue
		}
		func() {
			defer func() {
				if rec := recover(); rec != nil {
					apiLog().Debug("Failed to close API-server SessionDB", "error", rec)
				}
			}()
			state.ReleaseOrClose(db)
		}()
	}
}

// Session store of the active profile home, nil when unavailable
func (a *APIServerAdapter) ensureSessionDB(ctx context.Context) *state.SessionDB {
	if a.GatewayRunner != nil {
		return a.selectedSessionDB(ctx, home.Dir(ctx))
	}
	if a.sessionDB != nil {
		return a.sessionDB
	}
	a.sessionDBOpenMu.Lock()
	defer a.sessionDBOpenMu.Unlock()
	a.sessionDBCacheMu.Lock()
	cached := a.cachedSessionDBLocked(home.Dir(ctx))
	a.sessionDBCacheMu.Unlock()
	if cached != nil {
		return cached
	}
	db, err := a.openAndCacheSessionDB(ctx, home.Dir(ctx))
	if err != nil {
		apiLog().Debug(fmt.Sprintf("SessionDB unavailable for API server: %v", err))
		return nil
	}
	return db
}

// Session store of the active profile home, the stateless adapter surface delegation deliveries write through
func (a *APIServerAdapter) EnsureSessionDB(ctx context.Context) (*state.SessionDB, error) {
	return a.ensureSessionDB(ctx), nil
}

// Store a runtime authority reserved for the home, nil when none or the cache is closed
func (a *APIServerAdapter) selectedSessionDB(ctx context.Context, h string) *state.SessionDB {
	authority, err := gateway.APIAuthorityForHome(a.GatewayRunner, h)
	if err != nil || authority == nil {
		return nil
	}
	a.sessionDBCacheMu.Lock()
	closed := a.sessionDBCacheClosed
	a.sessionDBCacheMu.Unlock()
	if closed {
		return nil
	}
	return authority.DB
}
