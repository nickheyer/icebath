package gateway

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"math/big"
	"slices"
	"strings"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pythread"
	"github.com/nickheyer/icebath/pkg/state"
)

// Persisted history that cannot be read safely for the session
type TranscriptReadError struct {
	SessionID string
}

func (e *TranscriptReadError) Error() string {
	return "transcript read failed for session " + e.SessionID
}

// Latest compression continuation of the session, the id itself without a store or on a failed lookup
func (s *SessionStore) compressionTipForSessionID(ctx context.Context, sessionID string) string {
	if sessionID == "" {
		return sessionID
	}
	db := s.dbForSessionID(ctx, sessionID)
	if db == nil {
		return sessionID
	}
	tip, err := db.GetCompressionTip(ctx, sessionID)
	if err != nil {
		slog.Debug(fmt.Sprintf("Compression-tip lookup failed for session %s: %v", sessionID, err))
		return sessionID
	}
	if tip == "" {
		return sessionID
	}
	return tip
}

// Rewrites the entry onto its compression continuation when the mapping is stale, lock held
func (s *SessionStore) healCompressionTipLocked(entry *SessionEntry, originalSessionID, canonicalSessionID string) bool {
	if originalSessionID == "" || canonicalSessionID == "" {
		return false
	}
	if entry.SessionID != originalSessionID || canonicalSessionID == originalSessionID {
		return false
	}
	slog.Info(fmt.Sprintf("SessionStore healed compressed session mapping: %s -> %s", entry.SessionID, canonicalSessionID))
	entry.SessionID = canonicalSessionID
	return true
}

