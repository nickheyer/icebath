package gatewayapi

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/google/uuid"
	"github.com/nickheyer/icebath/pkg/botmode"
	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/provider"
	"github.com/nickheyer/icebath/pkg/skills"
	"github.com/nickheyer/icebath/pkg/state"
	"github.com/nickheyer/icebath/pkg/tools"
)

// Liveness probe answering ok with the platform and version
func (a *APIServerAdapter) handleHealth(ctx context.Context, request *gateway.WebRequest) (*gateway.WebResponse, error) {
	return jsonResponse(pyjson.ObjectOf("status", "ok", "platform", "icebath-agent", "version", hermesVersion()), http.StatusOK, nil), nil
}

// Gateway state, platforms, readiness and live API metrics for dashboard probing
func (a *APIServerAdapter) handleHealthDetailed(ctx context.Context, request *gateway.WebRequest) (*gateway.WebResponse, error) {
	runtime := gateway.ReadRuntimeStatus("")
	if runtime == nil {
		runtime = pyjson.NewObject()
	}
	gwState := runtime.GetOr("gateway_state", nil)
	gwActive, err := gateway.ParseActiveAgents(runtime.GetOr("active_agents", int64(0)))
	if err != nil {
		return nil, err
	}
	platforms, _ := runtime.GetOr("platforms", nil).(*pyjson.Object)
	if platforms == nil {
		platforms = pyjson.NewObject()
	}
	platforms = platforms.Copy()
	now := apiNow()
	apiStatus := a.apiServerStatusPayload(&now)
	apiPlatform := pyjson.NewObject()
	if existing, ok := platforms.GetOr("api_server", nil).(*pyjson.Object); ok {
		apiPlatform = existing.Copy()
	}
	connected := "disconnected"
	if a.IsConnected() {
		connected = "connected"
	}
	setDefault(apiPlatform, "state", connected)
	apiPlatform.Set("metrics", apiStatus)
	platforms.Set("api_server", apiPlatform)
	activeAPIRuns, processDepth, activeDelegations := a.readinessWorkCounts()
	readiness := gateway.CollectRuntimeReadiness(ctx, gateway.ResolveGatewayModel(ctx, nil), runtime, activeAPIRuns, processDepth, activeDelegations)
	busy, err := gateway.DeriveGatewayBusy(true, gwState, gwActive)
	if err != nil {
		return nil, err
	}
	updatedAt, hasUpdatedAt, err := gateway.NormalizeUpdatedAt(runtime.GetOr("updated_at", nil))
	if err != nil {
		return nil, err
	}
	var updated any
	if hasUpdatedAt {
		updated = updatedAt
	}
	status, _ := readiness.Get("status")
	metricsToday, _ := apiStatus.Get("metrics_today")
	lastHeartbeat, _ := apiStatus.Get("last_heartbeat")
	return jsonResponse(pyjson.ObjectOf(
		"status", status, "readiness", readiness, "platform", "icebath-agent",
		"version", hermesVersion(), "gateway_state", gwState, "platforms", platforms,
		"api_server", apiStatus, "metrics_today", metricsToday, "last_heartbeat", lastHeartbeat,
		"active_agents", gwActive, "gateway_busy", busy,
		"gateway_drainable", gateway.DeriveGatewayDrainable(true, gwState),
		"exit_reason", runtime.GetOr("exit_reason", nil), "updated_at", updated, "pid", int64(os.Getpid())), http.StatusOK, nil), nil
}

// Advertised model plus configured route aliases, never their credentials
func (a *APIServerAdapter) handleModels(ctx context.Context, request *gateway.WebRequest) (*gateway.WebResponse, error) {
	now := int64(apiNow())
	modelName := a.modelName
	if requestProfile(ctx) != "" {
		modelName = resolveModelName(ctx, "")
	}
	model := func(id, root string, parent any) *pyjson.Object {
		return pyjson.ObjectOf("id", id, "object", "model", "created", now, "owned_by", "hermes", "permission", []any{},
			"root", root, "parent", parent)
	}
	models := []any{model(modelName, modelName, nil)}
	for _, alias := range a.modelRoutes.keys {
		if alias == modelName {
			continue
		}
		root := a.modelRoutes.routes[alias]["model"]
		if root == "" {
			root = alias
		}
		models = append(models, model(alias, root, modelName))
	}
	return jsonResponse(pyjson.ObjectOf("object", "list", "data", models), http.StatusOK, nil), nil
}

