package tuiserver

import (
	"context"
	"errors"
	"fmt"

	"github.com/nickheyer/icebath/pkg/agent"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pyint"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/internal/pythread"
	"github.com/nickheyer/icebath/pkg/kanban"
	"github.com/nickheyer/icebath/pkg/state"
	"github.com/nickheyer/icebath/pkg/tools"
	"github.com/nickheyer/icebath/pkg/tuigateway"
)

// Declares session.resume on the session module registry
func registerSessionResumeHandlers() {
	sessionRegistry.method("session.resume", sessionResume)
}

// Per call session.resume state, ownsDB marking a dedicated profile handle that is still ours to close
type resumeCtx struct {
	ctx                                                  context.Context
	rid                                                  any
	params                                               map[string]any
	target                                               string
	db                                                   *state.DB
	ownsDB                                               bool
	found                                                *pyjson.Object
	profileResumeCwd                                     string
	cols                                                 int
	profile                                              string
	profileHome                                          string
	lazy, deferHistory, omitMessages, eagerBuild, inline bool
}

// Resume state of a request, raising when the named profile is unavailable
func newResumeCtx(ctx context.Context, rid any, params map[string]any, target string) *resumeCtx {
	profile := pystr.Strip(strOr(params["profile"]))
	profileHomeDir, err := profileHome(profile)
	panicIf(err)
	_, hasInline := params["inline_images"]
	return &resumeCtx{ctx: ctx, rid: rid, params: params, target: target, cols: intParam(params, "cols", 80), profile: profile,
		profileHome: profileHomeDir, lazy: flagParam(params, "lazy"), deferHistory: flagParam(params, "defer_history"),
		omitMessages: flagParam(params, "omit_messages"), eagerBuild: flagParam(params, "eager_build"),
		inline: !hasInline || flagParam(params, "inline_images")}
}

// Whether the located row holds any column, the empty lazy watch row reading false
func (r *resumeCtx) foundAny() bool { return r.found != nil && r.found.Len() > 0 }

// Profile home as the child run registry keys it, nil for the launch profile
func (r *resumeCtx) homeKey() any {
	if r.profileHome == "" {
		return nil
	}
	return r.profileHome
}

// Runtime id, source and cwd of the record this resume registers, gateway prompts enabled unless asked not to
func (r *resumeCtx) mint(prompts bool) (string, string, string) {
	sid, source := newRuntimeIDs(r.params)
	if prompts {
		enableGatewayPrompts()
	}
	cwd := r.profileResumeCwd
	if cwd == "" {
		cwd = defaultSessionCwd(r.ctx)
	}
	return sid, source, cwd
}

// Deferred record with this resume's common fields, stored overrides restoring the persisted route
func (r *resumeCtx) record(source, cwd string, history []any, overrides map[string]any, hasOverrides bool, o deferredRecordOptions) *tuigateway.Session {
	modelConfig, followsProfile := map[string]any{}, false
	if hasOverrides {
		o.modelOverride = overrides["model_override"]
		o.resumeRuntimeOverrides = overrides
		modelConfig, _ = parseModelConfig(r.foundValue("model_config"), true)
		followsProfile = r.found != nil && rowFollowsProfile(r.found)
	}
	o.cols, o.cwd, o.history, o.source = r.cols, cwd, history, source
	o.closeOnDisconnect, o.profileHome, o.explicitCwd = flagParam(r.params, "close_on_disconnect"), r.profileHome, r.profileResumeCwd != ""
	record := deferredSessionRecord(r.ctx, r.target, o)
	if followsProfile {
		record.Set("follow_profile_config", true)
		var composer any
		if len(overrides) > 0 && overrides["model_override"] != nil {
			composer = modelConfig["composer_override_profile"]
		}
		record.Set("composer_override_profile", composer)
	}
	return record
}

// Column of the located row, nil when absent
func (r *resumeCtx) foundValue(key string) any {
	if r.found == nil {
		return nil
	}
	return r.found.GetOr(key, nil)
}

// Registers record live or reuses a concurrent winner, repairing a named profile's row like the eager build
func (r *resumeCtx) claim(sid string, record *tuigateway.Session) map[string]any {
	liveSID, live, found := claimOrReuseLive(r.ctx, sid, r.target, record, nil)
	if !found {
		if r.profileHome != "" && !r.lazy {
			hydrateSessionCwd(r.ctx, sid, r.target, r.db, r.profileHome)
		}
		return nil
	}
	return resumeReuseLive(r, liveSID, live)
}

