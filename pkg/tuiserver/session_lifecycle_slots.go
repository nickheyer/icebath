package tuiserver

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/nickheyer/icebath/pkg/activesessions"
	"github.com/nickheyer/icebath/pkg/state"
	"github.com/nickheyer/icebath/pkg/tuigateway"
)

// Claims the cap slot of a session on its first real turn, nil when it holds one now
func ensureActiveSessionSlot(ctx context.Context, sid string, session *tuigateway.Session) *activesessions.Refusal {
	if canonical := canonicalLedgerRefusal(ctx, session); canonical != nil {
		return canonical
	}
	if session.Get("active_session_lease") != nil {
		return nil
	}
	key := strOr(session.Get("session_key"))
	lease, refusal := tuigateway.ClaimActiveSessionSlotRefusal(ctx, key, sid, sessionSource(session), strOr(session.Get("profile_home")))
	if refusal == nil {
		attachLease(session, lease)
		return nil
	}
	if refusal.Reason == activesessions.SessionNotOwned && takeOverDetachedRuntimeLease(ctx, sid, session, key) {
		return nil
	}
	return refusal
}

// Refusal while the gateway's durable ledger still holds work for this stored session, else nil
func canonicalLedgerRefusal(ctx context.Context, session *tuigateway.Session) *activesessions.Refusal {
	key := strOr(session.Get("session_key"))
	if key == "" {
		return nil
	}
	db, release := sessionDB(ctx, session)
	defer release()
	if db == nil {
		return nil
	}
	var status any
	found := false
	err := db.ReadCtx(ctx, func(conn state.SQLiteConn) error {
		switch err := conn.QueryRowContext(ctx, canonicalPendingSQL, key, key).Scan(&status); {
		case err == nil:
			found = true
			return nil
		case errors.Is(err, sql.ErrNoRows):
			return nil
		default:
			return err
		}
	})
	if err != nil {
		logger().Warn("canonical ledger read failed for "+key, "error", err)
		return &activesessions.Refusal{Message: tuigateway.SessionOwnershipUnavailable, Reason: activesessions.SessionCoordinationUnavailable}
	}
	if !found {
		return nil
	}
	ledger := strOr(status)
	logger().Info(fmt.Sprintf("Refusing legacy turn for %s: canonical ledger holds %s work", key, ledger))
	kind := "pending"
	if ledger == "unknown" {
		kind = "outcome-unknown"
	}
	return &activesessions.Refusal{
		Message: fmt.Sprintf("Session %s has %s work in the gateway's durable queue. Resume it through the gateway "+
			"(icebath chat --resume, Desktop or the TUI) to resolve it; nothing was run here.", key, kind),
		Reason: activesessions.SessionNotOwned, LedgerStatus: ledger}
}

// Sibling runtime of the same key and profile holding an unreleased lease with no client, under sessionsLock
func detachedLeaseHolder(session *tuigateway.Session, key string) (string, *tuigateway.Session, bool) {
	for _, live := range tuigateway.LiveSessions() {
		other := live.Session
		if other == session || truthy(other.Get("_finalized")) || strOr(other.Get("session_key")) != key ||
			!liveProfileMatches(other, strOr(session.Get("profile_home")), false) {
			continue
		}
		lease, _ := other.Get("active_session_lease").(*activesessions.Lease)
		transport, _ := other.Get("transport").(tuigateway.Transport)
		if lease != nil && !lease.Released() && tuigateway.TransportIsDead(transport) {
			return live.SID, other, true
		}
	}
	return "", nil, false
}

// Moves the lease of a detached sibling runtime to this session and asks its turn to stop, true when moved
func takeOverDetachedRuntimeLease(ctx context.Context, sid string, session *tuigateway.Session, key string) bool {
	otherSID, other, moved := func() (string, *tuigateway.Session, bool) {
		sessionResumeLock.Lock()
		defer sessionResumeLock.Unlock()
		sessionsLock.Lock()
		defer sessionsLock.Unlock()
		otherSID, other, found := detachedLeaseHolder(session, key)
		if !found {
			return "", nil, false
		}
		lease, _ := other.Get("active_session_lease").(*activesessions.Lease)
		transferred, err := activesessions.TransferActiveSession(lease, key, tuigateway.LeaseMetadata(sid))
		if err != nil {
			panic(err)
		}
		if !transferred {
			return "", nil, false
		}
		other.Pop("active_session_lease")
		other.Set("_lease_taken_over", true)
		attachLease(session, lease)
		return otherSID, other, true
	}()
	if !moved {
		return false
	}
	logger().Info(fmt.Sprintf("Session %s took over lease for %s from detached runtime %s", sid, key, otherSID))
	func() {
		defer func() {
			if r := recover(); r != nil {
				logger().Error("lease-takeover interrupt failed sid="+otherSID, "error", r)
			}
		}()
		interruptSessionTurn(ctx, otherSID, other, "lease-takeover-"+sid, false)
	}()
	return true
}

// Re anchors the session lease on a rotated session id, reserving the new slot before releasing the old
func transferActiveSessionSlot(ctx context.Context, sid string, session *tuigateway.Session, newSessionID string) bool {
	if newSessionID == "" {
		return false
	}
	lease, _ := session.Get("active_session_lease").(*activesessions.Lease)
	if lease == nil {
		return true
	}
	transferred := func() (ok bool) {
		defer func() {
			if r := recover(); r != nil {
				logger().Debug("Failed to transfer active session slot", "error", r)
				ok = false
			}
		}()
		done, err := activesessions.TransferActiveSession(lease, newSessionID, tuigateway.LeaseMetadata(sid))
		if err != nil {
			logger().Debug("Failed to transfer active session slot", "error", err)
			return false
		}
		return done
	}()
	if transferred {
		return true
	}
	if lease.TrackLiveness {
		return false
	}
	newLease, refusal := tuigateway.ClaimActiveSessionSlotRefusal(ctx, newSessionID, sid, sessionSource(session), strOr(session.Get("profile_home")))
	if newLease == nil {
		if refusal != nil && refusal.Message != "" {
			logger().Warn(fmt.Sprintf("Compression session lease re-anchor failed (kept old lease): sid=%s new_session_id=%s reason=%s",
				sid, newSessionID, refusal.Message))
		}
		return false
	}
	if old, _ := session.Pop("active_session_lease"); old != nil {
		if held, isLease := old.(*activesessions.Lease); isLease {
			if err := leaseRetry(1, held.Release); err != nil {
				logger().Debug("Failed to release stale active session slot", "error", err)
			}
		}
	}
	session.Set("active_session_lease", newLease)
	return true
}
