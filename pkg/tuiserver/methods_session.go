package tuiserver

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"sync"

	"github.com/google/uuid"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pyint"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/internal/pythread"
	"github.com/nickheyer/icebath/pkg/state"
	"github.com/nickheyer/icebath/pkg/tuigateway"
)

var sessionRegistry = &handlerRegistry{}

func init() {
	sessionRegistry.method("session.create", func(ctx context.Context, rid any, params map[string]any) map[string]any {
		return createSession(ctx, rid, params, false)
	})
	sessionRegistry.method("session.branch_stored", func(ctx context.Context, rid any, params map[string]any) map[string]any {
		return createSession(ctx, rid, params, true)
	})
	registerSessionBranchHandlers()
	registerSessionListHandlers()
	registerSessionResumeHandlers()
	sessionRegistry.install()
}

// Handler body of a session scoped method, given the resolved live session
type sessionHandler func(ctx context.Context, rid any, params map[string]any, session *tuigateway.Session) map[string]any

// Handler resolving params session_id without waiting on the agent build
func withSession(fn sessionHandler) Handler {
	return func(ctx context.Context, rid any, params map[string]any) map[string]any {
		session, errResp := sessNowait(ctx, params, rid)
		if errResp != nil {
			return errResp
		}
		return fn(ctx, rid, params, session)
	}
}

// Handler resolving params session_id once its agent build finished
func withLiveSession(fn sessionHandler) Handler {
	return func(ctx context.Context, rid any, params map[string]any) map[string]any {
		session, errResp := sess(ctx, params, rid)
		if errResp != nil {
			return errResp
		}
		return fn(ctx, rid, params, session)
	}
}

// Declares a session scoped method, live ones waiting on the agent build
func sessionMethod(name string, live bool, fn sessionHandler) {
	if live {
		sessionRegistry.method(name, withLiveSession(fn))
		return
	}
	sessionRegistry.method(name, withSession(fn))
}

// Session scoped handler handed the session's own profile db, the code error when it is unavailable
func withSessionDB(code int, fn func(ctx context.Context, rid any, params map[string]any, session *tuigateway.Session, db *state.DB) map[string]any) sessionHandler {
	return func(ctx context.Context, rid any, params map[string]any, session *tuigateway.Session) map[string]any {
		db, release := sessionDB(ctx, session)
		defer release()
		if db == nil {
			return dbUnavailableError(ctx, rid, code)
		}
		return fn(ctx, rid, params, session, db)
	}
}

// Handler handed the params profile db, the code error when it is unavailable
func withProfileDB(code int, fn func(ctx context.Context, rid any, params map[string]any, db *state.DB) map[string]any) Handler {
	return func(ctx context.Context, rid any, params map[string]any) map[string]any {
		db, release, err := profileDB(ctx, params, false)
		panicIf(err)
		defer release()
		if db == nil {
			return dbUnavailableError(ctx, rid, code)
		}
		return fn(ctx, rid, params, db)
	}
}

// Truthiness of a boolean flag param, absent reading false
func flagParam(params map[string]any, name string) bool {
	raw, present := params[name]
	if !present {
		raw = false
	}
	return isTruthyValue(raw)
}

// Python int of a param, the default when missing or unparsable and the overflow of an infinite value raised
func intParam(params map[string]any, key string, def int) int {
	raw, present := params[key]
	if !present {
		return def
	}
	n, err := pyint.Of(raw)
	if err != nil {
		var pyErr *pyint.Error
		if errors.As(err, &pyErr) && pyErr.Kind == "OverflowError" {
			panic(err)
		}
		return def
	}
	if !n.IsInt64() {
		panic(&pyint.Error{Kind: "OverflowError", Msg: "Python int too large to convert to C ssize_t"})
	}
	return int(n.Int64())
}

// Fresh runtime id and resolved db source of a session minted from params
func newRuntimeIDs(params map[string]any) (string, string) {
	return strings.ReplaceAll(uuid.NewString(), "-", "")[:8], resolveSessionSource(strParam(params, "source", ""))
}