// Model picker inventory of the configured provider catalog
func (a *APIServerAdapter) handleModelOptions(ctx context.Context, request *gateway.WebRequest) (*gateway.WebResponse, error) {
	refresh := gateway.APICoerceRequestBool(queryValue(request, "refresh"), false)
	includeUnconfigured := gateway.APICoerceRequestBool(queryValue(request, "include_unconfigured"), true)
	payload, err := func() (out map[string]any, err error) {
		defer func() {
			if rec := recover(); rec != nil {
				err = fmt.Errorf("%v", rec)
			}
		}()
		pc, err := provider.LoadPickerContext(ctx)
		if err != nil {
			return nil, err
		}
		return provider.BuildModelOptionsPayload(ctx, pc, false, includeUnconfigured, refresh)
	}()
	if err != nil {
		apiLog().Error(fmt.Sprintf("[%s] GET /api/model/options failed", a.Name()), "error", err)
		return errorResponse("Failed to list model options.", http.StatusInternalServerError, errOpts{Code: "model_options_failed"}), nil
	}
	return jsonResponse(payload, http.StatusOK, nil), nil
}

// Query value of a request, nil when the key is absent
func queryValue(request *gateway.WebRequest, key string) any {
	values, ok := request.Query()[key]
	if !ok || len(values) == 0 {
		return nil
	}
	return values[0]
}

// Stable machine readable API surface for external UIs
func (a *APIServerAdapter) handleCapabilities(ctx context.Context, request *gateway.WebRequest) (*gateway.WebResponse, error) {
	_, hasAuthority := a.sessionAuthority()
	features := pyjson.ObjectOf(
		"chat_completions", true, "chat_completions_streaming", true,
		"responses_api", true, "responses_streaming", true, "run_submission", true,
		"runs_idempotency", a.idempotencyCapabilities())
	features.Update(staticFeatureFlags)
	features.Set("run_unknown_resolution", hasAuthority)
	features.Set("cors", len(a.corsOrigins) > 0)
	endpoints := pyjson.NewObject()
	for _, e := range capabilityEndpoints {
		if e.feature != "" {
			if v, _ := features.Get(e.feature); !pyrepr.Truthy(v) {
				continue
			}
		}
		endpoints.Set(e.name, pyjson.ObjectOf("method", e.method, "path", e.path))
	}
	return jsonResponse(pyjson.ObjectOf(
		"object", "hermes.api_server.capabilities", "platform", "icebath-agent", "model", a.modelName,
		"auth", pyjson.ObjectOf("type", "bearer", "required", a.apiKey != ""),
		"runtime", pyjson.ObjectOf("mode", "server_agent", "tool_execution", "server", "split_runtime", false,
			"description", "The API server creates a server-side Hermes AIAgent; "+
				"tools execute on the API-server host unless a future "+
				"explicit split-runtime mode is enabled."),
		"features", features, "endpoints", endpoints), http.StatusOK, nil), nil
}

// Installed skills with name, description and category in listing order
func (a *APIServerAdapter) handleSkills(ctx context.Context, request *gateway.WebRequest) (*gateway.WebResponse, error) {
	list, err := func() (out []map[string]any, err error) {
		defer func() {
			if rec := recover(); rec != nil {
				err = fmt.Errorf("%v", rec)
			}
		}()
		h := home.Dir(ctx)
		cfg, err := config.LoadReadonly(ctx, h, nil)
		if err != nil {
			return nil, err
		}
		scope := skills.NewScope(h, cfg)
		return skills.SortSkills(scope.FindAllSkills(false)), nil
	}()
	if err != nil {
		apiLog().Error("GET /v1/skills failed", "error", err)
		return errorResponse("Failed to enumerate skills", http.StatusInternalServerError, errOpts{Type: "server_error"}), nil
	}
	data := make([]any, 0, len(list))
	for _, s := range list {
		data = append(data, pyjson.ObjectOf("name", s["name"], "description", s["description"], "category", s["category"]))
	}
	return jsonResponse(pyjson.ObjectOf("object", "list", "data", data), http.StatusOK, nil), nil
}

