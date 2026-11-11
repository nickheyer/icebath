package gateway

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"path/filepath"
	"time"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/internal/pyint"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pypath"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/provider"
	"github.com/nickheyer/icebath/pkg/state"
	"github.com/nickheyer/icebath/pkg/tools"
)

const viewerClosePrefix = state.ViewerClosePrefix

// Seconds of idleness the legacy startup sweep allowed a session before reaping it
const startupIdleS = 6 * 3600.0

// Viewer reasons main's TUI and classic CLI followed with a session reset
var viewerResetReasons = map[string]bool{"new_session": true, "tui_close": true}

// Close reasons nobody asked for, the session stays resumable
var orphanReasons = map[string]bool{"ws_orphan_reap": true, "startup_orphan_reap": true}

// One viewer close, its identity telling a settler the close is still pending, its record the stored value
type pendingClose struct {
	reason string
	record string
}

// Close with a fresh operation id record
func newPendingClose(reason string) *pendingClose {
	record, _ := pyjson.Dumps(map[string]any{"op": uuid4HexString(), "reason": reason}, pyjson.SortKeys(true))
	return &pendingClose{reason: reason, record: record}
}

// Close rebuilt from its stored record, a bare reason record from before operation ids kept as the reason
func pendingCloseFromRecord(record string) *pendingClose {
	reason := record
	if decoded, err := pyjson.Loads(record); err == nil {
		if obj, ok := decoded.(*pyjson.Object); ok {
			if value, found := obj.Get("reason"); found {
				if text, isText := value.(string); isText {
					reason = text
				} else {
					reason = pyrepr.Str(value)
				}
			}
		}
	}
	return &pendingClose{reason: reason, record: record}
}

// Runner that commits a cached agent's memory as it evicts it at a session boundary
type boundaryEvictingRunner interface {
	EvictCachedAgentAtBoundary(route string)
}

// Runner that fires the session finalize hooks off its loop and returns their messages
type sessionFinalizingRunner interface {
	FinalizeSessionOffLoop(ctx context.Context, sessionID, platform, reason, sessionKey string) ([]string, error)
}

// Runner that runs the gateway's own new session reset off its loop
type sessionResettingRunner interface {
	ResetSessionOffLoop(ctx context.Context, sessionKey string, sessionID *string, reason, platform string, oldSessionID, newSessionID *string) error
}

// Closes a session its last viewer left with an end reason now or at the FIFO's next idle point
func closeViewerSession(ctx context.Context, a *SessionAuthority, sessionID, endReason string) error {
	a.mu.Lock()
	live, ok := a.Sessions[sessionID]
	if !ok {
		a.mu.Unlock()
		return &pyKeyError{sessionID}
	}
	if len(live.Subscribers) > 0 || isHistoryMount(live) {
		a.mu.Unlock()
		return nil
	}
	route := live.Route
	a.mu.Unlock()
	target, err := viewerPhysicalTarget(ctx, a, sessionID)
	if err != nil {
		return err
	}
	parents := []string{sessionID}
	if target != sessionID {
		parents = append(parents, target)
	}
	tools.RefuseAsyncDelegationsForSession(delegationToken(a.ProfileID, sessionID), route, parents)
	for _, parent := range parents {
		tools.InterruptAsyncDelegationsForSession(route, "", parent, "viewer_close:"+endReason)
	}
	close := newPendingClose(endReason)
	a.mu.Lock()
	if a.pendingCloses == nil {
		a.pendingCloses = map[string]*pendingClose{}
	}
	a.pendingCloses[sessionID] = close
	a.mu.Unlock()
	return storeAndSettle(ctx, a, sessionID, live, close)
}

// Async delegation fence token of one session of a profile
func delegationToken(profileID, sessionID string) [2]string {
	return [2]string{profileID, sessionID}
}

// Lifts the async delegation refusal of one session a viewer attached to again
func allowForSession(profileID, sessionID string) {
	tools.AllowAsyncDelegationsForSession(delegationToken(profileID, sessionID))
}

// Persists a pending close and completes it at once when the FIFO is idle
func storeAndSettle(ctx context.Context, a *SessionAuthority, sessionID string, live *LiveSession, close *pendingClose) error {
	if err := persistClose(ctx, a, sessionID, close.record); err != nil {
		return err
	}
	if !stillPending(a, sessionID, live, close) {
		_, err := forgetPersistedClose(ctx, a, sessionID, close.record)
		return err
	}
	return settleViewerClose(ctx, a, sessionID)
}