// Home, secret and terminal scope of a profile home for an agent build, the same scope a turn binds
func profileBuildScope(ctx context.Context, profileHome string) context.Context {
	return profileRuntimeScope(ctx, profileHome, true)
}

// Builds an agent with the session context of its key bound for the build and cleared after
func makeAgentInContext(ctx context.Context, sid, key string, o makeAgentOptions) (any, error) {
	var cwd *string
	if o.cwdOverride != "" {
		override := o.cwdOverride
		cwd = &override
	}
	scoped, env := setSessionContext(ctx, key, cwd, "")
	defer clearSessionContext(env)
	o.sessionID = key
	return makeAgent(scoped, sid, key, o)
}

// Dedicated handle on a profile home's state db with its release, else the shared launch db
func profileSessionDB(ctx context.Context, profileHome string) (*state.DB, bool, error) {
	if profileHome != "" {
		db, err := state.Acquire(ctx, filepath.Join(profileHome, "state.db"))
		return db, true, err
	}
	return getDB(ctx), false, nil
}

// Releases a dedicated db handle, swallowing any failure
func releaseDB(db *state.DB) {
	suppress(func() { state.ReleaseOrClose(db) })
}

// Child row, copied parent transcript and title of a branch, compensating a failed copy unless the disk is full
func persistBranch(ctx context.Context, db *state.DB, newKey, parentKey, title string, history []map[string]any, source string, cwd *string,
	profileName, model string, copyFields []string, compensate bool, titleSource string, userID *string) error {
	parentPrompt := ""
	if failure := catchPanic(func() {
		row, err := db.GetSession(ctx, parentKey)
		panicIf(err)
		if row != nil {
			parentPrompt = strOr(row.GetOr("system_prompt", nil))
		}
	}); failure != nil {
		logger().Debug(fmt.Sprintf("branch: parent system prompt read failed for %s", parentKey), "exc_info", failureText(failure))
	}
	row := state.NewSession{ID: newKey, Source: source, Model: model, ModelConfig: map[string]any{"_branched_from": parentKey},
		ParentSessionID: parentKey, ProfileName: profileName, SystemPrompt: parentPrompt}
	if cwd != nil {
		row.Cwd = *cwd
	}
	if userID != nil {
		row.UserID = *userID
	}
	if err := db.CreateSession(row); err != nil {
		return err
	}
	err := func() error {
		rows := make([]map[string]any, 0, len(history))
		for _, msg := range history {
			role, present := msg["role"]
			if !present {
				role = "user"
			}
			copied := map[string]any{"role": role, "content": msg["content"]}
			for _, field := range copyFields {
				copied[field] = msg[field]
			}
			for field, value := range state.MessageIdentity(msg, false) {
				copied[field] = value
			}
			rows = append(rows, copied)
		}
		chunk := 500
		if _, err := db.AppendMessagesBatch(ctx, newKey, rows, state.AppendGuards{}, &chunk); err != nil {
			return err
		}
		if titleSource == "user" {
			if _, err := db.SetSessionTitle(ctx, newKey, title); err != nil {
				return err
			}
		} else if _, err := db.SetAutoTitleContext(ctx, newKey, title, titleSource); err != nil {
			return err
		}
		state.SyncFlushedMessageMarkers(history, rows)
		return nil
	}()
	if err != nil && compensate && !state.IsDiskFullError(err) {
		if _, discardErr := db.DiscardUnadmittedSession(ctx, newKey); discardErr != nil {
			logger().Debug(fmt.Sprintf("branch seed compensation delete failed for %s", newKey), "exc_info", discardErr.Error())
		}
	}
	return err
}

// Dict rows of a history list, the AttributeError of a non dict entry raised
func historyRows(history []any) []map[string]any {
	out := make([]map[string]any, 0, len(history))
	for _, msg := range history {
		m, isDict := msg.(map[string]any)
		if !isDict {
			panic(fmt.Errorf("'%T' object has no attribute 'get'", msg))
		}
		out = append(out, m)
	}
	return out
}