// Compare and swap advance of one route along a verified compression lineage, nil when the route moved
func (s *SessionStore) AdvanceCompressionSession(ctx context.Context, sessionKey, expectedSessionID, targetSessionID string) (*SessionEntry, error) {
	if sessionKey == "" || expectedSessionID == "" || targetSessionID == "" {
		return nil, nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	entry := s.entryLocked(ctx, sessionKey)
	if entry == nil {
		return nil, nil
	}
	if entry.SessionID == targetSessionID {
		return entry, nil
	}
	if !s.healCompressionTipLocked(entry, expectedSessionID, targetSessionID) {
		return nil, nil
	}
	if err := s.save(ctx); err != nil {
		return nil, err
	}
	return entry, nil
}

// Follows the compression reroute chain guarding against cycles
func (s *SessionStore) followReroutes(sessionID string) string {
	s.reroutesMu.Lock()
	defer s.reroutesMu.Unlock()
	seen := map[string]bool{}
	for {
		next, ok := s.transcriptReroutes[sessionID]
		if !ok || seen[sessionID] {
			return sessionID
		}
		seen[sessionID] = true
		sessionID = next
	}
}

// Every message of the session's canonical transcript, a failed read raising TranscriptReadError
func (s *SessionStore) LoadTranscript(ctx context.Context, sessionID string) ([]any, error) {
	if s.dbForSessionID(ctx, sessionID) == nil {
		return []any{}, nil
	}
	sessionID = s.followReroutes(sessionID)
	if db := s.dbForSessionID(ctx, sessionID); db != nil {
		if tip, err := db.GetCompressionTip(ctx, sessionID); err == nil && tip != "" {
			sessionID = tip
		}
	}
	var (
		messages []map[string]any
		err      error
	)
	if db := s.dbForSessionID(ctx, sessionID); db != nil {
		messages, err = db.GetMessagesAsConversation(ctx, sessionID, state.ConversationOptions{RepairAlternation: true})
	} else {
		err = fmt.Errorf("'NoneType' object has no attribute 'get_messages_as_conversation'")
	}
	if err != nil {
		slog.Error(fmt.Sprintf("Transcript read failed for session %s; refusing to treat the conversation as empty: %v", sessionID, err))
		return nil, &TranscriptReadError{SessionID: sessionID}
	}
	out := make([]any, len(messages))
	for i, m := range messages {
		out[i] = m
	}
	return out, nil
}

// Message keys persisted only on assistant rows
var transcriptAssistantOnlyKeys = []string{"reasoning", "reasoning_content", "reasoning_details", "codex_reasoning_items", "codex_message_items"}

// Message keys bound straight to a TEXT column
var transcriptBoundTextKeys = []string{"role", "tool_name", "tool_call_id", "reasoning", "reasoning_content", "platform_message_id"}

// Value sqlite binds as is, else its JSON text, else its str
func bindableText(value any) any {
	switch v := value.(type) {
	case nil, string, float64, float32, bool, int, int8, int16, int32, int64, uint8, uint16, uint32:
		return v
	case uint:
		if uint64(v) < 1<<63 {
			return v
		}
	case uint64:
		if v < 1<<63 {
			return v
		}
	case *big.Int:
		if v.IsInt64() {
			return v
		}
	}
	text, err := pyjson.Dumps(value, pyjson.Default(func(o any) (any, error) { return pyrepr.Str(o), nil }))
	if err != nil {
		return pyrepr.Str(value)
	}
	return text
}

// Append message fields of one transcript row, shared by the live writer and the spool replay
func transcriptAppendKwargs(sessionID string, message map[string]any, fallbackTS any) map[string]any {
	isAssistant := message["role"] == "assistant"
	role, ok := message["role"]
	if !ok {
		role = "unknown"
	}
	timestamp := message["timestamp"]
	if timestamp == nil {
		timestamp = fallbackTS
	}
	platformMessageID := message["platform_message_id"]
	if !pyrepr.Truthy(platformMessageID) {
		platformMessageID = message["message_id"]
	}
	var apiContent any
	if v, ok := message["api_content"].(string); ok {
		apiContent = v
	}
	kwargs := map[string]any{"session_id": sessionID, "role": role, "content": message["content"],
		"tool_name": message["tool_name"], "tool_calls": message["tool_calls"], "tool_call_id": message["tool_call_id"],
		"platform_message_id": platformMessageID, "observed": pyrepr.Truthy(message["observed"]),
		"timestamp": timestamp, "api_content": apiContent, "display_kind": message["display_kind"],
		"display_metadata": message["display_metadata"]}
	for _, key := range transcriptAssistantOnlyKeys {
		if isAssistant {
			kwargs[key] = message[key]
		} else {
			kwargs[key] = nil
		}
	}
	for _, key := range transcriptBoundTextKeys {
		kwargs[key] = bindableText(kwargs[key])
	}
	return kwargs
}

// Marks sessions whose next transcript write drains the on disk spool first
func (s *SessionStore) markSpooledDropSessions(sessionIDs map[string]bool) {
	if len(sessionIDs) == 0 {
		return
	}
	s.transcriptRetryMu.Lock()
	defer s.transcriptRetryMu.Unlock()
	for sid := range sessionIDs {
		s.spooledDropSessions[sid] = true
	}
}

// In memory pending messages per session while the database is broken, a var tests shrink
var transcriptMaxPendingPerSession = 200

// Seconds between FTS rebuild attempts so one failure never disables recovery for good
var transcriptFTSRebuildCooldownSeconds = 300.0

// Consecutive append failures of one session before the log escalates to ERROR and the backlog spools
var transcriptAppendFailureEscalationThreshold = 3

// Monotonic seconds the FTS rebuild cooldown measures, a var tests freeze
var transcriptMonotonic = pythread.Monotonic

// One queued transcript message whose pointer is its identity in the retry queue
type queuedTranscript struct{ message map[string]any }

// Per session retry queue shared by reference between the store map and the draining writer
type transcriptQueue struct{ items []*queuedTranscript }

// Store that writes one transcript row
type transcriptRowAppender interface {
	AppendMessage(ctx context.Context, sessionID string, fields map[string]any, guards state.AppendGuards) (int64, error)
}

// Store that rebuilds its FTS indexes, returning how many were rebuilt
type transcriptFTSRebuilder interface {
	RebuildFTS(ctx context.Context) (int, error)
}

// Store that names the foreign processes holding it or its WAL sidecars
type transcriptForeignHolders interface {
	ForeignStateDBHolders() ([]state.Holder, error)
}

// Store answering the transcript existence and tail probes
type transcriptProbeDB interface {
	HasPlatformMessageID(ctx context.Context, sessionID, platformMessageID string) (bool, error)
	LatestConversationRole(ctx context.Context, sessionID string) (string, error)
	HasGatewayInputOwner(ctx context.Context, sessionID, owner string) (bool, error)
}

// Store replacing a transcript and proving published compression children
type transcriptRewriteDB interface {
	ReplaceMessages(ctx context.Context, sessionID string, messages []map[string]any, opts state.ReplaceOptions) error
	IsCompressionChildRow(ctx context.Context, child *pyjson.Object) (bool, error)
	GetActiveMessageWatermark(ctx context.Context, sessionID string) (int64, error)
}

// AttributeError text hermes raises when a store lacks a method
func missingStoreMethod(db any, method string) error {
	return fmt.Errorf("'%T' object has no attribute '%s'", db, method)
}

// Spools one evicted or undeliverable message for replay after recovery, empty when the spool failed
func spoolDropped(ctx context.Context, sessionID string, message map[string]any) string {
	return SpoolDroppedTranscriptMessage(ctx, sessionID, message)
}

// Appends a message through the reroute chain under the drain lock, a skipped DB write doing nothing
func (s *SessionStore) AppendToTranscript(ctx context.Context, sessionID string, message map[string]any, skipDB bool) error {
	if skipDB {
		return nil
	}
	s.transcriptDrainMu.Lock()
	defer s.transcriptDrainMu.Unlock()
	return s.appendToTranscriptSerialized(ctx, s.followReroutes(sessionID), message)
}

// Queues a copy of the message, evicting and spooling the oldest past the cap, retry lock held
func (s *SessionStore) enqueueTranscriptMessage(ctx context.Context, sessionID string, message map[string]any) *transcriptQueue {
	pending := s.dirtyTranscripts[sessionID]
	if pending == nil {
		pending = &transcriptQueue{}
		s.dirtyTranscripts[sessionID] = pending
	}
	pending.items = append(pending.items, &queuedTranscript{message: maps.Clone(message)})
	if len(pending.items) > transcriptMaxPendingPerSession {
		oldest := pending.items[0]
		pending.items = pending.items[1:]
		if spoolPath := spoolDropped(ctx, sessionID, oldest.message); spoolPath != "" {
			s.spooledDropSessions[sessionID] = true
			slog.Warn(fmt.Sprintf("Session DB transcript pending queue full for %s (cap=%d); spooled oldest "+
				"message to %s for replay after DB recovery", sessionID, transcriptMaxPendingPerSession, spoolPath))
		} else {
			slog.Warn(fmt.Sprintf("Session DB transcript pending queue full for %s (cap=%d); dropping oldest "+
				"message to make room (on-disk spool unavailable)", sessionID, transcriptMaxPendingPerSession))
		}
	}
	return pending
}

// Python class name of a replaced or quarantined handle refusal
func stateRefusalName(err error) string {
	var replaced *state.StateDBReplacedError
	if errors.As(err, &replaced) {
		return "StateDbReplacedError"
	}
	return "StateDbCorruptError"
}

// Stops SQLite writes on a refused handle and diverts the backlog to the spool and the JSONL fallback
func (s *SessionStore) divertTranscriptAfterDBReplaced(ctx context.Context, sessionID, queueSessionID string, exc error) {
	slog.Error(fmt.Sprintf("Session DB refused further writes on this handle for %s (%s); stopping SQLite writes "+
		"and diverting pending transcripts to the on-disk fallback: %v", sessionID, stateRefusalName(exc), exc))
	s.transcriptRetryMu.Lock()
	remaining := []any{}
	if q := s.dirtyTranscripts[queueSessionID]; q != nil {
		for _, item := range q.items {
			remaining = append(remaining, item.message)
		}
	}
	delete(s.dirtyTranscripts, queueSessionID)
	delete(s.transcriptAppendFailures, sessionID)
	s.transcriptRetryMu.Unlock()
	for _, dropped := range remaining {
		if spoolDropped(ctx, sessionID, dropped.(map[string]any)) == "" {
			slog.Warn(fmt.Sprintf("pending fallback failed for replaced state.db transcript on %s", sessionID))
		}
	}
	if _, err := state.DivertSessionTranscriptJSONL(ctx, sessionID, remaining); err != nil {
		slog.Warn(fmt.Sprintf("JSONL divert failed for replaced state.db transcript on %s: %v", sessionID, err))
	}
}

// Transitive compression tip of the session when it is a different still live row, else empty
func (s *SessionStore) liveCompressionChild(ctx context.Context, sessionID string) (string, error) {
	ownerDB := s.dbForSessionID(ctx, sessionID)
	if ownerDB == nil {
		return "", nil
	}
	tip, err := ownerDB.GetCompressionTip(ctx, sessionID)
	if err != nil || tip == "" || tip == sessionID {
		return "", err
	}
	tipRow, err := ownerDB.GetSession(ctx, tip)
	if err != nil || tipRow == nil {
		return "", err
	}
	if endedAt, _ := tipRow.Get("ended_at"); endedAt == nil {
		return tip, nil
	}
	return "", nil
}

// Moves the retry queue and failure count from parent to child and records the reroute, retry lock held
func (s *SessionStore) migrateTranscriptQueueToChild(sessionID, queueSessionID, childID string, pending *transcriptQueue, msg *queuedTranscript) *transcriptQueue {
	if len(pending.items) > 0 && pending.items[0] == msg {
		pending.items = pending.items[1:]
	}
	existing := s.dirtyTranscripts[childID]
	if len(pending.items) > 0 {
		if existing != nil {
			pending.items = append(pending.items, existing.items...)
		}
		s.dirtyTranscripts[childID] = pending
	} else if existing != nil && len(existing.items) > 0 {
		pending = existing
	}
	delete(s.dirtyTranscripts, queueSessionID)
	previous := s.transcriptAppendFailures[queueSessionID]
	delete(s.transcriptAppendFailures, queueSessionID)
	if previous != 0 {
		s.transcriptAppendFailures[childID] = max(previous, s.transcriptAppendFailures[childID])
	}
	s.reroutesMu.Lock()
	s.transcriptReroutes[sessionID] = childID
	s.reroutesMu.Unlock()
	return pending
}

// Repoints every route at the compression child and saves, dropping the child's owner hint
func (s *SessionStore) publishTranscriptReroute(ctx context.Context, sessionID, childID string) error {
	s.mu.Lock()
	for _, entry := range s.entries.values() {
		if entry.SessionID == sessionID {
			entry.SessionID = childID
		}
	}
	err := s.save(ctx)
	delete(s.sessionOwnerHints, childID)
	s.mu.Unlock()
	return err
}

// Appends a message after the session's retry queue and spooled backlog, rerouting, repairing or escalating failures
func (s *SessionStore) appendToTranscriptSerialized(ctx context.Context, sessionID string, message map[string]any) error {
	s.transcriptRetryMu.Lock()
	pending := s.enqueueTranscriptMessage(ctx, sessionID, message)
	msg := pending.items[0]
	s.transcriptRetryMu.Unlock()
	queueSessionID := sessionID
	ackHead := func() bool {
		if len(pending.items) > 0 && pending.items[0] == msg {
			pending.items = pending.items[1:]
		}
		if len(pending.items) == 0 {
			delete(s.dirtyTranscripts, queueSessionID)
			delete(s.transcriptAppendFailures, sessionID)
			return true
		}
		return false
	}
	for {
		spoolErr := s.drainSpooledDrops(ctx, sessionID)
		exc := spoolErr
		if exc == nil {
			exc = s.appendTranscriptMessage(ctx, sessionID, msg.message)
		}
		if exc == nil {
			s.transcriptRetryMu.Lock()
			queueEmpty := ackHead()
			if !queueEmpty {
				msg = pending.items[0]
			}
			s.transcriptRetryMu.Unlock()
			if queueEmpty {
				return nil
			}
			continue
		}
		var replaced *state.StateDBReplacedError
		var corrupt *state.StateDBCorruptError
		if errors.As(exc, &replaced) || errors.As(exc, &corrupt) {
			s.divertTranscriptAfterDBReplaced(ctx, sessionID, queueSessionID, exc)
			return nil
		}
		var closed *state.CompressionSessionClosedError
		if errors.As(exc, &closed) {
			ownerKey := s.ownerKeyForSessionID(sessionID)
			childID, err := s.liveCompressionChild(ctx, sessionID)
			if err != nil {
				return err
			}
			if childID == "" {
				s.transcriptRetryMu.Lock()
				ackHead()
				s.transcriptRetryMu.Unlock()
				slog.Error(fmt.Sprintf("Session DB transcript append rejected for compression-ended %s with "+
					"no unique live child; not retrying", sessionID))
				return nil
			}
			if ownerKey != "" {
				s.mu.Lock()
				s.sessionOwnerHints[childID] = ownerKey
				s.mu.Unlock()
			}
			if rerouteErr := s.appendTranscriptMessage(ctx, childID, msg.message); rerouteErr != nil {
				exc = rerouteErr
			} else {
				s.transcriptRetryMu.Lock()
				pending = s.migrateTranscriptQueueToChild(sessionID, queueSessionID, childID, pending, msg)
				queueSessionID = childID
				s.transcriptRetryMu.Unlock()
				if err := s.publishTranscriptReroute(ctx, sessionID, childID); err != nil {
					return err
				}
				if len(pending.items) == 0 {
					return nil
				}
				msg = pending.items[0]
				sessionID = childID
				continue
			}
		}
		if isFTSCorruptionError(exc) {
			rebuilt, err := s.rebuildFTSOnce(ctx)
			if err != nil {
				return err
			}
			if rebuilt {
				if spoolErr != nil {
					continue
				}
				if retryErr := s.appendTranscriptMessage(ctx, sessionID, msg.message); retryErr != nil {
					exc = retryErr
				} else {
					s.transcriptRetryMu.Lock()
					queueEmpty := ackHead()
					if !queueEmpty {
						msg = pending.items[0]
					}
					s.transcriptRetryMu.Unlock()
					if queueEmpty {
						return nil
					}
					continue
				}
			}
		}
		s.transcriptRetryMu.Lock()
		failures := s.transcriptAppendFailures[sessionID] + 1
		s.transcriptAppendFailures[sessionID] = failures
		s.transcriptRetryMu.Unlock()
		if failures >= transcriptAppendFailureEscalationThreshold {
			spooled := s.spoolStalledBacklog(ctx, sessionID, queueSessionID)
			slog.Error(fmt.Sprintf("Session DB transcript append failed for %s (failure_count=%d, "+
				"pending=%d, spooled_to_disk=%d); session is stalled and needs operator "+
				"attention: %v", sessionID, failures, s.queueLen(pending), spooled, exc))
		} else {
			slog.Warn(fmt.Sprintf("Session DB transcript append failed for %s (failure_count=%d, pending=%d); "+
				"will retry: %v", sessionID, failures, s.queueLen(pending), exc))
		}
		return nil
	}
}

// Length of a retry queue read under the retry lock
func (s *SessionStore) queueLen(q *transcriptQueue) int {
	s.transcriptRetryMu.Lock()
	defer s.transcriptRetryMu.Unlock()
	return len(q.items)
}

// Moves a stalled session's backlog oldest first to the on disk spool, stopping at the first failure
func (s *SessionStore) spoolStalledBacklog(ctx context.Context, sessionID, queueSessionID string) int {
	s.transcriptRetryMu.Lock()
	pending := s.dirtyTranscripts[queueSessionID]
	var backlog []*queuedTranscript
	if pending != nil {
		backlog = slices.Clone(pending.items)
	}
	s.transcriptRetryMu.Unlock()
	spooled := 0
	for _, item := range backlog {
		if spoolDropped(ctx, sessionID, item.message) == "" {
			break
		}
		spooled++
	}
	if spooled > 0 {
		s.transcriptRetryMu.Lock()
		s.spooledDropSessions[sessionID] = true
		if pending != nil {
			pending.items = pending.items[spooled:]
			if len(pending.items) == 0 {
				delete(s.dirtyTranscripts, queueSessionID)
			}
		}
		s.transcriptRetryMu.Unlock()
	}
	return spooled
}

// Replays the session's spooled drops first, returning the replay failure while some stay on disk
func (s *SessionStore) drainSpooledDrops(ctx context.Context, sessionID string) error {
	s.transcriptRetryMu.Lock()
	marked := s.spooledDropSessions[sessionID]
	knownFailing := s.transcriptAppendFailures[sessionID] != 0
	s.transcriptRetryMu.Unlock()
	if !marked {
		return nil
	}
	var failures []error
	replay := func(message map[string]any) error {
		if err := s.appendTranscriptMessage(ctx, sessionID, message); err != nil {
			failures = append(failures, err)
			return err
		}
		return nil
	}
	_, remaining, err := DrainTranscriptSpool(ctx, sessionID, replay, knownFailing)
	if err != nil {
		slog.Warn(fmt.Sprintf("Failed to drain transcript spool for %s: %v", sessionID, err))
		return nil
	}
	if remaining == 0 {
		s.transcriptRetryMu.Lock()
		delete(s.spooledDropSessions, sessionID)
		s.transcriptRetryMu.Unlock()
		return nil
	}
	if len(failures) > 0 {
		return failures[len(failures)-1]
	}
	return fmt.Errorf("older spooled transcript rows for %s still pending", sessionID)
}

// Writes one transcript row into the owning store, a profile without a home deferring the write
func (s *SessionStore) appendTranscriptMessage(ctx context.Context, sessionID string, message map[string]any) error {
	db := s.dbForSessionID(ctx, sessionID)
	if db == nil {
		return fmt.Errorf("no owning session store for %s; deferring transcript write", sessionID)
	}
	appender, ok := db.(transcriptRowAppender)
	if !ok {
		return missingStoreMethod(db, "append_message")
	}
	_, err := appender.AppendMessage(ctx, sessionID, transcriptAppendKwargs(sessionID, message, nil), state.AppendGuards{})
	return err
}

// True only when the failure provably belongs to the FTS index, never a bare structural SQLITE_CORRUPT
func isFTSCorruptionError(exc error) bool {
	if strings.Contains(strings.ToLower(exc.Error()), "messages_fts") {
		return true
	}
	class, ok := state.SQLiteClassOf(exc)
	return ok && class != state.SQLiteInterfaceError && state.IsFTSScopedCorruptionError(exc)
}

// Rebuilds the FTS indexes at most once per cooldown window, true when any index was rebuilt
func (s *SessionStore) rebuildFTSOnce(ctx context.Context) (bool, error) {
	now := transcriptMonotonic()
	if last := s.ftsRebuildLastAttemptAt; last != nil && now-*last < transcriptFTSRebuildCooldownSeconds {
		return false, nil
	}
	db := s.db(ctx)
	rebuilder, ok := db.(transcriptFTSRebuilder)
	if db == nil || !ok {
		return false, nil
	}
	s.ftsRebuildLastAttemptAt = &now
	if probe, ok := db.(transcriptForeignHolders); ok {
		holders, err := probe.ForeignStateDBHolders()
		if err != nil {
			return false, err
		}
		if len(holders) > 0 {
			slog.Warn(fmt.Sprintf("Skipping Session DB FTS rebuild while foreign processes hold the database or "+
				"WAL sidecars (%v); canonical transcript writes remain available.", holders))
			return false, nil
		}
	}
	rebuilt, err := rebuilder.RebuildFTS(ctx)
	if err != nil {
		slog.Warn(fmt.Sprintf("Session DB FTS rebuild failed: %v", err))
		return false, nil
	}
	if rebuilt != 0 {
		slog.Warn(fmt.Sprintf("Rebuilt %d Session DB FTS index(es) after append corruption", rebuilt))
	}
	return rebuilt > 0, nil
}

// Drops queued pending messages so a rewrite or rewind never re inserts them
func (s *SessionStore) clearDirtyTranscript(sessionID string) {
	s.transcriptRetryMu.Lock()
	defer s.transcriptRetryMu.Unlock()
	delete(s.dirtyTranscripts, sessionID)
	delete(s.transcriptAppendFailures, sessionID)
}

// Whether a message with the platform message id is persisted, false without a store or on a failed lookup
func (s *SessionStore) HasPlatformMessageID(ctx context.Context, sessionID, platformMessageID string) bool {
	db := s.dbForSessionID(ctx, sessionID)
	if db == nil {
		return false
	}
	found, err := func() (bool, error) {
		probe, ok := db.(transcriptProbeDB)
		if !ok {
			return false, missingStoreMethod(db, "has_platform_message_id")
		}
		return probe.HasPlatformMessageID(ctx, sessionID, platformMessageID)
	}()
	if err != nil {
		slog.Debug(fmt.Sprintf("has_platform_message_id lookup failed: %v", err))
		return false
	}
	return found
}

// Role of the newest live conversation row on the route reads follow, empty when none or unreadable
func (s *SessionStore) TranscriptTailRole(ctx context.Context, sessionID string) string {
	sessionID = s.compressionTipForSessionID(ctx, s.followReroutes(sessionID))
	db := s.dbForSessionID(ctx, sessionID)
	if db == nil {
		return ""
	}
	role, err := func() (string, error) {
		probe, ok := db.(transcriptProbeDB)
		if !ok {
			return "", missingStoreMethod(db, "latest_conversation_role")
		}
		return probe.LatestConversationRole(ctx, sessionID)
	}()
	if err != nil {
		slog.Debug(fmt.Sprintf("transcript tail lookup failed for %s: %v", sessionID, err))
		return ""
	}
	return role
}

// Replaces a session's transcript under the drain lock, true when it lands or no store exists
func (s *SessionStore) RewriteTranscript(ctx context.Context, sessionID string, messages []map[string]any, activeOnly, rejectActiveTurnLease bool) bool {
	db := s.dbForSessionID(ctx, sessionID)
	if db == nil {
		return true
	}
	s.transcriptDrainMu.Lock()
	defer s.transcriptDrainMu.Unlock()
	rewriter, ok := db.(transcriptRewriteDB)
	var err error
	if !ok {
		err = missingStoreMethod(db, "replace_messages")
	} else {
		err = rewriter.ReplaceMessages(ctx, sessionID, messages, state.ReplaceOptions{ActiveOnly: activeOnly,
			RejectActiveTurnLease: rejectActiveTurnLease})
	}
	if err != nil {
		slog.Debug(fmt.Sprintf("Failed to rewrite transcript in DB: %v", err))
		return false
	}
	s.clearDirtyTranscript(sessionID)
	return true
}

// Whether the child is an atomically published compression continuation of the parent, false on any error
func (s *SessionStore) IsPublishedCompressionChild(ctx context.Context, parentSessionID, childSessionID string) bool {
	if parentSessionID == "" || childSessionID == "" {
		return false
	}
	db := s.dbForSessionID(ctx, parentSessionID)
	if db == nil {
		return false
	}
	published, err := func() (bool, error) {
		child, err := db.GetSession(ctx, childSessionID)
		if err != nil || child == nil {
			return false, err
		}
		if parent, _ := child.Get("parent_session_id"); !pyrepr.Equal(parent, parentSessionID) {
			return false, nil
		}
		rewriter, ok := db.(transcriptRewriteDB)
		if !ok {
			return false, missingStoreMethod(db, "_is_compression_child_row")
		}
		if isChild, err := rewriter.IsCompressionChildRow(ctx, child); err != nil || !isChild {
			return false, err
		}
		watermark, err := rewriter.GetActiveMessageWatermark(ctx, childSessionID)
		return watermark > 0, err
	}()
	if err != nil {
		slog.Debug(fmt.Sprintf("published-compression-child probe failed for %s: %v", childSessionID, err))
		return false
	}
	return published
}

// Makes a rotated compression child durable, a published child already is, anything else gets rewritten
func (s *SessionStore) PersistRotatedCompressionChild(ctx context.Context, parentSessionID, childSessionID string, messages []map[string]any) bool {
	return s.IsPublishedCompressionChild(ctx, parentSessionID, childSessionID) ||
		s.RewriteTranscript(ctx, childSessionID, messages, false, false)
}

// Whether the accepted input owner appears on the live continuation or its compression ancestors
func (s *SessionStore) HasInputOwner(ctx context.Context, sessionID, owner string) (bool, error) {
	found, err := func() (bool, error) {
		current := s.followReroutes(sessionID)
		db := s.dbForSessionID(ctx, current)
		if db == nil {
			return false, missingStoreMethod(db, "get_compression_tip")
		}
		tip, err := db.GetCompressionTip(ctx, current)
		if err != nil {
			return false, err
		}
		if tip != "" {
			current = tip
		}
		probe, ok := db.(transcriptProbeDB)
		if !ok {
			return false, missingStoreMethod(db, "has_gateway_input_owner")
		}
		rewriter, ok := db.(transcriptRewriteDB)
		if !ok {
			return false, missingStoreMethod(db, "_is_compression_child_row")
		}
		seen := map[string]bool{}
		for current != "" && !seen[current] {
			seen[current] = true
			has, err := probe.HasGatewayInputOwner(ctx, current, owner)
			if err != nil || has {
				return has, err
			}
			row, err := db.GetSession(ctx, current)
			if err != nil {
				return false, err
			}
			if row == nil {
				break
			}
			isChild, err := rewriter.IsCompressionChildRow(ctx, row)
			if err != nil {
				return false, err
			}
			if !isChild {
				break
			}
			parent, ok := row.Get("parent_session_id")
			if !ok {
				return false, fmt.Errorf("KeyError: 'parent_session_id'")
			}
			current, _ = parent.(string)
		}
		return false, nil
	}()
	if err != nil {
		return false, &TranscriptReadError{SessionID: sessionID}
	}
	return found, nil
}

// Fields of hermes_state_rewind.py RewindOutcome the gateway reports after an undo
type rewindOutcome struct {
	RewoundCount int
	TurnsUndone  int
	LiveText     string
}

// Whether an error is a ValueError hermes lets propagate out of the retry guard
func isPyValueError(err error) bool {
	var typed interface{ PyTypeName() string }
	return errors.As(err, &typed) && typed.PyTypeName() == "ValueError"
}

// Store rewind of a user turn, the AttributeError hermes raises for a store without one
func rewindUserTurn(ctx context.Context, db SessionStoreDB, sessionID string, userOrdinal int, requireRetryable, requireComposite bool) (*rewindOutcome, error) {
	rewinder, ok := db.(interface {
		RewindUserTurn(context.Context, string, int, state.RewindUserTurnOptions) (*state.RewindOutcome, error)
	})
	if !ok {
		return nil, fmt.Errorf("AttributeError: '%T' object has no attribute 'rewind_user_turn'", db)
	}
	out, err := rewinder.RewindUserTurn(ctx, sessionID, userOrdinal,
		state.RewindUserTurnOptions{RequireRetryable: requireRetryable, RequireComposite: requireComposite})
	if err != nil {
		return nil, err
	}
	return &rewindOutcome{RewoundCount: out.RewoundCount, TurnsUndone: out.TurnsUndone, LiveText: out.LiveText}, nil
}

// Whether a rewind failed because its target is not a rewindable user turn
func isRewindTargetUnavailable(err error) bool {
	var unavailable *state.RewindTargetUnavailableError
	return errors.As(err, &unavailable)
}

// Backs up n user turns by soft delete, nil without a store or rewindable turn, a replay policy ValueError raised
func (s *SessionStore) RewindSession(ctx context.Context, sessionID string, n int, requireRetryableComposite bool) (map[string]any, error) {
	db := s.dbForSessionID(ctx, sessionID)
	if db == nil {
		return nil, nil
	}
	s.transcriptDrainMu.Lock()
	defer s.transcriptDrainMu.Unlock()
	outcome, err := rewindUserTurn(ctx, db, sessionID, -max(n, 1), requireRetryableComposite, requireRetryableComposite)
	switch {
	case err != nil && isRewindTargetUnavailable(err):
		slog.Debug(fmt.Sprintf("rewind_session: %v", err))
		return nil, nil
	case err != nil && isPyValueError(err):
		return nil, err
	case err != nil:
		slog.Debug(fmt.Sprintf("rewind_session: rewind failed: %v", err))
		return nil, nil
	}
	s.clearDirtyTranscript(sessionID)
	return map[string]any{"rewound_count": outcome.RewoundCount, "turns_undone": outcome.TurnsUndone,
		"target_text": outcome.LiveText}, nil
}
