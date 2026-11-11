package tuiserver

import (
	"context"
	"fmt"
	"strings"

	"github.com/google/uuid"
	"github.com/nickheyer/icebath/pkg/agent"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pyint"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/state"
	"github.com/nickheyer/icebath/pkg/tuigateway"
)

// Declares session.branch and session.branch_whole on the session module registry
func registerSessionBranchHandlers() {
	sessionMethod("session.branch", true, func(ctx context.Context, rid any, params map[string]any, session *tuigateway.Session) map[string]any {
		return branchLive(ctx, rid, params, session, false)
	})
	sessionMethod("session.branch_whole", true, func(ctx context.Context, rid any, params map[string]any, session *tuigateway.Session) map[string]any {
		return branchLive(ctx, rid, params, session, true)
	})
}

// Next title in the parent's lineage, the TUI branch naming
func branchTitle(ctx context.Context, db *state.DB, parentKey string) (string, error) {
	current, _, err := db.GetSessionTitle(ctx, parentKey)
	if err != nil {
		return "", err
	}
	if current == "" {
		current = "branch"
	}
	return db.GetNextTitleInLineage(ctx, current)
}

// Full copies of the user and assistant rows with visible text
func visibleBranchHistory(messages []any) []any {
	out := []any{}
	for _, message := range messages {
		m, isDict := message.(map[string]any)
		if !isDict || (m["role"] != "user" && m["role"] != "assistant") {
			continue
		}
		if pystr.Strip(tuigateway.CoerceMessageText(m["content"], true)) == "" {
			continue
		}
		copied := make(map[string]any, len(m))
		for key, value := range m {
			copied[key] = value
		}
		out = append(out, copied)
	}
	return out
}

// Columns count of a session record, eighty when it holds none
func sessionCols(session *tuigateway.Session) int {
	raw, present := session.Lookup("cols")
	if !present {
		return 80
	}
	n, err := pyIntOf(raw)
	panicIf(err)
	return n
}

// Builds and registers the branched agent in the parent's profile, releasing an untransferred dedicated db
func buildBranchAgent(ctx context.Context, session *tuigateway.Session, newSID, newKey string, history []any, source string) (built any, err error) {
	parentHome := strOr(session.Get("profile_home"))
	parentUserID := tuigateway.SessionAuthUserID(session)
	var branchDB *state.DB
	branchOwnsDB := false
	if parentHome != "" {
		branchDB, branchOwnsDB, err = profileSessionDB(ctx, parentHome)
		if err != nil {
			return nil, err
		}
	}
	defer func() {
		if branchOwnsDB && branchDB != nil {
			releaseDB(branchDB)
		}
	}()
	if failure := catchPanic(func() {
		scoped := profileBuildScope(ctx, parentHome)
		launch := contextCwdIsLaunchArtifact(session)
		built, err = makeAgentInContext(scoped, newSID, newKey, makeAgentOptions{sessionDB: branchDB, platformOverride: source,
			cwdOverride: sessionCwd(scoped, session), contextCwdIsLaunchArtifact: &launch, authUserID: parentUserID})
		if err != nil {
			return
		}
		initSession(scoped, newSID, newKey, built, append([]any(nil), history...), sessionCols(session), sessionCwd(scoped, session),
			branchDB, source, parentHome, truthy(session.Get("explicit_cwd")))
		a, _ := built.(*agent.AIAgent)
		transferDBToAgent(scoped, a, branchDB)
		branchOwnsDB = false
	}); failure != nil {
		return nil, failureError(failure)
	}
	if err != nil {
		return nil, err
	}
	if child := tuigateway.SessionRecord(newSID); child != nil {
		child.Set("active_session_lease", nil)
		var userID any
		if parentUserID != nil {
			userID = *parentUserID
		}
		child.Set("auth_user_id", userID)
		child.Set("parent_session_id", session.Get("session_key"))
	}
	return built, nil
}

// Message fields a branch copies beside role and content
var branchCopyFields = []string{"reasoning", "reasoning_content", "reasoning_details", "codex_reasoning_items", "codex_message_items",
	"display_kind", "display_metadata", "timestamp"}

// Rows a branch copies, the persisted display projection reconciled with live memory, else memory alone
func branchSourceHistory(ctx context.Context, db *state.DB, session *tuigateway.Session, oldKey string) []any {
	unlock := lockHistory(session)
	var inMemory []any
	for _, msg := range append(append([]any(nil), historyList(session.Get("display_history_prefix"))...), historyList(session.Get("history"))...) {
		if m, isDict := msg.(map[string]any); isDict {
			copied := make(map[string]any, len(m))
			for key, value := range m {
				copied[key] = value
			}
			inMemory = append(inMemory, copied)
		}
	}
	unlock()
	var history []any
	if failure := catchPanic(func() {
		_, displayHistory, err := db.GetResumeConversations(ctx, oldKey)
		panicIf(err)
		display := make([]any, len(displayHistory))
		for i, msg := range displayHistory {
			display[i] = msg
		}
		history = visibleBranchHistory(reconcileDisplayWithLive(display, inMemory))
	}); failure != nil {
		logger().Debug("branch display projection read failed", "exc_info", failureText(failure))
	}
	if len(history) > 0 {
		return history
	}
	return visibleBranchHistory(inMemory)
}