// Toolsets the api_server agent exposes with enabled and configured state and their tool names
func (a *APIServerAdapter) handleToolsets(ctx context.Context, request *gateway.WebRequest) (*gateway.WebResponse, error) {
	data, err := func() (out []any, err error) {
		defer func() {
			if rec := recover(); rec != nil {
				err = fmt.Errorf("%v", rec)
			}
		}()
		h := home.Dir(ctx)
		cfg, err := config.LoadReadonly(ctx, h, nil)
		if err != nil {
			return nil, err
		}
		scope := tools.ScopeKey(h)
		enabled := tools.Default.PlatformTools(ctx, scope, cfg, "api_server", tools.PlatformToolsOptions{})
		out = []any{}
		for _, ts := range tools.EffectiveConfigurableToolsets(ctx) {
			names := func() (names []string) {
				defer func() {
					if recover() != nil {
						names = []string{}
					}
				}()
				set := map[string]bool{}
				for _, n := range tools.Default.ResolveToolset(ctx, scope, ts.Key) {
					set[n] = true
				}
				for n := range set {
					names = append(names, n)
				}
				sort.Strings(names)
				return names
			}()
			configured, err := tools.ToolsetHasKeys(ctx, ts.Key, cfg)
			if err != nil {
				return nil, err
			}
			toolList := make([]any, 0, len(names))
			for _, n := range names {
				toolList = append(toolList, n)
			}
			out = append(out, pyjson.ObjectOf("name", ts.Key, "label", ts.Label, "description", ts.Description,
				"enabled", enabled[ts.Key], "configured", configured, "tools", toolList))
		}
		return out, nil
	}()
	if err != nil {
		apiLog().Error("GET /v1/toolsets failed", "error", err)
		return errorResponse("Failed to enumerate toolsets", http.StatusInternalServerError, errOpts{Type: "server_error"}), nil
	}
	return jsonResponse(pyjson.ObjectOf("object", "list", "platform", "api_server", "data", data), http.StatusOK, nil), nil
}

// Non negative integer of a query value capped at the maximum, the default when unparseable or negative
func parseNonnegativeInt(value any, def, maximum int) int {
	if value == nil {
		return def
	}
	n, ok := pyIntOf(value)
	if !ok {
		return def
	}
	if n < 0 {
		return def
	}
	return min(n, maximum)
}

// Python int of a string or number, ok false when int raises
func pyIntOf(value any) (int, bool) {
	switch v := value.(type) {
	case string:
		n, err := strconv.Atoi(strings.ReplaceAll(strings.TrimSpace(v), "_", ""))
		return n, err == nil
	case int:
		return v, true
	case int64:
		return int(v), true
	case float64:
		return int(v), true
	case bool:
		if v {
			return 1, true
		}
		return 0, true
	}
	return 0, false
}

// 503 for a missing session store
func sessionDBUnavailable() *gateway.WebResponse {
	return errorResponse("Session database unavailable", http.StatusServiceUnavailable, errOpts{Code: "session_db_unavailable"})
}

var sessionSafeKeys = []string{"id", "source", "user_id", "model", "title", "started_at", "ended_at", "end_reason",
	"message_count", "tool_call_count", "input_tokens", "output_tokens",
	"cache_read_tokens", "cache_write_tokens", "reasoning_tokens", "estimated_cost_usd",
	"actual_cost_usd", "api_call_count", "parent_session_id", "last_active", "preview",
	"_lineage_root_id", "pinned", "archived", "hidden"}

// Client safe session representation with flags as booleans and prompt and model config reduced to presence
func sessionResponse(session *pyjson.Object) *pyjson.Object {
	payload := pyjson.NewObject()
	for _, key := range sessionSafeKeys {
		if v, ok := session.Get(key); ok {
			payload.Set(key, v)
		}
	}
	for _, flag := range []string{"pinned", "archived", "hidden"} {
		if v, ok := payload.Get(flag); ok {
			payload.Set(flag, pyrepr.Truthy(v))
		}
	}
	payload.Set("has_system_prompt", pyrepr.Truthy(session.GetOr("system_prompt", nil)))
	payload.Set("has_model_config", pyrepr.Truthy(session.GetOr("model_config", nil)))
	raw := session.GetOr("model_config", nil)
	var modelConfig any = raw
	if s, ok := raw.(string); ok {
		parsed, err := pyjson.Loads(s)
		if err != nil {
			modelConfig = nil
		} else {
			modelConfig = parsed
		}
	}
	internal := false
	if mc, ok := modelConfig.(*pyjson.Object); ok {
		internal = mc.GetOr("_delegate_from", nil) != nil
	}
	payload.Set("is_internal_child", internal)
	return payload
}

var messageSafeKeys = []string{"id", "session_id", "role", "content", "tool_call_id", "tool_calls", "tool_name",
	"timestamp", "token_count", "finish_reason", "reasoning", "reasoning_content", "display_kind"}

// Client safe message representation after compaction projection
func messageResponse(message *pyjson.Object) *pyjson.Object {
	projected := projectClientMessage(message)
	out := pyjson.NewObject()
	for _, key := range messageSafeKeys {
		if v, ok := projected.Get(key); ok {
			out.Set(key, v)
		}
	}
	return out
}

