package tuiserver

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/nickheyer/icebath/pkg/activesessions"
	"github.com/nickheyer/icebath/pkg/browser"
	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pythread"
	"github.com/nickheyer/icebath/pkg/plugins"
	"github.com/nickheyer/icebath/pkg/state"
	"github.com/nickheyer/icebath/pkg/tools"
	"github.com/nickheyer/icebath/pkg/tuigateway"
)

// End reasons of a backend initiated cleanup, where a Desktop session keeps its durable row open
var automaticSessionEndReasons = map[string]bool{
	"ws_orphan_reap": true, "ws_disconnect": true, "idle_timeout": true, "lru_evict": true, "tui_shutdown": true,
}

// Pending work of a stored session in the gateway's durable ledger, admissions then worker executions
const canonicalPendingSQL = `SELECT status FROM session_admissions WHERE target_session_id=? AND status!='terminal'
    UNION ALL SELECT status FROM worker_executions WHERE session_id=? AND status!='terminal' LIMIT 1`

// Ends the row unless the gateway ledger still holds work for it, true when the row may end
func endUnlessLedgerBusy(ctx context.Context, db *state.DB, sessionID string, sessionKey any, endReason string) bool {
	mayEnd := false
	err := db.ExecuteWrite(ctx, func(conn state.SQLiteConn) error {
		mayEnd = false
		ids := []string{sessionID, strOr(sessionKey)}
		var chatID any
		switch err := conn.QueryRowContext(ctx, "SELECT chat_id FROM sessions WHERE id=?", sessionID).Scan(&chatID); {
		case err == nil:
			if logical := strOr(chatID); strings.HasPrefix(logical, "local-") {
				ids = append(ids, logical)
			}
		case !errors.Is(err, sql.ErrNoRows):
			return err
		}
		owner, err := state.LocalLineageOwner(ctx, conn, sessionID)
		if err != nil {
			return err
		}
		ids = append(ids, owner)
		seen := map[string]bool{}
		for _, sid := range ids {
			if sid == "" || seen[sid] {
				continue
			}
			seen[sid] = true
			var status any
			switch err := conn.QueryRowContext(ctx, canonicalPendingSQL, sid, sid).Scan(&status); {
			case err == nil:
				return nil
			case !errors.Is(err, sql.ErrNoRows):
				return err
			}
		}
		if _, err := db.EndAndBump(ctx, conn, "UPDATE sessions SET ended_at = ?, end_reason = ? WHERE id = ? AND ended_at IS NULL",
			[]any{nowSeconds(), endReason, sessionID}, sessionID, endReason); err != nil {
			return err
		}
		mayEnd = true
		return nil
	})
	if err != nil {
		panic(err)
	}
	if mayEnd {
		return true
	}
	logger().Info(fmt.Sprintf("Preserving session %s during %s: the gateway's ledger still holds its turn", sessionID, endReason))
	return false
}

// Sources that never name a gateway platform, the self owned surfaces included
var nonGatewaySources = map[string]bool{
	"": true, "tui": true, "cli": true, "webui": true, "desktop": true, "cron": true, "kanban": true, "subagent": true,
	"test": true, "local": true, "acp": true, "webhook": true, "api_server": true, "msgraph_webhook": true,
}

// Whether a session source resolves to a gateway platform, plugin platforms included
func isGatewayOwnedSource(ctx context.Context, source string) bool {
	src := strings.ToLower(strings.TrimSpace(source))
	if nonGatewaySources[src] {
		return false
	}
	owned := false
	suppress(func() {
		_, err := gateway.ParsePlatform(ctx, src)
		owned = err == nil
	})
	return owned
}

// Drops the vault tokens this session unlocked, sibling sessions of the profile keeping theirs
func lockVaultManagers(session *tuigateway.Session) {
	defer func() {
		if failure := recover(); failure != nil {
			logger().Debug("vault manager lock on session end failed", "exc_info", failureText(failure))
		}
	}()
	if sid := session.Get("_sid"); truthy(sid) {
		browser.ReleaseVaultSession(strOr(sid))
	}
}

// Releases this runtime's lease and runs body under the registry lock with whether another runtime owns the row
func otherRuntimeLeaseGuard(ctx context.Context, sessionID string, session *tuigateway.Session, body func(otherOwns bool)) {
	lease, _ := session.Get("active_session_lease").(*activesessions.Lease)
	own := ownLiveLeaseIDs(lease)
	entered := false
	var bodyFailure any
	guarded := func(active bool) error {
		entered = true
		func() {
			defer func() { bodyFailure = recover() }()
			body(active)
		}()
		return nil
	}
	defer func() {
		if lease != nil && lease.Released() && session.Get("active_session_lease") == lease {
			session.Pop("active_session_lease")
		}
	}()
	lastErr := leaseRetry(3, func() error {
		var err error
		if lease != nil && lease.Enabled {
			err = activesessions.ReleaseLivenessGuard(ctx, lease, sessionID, own, guarded)
		} else {
			err = activesessions.LivenessGuard(ctx, sessionID, strOr(session.Get("profile_home")), own, guarded)
		}
		if entered {
			return nil
		}
		return err
	})
	if bodyFailure != nil {
		panic(bodyFailure)
	}
	if lastErr != nil && !entered {
		logger().Warn(fmt.Sprintf("Failed to inspect active session leases; preserving session %s: %s", sessionID, lastErr))
		body(true)
	}
}

