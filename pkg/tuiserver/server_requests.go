package tuiserver

import (
	"context"
	"encoding/json"
	"errors"
	"image"
	_ "image/gif"
	_ "image/jpeg"
	_ "image/png"
	"os"
	"path/filepath"
	"strings"
	"sync"

	_ "golang.org/x/image/webp"

	"github.com/nickheyer/icebath/pkg/agent"
	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pyint"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/provider"
	"github.com/nickheyer/icebath/pkg/tools"
	"github.com/nickheyer/icebath/pkg/tuigateway"
)

// JSON RPC handler of one registered method
type Handler func(ctx context.Context, rid any, params map[string]any) map[string]any

// Registered JSON RPC methods by name
var (
	methodsMu sync.RWMutex
	methods   = map[string]Handler{}
)

// Defaults of the dashboard process isolation keys the REST editor shares
const (
	dashboardTurnIsolationDefault            = false
	dashboardComputeHostHeartbeatSecsDefault = 15
	dashboardComputeHostRespawnMaxDefault    = 3
)

// Successful JSON RPC response
func ok(rid any, result any) map[string]any {
	return map[string]any{"jsonrpc": "2.0", "id": rid, "result": result}
}

// JSON RPC error response, data only when given
func rpcErr(rid any, code int, msg string, data any) map[string]any {
	errObj := map[string]any{"code": code, "message": msg}
	if data != nil {
		errObj["data"] = data
	}
	return map[string]any{"jsonrpc": "2.0", "id": rid, "error": errObj}
}

// Registers a JSON RPC method, the one registration seam every handler module goes through
func registerMethod(name string, fn Handler) {
	methodsMu.Lock()
	defer methodsMu.Unlock()
	methods[name] = fn
}

// Registered handler of a method name, nil when none
func lookupMethod(name string) Handler {
	methodsMu.RLock()
	defer methodsMu.RUnlock()
	return methods[name]
}

// Request id, method and params of a JSON RPC request valid enough for local dispatch, else its error response
func normalizeRequest(req any) (rid any, method string, params map[string]any, errResp map[string]any) {
	obj, isDict := req.(map[string]any)
	if !isDict {
		return nil, "", nil, rpcErr(nil, -32600, "invalid request: expected an object", nil)
	}
	rid = obj["id"]
	method, _ = obj["method"].(string)
	if method == "" {
		return rid, "", nil, rpcErr(rid, -32600, "invalid request: method must be a non-empty string", nil)
	}
	raw, present := obj["params"]
	if !present || raw == nil {
		return rid, method, map[string]any{}, nil
	}
	params, isDict = raw.(map[string]any)
	if !isDict {
		return rid, "", nil, rpcErr(rid, -32602, "invalid params: expected an object", nil)
	}
	return rid, method, params, nil
}

// Client safe pending approval with its choices filled in and command and description redacted
func approvalRequestPayload(ctx context.Context, data map[string]any) map[string]any {
	payload := map[string]any{}
	for key, value := range data {
		payload[key] = value
	}
	if _, present := payload["choices"]; !present {
		choices := []any{"once"}
		if !truthy(payload["smart_denied"]) && payload["allow_session"] != false {
			choices = append(choices, "session")
			if payload["allow_permanent"] != false {
				choices = append(choices, "always")
			}
		}
		payload["choices"] = append(choices, "deny")
	}
	for _, key := range []string{"command", "description"} {
		if value, present := payload[key]; present {
			payload[key] = gateway.RedactApprovalCommand(ctx, value)
		}
	}
	return payload
}

// Server to client requests still waiting on a session's renderer, else the compute host mirrored one
func openRequests(sid string) []any {
	if reqs := tuigateway.OpenRequests(sid); len(reqs) > 0 {
		out := make([]any, len(reqs))
		for i, req := range reqs {
			out[i] = req
		}
		return out
	}
	session := tuigateway.SessionRecord(sid)
	if session == nil {
		return []any{}
	}
	unlock := lockHistory(session)
	defer unlock()
	if mirrored, isDict := session.Get("_compute_host_open_request").(map[string]any); isDict {
		return []any{copyDict(mirrored)}
	}
	return []any{}
}

// Oldest unresolved approval of a session as its client payload, nil when none
func pendingApprovalRequestPayload(ctx context.Context, sessionKey string) (payload map[string]any) {
	defer func() {
		if r := recover(); r != nil {
			logger().Debug("failed to read pending approval for "+sessionKey, "error", r)
			payload = nil
		}
	}()
	approval := tools.GetPendingGatewayApproval(sessionKey)
	if len(approval) == 0 {
		return nil
	}
	return approvalRequestPayload(ctx, approval)
}

