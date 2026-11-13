package gatewayrun

import (
	"context"
	"fmt"
	"log/slog"
	"math"
	"sort"
	"sync"
	"time"

	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
)

// Cap on tracked leases, idle entries evicting oldest first while live leases never do
const defaultMaxLeases = 512

// Seconds a waiter waits when the caller passes no positive timeout
const defaultLeaseWait = 5.0

// Logger of the turn lease module
func turnLeaseLog() *slog.Logger { return slog.Default().With("logger", "gateway.turn_lease") }

// Seconds since the epoch as time.time reads them
func epochNow() float64 {
	return float64(time.Now().UnixNano()) / 1e9
}

// Owner key and generation of the holder, question marks when nobody holds it
func holderDesc(holder *TurnLeaseToken) (any, any) {
	if holder == nil {
		return "?", "?"
	}
	return holder.ownerKey, holder.generation
}

// Lease held for the whole wait budget, so the caller must not enter the turn region
type TurnLeaseTimeoutError struct {
	SessionID   string
	OwnerKey    string
	Generation  int
	WaitSeconds float64
}

// Message naming the session, routing key, generation and the rounded wait
func (e *TurnLeaseTimeoutError) Error() string {
	return fmt.Sprintf("turn lease wait timed out after %.0fs on session %s for routing key %s (gen %d)",
		e.WaitSeconds, e.SessionID, e.OwnerKey, e.Generation)
}

// Held lease handle whose released flag makes release idempotent
type TurnLeaseToken struct {
	sessionID  string
	ownerKey   string
	generation int
	released   bool
	lease      *sessionLease
}

// Debug text of the handle with its session, routing key, generation and released flag
func (t *TurnLeaseToken) String() string {
	return fmt.Sprintf("TurnLeaseToken(session_id=%s, owner_key=%s, generation=%d, released=%s)",
		pyrepr.Repr(t.sessionID), pyrepr.Repr(t.ownerKey), t.generation, pyrepr.Repr(t.released))
}

// Lock of one resolved session id with its holder and pending acquire count
type sessionLease struct {
	lock            chan struct{}
	holder          *TurnLeaseToken
	acquiredAt      float64
	lastUsed        float64
	pendingAcquires int
}

// Fresh unlocked lease stamped now
func newSessionLease() *sessionLease {
	return &sessionLease{lock: make(chan struct{}, 1), lastUsed: epochNow()}
}

// Whether the lock is held
func (l *sessionLease) locked() bool { return len(l.lock) == 1 }

// True when nobody holds or awaits the lease
func (l *sessionLease) idle() bool {
	return l.holder == nil && !l.locked() && l.pendingAcquires == 0
}

// Lease per resolved session id serializing the load history, run and flush region
type SessionTurnLeaseRegistry struct {
	mu         sync.Mutex
	leases     map[string]*sessionLease
	maxEntries int
}

// Registry capped at maxEntries leases, never fewer than one
func NewSessionTurnLeaseRegistry(maxEntries int) *SessionTurnLeaseRegistry {
	return &SessionTurnLeaseRegistry{leases: map[string]*sessionLease{}, maxEntries: max(1, maxEntries)}
}

// Lease of sessionID, created after evicting idle overflow, stamped as used now
func (r *SessionTurnLeaseRegistry) getOrCreate(sessionID string) *sessionLease {
	lease := r.leases[sessionID]
	if lease == nil {
		r.evictIdle()
		lease = newSessionLease()
		r.leases[sessionID] = lease
	}
	lease.lastUsed = epochNow()
	return lease
}

// Drops the oldest idle entries so a new lease fits under the cap
func (r *SessionTurnLeaseRegistry) evictIdle() {
	overflow := len(r.leases) - r.maxEntries + 1
	if overflow <= 0 {
		return
	}
	var idle []string
	for sid, lease := range r.leases {
		if lease.idle() {
			idle = append(idle, sid)
		}
	}
	sort.SliceStable(idle, func(i, j int) bool { return r.leases[idle[i]].lastUsed < r.leases[idle[j]].lastUsed })
	for _, sid := range idle[:min(overflow, len(idle))] {
		delete(r.leases, sid)
	}
}