// JSON object body of a request, a 400 when invalid or not an object
func readJSONBody(request *gateway.WebRequest) (*pyjson.Object, *gateway.WebResponse) {
	body, err := requestJSON(request)
	if err != nil {
		return pyjson.NewObject(), errorResponse("Invalid JSON in request body", http.StatusBadRequest, errOpts{})
	}
	obj, ok := body.(*pyjson.Object)
	if !ok {
		return pyjson.NewObject(), errorResponse("Request body must be a JSON object", http.StatusBadRequest, errOpts{})
	}
	return obj, nil
}

// Session row of an id, a 503 without a store and a 404 when absent
func (a *APIServerAdapter) getExistingSessionOr404(ctx context.Context, sessionID string) (*pyjson.Object, *gateway.WebResponse, error) {
	db := a.ensureSessionDB(ctx)
	if db == nil {
		return nil, sessionDBUnavailable(), nil
	}
	session, err := db.GetSession(ctx, sessionID)
	if err != nil {
		return nil, nil, err
	}
	if session == nil {
		return nil, errorResponse("Session not found: "+sessionID, http.StatusNotFound, errOpts{Code: "session_not_found"}), nil
	}
	return session, nil, nil
}

// Stored conversation of a session, empty when the store is missing or the read fails
func (a *APIServerAdapter) conversationHistoryForSession(ctx context.Context, sessionID string) []map[string]any {
	db := a.ensureSessionDB(ctx)
	if db == nil {
		return []map[string]any{}
	}
	history, err := db.GetMessagesAsConversation(ctx, sessionID, state.ConversationOptions{})
	if err != nil {
		apiLog().Warn(fmt.Sprintf("Failed to load session history for %s: %v", sessionID, err))
		return []map[string]any{}
	}
	return history
}

// Runs one background wake turn against a raw session id in process
func (a *APIServerAdapter) RunInternalSessionTurn(ctx context.Context, sessionID, text, profile, notificationCategory string) error {
	return a.runInternalSessionTurn(ctx, sessionID, text, profile, notificationCategory)
}

// Persisted sessions, the exact title lookup resurrecting an orphan archived canonical Bot Chat
func (a *APIServerAdapter) handleListSessions(ctx context.Context, request *gateway.WebRequest) (*gateway.WebResponse, error) {
	db := a.ensureSessionDB(ctx)
	if db == nil {
		return sessionDBUnavailable(), nil
	}
	limit := parseNonnegativeInt(queryValue(request, "limit"), 50, 200)
	offset := parseNonnegativeInt(queryValue(request, "offset"), 0, 1_000_000)
	source := request.QueryGet("source")
	includeChildren := gateway.APICoerceRequestBool(queryValue(request, "include_children"), false)
	titleFilter := strings.TrimSpace(request.QueryGet("title"))
	includeHidden := titleFilter != "" && gateway.APICoerceRequestBool(queryValue(request, "include_hidden"), false)
	list := func() ([]*pyjson.Object, error) {
		l := limit
		rows, err := db.ListSessionsRich(ctx, state.ListSessionsRichOptions{Source: source, Limit: &l, Offset: offset,
			IncludeChildren: includeChildren, OrderByLastActive: true, IncludePinned: true, SearchQuery: titleFilter,
			IncludeHidden: includeHidden})
		if err != nil {
			return nil, err
		}
		if titleFilter != "" {
			kept := rows[:0]
			for _, s := range rows {
				if strings.TrimSpace(pyrepr.Str(pyrepr.Or(s.GetOr("title", nil), ""))) == titleFilter {
					kept = append(kept, s)
				}
			}
			rows = kept
		}
		return rows, nil
	}
	sessions, err := list()
	if err != nil {
		return nil, err
	}
	if len(sessions) == 0 {
		if sessions, err = resetLocalTitleOwner(ctx, db, titleFilter, includeHidden, source); err != nil {
			return nil, err
		}
	}
	if titleFilter != "" && len(sessions) == 0 {
		func() {
			defer func() { _ = recover() }()
			if titleFilter != botmode.BotChatTitle {
				return
			}
			stale, err := db.GetSessionByTitle(ctx, titleFilter)
			if err != nil || stale == nil || !pyrepr.Truthy(stale.GetOr("archived", nil)) {
				return
			}
			ok, err := db.UnarchiveRecoverableSession(ctx, pyrepr.Str(stale.GetOr("id", "")))
			if err != nil || !ok {
				return
			}
			if again, err := list(); err == nil {
				sessions = again
			}
		}()
	}
	windowed := 0
	data := make([]any, 0, len(sessions))
	for _, s := range sessions {
		if !pyrepr.Truthy(s.GetOr("pinned", nil)) {
			windowed++
		}
		data = append(data, sessionResponse(s))
	}
	return jsonResponse(pyjson.ObjectOf("object", "list", "data", data, "limit", int64(limit), "offset", int64(offset),
		"has_more", windowed >= limit), http.StatusOK, nil), nil
}