// Branches a live session into a new child, a retried idempotency key answering with the same child
func branchLive(ctx context.Context, rid any, params map[string]any, session *tuigateway.Session, omitMessages bool) map[string]any {
	idemKey := strParam(params, "idempotency_key", "")
	if idemKey != "" {
		if hit, found := idempotentBranchHit(ctx, idemKey, omitMessages); found {
			return ok(rid, hit)
		}
	}
	db, release := sessionDB(ctx, session)
	if db == nil {
		release()
		return dbUnavailableError(ctx, rid, 5008)
	}
	oldKey := strOr(session.Get("session_key"))
	history := branchSourceHistory(ctx, db, session, oldKey)
	if len(history) == 0 {
		release()
		return rpcErr(rid, 4008, "nothing to branch — send a message first", nil)
	}
	if count, isInt := pyint.Instance(params["count"]); isInt && count.Sign() > 0 && count.IsInt64() && count.Int64() < int64(len(history)) {
		history = history[:count.Int64()]
	}
	newKey, newSID, source := newSessionKey(), strings.ReplaceAll(uuid.NewString(), "-", "")[:8], sessionSource(session)
	title := ""
	persistFailure := catchPanic(func() {
		if truthy(params["name"]) {
			title = strOr(params["name"])
		} else {
			derived, err := branchTitle(ctx, db, oldKey)
			panicIf(err)
			title = derived
		}
		var cwd *string
		if !isRemoteLaunchCwd(ctx, session) {
			current := sessionCwd(ctx, session)
			cwd = &current
		}
		profileName := home.ProfileNameFor(strOr(session.Get("profile_home")))
		if profileName == "" {
			profileName = currentProfileName(ctx)
		}
		model, _ := sessionDefaultRoute(ctx, session)
		titleSource := "derived"
		if truthy(params["name"]) {
			titleSource = "user"
		}
		panicIf(persistBranch(ctx, db, newKey, oldKey, title, historyRows(history), source, cwd, profileName, model,
			branchCopyFields, false, titleSource, tuigateway.SessionAuthUserID(session)))
	})
	release()
	if persistFailure != nil {
		return rpcErr(rid, 5008, "branch failed: "+failureText(persistFailure), nil)
	}
	built, err := buildBranchAgent(ctx, session, newSID, newKey, history, source)
	if err != nil {
		return rpcErr(rid, 5000, "agent init failed on branch: "+err.Error(), nil)
	}
	if idemKey != "" {
		sessionsLock.Lock()
		idempotencyKeys[idemKey] = idempotencyEntry{sid: newSID, at: nowSeconds()}
		if child := tuigateway.SessionRecord(newSID); child != nil {
			child.Set("branch_title", title)
		}
		sessionsLock.Unlock()
	}
	a, _ := built.(*agent.AIAgent)
	response := map[string]any{"session_id": newSID, "stored_session_id": newKey, "title": title, "parent": oldKey,
		"message_count": len(history), "info": sessionInfo(ctx, a, tuigateway.SessionRecord(newSID))}
	if omitMessages {
		response["messages_omitted"] = true
	} else {
		response["messages"] = wireMessages(ctx, history, strOr(session.Get("profile_home")))
	}
	return ok(rid, response)
}

// Result of a retried branch whose key still maps to a live child within the TTL, false otherwise
func idempotentBranchHit(ctx context.Context, idemKey string, omitMessages bool) (map[string]any, bool) {
	sessionsLock.Lock()
	defer sessionsLock.Unlock()
	now := nowSeconds()
	existing, known := idempotencyKeys[idemKey]
	if !known {
		return nil, false
	}
	child := tuigateway.SessionRecord(existing.sid)
	if child == nil {
		return nil, false
	}
	if now-existing.at > idempotencyKeyTTL {
		delete(idempotencyKeys, idemKey)
		return nil, false
	}
	idempotencyKeys[idemKey] = idempotencyEntry{sid: existing.sid, at: now}
	return branchIdempotentHit(ctx, existing.sid, child, omitMessages), true
}

// Result shape of a fresh branch for an existing child
func branchIdempotentHit(ctx context.Context, existingSID string, session *tuigateway.Session, omitMessages bool) map[string]any {
	history := historyList(session.Get("history"))
	title := session.Get("branch_title")
	if !truthy(title) {
		title = branchTitleFor(ctx, session)
	}
	response := map[string]any{"session_id": existingSID, "stored_session_id": strOr(session.Get("session_key")), "title": title,
		"parent": session.Get("parent_session_id"), "message_count": len(history), "info": fallbackSessionInfo(ctx, session)}
	if omitMessages {
		response["messages_omitted"] = true
	} else {
		response["messages"] = wireMessages(ctx, history, strOr(session.Get("profile_home")))
	}
	return response
}

// Persisted title of a branch child from its stored row, empty when unreadable
func branchTitleFor(ctx context.Context, session *tuigateway.Session) (title string) {
	suppress(func() {
		db, release := sessionDB(ctx, session)
		defer release()
		if db != nil {
			stored, _, err := db.GetSessionTitle(ctx, strOr(session.Get("session_key")))
			panicIf(err)
			title = stored
		}
	})
	return title
}

// Error value of a recovered panic
func failureError(failure any) error {
	if err, isError := failure.(error); isError {
		return err
	}
	return fmt.Errorf("%s", failureText(failure))
}