// Stores a session's close record under the owner epoch fence
func persistClose(ctx context.Context, a *SessionAuthority, sessionID, record string) error {
	return a.DB.ExecuteWrite(ctx, func(conn state.SQLiteConn) error {
		if err := state.RuntimeEpoch(ctx, conn, a.Epoch); err != nil {
			return err
		}
		_, err := conn.ExecContext(ctx, "INSERT INTO state_meta(key,value) VALUES(?,?) ON CONFLICT(key) DO UPDATE SET value=excluded.value",
			viewerClosePrefix+sessionID, record)
		return err
	})
}

// Drops a stored close only while it is still exactly record and this authority owns the store
func forgetPersistedClose(ctx context.Context, a *SessionAuthority, sessionID, record string) (bool, error) {
	var removed int64
	err := a.DB.ExecuteWrite(ctx, func(conn state.SQLiteConn) error {
		removed = 0
		if err := state.RuntimeEpoch(ctx, conn, a.Epoch); err != nil {
			return err
		}
		result, err := conn.ExecContext(ctx, "DELETE FROM state_meta WHERE key=? AND value=?", viewerClosePrefix+sessionID, record)
		if err != nil {
			return err
		}
		removed, err = result.RowsAffected()
		return err
	})
	var refusal *state.RuntimeStoreError
	if errors.As(err, &refusal) {
		slog.Info(fmt.Sprintf("Stored close for %s left in place: %s", sessionID, refusal.Error()))
		return false, nil
	}
	return removed != 0, err
}

// Drops the stored close of a session a viewer attached to unless it is the close pending in memory
func cancelPersistedClose(ctx context.Context, a *SessionAuthority, sessionID string) error {
	record, found, err := a.DB.GetMeta(ctx, viewerClosePrefix+sessionID)
	if err != nil || !found {
		return err
	}
	a.mu.Lock()
	pending := a.pendingCloses[sessionID]
	a.mu.Unlock()
	if pending != nil && pending.record == record {
		return nil
	}
	_, err = forgetPersistedClose(ctx, a, sessionID, record)
	return err
}