// Sanitized model history, display history and raw history of a cold or eager resume
func (r *resumeCtx) restore() ([]any, []any, []any, error) {
	raw, display, err := r.readHistory()
	if err != nil {
		return nil, nil, nil, err
	}
	return anyRows(agent.CanonicalizeReplayHistory(raw, nil)), anyRows(display), anyRows(raw), nil
}

// Lazy session info of the resumed route
func (r *resumeCtx) info(cwd string, overrides map[string]any) map[string]any {
	override, _ := overrides["model_override"].(map[string]any)
	model := ""
	if truthy(override["model"]) {
		model = strOr(override["model"])
	}
	providerName := ""
	if truthy(overrides["provider_override"]) {
		providerName = strOr(overrides["provider_override"])
	}
	return lazyResumeInfo(r.ctx, cwd, model, providerName, r.profile)
}

// The child's own conversation with row ids, no ancestors
func (r *resumeCtx) childHistory(repair bool) ([]map[string]any, error) {
	return r.db.GetMessagesAsConversation(r.ctx, r.target, state.ConversationOptions{RepairAlternation: repair, IncludeRowIDs: true})
}

// Wire messages of a display history, none when the client hydrates over REST
func (r *resumeCtx) messages(display []any) []any {
	if r.omitMessages {
		return []any{}
	}
	messages, err := tuigateway.HistoryToMessages(r.ctx, display, r.profileHome, r.inline)
	panicIf(err)
	return messages
}

// Model and display projections of one lineage read, undrained queue rows retired first
func (r *resumeCtx) readHistory() ([]map[string]any, []map[string]any, error) {
	if _, err := r.db.RetireUndrainedQueueRows(r.ctx, r.target); err != nil {
		return nil, nil, err
	}
	if r.omitMessages {
		raw, err := r.childHistory(true)
		return raw, []map[string]any{}, err
	}
	return r.db.GetResumeConversations(r.ctx, r.target)
}

// Ancestor display rows, none when messages are omitted
func (r *resumeCtx) displayPrefix() []any {
	if r.omitMessages {
		return []any{}
	}
	prefix, err := r.db.GetAncestorDisplayPrefix(r.ctx, r.target)
	panicIf(err)
	return anyRows(prefix)
}

// Untyped value of a task plan snapshot, nil when there is none
func planSnapshotValue(o *pyjson.Object) any {
	if o == nil {
		return nil
	}
	return o
}

// Runtime id of a live unpersisted session of a home matched by stored key or pending title
func findLiveUnpersisted(needle, profileHomeDir string) string {
	for _, live := range tuigateway.LiveSessions() {
		record := live.Session
		if strOr(record.Get("profile_home")) != profileHomeDir {
			continue
		}
		if strOr(record.Get("session_key")) == needle || strOr(record.Get("pending_title")) == needle {
			return live.SID
		}
	}
	return ""
}

// Reattaches a live lazy session that has no stored row yet
func resumeLiveUnpersisted(r *resumeCtx, liveSID string, live *tuigateway.Session) map[string]any {
	if r.ownsDB {
		releaseDB(r.db)
	}
	sessionResumeLock.Lock()
	if refusal := reattachRefusal(r.rid, liveSID, live); refusal != nil {
		sessionResumeLock.Unlock()
		return refusal
	}
	live.Set("last_active", nowSeconds())
	if transport := tuigateway.CurrentTransport(r.ctx); transport != nil {
		unlock := lockHistory(live)
		rebindLiveTransport(r.ctx, liveSID, live, transport)
		unlock()
	} else {
		cancelWSOrphanReap(liveSID)
	}
	sessionResumeLock.Unlock()
	messages := r.messages(historyList(live.Get("history")))
	model, providerName := liveSessionIdentity(r.ctx, live)
	profileName := home.ProfileNameFor(strOr(live.Get("profile_home")))
	if profileName == "" {
		profileName = responseProfileName(r.ctx, r.profile)
	}
	return ok(r.rid, attachTodoState(map[string]any{"session_id": liveSID, "stored_session_id": strOr(live.Get("session_key")),
		"message_count": len(messages), "messages": messages,
		"info": map[string]any{"model": model, "provider": providerName, "lazy": true, "desktop_contract": desktopBackendContract,
			"profile_name": profileName}}, live))
}

