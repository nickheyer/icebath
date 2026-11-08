package state

import (
	"context"
	"math"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
)

// Keys a worker may persist on a message dict, the owner's insert and coverage reading them
var messageFields = func() map[string]bool {
	fields := map[string]bool{"role": true, "content": true, "tool_name": true, "tool_calls": true, "tool_call_id": true, "token_count": true,
		"finish_reason": true, "reasoning": true, "reasoning_content": true, "reasoning_details": true,
		"codex_reasoning_items": true, "codex_message_items": true, "platform_message_id": true, "message_id": true,
		"observed": true, "effect_disposition": true, "_compressed_summary": true, "timestamp": true, "api_content": true,
		"display_kind": true, "display_metadata": true, "_row_id": true, "_canonical_content": true, DBRowSnapshot: true, CanonicalRow: true, DataUse: true,
		AbsorbedRowIDs: true}
	for k := range PersistenceOnlyMessageFields {
		fields[k] = true
	}
	return fields
}()

// Message keys a compaction handoff may carry, the closed boundary the owner validates
var HandoffMessageFields = unionKeys(messageFields, map[string]bool{"_compressed_summary_has_user_turn": true})

// Row state the owner stamps on each persisted dict and hands back to a worker as an annotation
var rowAnnotationKeys = []string{"_row_id", "timestamp", DBRowSnapshot, CanonicalRow, MessageUID, ToolCallUIDs, ToolCallUID}

// Stamped row state of each message as the worker applies it to its own dicts, nil for an absent key
func rowAnnotations(messages []map[string]any) []any {
	out := make([]any, 0, len(messages))
	for _, msg := range messages {
		annotation := pyjson.NewObject()
		for _, key := range rowAnnotationKeys {
			annotation.Set(key, msg[key])
		}
		out = append(out, annotation)
	}
	return out
}

// Appends validated messages to the assigned transcript under the turn lease guard
func workerAppend(d *SessionDB, ctx context.Context, conn SQLiteConn, sessionID string, payload *pyjson.Object) (*pyjson.Object, error) {
	rawMessages, hasMessages := payload.Get("messages")
	if payloadHasKeysOutside(payload, map[string]bool{"messages": true, "turn_lease_holder": true}) || !hasMessages {
		return nil, runtimeRefusal("invalid_params")
	}
	list, ok := rawMessages.([]any)
	if !ok || len(list) > 1000 {
		return nil, runtimeRefusal("invalid_params")
	}
	for _, item := range list {
		msg, ok := item.(*pyjson.Object)
		if !ok || payloadHasKeysOutside(msg, messageFields) || !validMessageRole(msg) {
			return nil, runtimeRefusal("invalid_params")
		}
		if rowID, has := msg.Get("_row_id"); has {
			row, err := connRow(ctx, conn, "SELECT 1 FROM messages WHERE id=? AND session_id=?", rowID, sessionID)
			if err != nil {
				return nil, err
			}
			if row == nil {
				return nil, runtimeRefusal("permission_denied")
			}
		}
	}
	holder, _ := payload.Get("turn_lease_holder")
	if holder != nil {
		if _, err := runtimeTextOf(holder); err != nil {
			return nil, err
		}
	}
	messages, _ := plainMessages(rawMessages)
	count, err := d.appendMessagesInTransaction(ctx, conn, sessionID, messages, AppendGuards{TurnLeaseHolder: optStringOf(holder)})
	if err != nil {
		return nil, err
	}
	return pyjson.ObjectOf("count", count, "annotations", rowAnnotations(messages)), nil
}

// Acquires, renews or releases the logical turn lease of the assigned session
func workerTurn(operation string) WorkerHandler {
	return func(d *SessionDB, ctx context.Context, conn SQLiteConn, sessionID string, payload *pyjson.Object) (*pyjson.Object, error) {
		allowed := []string{"holder", "ttl_seconds"}
		if operation == "turn.release" {
			allowed = []string{"holder"}
		}
		if !payloadKeysExactly(payload, allowed...) {
			return nil, runtimeRefusal("invalid_params")
		}
		rawHolder, _ := payload.Get("holder")
		holder, err := runtimeTextOf(rawHolder)
		if err != nil {
			return nil, err
		}
		rawTTL, has := payload.Get("ttl_seconds")
		if !has {
			rawTTL = int64(300)
		}
		if !payloadIsNumber(rawTTL) {
			return nil, runtimeRefusal("invalid_params")
		}
		ttl := payloadFloat(rawTTL)
		if !(0.1 <= ttl && ttl <= 3600) {
			return nil, runtimeRefusal("invalid_params")
		}
		key, err := d.sessionTurnLeaseKeyOnConn(ctx, conn, sessionID)
		if err != nil {
			return nil, err
		}
		t := now()
		var value any
		switch operation {
		case "turn.acquire":
			acquired, _, err := claimLeaseRow(ctx, conn, "session_turn_leases", "conversation_id", key, holder, t, t+ttl, func(h, e any) bool {
				expires, _ := pyFloatOf(e)
				name, _ := h.(string)
				return expires <= t || compressionLockHolderProcessIsDead(name)
			})
			if err != nil {
				return nil, err
			}
			value = acquired
		case "turn.renew":
			n, err := execRowcount(ctx, conn, "UPDATE session_turn_leases SET expires_at=? WHERE conversation_id=? AND holder=?", t+ttl, key, holder)
			if err != nil {
				return nil, err
			}
			value = n > 0
		default:
			if _, err := conn.ExecContext(ctx, "DELETE FROM session_turn_leases WHERE conversation_id=? AND holder=?", key, holder); err != nil {
				return nil, err
			}
		}
		return pyjson.ObjectOf("value", value), nil
	}
}