// Persists a seeded desktop branch child at create, leaving the first prompt to retry a failed seed
func seedBranchRow(ctx context.Context, record *tuigateway.Session, key, parentSessionID string, history []any, source, profileHome string) {
	failure := catchPanic(func() {
		db, release := sessionDB(ctx, record)
		defer release()
		if db == nil {
			return
		}
		title, err := branchTitle(ctx, db, parentSessionID)
		panicIf(err)
		var cwd *string
		if !isRemoteLaunchCwd(ctx, record) {
			recorded := strOr(record.Get("cwd"))
			cwd = &recorded
		}
		profileName := home.ProfileNameFor(profileHome)
		if profileName == "" {
			profileName = currentProfileName(ctx)
		}
		model, _ := sessionDefaultRoute(ctx, record)
		panicIf(persistBranch(ctx, db, key, parentSessionID, title, historyRows(history), source, cwd, profileName, model,
			nil, true, "derived", tuigateway.SessionAuthUserID(record)))
		record.Set("pending_title", nil)
		record.Set("_branch_seed_persisted", true)
	})
	if failure != nil {
		logger().Warn(fmt.Sprintf("seeded-branch persistence failed for %s; falling back to lazy row creation", key), "exc_info", failureText(failure))
	}
}

// Persists a parentless seeded session at create with its client title, rolling back a partial copy
func seedRow(ctx context.Context, record *tuigateway.Session) {
	key := strOr(record.Get("session_key"))
	stopped := false
	if failure := catchPanic(func() {
		if !ensureSessionDBRow(ctx, record) {
			stopped = true
			return
		}
		persistBranchSeed(ctx, record)
	}); failure != nil {
		logger().Warn(fmt.Sprintf("seeded-session persistence failed for %s; falling back to lazy row creation", key), "exc_info", failureText(failure))
	}
	if stopped {
		return
	}
	if !truthy(record.Get("_branch_seed_persisted")) {
		suppress(func() {
			db, release := sessionDB(ctx, record)
			defer release()
			if db != nil {
				_, err := db.DiscardUnadmittedSession(ctx, key)
				panicIf(err)
			}
		})
		return
	}
	if failure := catchPanic(func() {
		title := record.Get("pending_title")
		if !truthy(title) {
			return
		}
		db, release := sessionDB(ctx, record)
		defer release()
		if db == nil {
			return
		}
		written, err := db.SetSessionTitle(ctx, key, strOr(title))
		panicIf(err)
		if written {
			record.Set("pending_title", nil)
		}
	}); failure != nil {
		logger().Debug(fmt.Sprintf("seeded-session title write failed for %s; pending_title stays queued", key), "exc_info", failureText(failure))
	}
}

// Lazy session info of a created session, the client's route pick else the profile default
func createdSessionInfo(ctx context.Context, record *tuigateway.Session, override map[string]any, cwd, profile string) map[string]any {
	info := lazyInfoRoute(ctx, record, override)
	info["tools"] = map[string]any{}
	info["skills"] = map[string]any{}
	info["cwd"] = cwd
	info["branch"] = tuigateway.Branch(cwd)
	info["project"] = projectInfoForCwd(ctx, cwd)
	info["lazy"] = true
	info["desktop_contract"] = desktopBackendContract
	info["profile_name"] = responseProfileName(ctx, profile)
	return info
}

// Wire messages of a history in a profile home, raising a conversion failure
func wireMessages(ctx context.Context, history []any, profileHome string) []any {
	messages, err := tuigateway.HistoryToMessages(ctx, history, profileHome, true)
	panicIf(err)
	return messages
}