// Adopts a lineage stranded in the default store into this profile's store, exact id only
func resumeAdoptStranded(r *resumeCtx) {
	if failure := catchPanic(func() {
		defaultDB := getDB(r.ctx)
		if defaultDB == nil {
			return
		}
		donor, err := defaultDB.GetSession(r.ctx, r.target)
		panicIf(err)
		if donor == nil || donor.Len() == 0 || truthy(donor.GetOr("archived", nil)) {
			return
		}
		donorID := strOr(donor.GetOr("id", nil))
		adoption, err := r.db.AdoptSessionLineageFrom(r.ctx, defaultDB, donorID, true)
		panicIf(err)
		if !truthy(adoption.GetOr("adopted", nil)) {
			return
		}
		segments := len(historyList(adoption.GetOr("imported_ids", nil))) + len(historyList(adoption.GetOr("skipped_ids", nil)))
		profile := r.profile
		if profile == "" {
			profile = "?"
		}
		logger().Info(fmt.Sprintf("adopted stranded session %s (lineage of %d segment(s)) from default store into profile %s", donorID, segments, profile))
		r.found, err = r.db.GetSession(r.ctx, donorID)
		panicIf(err)
		if r.foundAny() {
			r.target = strOr(r.found.GetOr("id", nil))
		}
	}); failure != nil {
		logger().Error(fmt.Sprintf("stranded-session adoption failed for %s", r.target), "exc_info", failureText(failure))
	}
}

// Materializes the row of a minted but never persisted key so its resume proceeds with empty history
func resumeMaterializeMinted(r *resumeCtx) {
	if failure := catchPanic(func() {
		profileName := home.ProfileNameFor(r.profileHome)
		if profileName == "" {
			profileName = responseProfileName(r.ctx, r.profile)
		}
		panicIf(r.db.CreateSession(state.NewSession{ID: r.target, Source: resolveSessionSource(strParam(r.params, "source", "")),
			Model: resolveModel(r.ctx), ProfileName: profileName}))
		found, err := r.db.GetSession(r.ctx, r.target)
		panicIf(err)
		r.found = found
		logger().Info(fmt.Sprintf("materialized session row for minted-but-unpersisted key %s (resume no longer 4007s)", r.target))
	}); failure != nil {
		logger().Warn(fmt.Sprintf("failed to materialize session row for %s", r.target), "exc_info", failureText(failure))
	}
}

// Resolves the target to a stored row, a non nil answer ending the resume early
func resumeLocate(r *resumeCtx) map[string]any {
	found, err := r.db.GetSession(r.ctx, r.target)
	panicIf(err)
	r.found = found
	if r.foundAny() {
		return nil
	}
	found, err = r.db.GetSessionByTitle(r.ctx, r.target)
	panicIf(err)
	r.found = found
	if r.foundAny() {
		r.target = strOr(r.found.GetOr("id", nil))
		return nil
	}
	if r.lazy && childRunActive(r.target, r.homeKey()) {
		r.found = pyjson.NewObject()
		return nil
	}
	if liveSID := findLiveUnpersisted(r.target, r.profileHome); liveSID != "" {
		if live := tuigateway.SessionRecord(liveSID); live != nil {
			return resumeLiveUnpersisted(r, liveSID, live)
		}
	}
	if r.ownsDB {
		resumeAdoptStranded(r)
	}
	if !r.foundAny() && !r.lazy && isServerMintedKey(r.target) && !anyLiveSessionClaimsKey(r.target) {
		resumeMaterializeMinted(r)
	}
	if r.foundAny() {
		return nil
	}
	return rpcErr(r.rid, 4007, "session not found", nil)
}

// Rebinds a rotated out parent id to its compression tip, Bot Chat following proven compression edges only
func resumeFollowTip(r *resumeCtx) {
	if !r.foundAny() || r.lazy {
		return
	}
	tip := r.target
	suppress(func() {
		if pystr.Strip(strOr(r.foundValue("title"))) == state.CanonicalBotChatTitle {
			resolved, err := r.db.GetCompressionTip(r.ctx, r.target)
			panicIf(err)
			if resolved == "" {
				resolved = r.target
			}
			tip = resolved
		} else {
			resolved, err := r.db.ResolveResumeSessionID(r.target)
			panicIf(err)
			tip = resolved
		}
	})
	if tip != "" && tip != r.target {
		r.target = tip
		row, err := r.db.GetSession(r.ctx, tip)
		panicIf(err)
		if row != nil && row.Len() > 0 {
			r.found = row
		}
	}
}

