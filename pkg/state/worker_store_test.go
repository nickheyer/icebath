package state

import (
	"context"
	"errors"
	"os"
	"testing"

	"github.com/nickheyer/icebath/pkg/internal/hermetic"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
)

// Installs the wiring doubles the package tests reach before any test runs
func TestMain(m *testing.M) {
	hermetic.BlockExternalHTTP()
	hermetic.BlockExternalDNS()
	if runStateTestHelper() {
		return
	}
	installAutomationCodecDoubles()
	installRouteBuilderDoubles()
	os.Exit(m.Run())
}

// Refusal the worker facade raises before or after a receipt
type workerPersistenceError struct{ msg string }

func (e *workerPersistenceError) Error() string { return e.msg }

type workerJournalEntry struct {
	sequence  int64
	operation string
	payload   *pyjson.Object
}

// Test double of RuntimeSessionStore keeping its journal in memory and sending receipts through rpc
type workerStore struct {
	t            *testing.T
	db           *SessionDB
	scope        WorkerReceipt
	nextSequence int64
	pending      []workerJournalEntry
	failure      string
	rpc          func(in WorkerReceipt) (*pyjson.Object, error)
}

// Store over db for the registered worker scope, its rpc applying receipts directly
func newWorkerStore(t *testing.T, db *SessionDB, scope WorkerReceipt) *workerStore {
	t.Helper()
	s := &workerStore{t: t, db: db, scope: scope, nextSequence: 1}
	s.rpc = func(in WorkerReceipt) (*pyjson.Object, error) {
		return db.MutateWorkerExecution(context.Background(), in)
	}
	return s
}

// Registered compute worker execution on session owned beside a foreign session, the hermes worker fixture
func workerFixture(t *testing.T) (*SessionDB, *workerStore) {
	t.Helper()
	ctx := context.Background()
	db := mustSessionDB(t, t.TempDir()+"/state.db", false)
	for _, n := range []NewSession{{ID: "owned", Source: "cli", SystemPrompt: "prefix"}, {ID: "foreign", Source: "cli", SystemPrompt: "secret"}} {
		if err := db.CreateSession(n); err != nil {
			t.Fatal(err)
		}
	}
	epoch, err := db.BeginRuntimeEpoch(ctx, "fixture")
	if err != nil {
		t.Fatal(err)
	}
	scope := WorkerReceipt{Epoch: epoch, ExecutionID: "worker", SessionID: "owned", Generation: 0}
	if _, err := db.RegisterWorkerExecution(ctx, WorkerRegistration{Epoch: epoch, ExecutionID: "worker", SessionID: "owned", Generation: 0, Kind: "compute", AdoptionSecret: "secret"}); err != nil {
		t.Fatal(err)
	}
	return db, newWorkerStore(t, db, scope)
}

// Value after a JSON round trip, the shape every receipt result and journaled payload takes
func jsonRoundTrip(value any) any {
	text, err := pyDumps(value, pyJSON{ensureASCII: true, itemSep: ", ", keySep: ": "})
	if err != nil {
		panic(err)
	}
	decoded, err := pyjson.Loads(text)
	if err != nil {
		panic(err)
	}
	return decoded
}

// Payload as the facade journals it, a JSON round trip of the caller's value
func journalPayload(payload any) *pyjson.Object {
	return jsonRoundTrip(payload).(*pyjson.Object)
}

// Refuses any session other than the assignment
func (s *workerStore) session(sessionID string) error {
	if sessionID != s.scope.SessionID {
		return &workerPersistenceError{"permission_denied"}
	}
	return nil
}

// Journals one operation and sends it, the result of this entry
func (s *workerStore) apply(operation string, payload any) (*pyjson.Object, error) {
	if s.failure != "" {
		return nil, &workerPersistenceError{s.failure}
	}
	if len(s.pending) > 0 {
		return nil, &workerPersistenceError{"pending_receipt"}
	}
	s.pending = append(s.pending, workerJournalEntry{sequence: s.nextSequence, operation: operation, payload: journalPayload(payload)})
	s.nextSequence++
	results, err := s.retryPending()
	if err != nil {
		return nil, err
	}
	return results[0], nil
}

