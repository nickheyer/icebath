package state

import (
	"context"
	"math"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
)

// Stored automation state of one family as the worker rotation reads and rewrites it
type AutomationState interface {
	Status() any
	SetStatus(status string)
	ToJSON() (string, error)
}

// Decoders of the goal, heartbeat and loop state rows, assigned by the agent wiring
var WorkerAutomationCodecs = map[string]func(raw string) (AutomationState, error){}

// Payload dict holding every required key and nothing outside required plus optional, invalid_params otherwise
func fieldsOf(payload any, required []string, optional ...string) (*pyjson.Object, error) {
	obj, ok := payload.(*pyjson.Object)
	if !ok {
		return nil, runtimeRefusal("invalid_params")
	}
	allowed := make(map[string]bool, len(required)+len(optional))
	for _, k := range required {
		allowed[k] = true
		if _, has := obj.Get(k); !has {
			return nil, runtimeRefusal("invalid_params")
		}
	}
	for _, k := range optional {
		allowed[k] = true
	}
	if payloadHasKeysOutside(obj, allowed) {
		return nil, runtimeRefusal("invalid_params")
	}
	return obj, nil
}

// Finite int or float payload value kept in its own type beside its float reading
func payloadNumber(value any) (any, float64, error) {
	if !payloadIsNumber(value) {
		return nil, 0, runtimeRefusal("invalid_params")
	}
	f := payloadFloat(value)
	if math.IsInf(f, 0) || math.IsNaN(f) {
		return nil, 0, runtimeRefusal("invalid_params")
	}
	return value, f, nil
}

// Value with every decoded object at any depth turned into the plain map the transcript code reads
func plainJSON(v any) any {
	switch t := v.(type) {
	case *pyjson.Object:
		out := make(map[string]any, t.Len())
		for _, k := range t.Keys {
			out[k] = plainJSON(t.Vals[k])
		}
		return out
	case []any:
		out := make([]any, len(t))
		for i, item := range t {
			out[i] = plainJSON(item)
		}
		return out
	}
	return v
}

// Plain message maps of a payload list whose every member is a dict
func plainMessages(value any) ([]map[string]any, bool) {
	list, ok := value.([]any)
	if !ok {
		return nil, false
	}
	out := make([]map[string]any, 0, len(list))
	for _, item := range list {
		obj, ok := item.(*pyjson.Object)
		if !ok {
			return nil, false
		}
		out = append(out, plainJSON(obj).(map[string]any))
	}
	return out, true
}

// Message maps as a JSON list value
func anyMessages(values []map[string]any) []any {
	out := make([]any, len(values))
	for i, v := range values {
		out[i] = v
	}
	return out
}

// String pointer of a payload value, nil for nil
func optStringOf(value any) *string {
	s, ok := value.(string)
	if !ok {
		return nil
	}
	return &s
}

// Integer pointer of a payload value, nil for nil
func optInt64Of(value any) *int64 {
	n, ok := exactInt(value)
	if !ok {
		return nil
	}
	return &n
}

// Compression lock holder read, acquire, renew or release on the receipt connection
func workerCompressionLock(action string) WorkerHandler {
	return func(d *SessionDB, ctx context.Context, conn SQLiteConn, sid string, payload *pyjson.Object) (*pyjson.Object, error) {
		if action == "holder" {
			if _, err := fieldsOf(payload, nil); err != nil {
				return nil, err
			}
			holder, _, err := fetchScalar(ctx, conn, "SELECT holder FROM compression_locks WHERE session_id=? AND expires_at>?", sid, now())
			if err != nil {
				return nil, err
			}
			return pyjson.ObjectOf("value", holder), nil
		}
		optional := []string{"ttl_seconds"}
		if action == "release" {
			optional = nil
		}
		if _, err := fieldsOf(payload, []string{"holder"}, optional...); err != nil {
			return nil, err
		}
		rawHolder, _ := payload.Get("holder")
		holder, err := runtimeTextOf(rawHolder)
		if err != nil {
			return nil, err
		}
		t := now()
		rawTTL, has := payload.Get("ttl_seconds")
		if !has {
			rawTTL = 300.0
		}
		_, ttl, err := payloadNumber(rawTTL)
		if err != nil {
			return nil, err
		}
		if !(0.1 <= ttl && ttl <= 3600) {
			return nil, runtimeRefusal("invalid_params")
		}
		var value any
		switch action {
		case "acquire":
			acquired, _, err := claimLeaseRow(ctx, conn, "compression_locks", "session_id", sid, holder, t, t+ttl, func(h, e any) bool {
				expires, _ := pyFloatOf(e)
				name, _ := h.(string)
				return expires < t || compressionLockHolderProcessIsDead(name)
			})
			if err != nil {
				return nil, err
			}
			value = acquired
		case "renew":
			n, err := execRowcount(ctx, conn, "UPDATE compression_locks SET expires_at=? WHERE session_id=? AND holder=?", t+ttl, sid, holder)
			if err != nil {
				return nil, err
			}
			value = n > 0
		default:
			if _, err := conn.ExecContext(ctx, "DELETE FROM compression_locks WHERE session_id=? AND holder=?", sid, holder); err != nil {
				return nil, err
			}
		}
		return pyjson.ObjectOf("value", value), nil
	}
}