// Refuses a kanban worker transcript and a runaway transcript before any history read
func resumeGuard(r *resumeCtx) map[string]any {
	if refusal := kanban.KanbanResumeRefusal(r.ctx, r.db, r.target); refusal != "" {
		return rpcErr(r.rid, 4132, refusal, nil)
	}
	tipOnly := r.lazy || r.omitMessages || (r.deferHistory && !r.eagerBuild)
	if _, err := r.db.AssertResumeSafe(r.ctx, r.target, nil, tipOnly); err != nil {
		var tooLarge *state.SessionResumeTooLargeError
		if errors.As(err, &tooLarge) {
			return rpcErr(r.rid, 4130, err.Error(), nil)
		}
		logger().Warn(fmt.Sprintf("resume safety check failed for %s (proceeding without guard): %s", r.target, err))
	}
	return nil
}

// Reattaches an already live session under the resume lock
func resumeReuseLive(r *resumeCtx, sid string, session *tuigateway.Session) map[string]any {
	sessionResumeLock.Lock()
	defer sessionResumeLock.Unlock()
	return resumeReuseLiveLocked(r, sid, session)
}

// Reattach with the resume lock already held
func resumeReuseLiveLocked(r *resumeCtx, sid string, session *tuigateway.Session) map[string]any {
	if refusal := reattachRefusal(r.rid, sid, session); refusal != nil {
		return refusal
	}
	cancelWSOrphanReap(sid)
	cols := r.cols
	payload := liveSessionPayload(r.ctx, sid, session, livePayloadOptions{cols: &cols, touch: true, omitMessages: r.omitMessages,
		transport: callerTransport(r.ctx), inlineImages: r.inline})
	payload["resumed"] = r.target
	if r.deferHistory {
		payload["messages"] = []any{}
		payload["hydrating"] = truthy(session.Get("resume_hydrating"))
		count := payload["message_count"]
		if value := session.Get("resume_message_count"); truthy(value) {
			count = value
		}
		n, err := pyint.Of(count)
		panicIf(err)
		payload["message_count"] = int(n.Int64())
	}
	if session.Get("agent") == nil && childRunActive(r.target, r.homeKey()) {
		payload["running"] = true
		payload["status"] = "streaming"
	}
	return ok(r.rid, payload)
}

// Fields of a common resume payload
type resumeResponseOptions struct {
	info         map[string]any
	display      []any
	countSource  []any
	messages     []any
	messageCount *int
	running      bool
	status       string
	hydrating    *bool
	startedAt    any
	autoContinue map[string]any
}

// Common resume payload, an omitted transcript still reporting the stored size
func resumeResponse(r *resumeCtx, sid string, record *tuigateway.Session, o resumeResponseOptions) map[string]any {
	messages := o.messages
	if messages == nil {
		messages = r.messages(o.display)
	}
	count := len(messages)
	if o.messageCount != nil {
		count = *o.messageCount
	} else if r.omitMessages {
		count = len(o.countSource)
	}
	status := o.status
	if status == "" {
		status = "idle"
	}
	startedAt := o.startedAt
	if startedAt == nil {
		startedAt = record.Get("created_at")
	}
	payload := map[string]any{"session_id": sid, "resumed": r.target, "message_count": count, "messages": messages, "info": o.info,
		"inflight": nil, "running": o.running, "session_key": r.target, "started_at": startedAt, "status": status}
	if o.hydrating == nil {
		payload["messages_omitted"] = r.omitMessages
	} else {
		payload["hydrating"] = *o.hydrating
	}
	if o.autoContinue != nil {
		payload["auto_continue"] = o.autoContinue
	}
	return ok(r.rid, attachTodoState(payload, record))
}