// Sends every pending entry in order, a failure staying sticky until the next retry
func (s *workerStore) retryPending() ([]*pyjson.Object, error) {
	var results []*pyjson.Object
	for len(s.pending) > 0 {
		entry := s.pending[0]
		in := s.scope
		in.Sequence, in.Operation, in.Payload = entry.sequence, entry.operation, entry.payload
		result, err := s.rpc(in)
		if err != nil {
			s.failure = err.Error()
			return results, err
		}
		if assignment, has := result.Get("worker_assignment"); has && pyTruthy(assignment) {
			parent, _ := assignment.(*pyjson.Object).Get("parent")
			if !pyjson.Equal(parent, s.scope.SessionID) {
				s.failure = "outbox_scope_mismatch"
				return results, &workerPersistenceError{"outbox_scope_mismatch"}
			}
			child, _ := assignment.(*pyjson.Object).Get("session_id")
			s.scope.SessionID = child.(string)
		}
		s.pending = s.pending[1:]
		results = append(results, result)
	}
	s.failure = ""
	return results, nil
}

// Applies the owner's row stamps onto the caller's message maps, nil popping the key
func applyRowAnnotations(messages []map[string]any, annotations any) {
	for i, raw := range annotations.([]any) {
		annotation := raw.(*pyjson.Object)
		for _, key := range annotation.Keys {
			if value := annotation.Vals[key]; value == nil {
				delete(messages[i], key)
			} else {
				messages[i][key] = value
			}
		}
	}
}

// Messages projected onto the closed handoff fields
func handoffRows(messages []map[string]any) []any {
	out := make([]any, 0, len(messages))
	for _, m := range messages {
		row := map[string]any{}
		for k, v := range m {
			if HandoffMessageFields[k] {
				row[k] = v
			}
		}
		out = append(out, row)
	}
	return out
}

// Value of a result key the test expects to exist
func resultValue(result *pyjson.Object, key string) any {
	v, _ := result.Get(key)
	return v
}

type appendOptions struct {
	compressionLockHolder *string
	turnLeaseHolder       *string
	turnLeaseTTLSeconds   float64
}

func (s *workerStore) appendMessagesBatch(sessionID string, messages []map[string]any) (int64, error) {
	return s.appendMessagesBatchWith(sessionID, messages, appendOptions{turnLeaseTTLSeconds: 300})
}

func (s *workerStore) appendMessagesBatchWith(sessionID string, messages []map[string]any, o appendOptions) (int64, error) {
	if err := s.session(sessionID); err != nil {
		return 0, err
	}
	rows := make([]any, 0, len(messages))
	for _, m := range messages {
		rows = append(rows, m)
	}
	var result *pyjson.Object
	var err error
	if o.compressionLockHolder != nil || o.turnLeaseTTLSeconds != 300 {
		result, err = s.apply("compression.append", map[string]any{"messages": rows, "compression_lock_holder": optAny(o.compressionLockHolder),
			"turn_lease_holder": optAny(o.turnLeaseHolder), "turn_lease_ttl_seconds": o.turnLeaseTTLSeconds})
	} else {
		result, err = s.apply("transcript.append", map[string]any{"messages": rows, "turn_lease_holder": optAny(o.turnLeaseHolder)})
	}
	if err != nil {
		return 0, err
	}
	applyRowAnnotations(messages, resultValue(result, "annotations"))
	count, _ := exactInt(resultValue(result, "count"))
	return count, nil
}

// Interface value of an optional string
func optAny(p *string) any {
	if p == nil {
		return nil
	}
	return *p
}

func (s *workerStore) tryAcquireSessionTurnLease(sessionID, holder string) (bool, error) {
	if err := s.session(sessionID); err != nil {
		return false, err
	}
	result, err := s.apply("turn.acquire", map[string]any{"holder": holder, "ttl_seconds": 300.0})
	if err != nil {
		return false, err
	}
	return pyTruthy(resultValue(result, "value")), nil
}

func (s *workerStore) releaseSessionTurnLease(sessionID, holder string) error {
	_, err := s.apply("turn.cleanup", map[string]any{"target": sessionID, "holder": holder})
	return err
}

func (s *workerStore) queueTokenCounts(sessionID string, usage map[string]any) error {
	if err := s.session(sessionID); err != nil {
		return err
	}
	_, err := s.apply("usage.main", usage)
	return err
}