// Records a compression failure cooldown, keeping a later deadline already stored
func workerCooldownRecord(d *SessionDB, ctx context.Context, conn SQLiteConn, sid string, payload *pyjson.Object) (*pyjson.Object, error) {
	if _, err := fieldsOf(payload, []string{"cooldown_until", "error"}); err != nil {
		return nil, err
	}
	rawDeadline, _ := payload.Get("cooldown_until")
	deadline, _, err := payloadNumber(rawDeadline)
	if err != nil {
		return nil, err
	}
	errValue, _ := payload.Get("error")
	if _, ok := errValue.(string); errValue != nil && !ok {
		return nil, runtimeRefusal("invalid_params")
	}
	if _, err := conn.ExecContext(ctx, "UPDATE sessions SET compression_failure_cooldown_until = CASE "+
		"WHEN compression_failure_cooldown_until IS NOT NULL AND compression_failure_cooldown_until > ? "+
		"THEN compression_failure_cooldown_until ELSE ? END, compression_failure_error=? WHERE id=?",
		deadline, deadline, errValue, sid); err != nil {
		return nil, err
	}
	return pyjson.ObjectOf("value", nil), nil
}

// Restores a cooldown snapshot and verifies the row reads back exactly as written
func workerCooldownRestore(d *SessionDB, ctx context.Context, conn SQLiteConn, sid string, payload *pyjson.Object) (*pyjson.Object, error) {
	if _, err := fieldsOf(payload, []string{"snapshot"}); err != nil {
		return nil, err
	}
	rawSnapshot, _ := payload.Get("snapshot")
	snapshot, err := fieldsOf(rawSnapshot, []string{"session_exists", "cooldown_until", "error"})
	if err != nil {
		return nil, err
	}
	exists, isBool := func() (bool, bool) {
		v, _ := snapshot.Get("session_exists")
		b, ok := v.(bool)
		return b, ok
	}()
	if !isBool {
		return nil, runtimeRefusal("invalid_params")
	}
	if !exists {
		return nil, &stateTypedError{"RuntimeError", "cannot restore absent compression cooldown row: session now exists"}
	}
	deadline, _ := snapshot.Get("cooldown_until")
	errValue, _ := snapshot.Get("error")
	if deadline != nil {
		if _, _, err := payloadNumber(deadline); err != nil {
			return nil, err
		}
	}
	if _, ok := errValue.(string); errValue != nil && !ok {
		return nil, runtimeRefusal("invalid_params")
	}
	if _, err := conn.ExecContext(ctx, "UPDATE sessions SET compression_failure_cooldown_until=?, compression_failure_error=? WHERE id=?",
		deadline, errValue, sid); err != nil {
		return nil, err
	}
	actual, err := connRow(ctx, conn, "SELECT compression_failure_cooldown_until,compression_failure_error FROM sessions WHERE id=?", sid)
	if err != nil {
		return nil, err
	}
	if actual == nil || !pyjson.Equal(actual["compression_failure_cooldown_until"], deadline) || !pyjson.Equal(actual["compression_failure_error"], errValue) {
		return nil, &stateTypedError{"RuntimeError", "compression cooldown rollback verification failed"}
	}
	return pyjson.ObjectOf("value", nil), nil
}

// Clears the cooldown row through the verified restore
func workerCooldownClear(d *SessionDB, ctx context.Context, conn SQLiteConn, sid string, payload *pyjson.Object) (*pyjson.Object, error) {
	if _, err := fieldsOf(payload, nil); err != nil {
		return nil, err
	}
	return workerCooldownRestore(d, ctx, conn, sid, pyjson.ObjectOf("snapshot",
		pyjson.ObjectOf("session_exists", true, "cooldown_until", nil, "error", nil)))
}

// Writes one compression counter column, the recovery deadline accepting any finite number
func workerCompressionCounter(column string) WorkerHandler {
	return func(d *SessionDB, ctx context.Context, conn SQLiteConn, sid string, payload *pyjson.Object) (*pyjson.Object, error) {
		if _, err := fieldsOf(payload, []string{"value"}); err != nil {
			return nil, err
		}
		value, _ := payload.Get("value")
		if value != nil {
			if _, _, err := payloadNumber(value); err != nil {
				return nil, err
			}
		}
		if column != "compression_recovery_deadline" {
			if n, ok := exactInt(value); !ok || n < 0 {
				return nil, runtimeRefusal("invalid_params")
			}
		}
		if _, err := conn.ExecContext(ctx, "UPDATE sessions SET "+column+"=? WHERE id=?", value, sid); err != nil {
			return nil, err
		}
		return pyjson.ObjectOf("value", nil), nil
	}
}

// Refuses a target outside the assigned session's attribution ancestry and compression chain
func snapshotAuthorize(d *SessionDB, sctx context.Context, assigned string, target any) (string, error) {
	text, err := runtimeTextOf(target)
	if err != nil {
		return "", err
	}
	allowed, err := d.sessionLineageRootToTip(sctx, assigned)
	if err != nil {
		return "", err
	}
	chain, err := d.GetCompressionChain(sctx, assigned)
	if err != nil {
		return "", err
	}
	for _, id := range append(allowed, chain...) {
		if id == text {
			return text, nil
		}
	}
	return "", runtimeRefusal("permission_denied")
}

