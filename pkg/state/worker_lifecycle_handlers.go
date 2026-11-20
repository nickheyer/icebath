package state

import (
	"context"
	"errors"
	"math"

	"github.com/nickheyer/icebath/pkg/internal/pyint"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
)

var (
	workerIdentityFields = map[string]bool{"user_id": true, "session_key": true, "chat_id": true, "chat_type": true, "thread_id": true,
		"parent_session_id": true, "cwd": true, "profile_name": true, "git_repo_root": true, "origin_json": true, "display_name": true}
	workerCreateFields      = unionKeys(workerIdentityFields, map[string]bool{"source": true, "model": true, "model_config": true, "system_prompt": true})
	workerConstructorConfig = map[string]bool{"max_iterations": true, "max_tokens": true, "reasoning_config": true, "yolo_mode": true}
)

// Union of two key sets
func unionKeys(sets ...map[string]bool) map[string]bool {
	out := map[string]bool{}
	for _, s := range sets {
		for k := range s {
			out[k] = true
		}
	}
	return out
}

// True for a Python ValueError however the port represents it
func isPyValueError(err error) bool {
	var typed *pyint.Error
	if errors.As(err, &typed) {
		return typed.Kind == "ValueError"
	}
	var value *stateValueError
	return errors.As(err, &value)
}

// String of a payload value, empty for nil
func payloadString(payload *pyjson.Object, key string) string {
	v, _ := payload.Get(key)
	s, _ := v.(string)
	return s
}

// Keep existing constructor backfill of the reserved row, refusing foreign identity or unreserved config
func workerCreate(d *SessionDB, ctx context.Context, conn SQLiteConn, sessionID string, payload *pyjson.Object) (*pyjson.Object, error) {
	if _, has := payload.Get("source"); payloadHasKeysOutside(payload, workerCreateFields) || !has {
		return nil, runtimeRefusal("invalid_params")
	}
	row, err := connObject(ctx, conn, "SELECT * FROM sessions WHERE id=?", sessionID)
	if err != nil {
		return nil, err
	}
	source, _ := payload.Get("source")
	if row == nil || !pyjson.Equal(source, rowValue(row, "source")) {
		return nil, runtimeRefusal("permission_denied")
	}
	for key := range workerIdentityFields {
		if value, _ := payload.Get(key); value != nil && !pyjson.Equal(value, rowValue(row, key)) {
			return nil, runtimeRefusal("permission_denied")
		}
	}
	for key := range workerCreateFields {
		if key == "model_config" {
			continue
		}
		if value, _ := payload.Get(key); value != nil {
			if _, ok := value.(string); !ok {
				return nil, runtimeRefusal("invalid_params")
			}
		}
	}
	rawConfig, _ := payload.Get("model_config")
	var config *pyjson.Object
	if rawConfig != nil {
		obj, ok := rawConfig.(*pyjson.Object)
		if !ok || payloadHasKeysOutside(obj, workerConstructorConfig) {
			return nil, runtimeRefusal("invalid_params")
		}
		config = obj
	}
	n := NewSession{ID: sessionID, Source: payloadString(payload, "source"), Model: payloadString(payload, "model"),
		SystemPrompt: payloadString(payload, "system_prompt"), UserID: payloadString(payload, "user_id"),
		SessionKey: payloadString(payload, "session_key"), ChatID: payloadString(payload, "chat_id"),
		ChatType: payloadString(payload, "chat_type"), ThreadID: payloadString(payload, "thread_id"),
		ParentSessionID: payloadString(payload, "parent_session_id"), Cwd: payloadString(payload, "cwd"),
		ProfileName: payloadString(payload, "profile_name"), GitRepoRoot: payloadString(payload, "git_repo_root"),
		OriginJSON: payloadString(payload, "origin_json"), DisplayName: payloadString(payload, "display_name")}
	if config != nil {
		n.ModelConfig = objMap(config)
	}
	if err := d.insertSessionRowInTransaction(ctx, conn, n); err != nil {
		return nil, err
	}
	if config != nil && config.Len() > 0 {
		text, err := pyjson.Dumps(config)
		if err != nil {
			return nil, err
		}
		if _, err := conn.ExecContext(ctx, "UPDATE sessions SET model_config=json_patch(model_config,?) WHERE id=? "+
			"AND json_type(model_config,'$._delegate_from') IS NOT NULL", text, sessionID); err != nil {
			return nil, err
		}
	}
	return pyjson.ObjectOf("value", sessionID), nil
}