func (s *workerStore) getSession(sessionID string) (*pyjson.Object, error) {
	if sessionID != s.scope.SessionID {
		lineage, err := s.apply("compression.lineage", map[string]any{"target": s.scope.SessionID})
		if err != nil {
			return nil, err
		}
		readable := false
		for _, id := range resultValue(lineage, "readable_ids").([]any) {
			if id == sessionID {
				readable = true
			}
		}
		if !readable {
			return nil, &workerPersistenceError{"permission_denied"}
		}
	}
	result, err := s.apply("compression.context", map[string]any{"target": sessionID})
	if err != nil {
		return nil, err
	}
	return resultValue(result, "session").(*pyjson.Object), nil
}

func (s *workerStore) lineageField(sessionID, field string) (any, error) {
	result, err := s.apply("compression.lineage", map[string]any{"target": sessionID})
	if err != nil {
		return nil, err
	}
	return resultValue(result, field), nil
}

func (s *workerStore) latestConversationBoundary(sessionKey, source string) (any, error) {
	result, err := s.apply("compression.boundary", map[string]any{"session_key": sessionKey, "source": source})
	if err != nil {
		return nil, err
	}
	return resultValue(result, "value"), nil
}

func (s *workerStore) getMessagesAsConversation(sessionID string, o ConversationOptions) ([]any, error) {
	result, err := s.apply("compression.history", map[string]any{"target": sessionID, "include_ancestors": o.IncludeAncestors,
		"include_inactive": o.IncludeInactive, "repair_alternation": o.RepairAlternation, "include_row_ids": o.IncludeRowIDs,
		"include_compacted": o.IncludeCompacted})
	if err != nil {
		return nil, err
	}
	return resultValue(result, "messages").([]any), nil
}

func (s *workerStore) getActiveMessageWatermark(sessionID string) (any, error) {
	if err := s.session(sessionID); err != nil {
		return nil, err
	}
	result, err := s.apply("compression.watermark", map[string]any{})
	if err != nil {
		return nil, err
	}
	return resultValue(result, "value"), nil
}

type archiveOptions struct {
	modelConfigPatch any
	watermark        any
	lockHolder       any
	tailCount        int64
	carriedMessages  []map[string]any
	coveredIDs       any
	unresolvedHeld   []map[string]any
	heldForCAS       []map[string]any
}

func (s *workerStore) archiveAndCompact(sessionID string, compacted []map[string]any, o archiveOptions) (any, error) {
	if err := s.session(sessionID); err != nil {
		return nil, err
	}
	carried := []any{}
	for _, m := range o.carriedMessages {
		carried = append(carried, m)
	}
	var unresolved any
	if o.unresolvedHeld != nil {
		list := []any{}
		for _, m := range o.unresolvedHeld {
			list = append(list, m)
		}
		unresolved = list
	}
	var cas any
	if o.heldForCAS != nil {
		list := []any{}
		for _, m := range o.heldForCAS {
			row := map[string]any{}
			for k, v := range m {
				if HandoffMessageFields[k] || k == "_db_persisted" {
					row[k] = v
				}
			}
			list = append(list, row)
		}
		cas = list
	}
	result, err := s.apply("compression.archive", map[string]any{"messages": handoffRows(compacted), "model_config_patch": o.modelConfigPatch,
		"watermark": o.watermark, "lock_holder": o.lockHolder, "tail_count": o.tailCount, "carried_messages": carried,
		"covered_ids": o.coveredIDs, "unresolved_held": unresolved, "held_for_cas": cas})
	if err != nil {
		return nil, err
	}
	applyRowAnnotations(compacted, resultValue(result, "annotations"))
	return resultValue(result, "value"), nil
}

type publishOptions struct {
	model, systemPrompt, cwd, profileName any
	modelConfig                           any
	compressionLockHolder                 any
	requireLeaseRefresh                   bool
	watermark, watermarkCeiling           any
}