// Watch resume of a subagent window, a live session with stored history and no agent
func resumeLazy(r *resumeCtx) map[string]any {
	sid, source, cwd := r.mint(false)
	var history []map[string]any
	if failure := catchPanic(func() {
		_, err := r.db.RetireUndrainedQueueRows(r.ctx, r.target)
		panicIf(err)
		history, err = r.childHistory(true)
		panicIf(err)
	}); failure != nil {
		return rpcErr(r.rid, 5000, resumeFailedMessage(failure), nil)
	}
	record := r.record(source, cwd, anyRows(history), nil, false, deferredRecordOptions{lazy: true,
		todoState: planSnapshotValue(tuigateway.TodoStateFromHistory(history))})
	if reused := r.claim(sid, record); reused != nil {
		return reused
	}
	running := childRunActive(r.target, r.homeKey())
	display := anyRows(history)
	if verbatim, err := r.childHistory(false); err != nil {
		logger().Debug("child-watch display projection read failed", "exc_info", err.Error())
	} else {
		display = anyRows(verbatim)
	}
	status := "idle"
	if running {
		status = "streaming"
	}
	return resumeResponse(r, sid, record, resumeResponseOptions{info: lazyResumeInfo(r.ctx, cwd, "", "", r.profile), display: display,
		countSource: display, running: running, status: status})
}

// Bounded ack whose transcript hydrates in the background and pages over REST
func resumeDeferred(r *resumeCtx) map[string]any {
	sid, source, cwd := r.mint(true)
	overrides := storedSessionRuntimeOverrides(profileBuildScope(r.ctx, r.profileHome), r.found)
	record := r.record(source, cwd, []any{}, overrides, true, deferredRecordOptions{todoState: planSnapshotValue(todoStateFromDB(r.ctx, r.db, r.target))})
	count := 0
	if value := r.foundValue("message_count"); truthy(value) {
		n, err := pyint.Of(value)
		panicIf(err)
		count = int(n.Int64())
	}
	record.Set("resume_history_ready", &pythread.Event{})
	record.Set("resume_hydrating", true)
	record.Set("resume_message_count", count)
	if reused := r.claim(sid, record); reused != nil {
		return reused
	}
	scheduleResumeHydration(r.ctx, sid, r.target, r.db, r.ownsDB, source == "desktop" && r.omitMessages)
	r.ownsDB = false
	scheduleSessionCapEnforcement(r.ctx)
	hydrating := true
	return resumeResponse(r, sid, record, resumeResponseOptions{info: r.info(cwd, overrides), messages: []any{}, messageCount: &count,
		status: "resuming", hydrating: &hydrating})
}

// Cold resume answering with the transcript now and pre warming the agent off the response path
func resumeCold(r *resumeCtx) map[string]any {
	sid, source, cwd := r.mint(true)
	history, display, raw, err := r.restore()
	if err != nil {
		return rpcErr(r.rid, 5000, resumeFailedMessage(err), nil)
	}
	overrides := storedSessionRuntimeOverrides(profileBuildScope(r.ctx, r.profileHome), r.found)
	record := r.record(source, cwd, history, overrides, true, deferredRecordOptions{displayHistoryPrefix: r.displayPrefix(),
		todoState: planSnapshotValue(tuigateway.TodoStateFromHistory(historyDicts(history)))})
	if reused := r.claim(sid, record); reused != nil {
		return reused
	}
	scheduleAgentBuild(r.ctx, sid, 0.05)
	scheduleSessionCapEnforcement(r.ctx)
	return resumeResponse(r, sid, record, resumeResponseOptions{info: r.info(cwd, overrides), display: display, countSource: raw,
		autoContinue: maybeScheduleAutoContinue(r.ctx, sid, record, r.target)})
}

// Closes a built agent of any shape, swallowing failures
func closeBuiltAgent(built any) {
	suppress(func() {
		switch a := built.(type) {
		case interface{ Close() error }:
			_ = a.Close()
		case interface{ Close() }:
			a.Close()
		}
	})
}