// Session row of an authorized lineage target
func workerLineageContext(d *SessionDB, ctx context.Context, conn SQLiteConn, sid string, payload *pyjson.Object) (*pyjson.Object, error) {
	if _, err := fieldsOf(payload, []string{"target"}); err != nil {
		return nil, err
	}
	sctx := withReadConn(ctx, conn)
	rawTarget, _ := payload.Get("target")
	target, err := snapshotAuthorize(d, sctx, sid, rawTarget)
	if err != nil {
		return nil, err
	}
	session, err := workerSessionRow(d, ctx, conn, target)
	if err != nil {
		return nil, err
	}
	return pyjson.ObjectOf("session", session), nil
}

// Readable ids, lineage, root, tip, resume target and scope identity of an authorized target
func workerLineage(d *SessionDB, ctx context.Context, conn SQLiteConn, sid string, payload *pyjson.Object) (*pyjson.Object, error) {
	if _, err := fieldsOf(payload, []string{"target"}); err != nil {
		return nil, err
	}
	sctx := withReadConn(ctx, conn)
	rawTarget, _ := payload.Get("target")
	target, err := snapshotAuthorize(d, sctx, sid, rawTarget)
	if err != nil {
		return nil, err
	}
	rootToTip, err := d.sessionLineageRootToTip(sctx, sid)
	if err != nil {
		return nil, err
	}
	chain, err := d.GetCompressionChain(sctx, sid)
	if err != nil {
		return nil, err
	}
	lineage, err := d.GetCompressionLineage(sctx, target)
	if err != nil {
		return nil, err
	}
	root, err := d.conversationRootCtx(sctx, target)
	if err != nil {
		return nil, err
	}
	tip, err := d.GetCompressionTip(sctx, target)
	if err != nil {
		return nil, err
	}
	resume, err := d.resolveResumeSessionIDCtx(sctx, target)
	if err != nil {
		return nil, err
	}
	isFork, source, err := d.DeclaredScopeIdentity(sctx, target)
	if err != nil {
		return nil, err
	}
	return pyjson.ObjectOf("readable_ids", anyStrings(append(rootToTip, chain...)), "lineage", anyStrings(lineage),
		"root", root, "tip", tip, "resume", resume, "identity", []any{isFork, source}), nil
}

// Conversation boundary generation of the assigned peer, refusing a foreign key or source
func workerBoundary(d *SessionDB, ctx context.Context, conn SQLiteConn, sid string, payload *pyjson.Object) (*pyjson.Object, error) {
	if _, err := fieldsOf(payload, []string{"session_key", "source"}); err != nil {
		return nil, err
	}
	row, err := workerSessionRow(d, ctx, conn, sid)
	if err != nil {
		return nil, err
	}
	key, _ := payload.Get("session_key")
	source, _ := payload.Get("source")
	if !pyjson.Equal(rowValue(row, "session_key"), key) || !pyjson.Equal(rowValue(row, "source"), source) {
		return nil, runtimeRefusal("permission_denied")
	}
	keyText, _ := key.(string)
	sourceText, _ := source.(string)
	generation, ok, err := d.LatestConversationBoundary(withReadConn(ctx, conn), keyText, sourceText)
	if err != nil {
		return nil, err
	}
	var value any
	if ok {
		value = generation
	}
	return pyjson.ObjectOf("value", value), nil
}

// Conversation history of an authorized target under the owner's read rules
func workerHistory(d *SessionDB, ctx context.Context, conn SQLiteConn, sid string, payload *pyjson.Object) (*pyjson.Object, error) {
	if _, err := fieldsOf(payload, []string{"target", "include_ancestors", "include_inactive", "repair_alternation", "include_row_ids", "include_compacted"}); err != nil {
		return nil, err
	}
	flags := map[string]bool{}
	for _, k := range payload.Keys {
		if k == "target" {
			continue
		}
		b, ok := payload.Vals[k].(bool)
		if !ok {
			return nil, runtimeRefusal("invalid_params")
		}
		flags[k] = b
	}
	sctx := withReadConn(ctx, conn)
	rawTarget, _ := payload.Get("target")
	target, err := snapshotAuthorize(d, sctx, sid, rawTarget)
	if err != nil {
		return nil, err
	}
	ids := []string{target}
	if flags["include_ancestors"] {
		if ids, err = d.resumeLineageIDs(sctx, target); err != nil {
			return nil, err
		}
	}
	active := activeClause(flags["include_inactive"], flags["include_compacted"])
	display := flags["include_compacted"] && !flags["include_inactive"]
	head := ""
	if display {
		head = "session_id, compacted, display_order, "
	}
	rows, err := queryObjects(ctx, conn, "SELECT "+head+conversationRowColumns+" FROM messages "+
		"WHERE session_id IN ("+placeholders(len(ids))+")"+active+" ORDER BY id", stringArgs(ids)...)
	if err != nil {
		return nil, err
	}
	if display {
		if rows, err = d.withRemovedIdentities(sctx, rows, ids); err != nil {
			return nil, err
		}
	}
	if flags["include_compacted"] {
		if rows, err = dedupeDisplayGenerations(rows, display); err != nil {
			return nil, err
		}
	}
	messages, err := d.rowsToConversation(rows, target, flags["include_ancestors"], flags["repair_alternation"], flags["include_row_ids"], false)
	if err != nil {
		return nil, err
	}
	return pyjson.ObjectOf("messages", anyMessages(messages)), nil
}