func (s *workerStore) publishCompressionChild(parent, child, source string, messages []map[string]any, o publishOptions) error {
	if err := s.session(parent); err != nil {
		return err
	}
	result, err := s.apply("compression.publish", map[string]any{"child_session_id": child, "source": source, "messages": handoffRows(messages),
		"model": o.model, "model_config": o.modelConfig, "system_prompt": o.systemPrompt, "cwd": o.cwd, "profile_name": o.profileName,
		"compression_lock_holder": o.compressionLockHolder, "require_compression_lease": true, "require_lease_refresh": o.requireLeaseRefresh,
		"lease_ttl_seconds": 300.0, "watermark": o.watermark, "watermark_ceiling": o.watermarkCeiling})
	if err != nil {
		return err
	}
	applyRowAnnotations(messages, resultValue(result, "annotations"))
	return nil
}

func (s *workerStore) compressionLock(action, sessionID string, payload map[string]any) (any, error) {
	if err := s.session(sessionID); err != nil {
		return nil, err
	}
	result, err := s.apply("compression.lock."+action, payload)
	if err != nil {
		return nil, err
	}
	return resultValue(result, "value"), nil
}

func (s *workerStore) tryAcquireCompressionLock(sessionID, holder string) (bool, error) {
	v, err := s.compressionLock("acquire", sessionID, map[string]any{"holder": holder, "ttl_seconds": 300.0})
	return pyTruthy(v), err
}

func (s *workerStore) refreshCompressionLock(sessionID, holder string) (bool, error) {
	v, err := s.compressionLock("renew", sessionID, map[string]any{"holder": holder, "ttl_seconds": 300.0})
	return pyTruthy(v), err
}

func (s *workerStore) getCompressionLockHolder(sessionID string) (any, error) {
	return s.compressionLock("holder", sessionID, map[string]any{})
}

func (s *workerStore) releaseCompressionLock(sessionID, holder string) error {
	_, err := s.apply("compression.cleanup", map[string]any{"target": sessionID, "holder": holder})
	return err
}

func (s *workerStore) reopenOrphanedCompressionSession(sessionID string) (bool, error) {
	if err := s.session(sessionID); err != nil {
		return false, err
	}
	result, err := s.apply("compression.reopen", map[string]any{})
	if err != nil {
		return false, err
	}
	return pyTruthy(resultValue(result, "value")), nil
}

func (s *workerStore) recordCompressionFailureCooldown(sessionID string, cooldownUntil float64, errValue any) error {
	if err := s.session(sessionID); err != nil {
		return err
	}
	_, err := s.apply("compression.cooldown.record", map[string]any{"cooldown_until": cooldownUntil, "error": errValue})
	return err
}

func (s *workerStore) restoreCompressionFailureCooldownRow(sessionID string, snapshot any) error {
	if err := s.session(sessionID); err != nil {
		return err
	}
	_, err := s.apply("compression.cooldown.restore", map[string]any{"snapshot": snapshot})
	return err
}

func (s *workerStore) clearCompressionFailureCooldown(sessionID string) error {
	if err := s.session(sessionID); err != nil {
		return err
	}
	_, err := s.apply("compression.cooldown.clear", map[string]any{})
	return err
}

func (s *workerStore) setCompressionCounter(name, sessionID string, value any) error {
	if err := s.session(sessionID); err != nil {
		return err
	}
	_, err := s.apply("compression."+name, map[string]any{"value": value})
	return err
}

// Cooldown row read through the session row as the facade reads it
func (s *workerStore) getCompressionFailureCooldownRow(sessionID string) (*pyjson.Object, error) {
	row, err := s.getSession(sessionID)
	if err != nil {
		return nil, err
	}
	return pyjson.ObjectOf("session_exists", true, "cooldown_until", rowValue(row, "compression_failure_cooldown_until"),
		"error", rowValue(row, "compression_failure_error")), nil
}

func (s *workerStore) getCompressionFailureCooldown(sessionID string) (*pyjson.Object, error) {
	row, err := s.getCompressionFailureCooldownRow(sessionID)
	if err != nil {
		return nil, err
	}
	deadline := rowValue(row, "cooldown_until")
	if deadline == nil {
		return nil, nil
	}
	f, err := pyFloatOf(deadline)
	if err != nil {
		return nil, err
	}
	t := now()
	if f <= t {
		return nil, nil
	}
	return pyjson.ObjectOf("cooldown_until", f, "remaining_seconds", f-t, "error", rowValue(row, "error")), nil
}

func (s *workerStore) getSessionModelConfigValue(sessionID, key string) (any, error) {
	row, err := s.getSession(sessionID)
	if err != nil {
		return nil, err
	}
	value, _ := parseModelConfigValue(rowValue(row, "model_config")).Get(key)
	return value, nil
}