// Ends the session once with the given reason, keeping an earlier end stamp
func workerEnd(d *SessionDB, ctx context.Context, conn SQLiteConn, sessionID string, payload *pyjson.Object) (*pyjson.Object, error) {
	raw, _ := payload.Get("end_reason")
	reason, ok := raw.(string)
	if !payloadKeysExactly(payload, "end_reason") || !ok || reason == "" {
		return nil, runtimeRefusal("invalid_params")
	}
	if _, err := d.endAndBump(ctx, conn, "UPDATE sessions SET ended_at=?,end_reason=? WHERE id=? AND ended_at IS NULL",
		[]any{now(), reason, sessionID}, sessionID, reason); err != nil {
		return nil, err
	}
	return pyjson.ObjectOf("value", nil), nil
}

// Lifecycle status of the transcript tail, empty when no row exists
func workerLifecycle(d *SessionDB, ctx context.Context, conn SQLiteConn, sessionID string, payload *pyjson.Object) (*pyjson.Object, error) {
	if payload.Len() > 0 {
		return nil, runtimeRefusal("invalid_params")
	}
	row, err := connRow(ctx, conn, "SELECT role,tool_calls IS NOT NULL AS has_tool_calls,finish_reason "+
		"FROM messages WHERE session_id=? ORDER BY id DESC LIMIT 1", sessionID)
	if err != nil {
		return nil, err
	}
	value := SessionStatusEmpty
	if row != nil {
		value = ClassifySessionStatus(row["role"], pyTruthy(row["has_tool_calls"]), row["finish_reason"])
	}
	return pyjson.ObjectOf("value", value), nil
}

// True when the source is one of user, derived or llm
func validTitleSource(v any) bool {
	s, ok := v.(string)
	return ok && (s == "user" || s == "derived" || s == "llm")
}

// Applies the title rules in the receipt, an invalid or conflicting title answering false with its candidate
func workerTitle(d *SessionDB, ctx context.Context, conn SQLiteConn, sessionID string, payload *pyjson.Object) (*pyjson.Object, error) {
	title, _ := payload.Get("title")
	source, _ := payload.Get("source")
	if _, isStr := title.(string); !payloadKeysExactly(payload, "title", "source") || !validTitleSource(source) || (title != nil && !isStr) {
		return nil, runtimeRefusal("invalid_params")
	}
	text, _ := title.(string)
	affected, err := d.setSessionTitleInTransaction(ctx, conn, sessionID, text, source.(string))
	if err != nil {
		if isPyValueError(err) {
			return pyjson.ObjectOf("value", false, "error", "Title unavailable or invalid", "title_candidate", title), nil
		}
		return nil, err
	}
	return pyjson.ObjectOf("value", len(affected) > 0), nil
}

// Rewrites the title source of a titled session
func workerTitleSource(d *SessionDB, ctx context.Context, conn SQLiteConn, sessionID string, payload *pyjson.Object) (*pyjson.Object, error) {
	source, _ := payload.Get("source")
	if !payloadKeysExactly(payload, "source") || !validTitleSource(source) {
		return nil, runtimeRefusal("invalid_params")
	}
	n, err := execRowcount(ctx, conn, "UPDATE sessions SET title_source=? WHERE id=? AND title IS NOT NULL", source, sessionID)
	if err != nil {
		return nil, err
	}
	return pyjson.ObjectOf("value", n > 0), nil
}