// The owner's in place compaction body on the receipt connection
func archiveOnConnection(d *SessionDB, ctx context.Context, conn SQLiteConn, sessionID string, compacted []map[string]any, o ArchiveCompactOptions) (int, error) {
	return d.archiveAndCompactOnConn(ctx, conn, sessionID, compacted, o)
}

// The owner's rotation body on the receipt connection
func publishOnConnection(d *SessionDB, ctx context.Context, conn SQLiteConn, p CompressionChildPublish) error {
	return d.publishCompressionChildOnConn(ctx, conn, p)
}

// Highest active row id of the session, zero for none
func workerWatermark(d *SessionDB, ctx context.Context, conn SQLiteConn, sid string, payload *pyjson.Object) (*pyjson.Object, error) {
	if _, err := fieldsOf(payload, nil); err != nil {
		return nil, err
	}
	value, err := requiredScalar(ctx, conn, "SELECT COALESCE(MAX(id),0) FROM messages WHERE session_id=? AND active=1", sid)
	if err != nil {
		return nil, err
	}
	return pyjson.ObjectOf("value", value), nil
}

// Validates a handoff message list against the closed field set, row addresses staying inside this transcript
func handoffMessages(ctx context.Context, conn SQLiteConn, sid string, messages any) ([]map[string]any, error) {
	list, ok := messages.([]any)
	if !ok || len(list) > 1000 {
		return nil, runtimeRefusal("invalid_params")
	}
	for _, item := range list {
		msg, ok := item.(*pyjson.Object)
		if !ok || payloadHasKeysOutside(msg, HandoffMessageFields) || !validMessageRole(msg) {
			return nil, runtimeRefusal("invalid_params")
		}
		if rowID, has := msg.Get("_row_id"); has {
			row, err := connRow(ctx, conn, "SELECT 1 FROM messages WHERE session_id=? AND id=?", sid, rowID)
			if err != nil {
				return nil, err
			}
			if row == nil {
				return nil, runtimeRefusal("permission_denied")
			}
		}
	}
	plain, _ := plainMessages(messages)
	return plain, nil
}

// True when the message role is one of the four transcript roles
func validMessageRole(msg *pyjson.Object) bool {
	role, _ := msg.Get("role")
	switch role {
	case "user", "assistant", "system", "tool":
		return true
	}
	return false
}

// Refuses a watermark or watermark ceiling that is not nil or a non negative int
func validWatermarks(payload *pyjson.Object) error {
	for _, field := range []string{"watermark", "watermark_ceiling"} {
		if value, _ := payload.Get(field); value != nil {
			if n, ok := exactInt(value); !ok || n < 0 {
				return runtimeRefusal("invalid_params")
			}
		}
	}
	return nil
}

// List of dicts of an optional payload value, invalid_params for anything else
func optionalDictList(value any) ([]map[string]any, error) {
	if value == nil {
		return nil, nil
	}
	list, ok := plainMessages(value)
	if !ok {
		return nil, runtimeRefusal("invalid_params")
	}
	return list, nil
}

// In place compaction receipt returning the archived count and the handoff rows' annotations
func workerArchive(d *SessionDB, ctx context.Context, conn SQLiteConn, sid string, payload *pyjson.Object) (*pyjson.Object, error) {
	if _, err := fieldsOf(payload, []string{"messages", "model_config_patch", "watermark", "lock_holder", "tail_count"},
		"carried_messages", "covered_ids", "unresolved_held", "held_for_cas"); err != nil {
		return nil, err
	}
	rawMessages, _ := payload.Get("messages")
	messages, err := handoffMessages(ctx, conn, sid, rawMessages)
	if err != nil {
		return nil, err
	}
	rawCarried, _ := payload.Get("carried_messages")
	carried := []map[string]any{}
	if pyTruthy(rawCarried) {
		if carried, err = optionalDictList(rawCarried); err != nil {
			return nil, err
		}
	}
	rawCovered, _ := payload.Get("covered_ids")
	var covered []int64
	if rawCovered != nil {
		list, ok := rawCovered.([]any)
		if !ok {
			return nil, runtimeRefusal("invalid_params")
		}
		covered = make([]int64, 0, len(list))
		for _, item := range list {
			n, ok := exactInt(item)
			if !ok || n <= 0 {
				return nil, runtimeRefusal("invalid_params")
			}
			covered = append(covered, n)
		}
	}
	rawUnresolved, _ := payload.Get("unresolved_held")
	unresolved, err := optionalDictList(rawUnresolved)
	if err != nil {
		return nil, err
	}
	rawCAS, _ := payload.Get("held_for_cas")
	cas, err := optionalDictList(rawCAS)
	if err != nil {
		return nil, err
	}
	if err := validWatermarks(payload); err != nil {
		return nil, err
	}
	lockHolder, _ := payload.Get("lock_holder")
	if lockHolder != nil {
		if _, err := runtimeTextOf(lockHolder); err != nil {
			return nil, err
		}
	}
	rawPatch, _ := payload.Get("model_config_patch")
	var patch *pyjson.Object
	if rawPatch != nil {
		obj, ok := rawPatch.(*pyjson.Object)
		if !ok || payloadHasKeysOutside(obj, SidecarKeys) {
			return nil, runtimeRefusal("invalid_params")
		}
		patch = obj
	}
	rawTail, _ := payload.Get("tail_count")
	tail, ok := exactInt(rawTail)
	if !ok || tail < 0 {
		return nil, runtimeRefusal("invalid_params")
	}
	watermark, _ := payload.Get("watermark")
	value, err := archiveOnConnection(d, ctx, conn, sid, messages, ArchiveCompactOptions{ModelConfigPatch: patch,
		Watermark: optInt64Of(watermark), LockHolder: optStringOf(lockHolder), TailCount: int(tail), CarriedMessages: carried,
		CoveredIDs: covered, UnresolvedHeld: unresolved, HeldForCAS: cas})
	if err != nil {
		return nil, err
	}
	return pyjson.ObjectOf("value", value, "annotations", rowAnnotations(messages)), nil
}