// Empty session row created in one write so concurrent same id creates cannot both succeed
func (a *APIServerAdapter) handleCreateSession(ctx context.Context, request *gateway.WebRequest) (*gateway.WebResponse, error) {
	body, errResp := readJSONBody(request)
	if errResp != nil {
		return errResp, nil
	}
	if botChat, err := createPeerBotChat(ctx, a, body); err != nil {
		return nil, err
	} else if botChat != nil {
		return botChat, nil
	}
	db := a.ensureSessionDB(ctx)
	if db == nil {
		return sessionDBUnavailable(), nil
	}
	rawID := pyrepr.Or(body.GetOr("id", nil), body.GetOr("session_id", nil))
	sessionID := fmt.Sprintf("api_%d_%s", int64(apiNow()), strings.ReplaceAll(uuid.NewString(), "-", "")[:8])
	if pyrepr.Truthy(rawID) {
		sessionID = strings.TrimSpace(pyrepr.Str(rawID))
	}
	if sessionID == "" || sessionHeaderUnsafeRE.MatchString(sessionID) || gateway.IsPathUnsafe(sessionID) {
		return errorResponse("Invalid session ID", http.StatusBadRequest, errOpts{Code: "invalid_session_id"}), nil
	}
	if len([]rune(sessionID)) > maxSessionHeaderLen {
		return errorResponse("Session ID too long", http.StatusBadRequest, errOpts{Code: "invalid_session_id"}), nil
	}
	systemPrompt, hasPrompt := body.Get("system_prompt")
	if hasPrompt && systemPrompt != nil {
		if _, ok := systemPrompt.(string); !ok {
			return errorResponse("system_prompt must be a string", http.StatusBadRequest, errOpts{Code: "invalid_system_prompt"}), nil
		}
	}
	source := normalizeSessionSource(pyrepr.Or(body.GetOr("source", nil), "api_server"))
	r := a.sessionRuntimeRequestFromBody(body)
	if lockErr := a.runtimeLockError(r); lockErr != nil {
		return lockErr, nil
	}
	var modelName any
	if m := cleanRuntimeID(r.requestedModel, 200); m != "" {
		modelName = m
	}
	var modelConfig any
	if r.requestedModel != "" || r.requestedProvider != "" {
		cfg := pyjson.ObjectOf("browser_model_lock", pyjson.ObjectOf(
			"provider", r.requestedProvider, "model", r.requestedModel, "model_options", r.modelOptions,
			"route_source", r.routeSource, "confirmed", r.requireModelLock, "updated_at", apiNow()))
		encoded, err := pyjson.Dumps(cfg)
		if err != nil {
			return nil, err
		}
		modelConfig = encoded
	}
	title, hasTitle := body.Get("title")
	var session *pyjson.Object
	var conflict [2]string
	var titleErr string
	err := db.ExecuteWrite(ctx, func(conn state.SQLiteConn) error {
		code, message, err := state.CreateConflict(ctx, conn, sessionID)
		if err != nil {
			return err
		}
		if code != "" {
			conflict = [2]string{code, message}
			return nil
		}
		if _, err := conn.ExecContext(ctx, "INSERT INTO sessions (\n                   id, source, model, model_config, system_prompt, started_at\n                ) VALUES (?, ?, ?, ?, ?, ?)",
			sessionID, source, modelName, modelConfig, systemPrompt, apiNow()); err != nil {
			return err
		}
		if hasTitle && title != nil {
			clean, err := state.SanitizeTitle(pyrepr.Str(title))
			if err != nil {
				var valueErr interface{ PyTypeName() string }
				if errors.As(err, &valueErr) && valueErr.PyTypeName() == "ValueError" {
					if _, derr := conn.ExecContext(ctx, "DELETE FROM sessions WHERE id = ?", sessionID); derr != nil {
						return derr
					}
					titleErr = err.Error()
					return nil
				}
				return err
			}
			if clean != "" {
				if _, _, err := db.ResolveTitleConflictOnConn(ctx, conn, sessionID, clean); err != nil {
					var valueErr interface{ PyTypeName() string }
					if errors.As(err, &valueErr) && valueErr.PyTypeName() == "ValueError" {
						if _, derr := conn.ExecContext(ctx, "DELETE FROM sessions WHERE id = ?", sessionID); derr != nil {
							return derr
						}
						titleErr = err.Error()
						return nil
					}
					return err
				}
			}
			var cleanValue any
			if clean != "" {
				cleanValue = clean
			}
			if _, err := conn.ExecContext(ctx, "UPDATE sessions SET title = ? WHERE id = ?", cleanValue, sessionID); err != nil {
				return err
			}
		}
		row, err := state.QueryRowObject(ctx, conn, "SELECT * FROM sessions WHERE id = ?", sessionID)
		if err != nil {
			return err
		}
		if row == nil {
			row = pyjson.ObjectOf("id", sessionID, "source", source, "model", modelName, "title", title)
		}
		session = row
		return nil
	})
	if err != nil {
		return nil, err
	}
	if conflict[0] != "" {
		return errorResponse(fmt.Sprintf("%s: %s", conflict[1], sessionID), http.StatusConflict, errOpts{Code: conflict[0]}), nil
	}
	if titleErr != "" {
		return errorResponse(titleErr, http.StatusBadRequest, errOpts{Code: "invalid_title"}), nil
	}
	return jsonResponse(pyjson.ObjectOf("object", "hermes.session", "session", sessionResponse(session)), http.StatusCreated, nil), nil
}