// Synchronous build outside the resume lock, double checked against a concurrent live session
func resumeEager(r *resumeCtx) map[string]any {
	sid, source, _ := r.mint(true)
	scoped := profileBuildScope(r.ctx, r.profileHome)
	var history, display, raw, prefix []any
	var stored map[string]any
	var built any
	if failure := catchPanic(func() {
		var err error
		history, display, raw, err = r.restore()
		panicIf(err)
		prefix = r.displayPrefix()
		stored = storedSessionRuntimeOverrides(scoped, r.found)
		launch := launchCwdNotAWorkspace[source] && r.profileResumeCwd == ""
		options := makeAgentOptions{sessionDB: r.db, platformOverride: source, cwdOverride: r.profileResumeCwd,
			contextCwdIsLaunchArtifact: &launch, authUserID: tuigateway.TransportAuthUserID(tuigateway.CurrentTransport(r.ctx))}
		options.applyOverrides(stored)
		built, err = makeAgentInContext(scoped, sid, r.target, options)
		panicIf(err)
	}); failure != nil {
		return rpcErr(r.rid, 5000, resumeFailedMessage(failure), nil)
	}
	sessionResumeLock.Lock()
	if liveSID, live, found := findLiveSessionByKey(r.target, r.profileHome, false); found {
		closeBuiltAgent(built)
		defer sessionResumeLock.Unlock()
		return resumeReuseLiveLocked(r, liveSID, live)
	}
	failure := catchPanic(func() {
		initSession(scoped, sid, r.target, built, history, r.cols, r.profileResumeCwd, r.db, source, r.profileHome, r.profileResumeCwd != "")
		if r.ownsDB {
			a, _ := built.(*agent.AIAgent)
			transferDBToAgent(scoped, a, r.db)
		}
		r.ownsDB = false
		session := tuigateway.SessionRecord(sid)
		if session == nil {
			return
		}
		if stored["model_override"] != nil {
			session.Set("model_override", stored["model_override"])
		}
		modelConfig, _ := parseModelConfig(r.foundValue("model_config"), true)
		if r.found != nil && rowFollowsProfile(r.found) {
			session.Set("follow_profile_config", true)
			var composer any
			if truthy(stored["model_override"]) {
				composer = modelConfig["composer_override_profile"]
			}
			session.Set("composer_override_profile", composer)
		}
		if r.profileHome != "" {
			session.Set("profile_home", r.profileHome)
		}
		session.Set("display_history_prefix", prefix)
		session.Set("active_session_lease", nil)
	})
	if failure != nil {
		if r.ownsDB {
			sessionsLock.Lock()
			tuigateway.SetSessionRecord(sid, nil)
			sessionsLock.Unlock()
		}
		sessionResumeLock.Unlock()
		return rpcErr(r.rid, 5000, resumeFailedMessage(failure), nil)
	}
	session := tuigateway.SessionRecord(sid)
	sessionResumeLock.Unlock()
	if session == nil {
		session = tuigateway.NewSession(nil)
	}
	startedAt := nowSeconds()
	if value := session.Get("created_at"); truthy(value) {
		stamp, err := pyint.Float(value)
		panicIf(err)
		startedAt = stamp
	}
	var autoContinue map[string]any
	if tuigateway.SessionRecord(sid) == session {
		autoContinue = maybeScheduleAutoContinue(r.ctx, sid, session, r.target)
	}
	a, _ := built.(*agent.AIAgent)
	return resumeResponse(r, sid, session, resumeResponseOptions{info: sessionInfo(r.ctx, a, session), display: display, countSource: raw,
		startedAt: startedAt, autoContinue: autoContinue})
}

// session.resume, reattaching a live session of this profile or restoring a stored one lazily, deferred, cold or eager
func sessionResume(ctx context.Context, rid any, params map[string]any) map[string]any {
	target := strOr(params["session_id"])
	if !truthy(params["session_id"]) {
		return rpcErr(rid, 4006, "session_id required", nil)
	}
	r := newResumeCtx(ctx, rid, params, target)
	db, owns, err := profileSessionDB(ctx, r.profileHome)
	panicIf(err)
	r.db, r.ownsDB = db, owns
	defer func() {
		if r.ownsDB && r.db != nil {
			suppress(func() { _ = r.db.Close() })
		}
	}()
	if r.db == nil {
		return dbUnavailableError(ctx, rid, 5000)
	}
	if resp := resumeLocate(r); resp != nil {
		return resp
	}
	resumeFollowTip(r)
	if resp := resumeGuard(r); resp != nil {
		return resp
	}
	r.profileResumeCwd = resumableStoredCwd(ctx, strParam(objectMap(r.found), "cwd", ""), r.profileHome)
	if r.profileResumeCwd == "" {
		r.profileResumeCwd = profileDefaultCwd(ctx, r.profileHome)
	}
	sessionResumeLock.Lock()
	liveSID, live, found := findLiveSessionByKey(r.target, r.profileHome, false)
	sessionResumeLock.Unlock()
	if found {
		return resumeReuseLive(r, liveSID, live)
	}
	tools.RestoreSessionYolo(ctx, r.target, state.SessionYoloEnabled(objectMap(r.found)))
	switch {
	case r.lazy:
		return resumeLazy(r)
	case r.eagerBuild:
		return resumeEager(r)
	case r.deferHistory:
		return resumeDeferred(r)
	}
	return resumeCold(r)
}