// Moves goal, heartbeat and loop state from the parent to the child once, clearing the parent copy
func migrateWorkerAutomation(d *SessionDB, ctx context.Context, conn SQLiteConn, parent, child string) error {
	for _, family := range []string{"goal", "heartbeat", "loop"} {
		codec, ok := WorkerAutomationCodecs[family]
		if !ok {
			panic("unassigned wiring: state.WorkerAutomationCodecs[" + family + "]")
		}
		oldKey, newKey := family+":"+parent, family+":"+child
		raw, has, err := fetchScalar(ctx, conn, "SELECT value FROM state_meta WHERE key=?", oldKey)
		if err != nil {
			return err
		}
		if !has {
			continue
		}
		text, ok := raw.(string)
		if !ok {
			return &stateTypedError{"TypeError", "the JSON object must be str, bytes or bytearray, not " + pyTypeName(raw)}
		}
		state, err := codec(text)
		if err != nil {
			return err
		}
		if pyjson.Equal(state.Status(), "cleared") {
			continue
		}
		prior, hasPrior, err := fetchScalar(ctx, conn, "SELECT value FROM state_meta WHERE key=?", newKey)
		if err != nil {
			return err
		}
		if hasPrior {
			if family != "heartbeat" {
				continue
			}
			priorText, ok := prior.(string)
			if !ok {
				return &stateTypedError{"TypeError", "the JSON object must be str, bytes or bytearray, not " + pyTypeName(prior)}
			}
			priorState, err := codec(priorText)
			if err != nil {
				return err
			}
			if !pyjson.Equal(priorState.Status(), "cleared") {
				continue
			}
		}
		moved, err := state.ToJSON()
		if err != nil {
			return err
		}
		if err := d.setMeta(ctx, newKey, moved, conn); err != nil {
			return err
		}
		state.SetStatus("cleared")
		cleared, err := state.ToJSON()
		if err != nil {
			return err
		}
		if err := d.setMeta(ctx, oldKey, cleared, conn); err != nil {
			return err
		}
	}
	return nil
}