// Per model usage columns a usage receipt may carry
var modelUsageFields = map[string]bool{"model": true, "billing_provider": true, "billing_base_url": true, "billing_mode": true,
	"input_tokens": true, "output_tokens": true, "cache_read_tokens": true, "cache_write_tokens": true, "reasoning_tokens": true,
	"estimated_cost_usd": true, "actual_cost_usd": true, "cost_status": true, "cost_source": true, "api_call_count": true}

// Validates one usage receipt value by its key under the counter, cost, flag and text rules
func validUsageValue(key string, value any) bool {
	switch key {
	case "input_tokens", "output_tokens", "cache_read_tokens", "cache_write_tokens", "reasoning_tokens", "api_call_count":
		n, ok := exactInt(value)
		return ok && 0 <= n && n <= 1<<53
	case "estimated_cost_usd", "actual_cost_usd":
		if value == nil {
			return true
		}
		if !payloadIsNumber(value) {
			return false
		}
		f := payloadFloat(value)
		return 0 <= f && f <= 1e12 && !math.IsNaN(f)
	case "absolute":
		_, ok := value.(bool)
		return ok
	}
	if value == nil {
		return true
	}
	s, ok := value.(string)
	return ok && len([]rune(s)) <= 4096
}

// Optional float of a validated cost value
func usageCost(payload *pyjson.Object, key string) *float64 {
	value, _ := payload.Get(key)
	if value == nil {
		return nil
	}
	f := payloadFloat(value)
	return &f
}

// Counter of a validated usage value, zero when absent
func usageCounter(payload *pyjson.Object, key string) int64 {
	value, _ := payload.Get(key)
	n, _ := exactInt(value)
	return n
}