// Next numbered title for a base the session holds or has attempted in a receipt
func workerNextTitle(d *SessionDB, ctx context.Context, conn SQLiteConn, sessionID string, payload *pyjson.Object) (*pyjson.Object, error) {
	raw, _ := payload.Get("base_title")
	candidate, ok := raw.(string)
	if !payloadKeysExactly(payload, "base_title") || !ok {
		return nil, runtimeRefusal("invalid_params")
	}
	title, err := requiredScalar(ctx, conn, "SELECT title FROM sessions WHERE id=?", sessionID)
	if err != nil {
		return nil, err
	}
	attempted, err := connRow(ctx, conn, "SELECT 1 FROM worker_receipts r JOIN worker_executions w ON w.execution_id=r.execution_id "+
		"WHERE w.session_id=? AND w.status='running' "+
		"AND json_extract(r.result_json,'$.title_candidate')=? LIMIT 1", sessionID, candidate)
	if err != nil {
		return nil, err
	}
	if !pyjson.Equal(candidate, title) && attempted == nil {
		return nil, runtimeRefusal("permission_denied")
	}
	next, err := nextTitleInLineage(ctx, conn, candidate)
	if err != nil {
		return nil, err
	}
	return pyjson.ObjectOf("value", next), nil
}

// True when the value is a Python int or float
func payloadIsNumber(v any) bool {
	if _, ok := exactInt(v); ok {
		return true
	}
	_, ok := v.(float64)
	return ok
}

// Float of a Python int or float value
func payloadFloat(v any) float64 {
	if n, ok := exactInt(v); ok {
		return float64(n)
	}
	f, _ := v.(float64)
	return f
}

// Stamps mid turn activity without moving the activity clock backwards
func workerActivity(d *SessionDB, ctx context.Context, conn SQLiteConn, sessionID string, payload *pyjson.Object) (*pyjson.Object, error) {
	if !payloadKeysExactly(payload, "ts", "description", "provenance") {
		return nil, runtimeRefusal("invalid_params")
	}
	for _, k := range []string{"description", "provenance"} {
		if v, _ := payload.Get(k); v != nil {
			if _, ok := v.(string); !ok {
				return nil, runtimeRefusal("invalid_params")
			}
		}
	}
	ts, _ := payload.Get("ts")
	if ts != nil && (!payloadIsNumber(ts) || math.IsInf(payloadFloat(ts), 0) || math.IsNaN(payloadFloat(ts))) {
		return nil, runtimeRefusal("invalid_params")
	}
	when := now()
	if ts != nil {
		when = payloadFloat(ts)
	}
	_, err := conn.ExecContext(ctx, "UPDATE sessions SET last_activity_at=?,last_activity_description=?,last_activity_provenance=? "+
		"WHERE id=? AND (last_activity_at IS NULL OR last_activity_at<?)",
		when, BoundActivityDescription(payloadString(payload, "description")),
		string(NormalizeActivityProvenance(payloadString(payload, "provenance"))), sessionID, when)
	if err != nil {
		return nil, err
	}
	return pyjson.ObjectOf("value", nil), nil
}

// Clears the activity description and provenance
func workerActivityClear(d *SessionDB, ctx context.Context, conn SQLiteConn, sessionID string, payload *pyjson.Object) (*pyjson.Object, error) {
	if payload.Len() > 0 {
		return nil, runtimeRefusal("invalid_params")
	}
	if _, err := conn.ExecContext(ctx, "UPDATE sessions SET last_activity_description=?,last_activity_provenance=? WHERE id=?",
		"", string(ActivityProvenanceUnknown), sessionID); err != nil {
		return nil, err
	}
	return pyjson.ObjectOf("value", nil), nil
}

// Rewrites the billing route, the billing mode only when given
func workerBillingRoute(d *SessionDB, ctx context.Context, conn SQLiteConn, sessionID string, payload *pyjson.Object) (*pyjson.Object, error) {
	if !payloadKeysExactly(payload, "provider", "base_url", "billing_mode") {
		return nil, runtimeRefusal("invalid_params")
	}
	for _, k := range payload.Keys {
		if v := payload.Vals[k]; v != nil {
			if _, ok := v.(string); !ok {
				return nil, runtimeRefusal("invalid_params")
			}
		}
	}
	provider, _ := payload.Get("provider")
	baseURL, _ := payload.Get("base_url")
	mode, _ := payload.Get("billing_mode")
	if _, err := conn.ExecContext(ctx, "UPDATE sessions SET billing_provider=?,billing_base_url=?,billing_mode=COALESCE(?,billing_mode) "+
		"WHERE id=?", provider, baseURL, mode, sessionID); err != nil {
		return nil, err
	}
	return pyjson.ObjectOf("value", nil), nil
}