// Publishes the compression child in the receipt, moving the worker, admissions and automation to it
func workerPublish(d *SessionDB, ctx context.Context, conn SQLiteConn, sid string, payload *pyjson.Object) (*pyjson.Object, error) {
	if _, err := fieldsOf(payload, []string{"child_session_id", "source", "messages", "model", "model_config", "system_prompt",
		"cwd", "profile_name", "compression_lock_holder", "require_compression_lease",
		"require_lease_refresh", "lease_ttl_seconds", "watermark", "watermark_ceiling"}); err != nil {
		return nil, err
	}
	rawChild, _ := payload.Get("child_session_id")
	child, err := runtimeTextOf(rawChild)
	if err != nil {
		return nil, err
	}
	rawHolder, _ := payload.Get("compression_lock_holder")
	holder, err := runtimeTextOf(rawHolder)
	if err != nil {
		return nil, err
	}
	if err := validWatermarks(payload); err != nil {
		return nil, err
	}
	requireLease, _ := payload.Get("require_compression_lease")
	refresh, _ := payload.Get("require_lease_refresh")
	refreshBool, refreshIsBool := refresh.(bool)
	if requireLease != true || !refreshIsBool {
		return nil, runtimeRefusal("invalid_params")
	}
	rawTTL, _ := payload.Get("lease_ttl_seconds")
	_, ttl, err := payloadNumber(rawTTL)
	if err != nil {
		return nil, err
	}
	if !(0.1 <= ttl && ttl <= 3600) {
		return nil, runtimeRefusal("invalid_params")
	}
	rawMessages, _ := payload.Get("messages")
	messages, err := handoffMessages(ctx, conn, sid, rawMessages)
	if err != nil {
		return nil, err
	}
	parent, err := workerSessionRow(d, ctx, conn, sid)
	if err != nil {
		return nil, err
	}
	for _, key := range []string{"source", "cwd", "profile_name"} {
		if value, _ := payload.Get(key); value != nil && !pyjson.Equal(value, rowValue(parent, key)) {
			return nil, runtimeRefusal("permission_denied")
		}
	}
	for _, key := range []string{"model", "system_prompt"} {
		if value, _ := payload.Get(key); value != nil {
			if _, ok := value.(string); !ok {
				return nil, runtimeRefusal("invalid_params")
			}
		}
	}
	rawCfg, _ := payload.Get("model_config")
	cfg := pyjson.NewObject()
	if rawCfg != nil {
		obj, ok := rawCfg.(*pyjson.Object)
		if !ok {
			return nil, runtimeRefusal("invalid_params")
		}
		cfg = obj.Copy()
	}
	prior := parseModelConfigValue(rowValue(parent, "model_config"))
	for _, key := range []string{"_branched_from", "_delegate_from", "_reset_from"} {
		priorValue, inPrior := prior.Get(key)
		if value, has := cfg.Get(key); has && !pyjson.Equal(value, priorValue) {
			return nil, runtimeRefusal("permission_denied")
		}
		if inPrior {
			cfg.Set(key, priorValue)
		}
	}
	existing, err := connRow(ctx, conn, "SELECT 1 FROM sessions WHERE id=?", child)
	if err != nil {
		return nil, err
	}
	if existing != nil {
		return nil, runtimeRefusal("permission_denied")
	}
	worker, err := connObject(ctx, conn, "SELECT * FROM worker_executions WHERE session_id=? AND status IN ('registered','running')", sid)
	if err != nil {
		return nil, err
	}
	if worker == nil {
		return nil, runtimeRefusal("stale_generation")
	}
	source, _ := payload.Get("source")
	sourceText, _ := source.(string)
	model, _ := payload.Get("model")
	prompt, _ := payload.Get("system_prompt")
	cwd, _ := payload.Get("cwd")
	profile, _ := payload.Get("profile_name")
	watermark, _ := payload.Get("watermark")
	ceiling, _ := payload.Get("watermark_ceiling")
	publish := CompressionChildPublish{ParentSessionID: sid, ChildSessionID: child, Source: sourceText, Messages: messages,
		Model: optStringOf(model), ModelConfig: cfg, SystemPrompt: optStringOf(prompt), Cwd: optStringOf(cwd), ProfileName: optStringOf(profile),
		CompressionLockHolder: &holder, RequireCompressionLease: true, RequireLeaseRefresh: refreshBool, LeaseTTLSeconds: ttl,
		Watermark: optInt64Of(watermark), WatermarkCeiling: optInt64Of(ceiling)}
	if err := publishOnConnection(d, ctx, conn, publish); err != nil {
		return nil, err
	}
	if err := migrateWorkerAutomation(d, ctx, conn, sid, child); err != nil {
		return nil, err
	}
	if _, err := conn.ExecContext(ctx, "UPDATE sessions SET runtime_generation=? WHERE id=?", rowValue(worker, "generation"), child); err != nil {
		return nil, err
	}
	if _, err := conn.ExecContext(ctx, "UPDATE worker_executions SET session_id=? WHERE execution_id=?", child, rowValue(worker, "execution_id")); err != nil {
		return nil, err
	}
	if _, err := conn.ExecContext(ctx, "UPDATE session_admissions SET lineage_json=json_insert(lineage_json,'$[#]',?) "+
		"WHERE json_extract(lineage_json,'$[#-1]')=? AND generation=? AND owner_epoch=? "+
		"AND status IN ('started','unknown')", child, sid, rowValue(worker, "generation"), rowValue(worker, "owner_epoch")); err != nil {
		return nil, err
	}
	return pyjson.ObjectOf("value", nil, "worker_assignment", pyjson.ObjectOf("parent", sid, "session_id", child),
		"annotations", rowAnnotations(messages)), nil
}

// Worker row of the receipt, an identical committed publication retrying against its moved assignment
func workerReceiptAssignment(ctx context.Context, conn SQLiteConn, executionID, sessionID string, generation, sequence int64, digest string) (*pyjson.Object, error) {
	receipt, err := connObject(ctx, conn, "SELECT payload_digest,result_json FROM worker_receipts WHERE execution_id=? AND sequence=?", executionID, sequence)
	if err != nil {
		return nil, err
	}
	if receipt != nil && pyjson.Equal(rowValue(receipt, "payload_digest"), digest) {
		resultText, _ := rowValue(receipt, "result_json").(string)
		decoded, err := pyjson.Loads(resultText)
		if err != nil {
			return nil, pyValueError(err.Error())
		}
		result, _ := decoded.(*pyjson.Object)
		var assignment *pyjson.Object
		if result != nil {
			raw, _ := result.Get("worker_assignment")
			assignment, _ = raw.(*pyjson.Object)
		}
		if assignment != nil && assignment.Len() > 0 {
			parent, err := receiptItem(assignment, "parent")
			if err != nil {
				return nil, err
			}
			if pyjson.Equal(parent, sessionID) {
				current, err := requiredScalar(ctx, conn, "SELECT session_id FROM worker_executions WHERE execution_id=?", executionID)
				if err != nil {
					return nil, err
				}
				return workerAssignment(ctx, conn, executionID, pyStr(current), generation)
			}
		}
	}
	return workerAssignment(ctx, conn, executionID, sessionID, generation)
}