// One session row
func (a *APIServerAdapter) handleGetSession(ctx context.Context, request *gateway.WebRequest) (*gateway.WebResponse, error) {
	session, errResp, err := a.getExistingSessionOr404(ctx, request.MatchInfo["session_id"])
	if err != nil || errResp != nil {
		return errResp, err
	}
	return jsonResponse(pyjson.ObjectOf("object", "hermes.session", "session", sessionResponse(session)), http.StatusOK, nil), nil
}

// Updates the client safe session metadata, a pin applied last so it wins over hidden
func (a *APIServerAdapter) handlePatchSession(ctx context.Context, request *gateway.WebRequest) (*gateway.WebResponse, error) {
	sessionID := request.MatchInfo["session_id"]
	session, errResp, err := a.getExistingSessionOr404(ctx, sessionID)
	if err != nil || errResp != nil {
		return errResp, err
	}
	body, errResp := readJSONBody(request)
	if errResp != nil {
		return errResp, nil
	}
	allowed := map[string]bool{"title": true, "end_reason": true, "pinned": true, "archived": true, "hidden": true, "unread": true}
	var unknown []string
	for _, k := range body.Keys {
		if !allowed[k] {
			unknown = append(unknown, k)
		}
	}
	sort.Strings(unknown)
	if len(unknown) > 0 {
		return errorResponse("Unsupported session fields: "+strings.Join(unknown, ", "), http.StatusBadRequest,
			errOpts{Code: "unsupported_session_field"}), nil
	}
	for _, flag := range []string{"pinned", "archived", "hidden", "unread"} {
		if v, ok := body.Get(flag); ok {
			if _, isBool := v.(bool); !isBool {
				return errorResponse(fmt.Sprintf("'%s' must be a boolean", flag), http.StatusBadRequest,
					errOpts{Code: "invalid_session_field"}), nil
			}
		}
	}
	db := a.ensureSessionDB(ctx)
	if db == nil {
		return sessionDBUnavailable(), nil
	}
	if v, ok := body.Get("title"); ok {
		text := ""
		if v != nil {
			text = pyrepr.Str(v)
		}
		if _, err := db.SetSessionTitle(ctx, sessionID, text); err != nil {
			var valueErr interface{ PyTypeName() string }
			if errors.As(err, &valueErr) && valueErr.PyTypeName() == "ValueError" {
				return errorResponse(err.Error(), http.StatusBadRequest, errOpts{Code: "invalid_title"}), nil
			}
			return nil, err
		}
	}
	if v, ok := body.Get("archived"); ok {
		if _, err := db.SetSessionArchived(ctx, sessionID, v.(bool), true); err != nil {
			return nil, err
		}
	}
	if v, ok := body.Get("hidden"); ok {
		if _, err := db.SetSessionHidden(ctx, sessionID, v.(bool)); err != nil {
			return nil, err
		}
	}
	if v, ok := body.Get("pinned"); ok {
		if _, err := db.SetSessionPinned(ctx, sessionID, v.(bool)); err != nil {
			return nil, err
		}
	}
	if v, ok := body.Get("unread"); ok {
		if _, err := db.SetSessionRead(ctx, sessionID, !v.(bool)); err != nil {
			return nil, err
		}
	}
	if reason := body.GetOr("end_reason", nil); pyrepr.Truthy(reason) {
		if err := db.EndSession(sessionID, pyrepr.Str(reason)); err != nil {
			return nil, err
		}
	}
	if fresh, err := db.GetSession(ctx, sessionID); err != nil {
		return nil, err
	} else if fresh != nil {
		session = fresh
	}
	return jsonResponse(pyjson.ObjectOf("object", "hermes.session", "session", sessionResponse(session)), http.StatusOK, nil), nil
}