// Acquires the lease of sessionID waiting while held, nil for an empty id, a timeout error when the budget expires
func (r *SessionTurnLeaseRegistry) Acquire(ctx context.Context, sessionID, ownerKey string, generation int, timeout *float64) (*TurnLeaseToken, error) {
	if sessionID == "" {
		return nil, nil
	}
	wait := defaultLeaseWait
	if timeout != nil && *timeout > 0 {
		wait = *timeout
	}
	r.mu.Lock()
	lease := r.getOrCreate(sessionID)
	token := &TurnLeaseToken{sessionID: sessionID, ownerKey: ownerKey, generation: generation, lease: lease}
	if lease.locked() {
		heldFor := -1.0
		if lease.acquiredAt != 0 {
			heldFor = epochNow() - lease.acquiredAt
		}
		holderKey, holderGen := holderDesc(lease.holder)
		turnLeaseLog().Warn(fmt.Sprintf("turn lease contention on session %s: routing key %s (gen %d) waiting behind "+
			"in-flight turn held by routing key %v (gen %v, held %.0fs) — two routing keys "+
			"are mapped to one session_id (#64934); serializing this turn behind the previous "+
			"turn's flush", sessionID, ownerKey, generation, holderKey, holderGen, heldFor))
	}
	lease.pendingAcquires++
	r.mu.Unlock()
	timer := time.NewTimer(time.Duration(math.Min(wait, 1e9) * float64(time.Second)))
	defer timer.Stop()
	var failure error
	select {
	case lease.lock <- struct{}{}:
	case <-timer.C:
		r.mu.Lock()
		holderKey, holderGen := holderDesc(lease.holder)
		r.mu.Unlock()
		turnLeaseLog().Error(fmt.Sprintf("turn lease wait timed out after %.0fs on session %s (waiter: routing key %s gen "+
			"%d; holder: routing key %v gen %v) — failing closed: refusing to run this turn "+
			"UNSERIALIZED against the still-held lease", wait, sessionID, ownerKey, generation, holderKey, holderGen))
		failure = &TurnLeaseTimeoutError{SessionID: sessionID, OwnerKey: ownerKey, Generation: generation, WaitSeconds: wait}
	case <-ctx.Done():
		failure = ctx.Err()
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	lease.pendingAcquires--
	if failure != nil {
		return nil, failure
	}
	lease.holder = token
	lease.acquiredAt = epochNow()
	lease.lastUsed = lease.acquiredAt
	return token, nil
}

// Aliases a held lease onto a rotated session id, false for a non holder or a live target
func (r *SessionTurnLeaseRegistry) Rebind(token *TurnLeaseToken, newSessionID string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if token == nil || token.released || newSessionID == "" || newSessionID == token.sessionID {
		return false
	}
	lease := token.lease
	if lease.holder != token {
		return false
	}
	existing := r.leases[newSessionID]
	if existing != nil && existing != lease && !existing.idle() {
		holderKey, holderGen := holderDesc(existing.holder)
		turnLeaseLog().Warn(fmt.Sprintf("turn lease rebind blocked: session %s rotated to %s mid-turn (holder: routing key "+
			"%s gen %d) but the target session's lease is already live (holder: routing key %v "+
			"gen %v) — keeping the lease on the old id; transcript writes on %s may "+
			"interleave (#64934 rotation-alias edge)", token.sessionID, newSessionID, token.ownerKey,
			token.generation, holderKey, holderGen, newSessionID))
		return false
	}
	r.leases[newSessionID] = lease
	lease.lastUsed = epochNow()
	token.sessionID = newSessionID
	return true
}

// Releases the lease of token, true only when this exact token was the current holder
func (r *SessionTurnLeaseRegistry) Release(token *TurnLeaseToken) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if token == nil || token.released {
		return false
	}
	token.released = true
	lease := token.lease
	if lease.holder != token {
		turnLeaseLog().Debug(fmt.Sprintf("turn lease release skipped on session %s: token (key %s gen %d) is not "+
			"the current holder", token.sessionID, token.ownerKey, token.generation))
		return false
	}
	lease.holder, lease.acquiredAt, lease.lastUsed = nil, 0, epochNow()
	if lease.locked() {
		<-lease.lock
	}
	return true
}