// Re-arms every stored close of a mounted session with no viewer and returns their ids
func rearmPersistedCloses(ctx context.Context, a *SessionAuthority) ([]string, error) {
	type storedClose struct{ key, record string }
	var stored []storedClose
	err := a.DB.ReadCtx(ctx, func(conn state.SQLiteConn) error {
		stored = nil
		rows, err := conn.QueryContext(ctx, "SELECT key, value FROM state_meta WHERE key LIKE ?", viewerClosePrefix+"%")
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var item storedClose
			if err := rows.Scan(&item.key, &item.record); err != nil {
				return err
			}
			stored = append(stored, item)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, err
	}
	armed := []string{}
	for _, item := range stored {
		sessionID := item.key[len(viewerClosePrefix):]
		a.mu.Lock()
		live := a.Sessions[sessionID]
		if live == nil || isHistoryMount(live) {
			a.mu.Unlock()
			continue
		}
		if len(live.Subscribers) > 0 {
			a.mu.Unlock()
			if _, err := forgetPersistedClose(ctx, a, sessionID, item.record); err != nil {
				return nil, err
			}
			continue
		}
		if a.pendingCloses == nil {
			a.pendingCloses = map[string]*pendingClose{}
		}
		if _, held := a.pendingCloses[sessionID]; !held {
			a.pendingCloses[sessionID] = pendingCloseFromRecord(item.record)
		}
		a.mu.Unlock()
		armed = append(armed, sessionID)
	}
	return armed, nil
}

// Non negative float seconds of a config value, default when it is no number
func closeSeconds(raw any, def float64) (float64, error) {
	value, err := pyint.Float(raw)
	if err != nil {
		var pyErr *pyint.Error
		if errors.As(err, &pyErr) && (pyErr.Kind == "ValueError" || pyErr.Kind == "TypeError") {
			return def, nil
		}
		return 0, err
	}
	if value > 0 {
		return value, nil
	}
	return 0, nil
}

// Mapping under a config key, empty when absent or no mapping
func closeConfigSection(cfg map[string]any, key string) map[string]any {
	if section, ok := cfg[key].(map[string]any); ok {
		return section
	}
	return map[string]any{}
}

// Orphan end grace in seconds of the profile owning the store, zero meaning never
func orphanCloseGrace(ctx context.Context, a *SessionAuthority, startup bool) (float64, error) {
	def, _ := pyint.Float(closeConfigSection(config.Defaults(), "sessions")["orphan_end_grace_s"])
	resolved, err := pypath.Resolve(a.DB.Path())
	if err != nil {
		return 0, err
	}
	profileHome := filepath.Dir(resolved)
	scoped := profileEnvScope(ctx, profileHome)
	cfg, err := config.LoadUserEffective(filepath.Join(profileHome, "config.yaml"), func(name string) (string, bool) { return getenv(scoped, name) })
	if err != nil {
		return 0, err
	}
	sessions, dashboard := closeConfigSection(cfg, "sessions"), closeConfigSection(cfg, "dashboard")
	if raw, ok := sessions["orphan_end_grace_s"]; ok {
		return closeSeconds(raw, def)
	}
	reap, err := closeSeconds(dashboard["ws_orphan_reap_grace_s"], def)
	if err != nil {
		return 0, err
	}
	sweep := dashboard["startup_orphan_sweep"]
	if reap == 0 || (startup && !(sweep == nil || provider.IsTruthyValue(sweep))) {
		return 0, nil
	}
	return def, nil
}

// True for a local session with an open row whose policy is not ACP
func orphanCandidate(ctx context.Context, a *SessionAuthority, sessionID string) (bool, error) {
	receipt, row, err := orphanReceiptRow(ctx, a, sessionID)
	if err != nil {
		var refusal *state.RuntimeStoreError
		if class := checkErrorClass(err); errors.As(err, &refusal) || class == "KeyError" || class == "TypeError" {
			return false, nil
		}
		return false, err
	}
	var source any
	if policy, ok := receipt.Get("policy"); ok {
		obj, isObj := policy.(*pyjson.Object)
		if !isObj {
			return false, &PyError{Kind: "AttributeError", Msg: "'" + jsonValueTypeName(policy) + "' object has no attribute 'get'"}
		}
		source, _ = obj.Get("source")
	}
	if source == "acp" || row == nil {
		return false, nil
	}
	ended, _ := row.Get("ended_at")
	return ended == nil, nil
}

// Stored local receipt of a session and the row of the transcript its entry names
func orphanReceiptRow(ctx context.Context, a *SessionAuthority, sessionID string) (*pyjson.Object, *pyjson.Object, error) {
	receipt, err := a.DB.LocalReceipt(ctx, sessionID)
	if err != nil {
		return nil, nil, err
	}
	rawEntry, err := receiptField(receipt, "entry")
	if err != nil {
		return nil, nil, err
	}
	entry, ok := rawEntry.(*pyjson.Object)
	if !ok {
		return nil, nil, &PyError{Kind: "TypeError", Msg: "'" + jsonValueTypeName(rawEntry) + "' object is not subscriptable"}
	}
	target, err := receiptField(entry, "session_id")
	if err != nil {
		return nil, nil, err
	}
	text, isText := target.(string)
	if !isText {
		return receipt, nil, nil
	}
	row, err := a.DB.GetSession(ctx, text)
	return receipt, row, err
}

// True while the authority retires or its runner drains
func closeStopping(a *SessionAuthority) bool {
	return a.Retiring || a.Retired || a.Runner.Draining()
}

// Arms a close that fires after grace seconds unless a viewer attaches, a stopping owner arming nothing
func armOrphanClose(a *SessionAuthority, sessionID, reason string, grace float64) {
	a.mu.Lock()
	live := a.Sessions[sessionID]
	if grace == 0 || live == nil || len(live.Subscribers) > 0 || isHistoryMount(live) || closeStopping(a) {
		a.mu.Unlock()
		return
	}
	a.mu.Unlock()
	cancelOrphanClose(a, sessionID)
	delay := time.Duration(math.Min(grace, float64(math.MaxInt64)/float64(time.Second)) * float64(time.Second))
	a.mu.Lock()
	if a.orphanCloses == nil {
		a.orphanCloses = map[string]*time.Timer{}
	}
	var timer *time.Timer
	timer = time.AfterFunc(delay, func() { fireOrphanClose(a, sessionID, reason, &timer) })
	a.orphanCloses[sessionID] = timer
	a.mu.Unlock()
}

// Arms the orphan close of a session a viewer left with no end reason
func armDetachedOrphanClose(ctx context.Context, a *SessionAuthority, sessionID string) error {
	candidate, err := orphanCandidate(ctx, a, sessionID)
	if err != nil {
		return err
	}
	grace := 0.0
	if candidate {
		if grace, err = orphanCloseGrace(ctx, a, false); err != nil {
			return err
		}
	}
	armOrphanClose(a, sessionID, "ws_orphan_reap", grace)
	return nil
}

// Seconds since the session's physical transcript was last active, zero when it has no recency
func closeIdleSeconds(ctx context.Context, a *SessionAuthority, sessionID string) (float64, error) {
	target, err := viewerPhysicalTarget(ctx, a, sessionID)
	if err != nil {
		return 0, err
	}
	var last sql.NullFloat64
	err = a.DB.ReadCtx(ctx, func(conn state.SQLiteConn) error {
		err := conn.QueryRowContext(ctx, "SELECT "+state.SQLSessionLastActive("s")+" FROM sessions s WHERE s.id = ?", target).Scan(&last)
		if errors.Is(err, sql.ErrNoRows) {
			last = sql.NullFloat64{}
			return nil
		}
		return err
	})
	if err != nil || !last.Valid || last.Float64 == 0 {
		return 0, err
	}
	return float64(time.Now().UnixNano())/1e9 - last.Float64, nil
}

// Arms the startup orphan close of sessions the previous owner stopped under, after the legacy sweep TTL
func armStartupOrphanCloses(ctx context.Context, a *SessionAuthority, orphans []string) error {
	grace, err := orphanCloseGrace(ctx, a, true)
	if err != nil {
		return err
	}
	for _, sessionID := range orphans {
		if grace == 0 {
			continue
		}
		a.mu.Lock()
		_, pending := a.pendingCloses[sessionID]
		a.mu.Unlock()
		if pending {
			continue
		}
		candidate, err := orphanCandidate(ctx, a, sessionID)
		if err != nil {
			return err
		}
		if !candidate {
			continue
		}
		idle, err := closeIdleSeconds(ctx, a, sessionID)
		if err != nil {
			return err
		}
		armOrphanClose(a, sessionID, "startup_orphan_reap", math.Max(grace, startupIdleS-idle))
	}
	return nil
}

// Disarms a session's orphan close timer
func cancelOrphanClose(a *SessionAuthority, sessionID string) {
	a.mu.Lock()
	timer := a.orphanCloses[sessionID]
	delete(a.orphanCloses, sessionID)
	a.mu.Unlock()
	if timer != nil {
		timer.Stop()
	}
}

// Turns a fired orphan close into the persisted pending close a viewer's own close uses
func fireOrphanClose(a *SessionAuthority, sessionID, reason string, timer **time.Timer) {
	a.mu.Lock()
	if a.orphanCloses[sessionID] != *timer {
		a.mu.Unlock()
		return
	}
	delete(a.orphanCloses, sessionID)
	live := a.Sessions[sessionID]
	_, pending := a.pendingCloses[sessionID]
	if live == nil || len(live.Subscribers) > 0 || pending || closeStopping(a) {
		a.mu.Unlock()
		return
	}
	close := newPendingClose(reason)
	if a.pendingCloses == nil {
		a.pendingCloses = map[string]*pendingClose{}
	}
	a.pendingCloses[sessionID] = close
	a.mu.Unlock()
	trackMutation(a, func(taskCtx context.Context) (any, error) {
		return nil, storeAndSettle(taskCtx, a, sessionID, live, close)
	})
}

// Completes a pending viewer close once the session has no viewer and an idle FIFO, running its end lifecycle
func settleViewerClose(ctx context.Context, a *SessionAuthority, sessionID string) error {
	a.mu.Lock()
	close := a.pendingCloses[sessionID]
	live := a.Sessions[sessionID]
	if close == nil || live == nil || len(live.Subscribers) > 0 {
		delete(a.pendingCloses, sessionID)
		a.mu.Unlock()
		return nil
	}
	a.mu.Unlock()
	proceed, err := resolveAbandonedUnknown(ctx, a, sessionID, live, close)
	if err != nil || !proceed {
		return err
	}
	target, err := viewerPhysicalTarget(ctx, a, sessionID)
	if err != nil {
		return err
	}
	busy, err := viewerBusy(ctx, a, sessionID, target)
	if err != nil || busy {
		return err
	}
	idleStamp := live.EventStream.Execution
	row, err := a.DB.GetSession(ctx, target)
	if err != nil {
		return err
	}
	a.mu.Lock()
	if len(live.Subscribers) > 0 || a.pendingCloses[sessionID] != close {
		a.mu.Unlock()
		return nil
	}
	delete(a.pendingCloses, sessionID)
	a.mu.Unlock()
	result, err := trackedWrite(ctx, a, func(taskCtx context.Context) (any, error) {
		return endIdleViewerSession(taskCtx, a, sessionID, close.reason)
	}, func(taskCtx context.Context, ended any) (any, error) {
		return reopenIfReattached(taskCtx, a, sessionID, ended.(bool))
	}, false, nil)
	if err != nil {
		return err
	}
	if ended, _ := result.(bool); !ended {
		return requeueIfBusy(ctx, a, sessionID, live, target, close)
	}
	if _, err := forgetPersistedClose(ctx, a, sessionID, close.record); err != nil {
		return err
	}
	if liveHasViewers(a, live) {
		return nil
	}
	platform := ""
	if row != nil {
		if source, _ := row.Get("source"); pyTruthy(source) {
			platform = pyrepr.Str(source)
		}
	}
	if platform == "" && live.Source != nil {
		platform = string(live.Source.Platform)
	}
	releaseUnlocks(sessionID)
	if err := runCloseLifecycle(ctx, a, live, target, platform, close.reason); err != nil {
		return err
	}
	if !liveHasViewers(a, live) {
		live.EventStream.Release()
	}
	_, err = retireClosed(ctx, a, sessionID, live, target, idleStamp)
	return err
}

// Resolves the unknown turns a closed session left as Discard does, false when the close must wait
func resolveAbandonedUnknown(ctx context.Context, a *SessionAuthority, sessionID string, live *LiveSession, close *pendingClose) (bool, error) {
	rows, err := a.DB.ListSessionAdmissions(ctx, sessionID, true)
	if err != nil {
		return false, err
	}
	var unknown []*pyjson.Object
	for _, row := range rows {
		if rowString(row, "status") == "unknown" {
			unknown = append(unknown, row)
		}
	}
	if len(unknown) == 0 {
		return true, nil
	}
	if orphanReasons[close.reason] {
		return false, nil
	}
	ref := SessionRef{ProfileID: a.ProfileID, SessionID: sessionID}
	for _, row := range unknown {
		if !stillPending(a, sessionID, live, close) {
			return false, nil
		}
		id := rowString(row, "admission_id")
		var generation int64
		if stamped := rowInt(row, "generation"); stamped != nil {
			generation = *stamped
		}
		if _, err := a.ResolveUnknownAsOwner(ctx, ref, id, generation, nil); err != nil {
			var refusal *state.RuntimeStoreError
			if errors.As(err, &refusal) {
				slog.Warn(fmt.Sprintf("Unknown turn %s on closed session %s left paused: %s", id, sessionID, refusal.Reason))
				return false, nil
			}
			return false, err
		}
		slog.Info(fmt.Sprintf("Unknown turn %s resolved interrupted: its session %s was closed by its last viewer", id, sessionID))
	}
	return stillPending(a, sessionID, live, close), nil
}

// True while the live session has a subscribed viewer
func liveHasViewers(a *SessionAuthority, live *LiveSession) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return len(live.Subscribers) > 0
}