// Answer of a retried create whose key still maps to a live session, false once the key expired or closed
func idempotentCreateHit(ctx context.Context, rid any, idemKey, profile string, copyParentHistory bool) (map[string]any, bool) {
	sessionsLock.Lock()
	defer sessionsLock.Unlock()
	now := nowSeconds()
	for key, entry := range idempotencyKeys {
		if now-entry.at > idempotencyKeyTTL {
			delete(idempotencyKeys, key)
		}
	}
	existing, known := idempotencyKeys[idemKey]
	if !known {
		return nil, false
	}
	session := tuigateway.SessionRecord(existing.sid)
	if session == nil {
		delete(idempotencyKeys, idemKey)
		return nil, false
	}
	idempotencyKeys[idemKey] = idempotencyEntry{sid: existing.sid, at: now}
	history := historyList(session.Get("history"))
	override, _ := session.Get("model_override").(map[string]any)
	if override == nil {
		override = map[string]any{}
	}
	cwd := strOr(session.Get("cwd"))
	result := map[string]any{"session_id": existing.sid, "stored_session_id": session.Get("session_key"), "message_count": len(history),
		"info": createdSessionInfo(ctx, session, override, cwd, profile)}
	if copyParentHistory {
		result["messages_omitted"] = true
	} else {
		result["messages"] = wireMessages(ctx, history, strOr(session.Get("profile_home")))
	}
	return ok(rid, result), true
}