// Original parent a producer claim was bound to before physical rotation
func (d *SessionDB) WorkerClaimOrigin(ctx context.Context, params *pyjson.Object) (any, error) {
	executionID, err := receiptItem(params, "execution_id")
	if err != nil {
		return nil, err
	}
	var row *pyjson.Object
	if err := d.readCtx(ctx, false, func(conn SQLiteConn) error {
		row, err = connObject(ctx, conn, "SELECT result_json FROM worker_receipts WHERE execution_id=? "+
			"AND json_extract(result_json,'$.worker_assignment.parent') IS NOT NULL ORDER BY sequence LIMIT 1", executionID)
		return err
	}); err != nil {
		return nil, err
	}
	if row == nil {
		return receiptItem(params, "session_id")
	}
	text, _ := rowValue(row, "result_json").(string)
	decoded, err := pyjson.Loads(text)
	if err != nil {
		return nil, pyValueError(err.Error())
	}
	return receiptPath(decoded, "worker_assignment", "parent")
}

// Session the transport verifies a persist against, an old target accepted only for an exact publication replay
func (d *SessionDB) WorkerRetryTarget(ctx context.Context, params *pyjson.Object) (any, error) {
	sessionID, err := receiptItem(params, "session_id")
	if err != nil {
		return nil, err
	}
	if operation, _ := params.Get("operation"); !pyjson.Equal(operation, "compression.publish") {
		return sessionID, nil
	}
	target, ok := sessionID.(string)
	if !ok {
		return nil, &stateTypedError{"TypeError", "session_id must be str, not " + pyTypeName(sessionID)}
	}
	operation, _ := params.Get("operation")
	payload, _ := params.Get("payload")
	digest, err := AdmissionFingerprint(target, map[string]any{"operation": operation, "payload": payload})
	if err != nil {
		return nil, err
	}
	executionID, err := receiptItem(params, "execution_id")
	if err != nil {
		return nil, err
	}
	rawGeneration, err := receiptItem(params, "generation")
	if err != nil {
		return nil, err
	}
	rawSequence, err := receiptItem(params, "sequence")
	if err != nil {
		return nil, err
	}
	generation, ok := exactInt(rawGeneration)
	if !ok {
		return nil, runtimeRefusal("stale_generation")
	}
	sequence, _ := exactInt(rawSequence)
	var row *pyjson.Object
	if err := d.readCtx(ctx, false, func(conn SQLiteConn) error {
		row, err = workerReceiptAssignment(ctx, conn, pyStr(executionID), target, generation, sequence, digest)
		return err
	}); err != nil {
		return nil, err
	}
	return rowValue(row, "session_id"), nil
}

// Reopens an orphaned compression parent on the caller connection, false when a child or live lease exists
func reopenOnConnection(ctx context.Context, conn SQLiteConn, sessionID string) (bool, error) {
	ended, err := connRow(ctx, conn, endedRowSQL, sessionID)
	if err != nil || !endedByCompression(ended) {
		return false, err
	}
	child, err := connRow(ctx, conn, "\n                SELECT 1 AS one\n                FROM sessions\n                WHERE parent_session_id = ?\n                "+
		nonContinuationChild+"\n                LIMIT 1\n                ", sessionID, sessionID, sessionID, sessionID)
	if err != nil || child != nil {
		return false, err
	}
	current := now()
	lock, err := connRow(ctx, conn, compressionLockRowSQL, sessionID)
	if err != nil {
		return false, err
	}
	if lock != nil {
		expiresAt := lock["expires_at"]
		if expiresAt == nil {
			return false, nil
		}
		exp, err := pyFloatOf(expiresAt)
		if err != nil {
			return false, err
		}
		if exp >= current {
			return false, nil
		}
		n, err := execRowcount(ctx, conn, "DELETE FROM compression_locks WHERE session_id = ? AND holder = ? AND expires_at = ?",
			sessionID, lock["holder"], expiresAt)
		if err != nil {
			return false, err
		}
		if n != 1 {
			return false, nil
		}
	}
	n, err := execRowcount(ctx, conn, "UPDATE sessions SET ended_at = NULL, end_reason = NULL "+
		"WHERE id = ? AND ended_at IS NOT NULL AND end_reason = 'compression'", sessionID)
	if err != nil {
		return false, err
	}
	return n == 1, nil
}

// Reopens the assigned session when its compression end was orphaned
func workerReopen(d *SessionDB, ctx context.Context, conn SQLiteConn, sid string, payload *pyjson.Object) (*pyjson.Object, error) {
	if _, err := fieldsOf(payload, nil); err != nil {
		return nil, err
	}
	value, err := reopenOnConnection(ctx, conn, sid)
	if err != nil {
		return nil, err
	}
	return pyjson.ObjectOf("value", value), nil
}

