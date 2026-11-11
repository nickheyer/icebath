package tuiserver

import (
	"context"
	"fmt"
	"regexp"
	"sync"

	"github.com/nickheyer/icebath/pkg/activesessions"
	"github.com/nickheyer/icebath/pkg/internal/pythread"
	"github.com/nickheyer/icebath/pkg/state"
	"github.com/nickheyer/icebath/pkg/tools"
	"github.com/nickheyer/icebath/pkg/tuigateway"
)

// Server minted session key shape, a timestamp plus six hex characters
var mintedSessionKeyRE = regexp.MustCompile(`^\d{8}_\d{6}_[0-9a-f]{6}$`)

// Whether a key has the shape only the server itself mints
func isServerMintedKey(value string) bool {
	return value != "" && mintedSessionKeyRE.MatchString(value)
}

// Whether any live record of any profile claims a stored key
func anyLiveSessionClaimsKey(target string) bool {
	for _, live := range tuigateway.LiveSessions() {
		if strOr(live.Session.Get("session_key")) == target {
			return true
		}
	}
	return false
}

// Session info of a not yet built session, the profile default route when no model is given
func lazyResumeInfo(ctx context.Context, cwd, model, providerName, profile string) map[string]any {
	profileHomeDir, err := profileHome(profile)
	panicIf(err)
	probe := tuigateway.NewSession(map[string]any{"profile_home": profileHomeDir})
	if model == "" {
		var defaultProvider string
		model, defaultProvider = sessionDefaultRoute(ctx, probe)
		if providerName == "" {
			providerName = defaultProvider
		}
	}
	override := map[string]any{}
	if model != "" {
		override = map[string]any{"model": model, "provider": providerName}
	}
	info := map[string]any{"cwd": cwd, "branch": tuigateway.Branch(cwd), "project": projectInfoForCwd(ctx, cwd)}
	for key, value := range lazyInfoRoute(ctx, probe, override) {
		info[key] = value
	}
	info["tools"] = map[string]any{}
	info["skills"] = map[string]any{}
	info["lazy"] = true
	info["desktop_contract"] = desktopBackendContract
	info["profile_name"] = responseProfileName(ctx, profile)
	return info
}

// Fields of a deferred live record beyond the common ones
type deferredRecordOptions struct {
	cols                   int
	cwd                    string
	history                []any
	lease                  *activesessions.Lease
	source                 string
	closeOnDisconnect      bool
	displayHistoryPrefix   []any
	profileHome            string
	lazy                   bool
	modelOverride          any
	resumeRuntimeOverrides map[string]any
	todoState              any
	explicitCwd            bool
}

// Live record whose agent is built later, the shape of an initialized session minus the agent
func deferredSessionRecord(ctx context.Context, sessionKey string, o deferredRecordOptions) *tuigateway.Session {
	now := nowSeconds()
	prefix := o.displayHistoryPrefix
	if prefix == nil {
		prefix = []any{}
	}
	var lease, profile, overrides, authUserID any
	if o.lease != nil {
		lease = o.lease
	}
	if o.profileHome != "" {
		profile = o.profileHome
	}
	if len(o.resumeRuntimeOverrides) > 0 {
		overrides = o.resumeRuntimeOverrides
	}
	if owner := tuigateway.TransportAuthUserID(tuigateway.CurrentTransport(ctx)); owner != nil {
		authUserID = *owner
	}
	source := o.source
	if source == "" {
		source = "tui"
	}
	return tuigateway.NewSession(map[string]any{
		"agent": nil, "agent_error": nil, "agent_ready": &pythread.Event{}, "attached_images": []any{},
		"close_on_disconnect": o.closeOnDisconnect, "active_session_lease": lease, "cols": o.cols,
		"created_at": now, "cwd": o.cwd, "display_history_prefix": prefix,
		"edit_snapshots": map[string]any{}, "explicit_cwd": o.explicitCwd, "history": o.history,
		"history_lock": &sync.Mutex{}, "history_version": int64(0), "image_counter": 0,
		"inflight_turn": nil, "last_active": now, "lazy": o.lazy, "model_override": o.modelOverride,
		"pending_title": nil, "profile_home": profile,
		"resume_runtime_overrides": overrides, "resume_session_id": sessionKey,
		"running": false, "session_key": sessionKey, "show_reasoning": loadShowReasoning(ctx),
		"slash_worker": nil, "source": source, "tool_progress_mode": loadToolProgressMode(ctx),
		"tool_started_at": map[string]any{}, "todo_state": o.todoState,
		"transport": callerTransport(ctx), "auth_user_id": authUserID})
}

// Registers record as the live session of its key, else releases lease and returns the concurrent winner
func claimOrReuseLive(ctx context.Context, sid, sessionKey string, record *tuigateway.Session, lease *activesessions.Lease) (string, *tuigateway.Session, bool) {
	profileHomeDir := strOr(record.Get("profile_home"))
	sessionResumeLock.Lock()
	if liveSID, live, found := findLiveSessionByKey(sessionKey, profileHomeDir, false); found {
		sessionResumeLock.Unlock()
		if lease != nil {
			panicIf(lease.Release())
		}
		return liveSID, live, true
	}
	func() {
		sessionsLock.Lock()
		defer sessionsLock.Unlock()
		tuigateway.SetSessionRecord(sid, record)
		registerSessionCwd(ctx, record)
	}()
	cancelWSOrphanReap(sid)
	stale := claimParkedRuntimes(ctx, sessionKey, sid, profileHomeDir, false)
	sessionResumeLock.Unlock()
	finalizeSupersededRuntimes(ctx, stale)
	return "", nil, false
}