func (s *workerStore) getSessionTitle(sessionID string) (any, error) {
	row, err := s.getSession(sessionID)
	if err != nil {
		return nil, err
	}
	return rowValue(row, "title"), nil
}

func (s *workerStore) updateSystemPrompt(sessionID string, prompt any) error {
	if err := s.session(sessionID); err != nil {
		return err
	}
	_, err := s.apply("session.prompt", map[string]any{"system_prompt": prompt})
	return err
}

func (s *workerStore) patchSessionModelConfig(sessionID string, patch map[string]any) error {
	if err := s.session(sessionID); err != nil {
		return err
	}
	_, err := s.apply("session.sidecars", map[string]any{"patch": patch})
	return err
}

func (s *workerStore) finish() (*pyjson.Object, error) {
	return s.apply("execution.finish", map[string]any{})
}

func (s *workerStore) createSession(sessionID, source string, fields map[string]any) (any, error) {
	if err := s.session(sessionID); err != nil {
		return nil, err
	}
	payload := map[string]any{"source": source}
	for _, k := range []string{"model", "model_config", "system_prompt", "user_id", "session_key", "chat_id", "chat_type", "thread_id",
		"parent_session_id", "cwd", "profile_name", "git_repo_root", "origin_json", "display_name"} {
		payload[k] = fields[k]
	}
	result, err := s.apply("session.create", payload)
	if err != nil {
		return nil, err
	}
	return resultValue(result, "value"), nil
}

// Title result of the facade, an error entry raised as the ValueError the consumer catches
func titleResult(result *pyjson.Object) (bool, error) {
	if errValue := resultValue(result, "error"); pyTruthy(errValue) {
		return false, pyValueError(pyStr(errValue))
	}
	return pyTruthy(resultValue(result, "value")), nil
}

func (s *workerStore) setSessionTitle(sessionID, title string) (bool, error) {
	if err := s.session(sessionID); err != nil {
		return false, err
	}
	result, err := s.apply("session.title", map[string]any{"title": title, "source": "user"})
	if err != nil {
		return false, err
	}
	return titleResult(result)
}

func (s *workerStore) setAutoTitle(sessionID, title, source string) (bool, error) {
	if source != "derived" && source != "llm" {
		return false, pyValueError("invalid automatic title source: " + pyRepr(source))
	}
	if err := s.session(sessionID); err != nil {
		return false, err
	}
	result, err := s.apply("session.title", map[string]any{"title": title, "source": source})
	if err != nil {
		return false, err
	}
	return titleResult(result)
}

func (s *workerStore) getSessionTitleSource(sessionID string) (any, error) {
	row, err := s.getSession(sessionID)
	if err != nil {
		return nil, err
	}
	return rowValue(row, "title_source"), nil
}

func (s *workerStore) setSessionTitleSource(sessionID, source string) (bool, error) {
	if err := s.session(sessionID); err != nil {
		return false, err
	}
	result, err := s.apply("session.title_source", map[string]any{"source": source})
	if err != nil {
		return false, err
	}
	return pyTruthy(resultValue(result, "value")), nil
}

func (s *workerStore) getNextTitleInLineage(baseTitle string) (any, error) {
	result, err := s.apply("session.next_title", map[string]any{"base_title": baseTitle})
	if err != nil {
		return nil, err
	}
	return resultValue(result, "value"), nil
}

func (s *workerStore) touchSessionActivity(sessionID string, ts, description, provenance any) error {
	if err := s.session(sessionID); err != nil {
		return err
	}
	_, err := s.apply("session.activity", map[string]any{"ts": ts, "description": description, "provenance": provenance})
	return err
}

func (s *workerStore) clearSessionActivityLabels(sessionID string) error {
	if err := s.session(sessionID); err != nil {
		return err
	}
	_, err := s.apply("session.activity_clear", map[string]any{})
	return err
}

func (s *workerStore) updateSessionBillingRoute(sessionID string, provider, baseURL, billingMode any) error {
	if err := s.session(sessionID); err != nil {
		return err
	}
	_, err := s.apply("session.billing_route", map[string]any{"provider": provider, "base_url": baseURL, "billing_mode": billingMode})
	return err
}