// Records main loop or auxiliary usage of the assigned session, a main report also stamping its created surface
func workerUsage(auxiliary bool) WorkerHandler {
	return func(d *SessionDB, ctx context.Context, conn SQLiteConn, sessionID string, payload *pyjson.Object) (*pyjson.Object, error) {
		var source any
		if !auxiliary {
			raw, _ := payload.Get("source")
			if _, ok := raw.(string); ok {
				source = raw
			}
			trimmed := pyjson.NewObject()
			for _, k := range payload.Keys {
				if k != "source" {
					trimmed.Set(k, payload.Vals[k])
				}
			}
			payload = trimmed
		}
		allowed := unionKeys(modelUsageFields, map[string]bool{"pricing_version": true, "absolute": true, "task": true})
		if auxiliary {
			allowed = unionKeys(modelUsageFields, map[string]bool{"task": true})
			for _, k := range []string{"billing_mode", "actual_cost_usd", "cost_status", "cost_source"} {
				delete(allowed, k)
			}
		}
		if payloadHasKeysOutside(payload, allowed) {
			return nil, runtimeRefusal("invalid_params")
		}
		for _, k := range payload.Keys {
			if !validUsageValue(k, payload.Vals[k]) {
				return nil, runtimeRefusal("invalid_params")
			}
		}
		taskValue, hasTask := payload.Get("task")
		task, _ := taskValue.(string)
		if auxiliary {
			if !pyTruthy(taskValue) {
				return nil, runtimeRefusal("invalid_params")
			}
			calls := 1
			if n, has := payload.Get("api_call_count"); has {
				count, _ := exactInt(n)
				calls = int(count)
			}
			usage := modelUsage{model: optStringOf(payloadValue(payload, "model")), billingProvider: optStringOf(payloadValue(payload, "billing_provider")),
				billingBaseURL: optStringOf(payloadValue(payload, "billing_base_url")), inputTokens: usageCounter(payload, "input_tokens"),
				outputTokens: usageCounter(payload, "output_tokens"), cacheReadTokens: usageCounter(payload, "cache_read_tokens"),
				cacheWriteTokens: usageCounter(payload, "cache_write_tokens"), reasoningTokens: usageCounter(payload, "reasoning_tokens"),
				estimatedCostUSD: usageCost(payload, "estimated_cost_usd"), apiCallCount: calls}
			if err := d.recordModelUsage(ctx, conn, sessionID, task, usage); err != nil {
				return nil, err
			}
			return pyjson.ObjectOf("value", nil), nil
		}
		absolute, _ := payload.Get("absolute")
		absoluteBool, _ := absolute.(bool)
		delta := TokenCountDelta{InputTokens: usageCounter(payload, "input_tokens"), OutputTokens: usageCounter(payload, "output_tokens"),
			CacheReadTokens: usageCounter(payload, "cache_read_tokens"), CacheWriteTokens: usageCounter(payload, "cache_write_tokens"),
			ReasoningTokens: usageCounter(payload, "reasoning_tokens"), Model: optStringOf(payloadValue(payload, "model")),
			EstimatedCostUSD: usageCost(payload, "estimated_cost_usd"), ActualCostUSD: usageCost(payload, "actual_cost_usd"),
			CostStatus: optStringOf(payloadValue(payload, "cost_status")), CostSource: optStringOf(payloadValue(payload, "cost_source")),
			PricingVersion: optStringOf(payloadValue(payload, "pricing_version")), BillingProvider: optStringOf(payloadValue(payload, "billing_provider")),
			BillingBaseURL: optStringOf(payloadValue(payload, "billing_base_url")), BillingMode: optStringOf(payloadValue(payload, "billing_mode")),
			APICallCount: int(usageCounter(payload, "api_call_count")), Absolute: absoluteBool, Task: task}
		hasUsage := delta.InputTokens != 0 || delta.OutputTokens != 0 || delta.CacheReadTokens != 0 || delta.CacheWriteTokens != 0 ||
			delta.ReasoningTokens != 0 || delta.APICallCount != 0 || (delta.EstimatedCostUSD != nil && *delta.EstimatedCostUSD != 0)
		if hasTask && taskValue == nil && !absoluteBool && hasUsage {
			return nil, &SQLiteError{Class: SQLiteIntegrityError, Msg: "NOT NULL constraint failed: session_model_usage.task"}
		}
		if err := d.updateTokenCountsInTransaction(ctx, conn, sessionID, delta); err != nil {
			return nil, err
		}
		if surfaced := CreatedSurface(source); pyTruthy(surfaced) {
			if _, err := conn.ExecContext(ctx, "UPDATE sessions SET created_source=? WHERE id=? AND "+unsetCreatedSQL("created_source"),
				surfaced, sessionID); err != nil {
				return nil, err
			}
			if err := stampCompressionAncestors(ctx, conn, sessionID); err != nil {
				return nil, err
			}
		}
		return pyjson.ObjectOf("value", nil), nil
	}
}

// Payload value of a key, nil when absent
func payloadValue(payload *pyjson.Object, key string) any {
	v, _ := payload.Get(key)
	return v
}

// Closes the execution, the only operation that makes it terminal
func workerFinish(d *SessionDB, ctx context.Context, conn SQLiteConn, sessionID string, payload *pyjson.Object) (*pyjson.Object, error) {
	if payload.Len() > 0 {
		return nil, runtimeRefusal("invalid_params")
	}
	return pyjson.ObjectOf("status", "terminal"), nil
}

// Every worker.persist operation by wire name
var workerExecutionHandlers = func() map[string]WorkerHandler {
	handlers := map[string]WorkerHandler{}
	for name, h := range workerLifecycleHandlers {
		handlers[name] = h
	}
	for name, h := range workerCompressionHandlers {
		handlers[name] = h
	}
	handlers["session.context"] = workerContext
	handlers["session.prompt"] = workerPrompt
	handlers["session.sidecars"] = workerSidecars
	handlers["session.tools"] = workerToolNames
	handlers["session.tail_role"] = workerTailRole
	handlers["session.row_role"] = workerRowRole
	handlers["transcript.append"] = workerAppend
	handlers["execution.finish"] = workerFinish
	handlers["usage.main"] = workerUsage(false)
	handlers["usage.auxiliary"] = workerUsage(true)
	for _, name := range []string{"turn.acquire", "turn.renew", "turn.release"} {
		handlers[name] = workerTurn(name)
	}
	return handlers
}()

// Keyword arguments of one worker receipt
type WorkerReceipt struct {
	Epoch       int64
	ExecutionID string
	SessionID   string
	Generation  int64
	Sequence    int64
	Operation   string
	Payload     any
}

// One closed durable worker mutation and its receipt through the built in operation table
func (d *SessionDB) MutateWorkerExecution(ctx context.Context, in WorkerReceipt) (*pyjson.Object, error) {
	return d.ApplyWorkerReceipt(ctx, in, workerExecutionHandlers)
}