// History messages of a session as dicts
func historyDicts(history []any) []map[string]any {
	out := make([]map[string]any, 0, len(history))
	for _, item := range history {
		if msg, ok := item.(map[string]any); ok {
			out = append(out, msg)
		}
	}
	return out
}

// Closes a session's slash worker, any worker shape with a close method
func closeSlashWorker(worker any) {
	switch w := worker.(type) {
	case interface{ Close() error }:
		_ = w.Close()
	case interface{ Close() }:
		w.Close()
	}
}

// Runs the end of session hooks, the memory commit and the row end once, mirroring the CLI exit path
func finalizeSession(ctx context.Context, session *tuigateway.Session, endReason string) {
	if session == nil || len(session.Fields()) == 0 || truthy(session.Get("_finalized")) {
		return
	}
	session.Set("_finalized", true)
	lockVaultManagers(session)
	if ready, ok := session.Get("resume_history_ready").(*pythread.Event); ok && ready != nil && !ready.IsSet() {
		session.Set("resume_history_error", "session resume cancelled")
		ready.Set()
	}
	desktopAutomaticCleanup := automaticSessionEndReasons[endReason] && strings.ToLower(strings.TrimSpace(sessionSource(session))) == "desktop"
	if !desktopAutomaticCleanup {
		releaseActiveSessionSlot(session)
	}
	if stop, ok := session.Get("_notif_stop").(*pythread.Event); ok && stop != nil {
		stop.Set()
	}
	a := sessionAgent(session)
	unlock := lockHistory(session)
	stored, _ := session.Get("history").([]any)
	history := slices.Clone(stored)
	unlock()
	if a != nil {
		if snapshot := a.SessionMessages(); len(snapshot) > 0 {
			suppress(func() { _ = a.PersistSession(&snapshot, nil) })
		}
	}
	scoped := sessionProfileRuntimeScope(ctx, session, true)
	if a != nil {
		suppress(func() {
			sid := a.SessionID()
			if sid == "" {
				sid = strOr(session.Get("session_key"))
			}
			platform := a.Platform()
			if platform == "" {
				platform = "tui"
			}
			plugins.InvokeHook(scoped, "on_session_end", map[string]any{"completed": false, "interrupted": true,
				"session_id": sid, "model": a.Model(), "platform": platform})
		})
	}
	if a != nil && len(history) > 0 {
		suppress(func() { a.CommitMemorySession(scoped, historyDicts(history)) })
	}
	sessionKey := session.Get("session_key")
	sessionID := ""
	if a != nil {
		sessionID = a.SessionID()
	}
	if sessionID == "" {
		sessionID = strOr(sessionKey)
	}
	var boundaryID *string
	if sessionID != "" {
		boundaryID = &sessionID
	}
	session.Set("_end_msgs", notifySessionBoundary(ctx, "on_session_finalize", boundaryID, sessionSource(session)))
	if desktopAutomaticCleanup && sessionID == "" {
		releaseActiveSessionSlot(session)
	}
	tuiOwnsLifecycle := true
	lifecycle := func(otherOwns bool) {
		tuiOwnsLifecycle = !otherOwns
		if otherOwns {
			logger().Info(fmt.Sprintf("Preserving session %s during %s: another backend owns an active lease", sessionID, endReason))
		}
		if sessionID == "" {
			return
		}
		suppress(func() {
			db, release := sessionDB(ctx, session)
			defer release()
			if db == nil {
				return
			}
			row, err := db.GetSession(ctx, sessionID)
			if err != nil {
				panic(err)
			}
			source := ""
			if row != nil {
				source = strOr(row.GetOr("source", ""))
			}
			if isGatewayOwnedSource(ctx, source) {
				tuiOwnsLifecycle = false
			} else if tuiOwnsLifecycle && !desktopAutomaticCleanup {
				if !endUnlessLedgerBusy(ctx, db, sessionID, sessionKey, endReason) {
					tuiOwnsLifecycle = false
				}
			}
		})
	}
	if desktopAutomaticCleanup && sessionID != "" {
		otherRuntimeLeaseGuard(ctx, sessionID, session, lifecycle)
	} else {
		lifecycle(truthy(session.Get("_lease_taken_over")))
	}
	if a != nil && (desktopAutomaticCleanup || !tuiOwnsLifecycle) {
		a.SetEndSessionOnClose(false)
	}
	suppress(func() {
		key := ""
		if tuiOwnsLifecycle {
			key = strOr(sessionKey)
		}
		tools.InterruptAsyncDelegationsForSession(key, lifecycleOwnSID(session, ""), "", endReason)
	})
	if tuiOwnsLifecycle && truthy(sessionKey) {
		suppress(func() { tools.ClearSession(pyrepr.Str(sessionKey)) })
	}
	if a != nil {
		suppress(func() { a.CancelBackgroundReview(fmt.Sprintf("session ended (%s)", endReason), "session ended") })
	}
	suppress(func() {
		if worker := session.Get("slash_worker"); truthy(worker) {
			closeSlashWorker(worker)
		}
	})
}