// Sends a queue backed approval server request with the command redacted, withdrawn when no client can answer
func emitApprovalRequest(ctx context.Context, sid string, data map[string]any) {
	payload := approvalRequestPayload(ctx, data)
	requestID := strOr(payload["request_id"])
	sessionKey := strOr(tuigateway.SessionRecord(sid).Get("session_key"))
	onResult := func(result *pyjson.Object) {
		if result == nil {
			if requestID != "" {
				tools.WithdrawGatewayApproval(sessionKey, requestID,
					"the attached client cannot answer approval requests (update the Icebath app)")
			}
			return
		}
		choice := strOr(result.GetOr("choice", nil))
		if choice == "" {
			choice = "deny"
		}
		tools.ResolveGatewayApproval(sessionKey, choice, truthy(result.GetOr("all", nil)), "", requestID)
	}
	settle, err := tuigateway.SendAsync("approval", sid, objectOf(payload), onResult)
	if err != nil {
		panic(err)
	}
	if requestID != "" && !tools.RegisterGatewaySettle(sessionKey, requestID, settle) {
		settle("resolved")
	}
}

// Emits a status update, re-tagging compaction progress lifecycle text as compacting
func statusUpdate(ctx context.Context, sid, kind string, text *string) {
	body := kind
	if text != nil {
		body = *text
	}
	if body = strings.TrimSpace(body); body == "" {
		return
	}
	outKind := "status"
	if text != nil {
		outKind = kind
	}
	if outKind == "lifecycle" && agent.IsCompactionProgressStatus(body) {
		outKind = "compacting"
	}
	tuigateway.Emit(ctx, "status.update", sid, map[string]any{"kind": outKind, "text": body})
}

// Attachment name with its pixel size and a rough tile based token estimate when the image decodes
func imageMeta(path string) map[string]any {
	meta := map[string]any{"name": filepath.Base(path)}
	f, err := os.Open(path)
	if err != nil {
		return meta
	}
	defer f.Close()
	cfg, _, err := image.DecodeConfig(f)
	if err != nil {
		return meta
	}
	width, height := cfg.Width, cfg.Height
	tiles := 0
	if width > 0 && height > 0 {
		tiles = max(1, (width+511)/512) * max(1, (height+511)/512)
	}
	meta["width"], meta["height"], meta["token_estimate"] = width, height, tiles*85
	return meta
}

// Integer config value at or above a floor, the default otherwise
func coerceIntConfigValue(value any, def, minValue int) int {
	coerced, err := pyint.Int64(value)
	if err != nil {
		return def
	}
	if int(coerced) >= minValue {
		return int(coerced)
	}
	return def
}

// Dashboard process isolation config with read site defaults
func loadDashboardProcessIsolationConfig(ctx context.Context, cfg map[string]any) map[string]any {
	root := cfg
	if root == nil {
		root = loadCfg(ctx)
	}
	dash, _ := root["dashboard"].(map[string]any)
	turnIsolation := dashboardTurnIsolationDefault
	if raw, present := dash["turn_isolation"]; present && raw != nil {
		turnIsolation = isTruthyValue(raw)
	}
	return map[string]any{
		"turn_isolation":              turnIsolation,
		"compute_host_heartbeat_secs": coerceIntConfigValue(dash["compute_host_heartbeat_secs"], dashboardComputeHostHeartbeatSecsDefault, 1),
		"compute_host_respawn_max":    coerceIntConfigValue(dash["compute_host_respawn_max"], dashboardComputeHostRespawnMaxDefault, 0),
	}
}

// First live record holding the session key
func sessionForKey(sessionKey string) *tuigateway.Session {
	for _, live := range tuigateway.LiveSessions() {
		if live.Session.Get("session_key") == sessionKey {
			return live.Session
		}
	}
	return nil
}

// Session db source of a record, its own source else the resolved platform
func sessionSource(session *tuigateway.Session) string {
	if source := strings.TrimSpace(strOr(session.Get("source"))); source != "" {
		return source
	}
	return resolveSessionPlatform()
}

// Context carrying a fresh tool env with the session variables of a live session bound, and that env
func setSessionContext(ctx context.Context, sessionKey string, cwd *string, uiSessionID string) (context.Context, *tools.Env) {
	var env *tools.Env
	func() {
		defer func() {
			if recover() != nil {
				env = nil
			}
		}()
		var sess *tuigateway.Session
		if sessionKey != "" {
			sess = sessionForKey(sessionKey)
		}
		resolved := ""
		if cwd != nil {
			resolved = *cwd
		} else if sess != nil {
			resolved = strOr(sess.Get("cwd"))
		}
		source, profile, sessionID := resolveSessionPlatform(), currentProfileName(ctx), sessionKey
		if sess != nil {
			source = sessionSource(sess)
			if name := home.ProfileNameFor(strOr(sess.Get("profile_home"))); name != "" {
				profile = name
			}
			if a := sessionAgent(sess); a != nil && a.SessionID() != "" {
				sessionID = a.SessionID()
			}
		}
		cronSession := ""
		bound := &tools.Env{Home: home.Dir(ctx), Values: &sync.Map{}, TerminalScope: tools.GetTerminalScope(ctx)}
		tools.SetSessionVars(bound, tools.SessionVars{SessionKey: sessionKey, SessionID: sessionID, Source: source,
			Profile: profile, Cwd: resolved, UISessionID: uiSessionID, CronSession: &cronSession})
		env = bound
	}()
	if env == nil {
		return ctx, nil
	}
	return tools.WithToolEnv(ctx, env), env
}