// One fenced worker receipt transaction, an exact retry replaying the stored result
func (d *SessionDB) ApplyWorkerReceipt(ctx context.Context, in WorkerReceipt, handlers map[string]WorkerHandler) (*pyjson.Object, error) {
	handler, known := handlers[in.Operation]
	if in.Sequence < 1 || !known {
		return nil, runtimeRefusal("invalid_params")
	}
	encoded, err := runtimeJSON(in.Payload)
	if err != nil {
		return nil, err
	}
	if len(encoded) > 4*1024*1024 {
		return nil, runtimeRefusal("invalid_params")
	}
	decodedForDigest, err := pyjson.Loads(encoded)
	if err != nil {
		return nil, pyValueError(err.Error())
	}
	digest, err := AdmissionFingerprint(in.SessionID, map[string]any{"operation": in.Operation, "payload": decodedForDigest})
	if err != nil {
		return nil, err
	}
	var result *pyjson.Object
	patience := sessionTranscriptWritePatienceS
	err = d.executeWrite(ctx, func(conn SQLiteConn) error {
		result = nil
		if err := runtimeEpoch(ctx, conn, in.Epoch); err != nil {
			return err
		}
		row, err := workerReceiptAssignment(ctx, conn, in.ExecutionID, in.SessionID, in.Generation, in.Sequence, digest)
		if err != nil {
			return err
		}
		if !pyjson.Equal(rowValue(row, "owner_epoch"), in.Epoch) {
			return runtimeRefusal("stale_epoch")
		}
		old, err := connObject(ctx, conn, "SELECT * FROM worker_receipts WHERE execution_id=? AND sequence=?", in.ExecutionID, in.Sequence)
		if err != nil {
			return err
		}
		if old != nil {
			if !pyjson.Equal(rowValue(old, "payload_digest"), digest) {
				return runtimeRefusal("admission_conflict")
			}
			resultText, _ := rowValue(old, "result_json").(string)
			retired, err := RetiredReceipt(resultText)
			if err != nil {
				return err
			}
			if retired {
				return runtimeRefusal("stale_generation")
			}
			decoded, err := pyjson.Loads(resultText)
			if err != nil {
				return pyValueError(err.Error())
			}
			stored, ok := decoded.(*pyjson.Object)
			if !ok {
				return &stateTypedError{"TypeError", "stored receipt result is not a dict"}
			}
			result = stored
			return nil
		}
		status, _ := rowValue(row, "status").(string)
		if status != "registered" && status != "running" {
			return runtimeRefusal("stale_generation")
		}
		if !pyjson.Equal(in.Sequence, pyAdd(rowValue(row, "last_sequence"), 1)) {
			return runtimeRefusal("invalid_params")
		}
		changes, err := totalChanges(ctx, conn)
		if err != nil {
			return err
		}
		decoded, err := pyjson.Loads(encoded)
		if err != nil {
			return pyValueError(err.Error())
		}
		out, err := handler(d, ctx, conn, in.SessionID, decoded.(*pyjson.Object))
		if err != nil {
			return err
		}
		after, err := totalChanges(ctx, conn)
		if err != nil {
			return err
		}
		changed := after != changes || in.Operation == "execution.finish"
		resultJSON, err := pyDumps(out, pyJSON{ensureASCII: true, itemSep: ", ", keySep: ": "})
		if err != nil {
			return err
		}
		stored, err := pyjson.Loads(resultJSON)
		if err != nil {
			return pyValueError(err.Error())
		}
		if _, err := conn.ExecContext(ctx, "INSERT INTO worker_receipts(execution_id,sequence,payload_digest,result_json) VALUES(?,?,?,?)",
			in.ExecutionID, in.Sequence, digest, resultJSON); err != nil {
			return err
		}
		nextStatus := "running"
		if in.Operation == "execution.finish" {
			nextStatus = "terminal"
		}
		if _, err := conn.ExecContext(ctx, "UPDATE worker_executions SET last_sequence=?,status=? WHERE execution_id=?",
			in.Sequence, nextStatus, in.ExecutionID); err != nil {
			return err
		}
		if changed {
			if _, err := conn.ExecContext(ctx, "UPDATE sessions SET runtime_revision=runtime_revision+1 WHERE id=?", in.SessionID); err != nil {
				return err
			}
		}
		if in.Operation == "execution.finish" {
			if err := compactTerminalReceipts(ctx, conn, in.ExecutionID); err != nil {
				return err
			}
		}
		result = stored.(*pyjson.Object)
		return nil
	}, &patience)
	if err != nil {
		return nil, err
	}
	return result, nil
}

// Sum of a stored integer column and an increment, the column read as a Python int
func pyAdd(value any, increment int64) any {
	n, ok := exactInt(value)
	if !ok {
		return value
	}
	return n + increment
}