// Hard deletes a session with its routing entries and the image bytes nothing else holds
func (a *APIServerAdapter) handleDeleteSession(ctx context.Context, request *gateway.WebRequest) (*gateway.WebResponse, error) {
	sessionID := request.MatchInfo["session_id"]
	_, errResp, err := a.getExistingSessionOr404(ctx, sessionID)
	if err != nil || errResp != nil {
		return errResp, err
	}
	db := a.ensureSessionDB(ctx)
	if db == nil {
		return sessionDBUnavailable(), nil
	}
	sessionsDir := filepath.Join(home.Dir(ctx), "sessions")
	deleted, err := db.DeleteSession(ctx, sessionID, state.DeleteSessionOptions{SessionsDir: sessionsDir, ExcludeActiveWriteGuards: true})
	if err != nil {
		var guard *state.SessionActiveWriteGuardError
		if errors.As(err, &guard) {
			return errorResponse(err.Error(), http.StatusConflict, errOpts{Code: "session_active_turn"}), nil
		}
		return nil, err
	}
	if deleted {
		runner := a.GatewayRunner
		if runner == nil {
			if app := a.currentApp(request); app != nil {
				runner = app.State["gateway_runner"]
			}
		}
		if holder, ok := runner.(interface{ SessionStore() *gateway.SessionStore }); ok && holder.SessionStore() != nil {
			if _, err := holder.SessionStore().RemoveBySessionID(ctx, sessionID); err != nil {
				return nil, err
			}
		}
		if err := gateway.CollectUnheldAPIImages(ctx, db); err != nil {
			return nil, err
		}
	}
	return jsonResponse(pyjson.ObjectOf("object", "hermes.session.deleted", "id", sessionID, "deleted", deleted), http.StatusOK, nil), nil
}

// One page of a session's messages across its compression lineage
func (a *APIServerAdapter) handleSessionMessages(ctx context.Context, request *gateway.WebRequest) (*gateway.WebResponse, error) {
	sessionID := request.MatchInfo["session_id"]
	_, errResp, err := a.getExistingSessionOr404(ctx, sessionID)
	if err != nil || errResp != nil {
		return errResp, err
	}
	db := a.ensureSessionDB(ctx)
	resolvedID, err := db.ResolveResumeSessionID(sessionID)
	if err != nil {
		return nil, err
	}
	if resolvedID == "" {
		resolvedID = sessionID
	}
	rawLimit, hasLimit := request.Query()["limit"]
	rawOffset := "0"
	if v, ok := request.Query()["offset"]; ok && len(v) > 0 {
		rawOffset = v[0]
	}
	order, hasOrder := "", false
	if v, ok := request.Query()["order"]; ok && len(v) > 0 {
		order, hasOrder = v[0], true
	}
	if hasOrder && order != "oldest" && order != "latest" {
		return errorResponse("order must be one of: oldest, latest", http.StatusBadRequest, errOpts{Code: "invalid_pagination"}), nil
	}
	offset, okOffset := pyIntOf(rawOffset)
	requestedLimit := -2
	okLimit := true
	if hasLimit && len(rawLimit) > 0 {
		requestedLimit, okLimit = pyIntOf(rawLimit[0])
	}
	if !okOffset || !okLimit {
		offset, requestedLimit = -1, -1
	}
	if offset < 0 || (requestedLimit != -2 && requestedLimit < 0) {
		return errorResponse("limit and offset must be non-negative integers", http.StatusBadRequest, errOpts{Code: "invalid_pagination"}), nil
	}
	defaultPage := requestedLimit == -2
	latestPage := order == "latest" || (!hasOrder && defaultPage)
	limit := 500
	if !defaultPage {
		limit = min(requestedLimit, 500)
	}
	includeCompacted := gateway.APICoerceRequestBool(queryValue(request, "include_compacted"), false)
	messages, err := db.GetMessages(ctx, resolvedID, state.GetMessagesOptions{Limit: &limit, Offset: offset, Latest: latestPage,
		IncludeCompacted: includeCompacted, IncludeAncestors: true})
	if err != nil {
		return nil, err
	}
	data := make([]any, 0, len(messages))
	for _, m := range messages {
		data = append(data, messageResponse(m))
	}
	orderOut := order
	if !hasOrder {
		orderOut = "oldest"
		if defaultPage {
			orderOut = "latest"
		}
	}
	return jsonResponse(pyjson.ObjectOf("object", "list", "session_id", resolvedID, "data", data,
		"pagination", pyjson.ObjectOf("limit", int64(limit), "offset", int64(offset), "order", orderOut,
			"returned", int64(len(messages)))), http.StatusOK, nil), nil
}

