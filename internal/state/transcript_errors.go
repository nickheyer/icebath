package state

import "errors"

// Quoted form of an identifier inside a refusal text
func quotedText(s string) string {
	var k keyText
	k.str(s)
	return k.b.String()
}

// Durable write that targeted a parent already closed by compression
type CompressionClosedError struct{ SessionID string }

// Session that rotated
func (e *CompressionClosedError) Error() string {
	return "Session " + quotedText(e.SessionID) + " is closed by compression; adopt its live continuation before appending messages"
}

// Bucket telling the caller to adopt the rotated session id
func (e *CompressionClosedError) PersistenceCause() Cause { return CauseCompressionClosed }

// Write refused because compression owns the session, a live lock being worth a short wait
type CompressionBusyError struct {
	msg string
	// The refusal came from a live lock the write loop waits out
	Live bool
}

// Refusal text
func (e *CompressionBusyError) Error() string { return e.msg }

// Matches the short wait sentinel while the lock is live
func (e *CompressionBusyError) Is(target error) bool {
	return e.Live && target == ErrCompressionInProgress
}

// Bucket of a compression lease refusing the write
func (e *CompressionBusyError) PersistenceCause() Cause { return CauseCompression }

// Transcript write that presented a turn lease it no longer owns, or met one it does not own
type TurnLeaseLostError struct{ msg string }

// Refusal text
func (e *TurnLeaseLostError) Error() string { return e.msg }

// Bucket of turn fencing, never a storage failure
func (e *TurnLeaseLostError) PersistenceCause() Cause { return CauseTurnLease }

// Commit that found a held row rewritten in place by another writer before any row was archived
type HeldRowRewrittenError struct{ SessionID string }

// Session whose held row changed
func (e *HeldRowRewrittenError) Error() string {
	return "a held row of session " + e.SessionID + " was rewritten in place before the commit"
}

// In-place prune that could not name the exact live row of a changed message, raised before any write
type PruneRowError struct {
	msg string
	// Another writer already rewrote the named row, so the full writer must not run either
	Stale bool
}

// Refusal text
func (e *PruneRowError) Error() string { return e.msg }

// Rewrite refused because the active transcript changed after it was computed
var ErrTranscriptChanged = errors.New("active transcript changed before the rewrite could be persisted")

// Row vanished between the steps of one transaction that owns it
var ErrSessionRowGone = errors.New("session row is gone")