// session.create, copyParentHistory reading the parent transcript server side and leaving it off the reply
func createSession(ctx context.Context, rid any, params map[string]any, copyParentHistory bool) map[string]any {
	profile := pystr.Strip(strOr(params["profile"]))
	profileHomeDir, err := profileHome(profile)
	panicIf(err)
	conflict, err := modelOverrideConflict(ctx, params, func(c context.Context) context.Context { return profileBuildScope(c, profileHomeDir) })
	panicIf(err)
	if conflict != nil {
		message := strOr(conflict["message"])
		delete(conflict, "message")
		return rpcErr(rid, -32602, message, conflict)
	}
	idemKey := strParam(params, "idempotency_key", "")
	if idemKey != "" {
		if hit, found := idempotentCreateHit(ctx, rid, idemKey, profile, copyParentHistory); found {
			return hit
		}
	}
	sid, source := newRuntimeIDs(params)
	key := newSessionKey()
	history := coerceSeedHistory(params["messages"])
	parentSessionID := strParam(params, "parent_session_id", "")
	if copyParentHistory {
		if parentSessionID == "" {
			return rpcErr(rid, 4008, "parent_session_id is required when copying parent history", nil)
		}
		db, release, err := profileDB(ctx, params, false)
		panicIf(err)
		if db == nil {
			release()
			return dbUnavailableError(ctx, rid, 5008)
		}
		_, displayHistory, err := db.GetResumeConversations(ctx, parentSessionID)
		release()
		if err != nil {
			return rpcErr(rid, 4008, "nothing to branch — "+err.Error(), nil)
		}
		display := make([]any, len(displayHistory))
		for i, msg := range displayHistory {
			display[i] = msg
		}
		history = visibleBranchHistory(display)
		if len(history) == 0 {
			return rpcErr(rid, 4008, "send a message first", nil)
		}
	}
	explicitCwd, sessionCwdValue, remoteCwd, err := resolveCreateCwd(ctx, params, source, profileHomeDir)
	panicIf(err)
	enableGatewayPrompts()
	modelOverride, reasoningOverride, serviceTierOverride, err := createOverrides(params)
	if err != nil {
		return rpcErr(rid, 4002, err.Error(), nil)
	}
	var composerOverrideProfile any
	if len(modelOverride) > 0 && flagParam(params, "follow_profile_config") {
		profileModel, profileProvider := configModelTarget(profileBuildScope(ctx, profileHomeDir))
		composerOverrideProfile = map[string]any{"model": profileModel, "provider": profileProvider}
	}
	cols := 80
	if raw, present := params["cols"]; present {
		n, err := pyint.Of(raw)
		panicIf(err)
		if !n.IsInt64() {
			panic(&pyint.Error{Kind: "OverflowError", Msg: "Python int too large to convert to C ssize_t"})
		}
		cols = int(n.Int64())
	}
	var pendingTitle, profileValue, authUserID, overrideValue, reasoningValue, tierValue any
	if title := strParam(params, "title", ""); title != "" {
		pendingTitle = title
	}
	if profileHomeDir != "" {
		profileValue = profileHomeDir
	}
	if owner := tuigateway.TransportAuthUserID(tuigateway.CurrentTransport(ctx)); owner != nil {
		authUserID = *owner
	}
	if len(modelOverride) > 0 {
		overrideValue = modelOverride
	}
	if reasoningOverride != nil {
		reasoningValue = reasoningOverride
	}
	if serviceTierOverride != nil {
		tierValue = *serviceTierOverride
	}
	var parentValue any
	if parentSessionID != "" {
		parentValue = parentSessionID
	}
	now := nowSeconds()
	record := tuigateway.NewSession(map[string]any{
		"agent": nil, "agent_error": nil, "agent_ready": &pythread.Event{}, "attached_images": []any{},
		"close_on_disconnect": flagParam(params, "close_on_disconnect"), "active_session_lease": nil,
		"cols": cols, "created_at": now, "edit_snapshots": map[string]any{}, "explicit_cwd": explicitCwd,
		"history": history, "history_lock": &sync.Mutex{}, "history_version": int64(0), "image_counter": 0,
		"seeded": len(history) > 0, "cwd": sessionCwdValue, "inflight_turn": nil, "last_active": now,
		"model_override": overrideValue, "composer_override_profile": composerOverrideProfile,
		"create_reasoning_override": reasoningValue, "create_service_tier_override": tierValue,
		"parent_session_id": parentValue, "pending_title": pendingTitle,
		"pending_hidden": flagParam(params, "hidden"), "room_plumbing": flagParam(params, "room_plumbing"),
		"follow_profile_config": flagParam(params, "follow_profile_config"), "profile_home": profileValue,
		"running": false, "session_key": key, "show_reasoning": loadShowReasoning(ctx), "source": source,
		"slash_worker": nil, "tool_progress_mode": loadToolProgressMode(ctx), "tool_started_at": map[string]any{},
		"transport": callerTransport(ctx), "auth_user_id": authUserID})
	func() {
		sessionsLock.Lock()
		defer sessionsLock.Unlock()
		tuigateway.SetSessionRecord(sid, record)
		registerSessionCwd(ctx, record)
		if idemKey != "" {
			idempotencyKeys[idemKey] = idempotencyEntry{sid: sid, at: now}
		}
	}()
	if len(modelOverride) > 0 {
		providerName := strOr(modelOverride["provider"])
		if !truthy(modelOverride["provider"]) {
			providerName = "-"
		}
		defaultModel, _ := sessionDefaultRoute(ctx, record)
		logger().Info(fmt.Sprintf("session.create %s: model=%s provider=%s source=client override (profile default: %s)",
			key, strOr(modelOverride["model"]), providerName, defaultModel))
	}
	switch {
	case parentSessionID != "" && len(history) > 0:
		seedBranchRow(ctx, record, key, parentSessionID, history, source, profileHomeDir)
	case len(history) > 0:
		seedRow(ctx, record)
	case explicitCwd && remoteCwd:
		ensureSessionDBRow(ctx, record)
	}
	scheduleAgentBuild(ctx, sid, 0.05)
	scheduleSessionCapEnforcement(ctx)
	cwd := strOr(record.Get("cwd"))
	override := modelOverride
	if override == nil {
		override = map[string]any{}
	}
	messages := wireMessages(ctx, history, profileHomeDir)
	result := map[string]any{"session_id": sid, "stored_session_id": key, "message_count": len(messages),
		"info": createdSessionInfo(ctx, record, override, cwd, profile)}
	if copyParentHistory {
		result["messages_omitted"] = true
	} else {
		result["messages"] = messages
	}
	return ok(rid, result)
}