// End reasons where the backend reclaimed a session its client never asked to close
var reclaimEndReasons = map[string]bool{"idle_timeout": true, "lru_evict": true, "ws_orphan_reap": true}

// Broadcasts session.reclaimed when the backend reclaimed the session out from under its clients
func announceSessionReclaimed(session *tuigateway.Session, endReason string) {
	if !reclaimEndReasons[endReason] {
		return
	}
	defer func() {
		if failure := recover(); failure != nil {
			logger().Debug("session.reclaimed broadcast failed", "exc_info", failureText(failure))
		}
	}()
	broadcastGlobalEvent("session.reclaimed", map[string]any{
		"session_id": strOr(session.Get("_sid")), "stored_session_id": strOr(session.Get("session_key")), "reason": endReason})
}

// Broadcasts approval.cancelled naming the pending gateway approvals an interrupt or teardown drops
func announceCancelledGatewayApprovals(session *tuigateway.Session, reason, sessionID string) {
	sessionKey := strOr(session.Get("session_key"))
	if sessionKey == "" {
		return
	}
	var pending []map[string]any
	failed := false
	func() {
		defer func() {
			if failure := recover(); failure != nil {
				logger().Debug("list_gateway_approvals failed", "exc_info", failureText(failure))
				failed = true
			}
		}()
		pending = tools.ListGatewayApprovals(sessionKey)
	}()
	if failed || len(pending) == 0 {
		return
	}
	requestIDs := []any{}
	for _, item := range pending {
		if rid := strOr(item["request_id"]); rid != "" {
			requestIDs = append(requestIDs, rid)
		}
	}
	if sessionID == "" {
		sessionID = strOr(session.Get("_sid"))
	}
	defer func() {
		if failure := recover(); failure != nil {
			logger().Debug("approval.cancelled broadcast failed", "exc_info", failureText(failure))
		}
	}()
	broadcastGlobalEvent("approval.cancelled", map[string]any{"session_id": sessionID, "stored_session_id": sessionKey,
		"reason": reason, "cancelled_count": len(pending), "request_ids": requestIDs})
}

// Finalizes a session, announces a reclaim and dropped approvals, unregisters its notifier and closes its agent
func teardownSession(ctx context.Context, session *tuigateway.Session, endReason string) {
	if session == nil || len(session.Fields()) == 0 {
		return
	}
	finalizeSession(ctx, session, endReason)
	announceSessionReclaimed(session, endReason)
	suppress(func() { announceCancelledGatewayApprovals(session, endReason, "") })
	suppress(func() {
		if key := session.Get("session_key"); truthy(key) && !truthy(session.Get("_lease_taken_over")) {
			tools.UnregisterGatewayNotify(pyrepr.Str(key))
		}
	})
	suppress(func() {
		scoped := sessionProfileRuntimeScope(ctx, session, true)
		if a := sessionAgent(session); a != nil {
			_ = a.CloseContext(scoped)
		}
	})
}

// Stores a worker on a session that is still live under sid, else closes it
func attachWorker(sid string, session *tuigateway.Session, worker any) {
	sessionsLock.Lock()
	if tuigateway.SessionRecord(sid) == session {
		session.Set("slash_worker", worker)
		sessionsLock.Unlock()
		return
	}
	sessionsLock.Unlock()
	closeSlashWorker(worker)
}

// Whether a live UI session still owns background delegations by its sid or, when it owns the row, its key
func sessionHasActiveDelegations(ctx context.Context, sid string, session *tuigateway.Session) (active bool) {
	if session == nil {
		session = tuigateway.SessionRecord(sid)
	}
	if session == nil || len(session.Fields()) == 0 {
		return false
	}
	ownSID := lifecycleOwnSID(session, sid)
	sessionKey := strOr(session.Get("session_key"))
	ownedSessionKey := sessionKey
	sessionID := sessionKey
	if a := sessionAgent(session); a != nil && a.SessionID() != "" {
		sessionID = a.SessionID()
	}
	if sessionID != "" {
		suppress(func() {
			db, release := sessionDB(ctx, session)
			defer release()
			if db == nil {
				return
			}
			row, err := db.GetSession(ctx, sessionID)
			if err != nil {
				panic(err)
			}
			source := ""
			if row != nil {
				source = strOr(row.GetOr("source", ""))
			}
			if isGatewayOwnedSource(ctx, source) {
				ownedSessionKey = ""
			}
		})
	}
	if ownSID == "" && ownedSessionKey == "" {
		return false
	}
	defer func() {
		if failure := recover(); failure != nil {
			logger().Debug(fmt.Sprintf("Failed to query active delegations for UI session %s", sid), "exc_info", failureText(failure))
			active = true
		}
	}()
	return tools.HasLiveForSession(ownedSessionKey, ownSID, "")
}