// Authorized target of a cleanup sharing the assigned session's logical turn lease key
func cleanupTarget(d *SessionDB, ctx context.Context, conn SQLiteConn, sid string, payload *pyjson.Object) (string, string, string, error) {
	if _, err := fieldsOf(payload, []string{"target", "holder"}); err != nil {
		return "", "", "", err
	}
	rawTarget, _ := payload.Get("target")
	target, err := runtimeTextOf(rawTarget)
	if err != nil {
		return "", "", "", err
	}
	rawHolder, _ := payload.Get("holder")
	holder, err := runtimeTextOf(rawHolder)
	if err != nil {
		return "", "", "", err
	}
	if _, err := snapshotAuthorize(d, withReadConn(ctx, conn), sid, target); err != nil {
		return "", "", "", err
	}
	key, err := d.sessionTurnLeaseKeyOnConn(ctx, conn, sid)
	if err != nil {
		return "", "", "", err
	}
	targetKey, err := d.sessionTurnLeaseKeyOnConn(ctx, conn, target)
	if err != nil {
		return "", "", "", err
	}
	if key != targetKey {
		return "", "", "", runtimeRefusal("permission_denied")
	}
	return target, holder, key, nil
}

// Releases the compression lock of an authorized lineage target held by holder
func workerCompressionCleanup(d *SessionDB, ctx context.Context, conn SQLiteConn, sid string, payload *pyjson.Object) (*pyjson.Object, error) {
	target, holder, _, err := cleanupTarget(d, ctx, conn, sid, payload)
	if err != nil {
		return nil, err
	}
	if _, err := conn.ExecContext(ctx, "DELETE FROM compression_locks WHERE session_id=? AND holder=?", target, holder); err != nil {
		return nil, err
	}
	return pyjson.ObjectOf("value", nil), nil
}

// Appends handoff rows under the compression lock and turn lease guards
func workerCompressionAppend(d *SessionDB, ctx context.Context, conn SQLiteConn, sid string, payload *pyjson.Object) (*pyjson.Object, error) {
	if _, err := fieldsOf(payload, []string{"messages", "compression_lock_holder", "turn_lease_holder", "turn_lease_ttl_seconds"}); err != nil {
		return nil, err
	}
	rawMessages, _ := payload.Get("messages")
	messages, err := handoffMessages(ctx, conn, sid, rawMessages)
	if err != nil {
		return nil, err
	}
	for _, key := range []string{"compression_lock_holder", "turn_lease_holder"} {
		if value, _ := payload.Get(key); value != nil {
			if _, err := runtimeTextOf(value); err != nil {
				return nil, err
			}
		}
	}
	rawTTL, _ := payload.Get("turn_lease_ttl_seconds")
	_, ttl, err := payloadNumber(rawTTL)
	if err != nil {
		return nil, err
	}
	if !(0.1 <= ttl && ttl <= 3600) {
		return nil, runtimeRefusal("invalid_params")
	}
	lockHolder, _ := payload.Get("compression_lock_holder")
	turnHolder, _ := payload.Get("turn_lease_holder")
	count, err := d.appendMessagesInTransaction(ctx, conn, sid, messages, AppendGuards{CompressionLockHolder: optStringOf(lockHolder),
		TurnLeaseHolder: optStringOf(turnHolder), TurnLeaseTTLSeconds: &ttl})
	if err != nil {
		return nil, err
	}
	return pyjson.ObjectOf("count", count, "annotations", rowAnnotations(messages)), nil
}

// Releases the logical turn lease of an authorized target held by holder
func workerTurnCleanup(d *SessionDB, ctx context.Context, conn SQLiteConn, sid string, payload *pyjson.Object) (*pyjson.Object, error) {
	_, holder, key, err := cleanupTarget(d, ctx, conn, sid, payload)
	if err != nil {
		return nil, err
	}
	if _, err := conn.ExecContext(ctx, "DELETE FROM session_turn_leases WHERE conversation_id=? AND holder=?", key, holder); err != nil {
		return nil, err
	}
	return pyjson.ObjectOf("released", true), nil
}

// Worker compression operations by wire name
var workerCompressionHandlers = func() map[string]WorkerHandler {
	handlers := map[string]WorkerHandler{
		"turn.cleanup":                 workerTurnCleanup,
		"compression.append":           workerCompressionAppend,
		"compression.reopen":           workerReopen,
		"compression.cleanup":          workerCompressionCleanup,
		"compression.watermark":        workerWatermark,
		"compression.archive":          workerArchive,
		"compression.publish":          workerPublish,
		"compression.context":          workerLineageContext,
		"compression.lineage":          workerLineage,
		"compression.boundary":         workerBoundary,
		"compression.history":          workerHistory,
		"compression.cooldown.record":  workerCooldownRecord,
		"compression.cooldown.restore": workerCooldownRestore,
		"compression.cooldown.clear":   workerCooldownClear,
	}
	for _, action := range []string{"acquire", "renew", "release", "holder"} {
		handlers["compression.lock."+action] = workerCompressionLock(action)
	}
	for name, column := range map[string]string{"fallback": "compression_fallback_streak",
		"ineffective": "compression_ineffective_count", "recovery": "compression_recovery_deadline"} {
		handlers["compression."+name] = workerCompressionCounter(column)
	}
	return handlers
}()