// Sentinel parked stale runtimes of a key, their orphan reap cancelled and popped for supersession
func claimParkedRuntimes(ctx context.Context, sessionKey, keepSID, profileHomeDir string, anyProfile bool) []tuigateway.LiveSession {
	var candidates []tuigateway.LiveSession
	func() {
		sessionsLock.Lock()
		defer sessionsLock.Unlock()
		detached := tuigateway.DetachedWSTransport()
		for _, live := range tuigateway.LiveSessions() {
			old := live.Session
			transport, _ := old.Get("transport").(tuigateway.Transport)
			if live.SID != keepSID && !truthy(old.Get("_finalized")) && sessionLookupKey(old, live.SID) == sessionKey &&
				liveProfileMatches(old, profileHomeDir, anyProfile) && transport != nil && transport == detached {
				candidates = append(candidates, live)
			}
		}
	}()
	var stale []tuigateway.LiveSession
	for _, candidate := range candidates {
		cancelWSOrphanReap(candidate.SID)
		if popped := popSessionByID(ctx, candidate.SID); popped != nil {
			stale = append(stale, tuigateway.LiveSession{SID: candidate.SID, Session: popped})
		}
	}
	return stale
}

// Tears down superseded runtimes quietly, with no reclaim broadcast
func finalizeSupersededRuntimes(ctx context.Context, stale []tuigateway.LiveSession) {
	for _, entry := range stale {
		if failure := catchPanic(func() { teardownPoppedSession(ctx, entry.Session, "superseded_by_resume") }); failure != nil {
			logger().Error(fmt.Sprintf("superseded runtime teardown failed sid=%s", entry.SID), "exc_info", failureText(failure))
		}
	}
}

// Persisted display lineage of a live session reconciled with its memory tail, memory alone when the store fails
func liveVisibleHistory(ctx context.Context, session *tuigateway.Session, db *state.DB, inMemory []any) []any {
	key := strOr(session.Get("session_key"))
	if db != nil && key != "" {
		var reconciled []any
		failure := catchPanic(func() {
			display, err := db.GetMessagesAsConversation(ctx, key, state.ConversationOptions{IncludeAncestors: true, IncludeRowIDs: true, IncludeCompacted: true})
			panicIf(err)
			rows := make([]any, len(display))
			for i, row := range display {
				rows[i] = row
			}
			reconciled = reconcileDisplayWithLive(rows, inMemory)
		})
		if failure == nil {
			return reconciled
		}
		logger().Debug("live display projection read failed", "exc_info", failureText(failure))
	}
	return inMemory
}

// Open connection operation of a session as its connection.request payload, nil when none
func pendingConnectionRequestPayload(sid string) map[string]any {
	session := tuigateway.SessionRecord(sid)
	if session == nil {
		return nil
	}
	operation := tools.LiveCurrent(strOr(session.Get("session_key")), strOr(session.Get("profile_home")))
	if operation == nil {
		return nil
	}
	return objectMap(operation.RequestPayload())
}

// Options of a live session payload
type livePayloadOptions struct {
	cols         *int
	touch        bool
	transport    tuigateway.Transport
	omitMessages bool
	inlineImages bool
}

// Resume shaped payload of a live session with its running turn, queue and open requests
func liveSessionPayload(ctx context.Context, sid string, session *tuigateway.Session, o livePayloadOptions) map[string]any {
	unlock := lockHistory(session)
	if o.cols != nil {
		session.Set("cols", *o.cols)
	}
	if o.transport != nil {
		rebindLiveTransport(ctx, sid, session, o.transport)
	}
	if o.touch {
		session.Set("last_active", nowSeconds())
	}
	inMemory := append(append([]any(nil), historyList(session.Get("display_history_prefix"))...), historyList(session.Get("history"))...)
	inflight, queued := inflightSnapshot(session), queuedPromptSnapshot(session)
	running, startedAt := truthy(session.Get("running")), turnStartedAt(session)
	unlock()
	history := inMemory
	if !o.omitMessages {
		db, release := sessionDB(ctx, session)
		history = liveVisibleHistory(ctx, session, db, inMemory)
		release()
	}
	messages := []any{}
	messageCount := len(history)
	if !o.omitMessages {
		converted, err := tuigateway.HistoryToMessages(ctx, history, strOr(session.Get("profile_home")), o.inlineImages)
		panicIf(err)
		messages = converted
		messageCount = len(messages)
	}
	created := nowSeconds()
	if value := session.Get("created_at"); truthy(value) {
		if stamp, isFloat := pyFloatOf(value); isFloat {
			created = stamp
		}
	}
	payload := map[string]any{"info": fallbackSessionInfo(ctx, session), "message_count": messageCount, "messages": messages,
		"messages_omitted": o.omitMessages, "running": running, "turn_started_at": startedAt, "session_id": sid,
		"session_key": sessionLookupKey(session, sid), "started_at": created, "status": sessionLiveStatus(sid, session)}
	if len(inflight) > 0 {
		payload["inflight"] = inflight
	}
	if len(queued) > 0 {
		payload["queued"] = queued
	}
	if approval := pendingApprovalRequestPayload(ctx, strOr(session.Get("session_key"))); len(approval) > 0 {
		payload["pending_approval"] = approval
	}
	if requests := openRequests(sid); len(requests) > 0 {
		payload["open_requests"] = requests
	}
	if connection := pendingConnectionRequestPayload(sid); len(connection) > 0 {
		payload["pending_connection"] = connection
	}
	return attachTodoState(payload, session)
}