func (s *workerStore) setLatestUserAPIContent(sessionID string, content, apiContent any) (any, error) {
	if err := s.session(sessionID); err != nil {
		return nil, err
	}
	result, err := s.apply("session.api_content", map[string]any{"content": content, "api_content": apiContent})
	if err != nil {
		return nil, err
	}
	return resultValue(result, "value"), nil
}

func (s *workerStore) setMessageAPIContent(sessionID string, rowID int64, content, apiContent any) (any, error) {
	if err := s.session(sessionID); err != nil {
		return nil, err
	}
	result, err := s.apply("session.message_api_content", map[string]any{"row_id": rowID, "content": content, "api_content": apiContent})
	if err != nil {
		return nil, err
	}
	return resultValue(result, "value"), nil
}

func (s *workerStore) setUserMessageContent(sessionID string, rowID int64, content any) (any, error) {
	if err := s.session(sessionID); err != nil {
		return nil, err
	}
	result, err := s.apply("session.user_content", map[string]any{"row_id": rowID, "content": content})
	if err != nil {
		return nil, err
	}
	return resultValue(result, "value"), nil
}

func (s *workerStore) endSession(sessionID, reason string) error {
	if err := s.session(sessionID); err != nil {
		return err
	}
	_, err := s.apply("session.end", map[string]any{"end_reason": reason})
	return err
}

func (s *workerStore) sessionLifecycleStatuses(sessionIDs []string) (map[string]any, error) {
	for _, sid := range sessionIDs {
		if err := s.session(sid); err != nil {
			return nil, err
		}
	}
	result, err := s.apply("session.lifecycle", map[string]any{})
	if err != nil {
		return nil, err
	}
	return map[string]any{s.scope.SessionID: resultValue(result, "value")}, nil
}

func (s *workerStore) latestConversationRole(sessionID string) (any, error) {
	if err := s.session(sessionID); err != nil {
		return nil, err
	}
	result, err := s.apply("session.tail_role", map[string]any{})
	if err != nil {
		return nil, err
	}
	return resultValue(result, "value"), nil
}

// Rpc that performs the receipt and then reports a lost acknowledgement
func lostAckRPC(original func(WorkerReceipt) (*pyjson.Object, error), message string) func(WorkerReceipt) (*pyjson.Object, error) {
	return func(in WorkerReceipt) (*pyjson.Object, error) {
		if _, err := original(in); err != nil {
			return nil, err
		}
		return nil, errors.New(message)
	}
}

// Receipt of the scope at the journal's next sequence with the operation and payload
func (s *workerStore) rawReceipt(operation string, payload any) WorkerReceipt {
	in := s.scope
	in.Sequence, in.Operation, in.Payload = s.nextSequence, operation, journalPayload(payload)
	return in
}

// Message contents of a session as the direct store reads them
func messageContents(t *testing.T, db *SessionDB, sessionID string) []string {
	t.Helper()
	rows, err := db.GetMessages(context.Background(), sessionID, GetMessagesOptions{})
	if err != nil {
		t.Fatal(err)
	}
	out := []string{}
	for _, row := range rows {
		out = append(out, pyStr(rowValue(row, "content")))
	}
	return out
}

// Conversation contents of a session as the direct store reads them
func conversationContents(t *testing.T, db *SessionDB, sessionID string) []string {
	t.Helper()
	rows, err := db.GetMessagesAsConversation(context.Background(), sessionID, ConversationOptions{})
	if err != nil {
		t.Fatal(err)
	}
	out := []string{}
	for _, row := range rows {
		out = append(out, pyStr(row["content"]))
	}
	return out
}

// Session row of the direct store, fatal when missing
func mustSession(t *testing.T, db *SessionDB, sessionID string) *pyjson.Object {
	t.Helper()
	row, err := db.GetSession(context.Background(), sessionID)
	if err != nil || row == nil {
		t.Fatalf("session %s %v %v", sessionID, row, err)
	}
	return row
}

// Message maps with role and content
func textMessages(pairs ...string) []map[string]any {
	out := []map[string]any{}
	for i := 0; i+1 < len(pairs); i += 2 {
		out = append(out, map[string]any{"role": pairs[i], "content": pairs[i+1]})
	}
	return out
}