// Branches a session the way the CLI branch command does, the child created before the source ends
func (a *APIServerAdapter) handleForkSession(ctx context.Context, request *gateway.WebRequest) (*gateway.WebResponse, error) {
	sourceID := request.MatchInfo["session_id"]
	source, errResp, err := a.getExistingSessionOr404(ctx, sourceID)
	if err != nil || errResp != nil {
		return errResp, err
	}
	body, errResp := readJSONBody(request)
	if errResp != nil {
		return errResp, nil
	}
	db := a.ensureSessionDB(ctx)
	forkRaw := pyrepr.Or(body.GetOr("id", nil), body.GetOr("session_id", nil))
	forkID := fmt.Sprintf("api_%d_%s", int64(apiNow()), strings.ReplaceAll(uuid.NewString(), "-", "")[:8])
	if pyrepr.Truthy(forkRaw) {
		forkID = pyrepr.Str(forkRaw)
	}
	forkID = strings.TrimSpace(forkID)
	if forkID == "" || sessionHeaderUnsafeRE.MatchString(forkID) {
		return errorResponse("Invalid session ID", http.StatusBadRequest, errOpts{Code: "invalid_session_id"}), nil
	}
	if existing, err := db.GetSession(ctx, forkID); err != nil {
		return nil, err
	} else if existing != nil {
		return errorResponse("Session already exists: "+forkID, http.StatusConflict, errOpts{Code: "session_exists"}), nil
	}
	if err := db.CreateSession(state.NewSession{ID: forkID, Source: "api_server", Model: textOf(source.GetOr("model", nil)),
		SystemPrompt: textOf(source.GetOr("system_prompt", nil)), ParentSessionID: sourceID,
		ModelConfig: map[string]any{"_branched_from": sourceID}}); err != nil {
		return nil, err
	}
	if err := db.EndSession(sourceID, "branched"); err != nil {
		return nil, err
	}
	messages, err := db.GetMessages(ctx, sourceID, state.GetMessagesOptions{})
	if err != nil {
		return nil, err
	}
	rows := make([]map[string]any, 0, len(messages))
	for _, m := range messages {
		rows = append(rows, pyjson.Plain(m).(map[string]any))
	}
	if err := db.ReplaceMessages(ctx, forkID, rows, state.ReplaceOptions{}); err != nil {
		return nil, err
	}
	title, ok := body.Get("title")
	if !ok || title == nil {
		base := pyrepr.Str(pyrepr.Or(source.GetOr("title", nil), "fork"))
		title = base + " fork"
		if next, err := db.GetNextTitleInLineage(ctx, base); err == nil {
			title = next
		}
	}
	if _, err := db.SetSessionTitle(ctx, forkID, pyrepr.Str(title)); err != nil {
		var valueErr interface{ PyTypeName() string }
		if errors.As(err, &valueErr) && valueErr.PyTypeName() == "ValueError" {
			return errorResponse(err.Error(), http.StatusBadRequest, errOpts{Code: "invalid_title"}), nil
		}
		return nil, err
	}
	fork, err := db.GetSession(ctx, forkID)
	if err != nil {
		return nil, err
	}
	if fork == nil {
		fork = pyjson.ObjectOf("id", forkID, "parent_session_id", sourceID)
	}
	return jsonResponse(pyjson.ObjectOf("object", "hermes.session", "session", sessionResponse(fork)), http.StatusCreated, nil), nil
}

// Session authority of the runner, ok false when the runner has none
func (a *APIServerAdapter) sessionAuthority() (*gateway.SessionAuthority, bool) {
	holder, ok := a.GatewayRunner.(interface {
		SessionAuthority() *gateway.SessionAuthority
	})
	if !ok || holder == nil {
		return nil, false
	}
	authority := holder.SessionAuthority()
	return authority, authority != nil
}