// True while the session has no viewer and close is still its pending close
func stillPending(a *SessionAuthority, sessionID string, live *LiveSession, close *pendingClose) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return len(live.Subscribers) == 0 && a.pendingCloses[sessionID] == close
}

// Returns a refused close to the drain's next idle point when input was admitted after the idle check
func requeueIfBusy(ctx context.Context, a *SessionAuthority, sessionID string, live *LiveSession, target string, close *pendingClose) error {
	busy, err := viewerBusy(ctx, a, sessionID, target)
	if err != nil {
		return err
	}
	if !busy {
		a.mu.Lock()
		_, pending := a.pendingCloses[sessionID]
		a.mu.Unlock()
		if !pending {
			_, err = forgetPersistedClose(ctx, a, sessionID, close.record)
		}
		return err
	}
	a.mu.Lock()
	if _, pending := a.pendingCloses[sessionID]; len(live.Subscribers) > 0 || pending {
		a.mu.Unlock()
		return nil
	}
	if a.pendingCloses == nil {
		a.pendingCloses = map[string]*pendingClose{}
	}
	a.pendingCloses[sessionID] = close
	a.mu.Unlock()
	return settleViewerClose(ctx, a, sessionID)
}

// Runs the memory commit, finalize hooks and viewer reset main's client ran at exit and new
func runCloseLifecycle(ctx context.Context, a *SessionAuthority, live *LiveSession, target, platform, reason string) error {
	if evict, ok := a.Runner.(boundaryEvictingRunner); ok {
		evict.EvictCachedAgentAtBoundary(live.Route)
	}
	finalize, ok := a.Runner.(sessionFinalizingRunner)
	if !ok {
		return nil
	}
	messages, err := finalize.FinalizeSessionOffLoop(ctx, target, platform, reason, live.Route)
	if err != nil {
		return err
	}
	if len(messages) > 0 {
		slog.Info(fmt.Sprintf("Session-end plugin message(s) for %s dropped: no viewer", target))
	}
	reset, ok := a.Runner.(sessionResettingRunner)
	if viewerResetReasons[reason] && ok {
		oldSessionID := target
		return reset.ResetSessionOffLoop(ctx, live.Route, nil, "new_session", platform, &oldSessionID, nil)
	}
	return nil
}