// Backfills the newest active user row's api_content when its content still matches
func workerAPIContent(d *SessionDB, ctx context.Context, conn SQLiteConn, sessionID string, payload *pyjson.Object) (*pyjson.Object, error) {
	apiContent, _ := payload.Get("api_content")
	if _, ok := apiContent.(string); !payloadKeysExactly(payload, "content", "api_content") || !ok {
		return nil, runtimeRefusal("invalid_params")
	}
	content, _ := payload.Get("content")
	n, err := execRowcount(ctx, conn, "UPDATE messages SET api_content=? WHERE id=(SELECT id FROM messages WHERE session_id=? "+
		"AND role='user' AND active=1 ORDER BY id DESC LIMIT 1) AND content IS ?",
		scrubSurrogates(apiContent), sessionID, encodeMessageContent(content))
	if err != nil {
		return nil, err
	}
	return pyjson.ObjectOf("value", n), nil
}

// Positive integer row id of the payload
func payloadRowID(payload *pyjson.Object) (int64, error) {
	raw, _ := payload.Get("row_id")
	rowID, ok := exactInt(raw)
	if !ok || rowID <= 0 {
		return 0, runtimeRefusal("invalid_params")
	}
	return rowID, nil
}

// Backfills one active user row's api_content by row id when its content still matches
func workerMessageAPIContent(d *SessionDB, ctx context.Context, conn SQLiteConn, sessionID string, payload *pyjson.Object) (*pyjson.Object, error) {
	apiContent, _ := payload.Get("api_content")
	if _, ok := apiContent.(string); !payloadKeysExactly(payload, "row_id", "content", "api_content") || !ok {
		return nil, runtimeRefusal("invalid_params")
	}
	rowID, err := payloadRowID(payload)
	if err != nil {
		return nil, err
	}
	content, _ := payload.Get("content")
	n, err := execRowcount(ctx, conn, "UPDATE messages SET api_content=? WHERE id=? AND session_id=? AND role='user' AND active=1 AND content IS ?",
		scrubSurrogates(apiContent), rowID, sessionID, encodeMessageContent(content))
	if err != nil {
		return nil, err
	}
	return pyjson.ObjectOf("value", n), nil
}

// Rewrites one active user row of this session, display metadata replacing the api copy when given
func workerUserContent(d *SessionDB, ctx context.Context, conn SQLiteConn, sessionID string, payload *pyjson.Object) (*pyjson.Object, error) {
	_, hasDisplay := payload.Get("display_metadata")
	_, hasRow := payload.Get("row_id")
	_, hasContent := payload.Get("content")
	expected := 2
	if hasDisplay {
		expected = 3
	}
	if !hasRow || !hasContent || payload.Len() != expected {
		return nil, runtimeRefusal("invalid_params")
	}
	content, _ := payload.Get("content")
	encoded := encodeMessageContent(content)
	rowID, err := payloadRowID(payload)
	if err != nil {
		return nil, err
	}
	if !hasDisplay {
		n, err := execRowcount(ctx, conn, "UPDATE messages SET content=? WHERE id=? AND session_id=? AND role='user' AND active=1",
			encoded, rowID, sessionID)
		if err != nil {
			return nil, err
		}
		return pyjson.ObjectOf("value", n), nil
	}
	display, _ := payload.Get("display_metadata")
	metadata, err := encodeDisplayMetadata(display)
	if err != nil {
		return nil, err
	}
	n, err := execRowcount(ctx, conn, "UPDATE messages SET content=?, api_content=NULL, display_metadata=? "+
		"WHERE id=? AND session_id=? AND role='user' AND active=1", encoded, metadata, rowID, sessionID)
	if err != nil {
		return nil, err
	}
	return pyjson.ObjectOf("value", n), nil
}

// Worker lifecycle operations by wire name
var workerLifecycleHandlers = map[string]WorkerHandler{
	"session.title":               workerTitle,
	"session.title_source":        workerTitleSource,
	"session.next_title":          workerNextTitle,
	"session.activity":            workerActivity,
	"session.activity_clear":      workerActivityClear,
	"session.billing_route":       workerBillingRoute,
	"session.api_content":         workerAPIContent,
	"session.message_api_content": workerMessageAPIContent,
	"session.user_content":        workerUserContent,
	"session.create":              workerCreate,
	"session.end":                 workerEnd,
	"session.lifecycle":           workerLifecycle,
}