// Clears the session variables a context bound
func clearSessionContext(env *tools.Env) {
	if env != nil {
		suppress(func() { tools.ClearSessionVars(env) })
	}
}

// Routes approvals through the gateway callbacks instead of a terminal prompt
func enableGatewayPrompts() {
	for key, value := range map[string]string{"ICEBATH_GATEWAY_SESSION": "1", "ICEBATH_EXEC_ASK": "1", "ICEBATH_INTERACTIVE": "1"} {
		_ = os.Setenv(key, value)
	}
}

// Blocking server to client string request every bridge goes through
var askRequest = ask

// Server to client request answered by one string, empty when skipped, timed out or cancelled
func ask(ctx context.Context, method, sid string, params map[string]any, timeout *float64) string {
	result := sendResult(ctx, tuigateway.Send)(method, sid, objectOf(params), timeout, nil)
	if result == nil {
		return ""
	}
	value, present := result.Get("value")
	if !present {
		return ""
	}
	if text, isText := value.(string); isText {
		return text
	}
	encoded, err := pyjson.Dumps(value, pyjson.EnsureASCII(false))
	if err != nil {
		panic(err)
	}
	return encoded
}

// Clarify wait from the canonical config, nil for unlimited, 300 seconds when the config cannot be read
func clarifyTimeoutSeconds(ctx context.Context) (timeout *float64) {
	fallback := 300.0
	defer func() {
		if recover() != nil {
			timeout = &fallback
		}
	}()
	seconds := tools.GetClarifyTimeout(&tools.Env{Home: home.Dir(ctx)})
	if seconds <= 0 {
		return nil
	}
	value := float64(seconds)
	return &value
}

// Clarify tool callback bridged onto one clarify server request carrying only the wire fields
func clarifyBlock(ctx context.Context, sid string, questions []map[string]any) map[string]any {
	wire := make([]any, 0, len(questions))
	qids := make([]string, 0, len(questions))
	for _, entry := range questions {
		wire = append(wire, map[string]any{"qid": entry["qid"], "question": entry["question"], "choices": entry["choices"],
			"multi_select": truthy(entry["multi_select"])})
		qids = append(qids, strOr(entry["qid"]))
	}
	result := sendResult(ctx, tuigateway.Send)("clarify", sid, objectOf(map[string]any{"questions": wire}), clarifyTimeoutSeconds(ctx), qids)
	if result == nil {
		return map[string]any{"answers": map[string]any{}, "outcome": "undelivered"}
	}
	settled, _ := pyjson.Plain(result).(map[string]any)
	return settled
}

// Ordered JSON object of a payload map
func objectOf(payload map[string]any) *pyjson.Object {
	raw, err := json.Marshal(payload)
	if err != nil {
		panic(err)
	}
	obj, err := pyjson.LoadsDict(string(raw))
	if err != nil {
		panic(err)
	}
	return obj
}

// Locks the history lock of a session record, creating it on first use, and returns the unlock
func lockHistory(session *tuigateway.Session) func() {
	lock, _ := session.SetDefault("history_lock", &sync.Mutex{}).(sync.Locker)
	if lock == nil {
		return func() {}
	}
	lock.Lock()
	return lock.Unlock
}

// is_truthy_value of utils, strings truthy only when they spell an affirmative word
func isTruthyValue(v any) bool { return provider.IsTruthyValue(v) }

// Server request sender of the request helpers, its answer nil when the wait was cancelled or timed out
type requestSender func(ctx context.Context, method, sid string, params *pyjson.Object, timeout *float64, qids []string) (*pyjson.Object, error)

// Sender whose cancelled wait reads as no answer and whose registration failure raises
func sendResult(ctx context.Context, send requestSender) func(method, sid string, params *pyjson.Object, timeout *float64, qids []string) *pyjson.Object {
	return func(method, sid string, params *pyjson.Object, timeout *float64, qids []string) *pyjson.Object {
		result, err := send(ctx, method, sid, params, timeout, qids)
		if err != nil {
			if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
				return nil
			}
			panic(err)
		}
		return result
	}
}
