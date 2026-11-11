package agent

import (
	"context"
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/state"
	"github.com/nickheyer/icebath/pkg/tools"
)

// None for a nil pointer, the pointed value otherwise
func noneIfNil[T any](p *T) any {
	if p == nil {
		return nil
	}
	return *p
}

// Plain dicts of a receipt list of message dicts
func receiptMessages(raw any) ([]map[string]any, error) {
	list, ok := raw.([]any)
	if !ok {
		return nil, &workerStoreError{"TypeError", fmt.Sprintf("'%s' object is not a list of messages", pyrepr.TypeName(raw))}
	}
	out := make([]map[string]any, 0, len(list))
	for _, item := range list {
		m, ok := pyjson.Plain(item).(map[string]any)
		if !ok {
			return nil, &workerStoreError{"TypeError", fmt.Sprintf("'%s' object is not a message dict", pyrepr.TypeName(item))}
		}
		out = append(out, m)
	}
	return out, nil
}

// Message dicts as JSON values for a receipt payload
func messagesPayload(messages []map[string]any) []any {
	out := make([]any, len(messages))
	for i, m := range messages {
		out[i] = m
	}
	return out
}

// Integer value of a receipt field
func receiptInt(v any) (int64, error) {
	switch n := v.(type) {
	case int64:
		return n, nil
	case bool:
		if n {
			return 1, nil
		}
		return 0, nil
	}
	return 0, &workerStoreError{"TypeError", fmt.Sprintf("receipt value is a %s, not an int", pyrepr.TypeName(v))}
}

// Text of a receipt field, empty for None
func receiptText(v any) (string, error) {
	if v == nil {
		return "", nil
	}
	s, ok := v.(string)
	if !ok {
		return "", &workerStoreError{"TypeError", fmt.Sprintf("receipt value is a %s, not a str", pyrepr.TypeName(v))}
	}
	return s, nil
}

// Appends messages through the owner, mirroring its row stamps onto the dicts
func (s *RuntimeSessionStore) AppendMessagesBatch(ctx context.Context, sessionID string, messages []map[string]any, guards state.AppendGuards, chunkRows *int) (int, error) {
	if err := s.session(sessionID); err != nil {
		return 0, err
	}
	if chunkRows != nil {
		return 0, tools.NewWorkerPersistenceError("unsupported_operation")
	}
	if guards.CompressionLockHolder != nil || (guards.TurnLeaseTTLSeconds != nil && *guards.TurnLeaseTTLSeconds != 300.0) {
		ttl := 300.0
		if guards.TurnLeaseTTLSeconds != nil {
			ttl = *guards.TurnLeaseTTLSeconds
		}
		return s.appendCompressionMessages(ctx, sessionID, messages, guards.CompressionLockHolder, guards.TurnLeaseHolder, ttl)
	}
	result, err := s.apply(ctx, "transcript.append", pyjson.ObjectOf("messages", messagesPayload(messages), "turn_lease_holder", noneIfNil(guards.TurnLeaseHolder)))
	if err != nil {
		return 0, err
	}
	annotations, err := receiptItem(result, "annotations")
	if err != nil {
		return 0, err
	}
	if err := ApplyRowAnnotations(messages, annotations); err != nil {
		return 0, err
	}
	count, err := receiptItem(result, "count")
	if err != nil {
		return 0, err
	}
	n, err := receiptInt(count)
	return int(n), err
}

// One turn lease acquire through the owner
func (s *RuntimeSessionStore) TryAcquireSessionTurnLease(sessionID, holder string, ttlSeconds float64, patienceS *float64) (bool, error) {
	if err := s.session(sessionID); err != nil {
		return false, err
	}
	value, err := s.applyValue(context.Background(), "turn.acquire", pyjson.ObjectOf("holder", holder, "ttl_seconds", ttlSeconds))
	return pyrepr.Truthy(value), err
}

// Polls the owner for the turn lease with the canonical waiting policy
func (s *RuntimeSessionStore) AcquireSessionTurnLease(sessionID, holder string, o state.TurnLeaseWait) (bool, error) {
	return state.AcquireTurnLeasePolling(s.kind, s.TryAcquireSessionTurnLease, nil, sessionID, holder, o)
}

// Renews the turn lease through the owner
func (s *RuntimeSessionStore) RefreshSessionTurnLease(sessionID, holder string, ttlSeconds float64, patienceS *float64) (bool, error) {
	if err := s.session(sessionID); err != nil {
		return false, err
	}
	value, err := s.applyValue(context.Background(), "turn.renew", pyjson.ObjectOf("holder", holder, "ttl_seconds", ttlSeconds))
	return pyrepr.Truthy(value), err
}

// Releases the turn lease under the admission target even after physical rotation
func (s *RuntimeSessionStore) ReleaseSessionTurnLease(sessionID, holder string) error {
	_, err := s.apply(context.Background(), "turn.cleanup", pyjson.ObjectOf("target", sessionID, "holder", holder))
	return err
}

// Main loop usage delta as the update_token_counts keyword arguments
func tokenDeltaPayload(d state.TokenCountDelta) *pyjson.Object {
	return pyjson.ObjectOf("input_tokens", d.InputTokens, "output_tokens", d.OutputTokens, "model", noneIfNil(d.Model),
		"cache_read_tokens", d.CacheReadTokens, "cache_write_tokens", d.CacheWriteTokens, "reasoning_tokens", d.ReasoningTokens,
		"estimated_cost_usd", noneIfNil(d.EstimatedCostUSD), "actual_cost_usd", noneIfNil(d.ActualCostUSD),
		"cost_status", noneIfNil(d.CostStatus), "cost_source", noneIfNil(d.CostSource), "pricing_version", noneIfNil(d.PricingVersion),
		"billing_provider", noneIfNil(d.BillingProvider), "billing_base_url", noneIfNil(d.BillingBaseURL), "billing_mode", noneIfNil(d.BillingMode),
		"api_call_count", int64(d.APICallCount), "absolute", d.Absolute, "source", noneIfNil(d.Source), "task", d.Task)
}

// Records a main loop usage delta through the owner
func (s *RuntimeSessionStore) QueueTokenCounts(sessionID string, delta state.TokenCountDelta) error {
	if err := s.session(sessionID); err != nil {
		return err
	}
	_, err := s.apply(context.Background(), "usage.main", tokenDeltaPayload(delta))
	return err
}

// Records one auxiliary model call through the owner
func (s *RuntimeSessionStore) RecordAuxiliaryUsage(ctx context.Context, sessionID, task string, usage state.AuxiliaryUsage) error {
	if err := s.session(sessionID); err != nil {
		return err
	}
	payload := pyjson.ObjectOf("model", noneIfNil(usage.Model), "billing_provider", noneIfNil(usage.BillingProvider),
		"billing_base_url", noneIfNil(usage.BillingBaseURL), "input_tokens", usage.InputTokens, "output_tokens", usage.OutputTokens,
		"cache_read_tokens", usage.CacheReadTokens, "cache_write_tokens", usage.CacheWriteTokens, "reasoning_tokens", usage.ReasoningTokens,
		"estimated_cost_usd", noneIfNil(usage.EstimatedCostUSD))
	if usage.APICallCount != nil {
		payload.Set("api_call_count", int64(*usage.APICallCount))
	}
	payload.Set("task", task)
	_, err := s.apply(ctx, "usage.auxiliary", payload)
	return err
}

// Session row as the receipt reads it, AttributeError when the owner answered None
func (s *RuntimeSessionStore) sessionRow(ctx context.Context, sessionID string) (*pyjson.Object, error) {
	row, err := s.GetSession(ctx, sessionID)
	if err != nil {
		return nil, err
	}
	if row == nil {
		return nil, &workerStoreError{"AttributeError", "'NoneType' object has no attribute 'get'"}
	}
	return row, nil
}

// Title of the session row
func (s *RuntimeSessionStore) GetSessionTitle(ctx context.Context, sessionID string) (string, bool, error) {
	row, err := s.sessionRow(ctx, sessionID)
	if err != nil {
		return "", false, err
	}
	title := row.GetOr("title", nil)
	if title == nil {
		return "", false, nil
	}
	text, err := receiptText(title)
	return text, err == nil, err
}

// Role of the newest active conversation row, the failed turn boundary
func (s *RuntimeSessionStore) LatestConversationRole(ctx context.Context, sessionID string) (string, error) {
	if err := s.session(sessionID); err != nil {
		return "", err
	}
	value, err := s.applyValue(ctx, "session.tail_role", pyjson.NewObject())
	if err != nil {
		return "", err
	}
	return receiptText(value)
}

// Role of one row of this transcript, false when the row is absent
func (s *RuntimeSessionStore) GetMessageRole(ctx context.Context, sessionID string, rowID int64) (string, bool, error) {
	if err := s.session(sessionID); err != nil {
		return "", false, err
	}
	value, err := s.applyValue(ctx, "session.row_role", pyjson.ObjectOf("row_id", rowID))
	if err != nil || value == nil {
		return "", false, err
	}
	text, err := receiptText(value)
	return text, err == nil, err
}

// Exact stored cooldown columns of the session row
func (s *RuntimeSessionStore) GetCompressionFailureCooldownRow(sessionID string) (state.CompressionCooldownRow, error) {
	row, err := s.sessionRow(context.Background(), sessionID)
	if err != nil {
		return state.CompressionCooldownRow{}, err
	}
	out := state.CompressionCooldownRow{SessionExists: true, Error: row.GetOr("compression_failure_error", nil)}
	if v := row.GetOr("compression_failure_cooldown_until", nil); v != nil {
		f, err := pyFloatValue(v)
		if err != nil {
			return state.CompressionCooldownRow{}, err
		}
		out.CooldownUntil = &f
	}
	return out, nil
}

// float() of a receipt number or numeric text
func pyFloatValue(v any) (float64, error) {
	switch n := v.(type) {
	case float64:
		return n, nil
	case int64:
		return float64(n), nil
	case bool:
		if n {
			return 1, nil
		}
		return 0, nil
	case string:
		f, err := strconv.ParseFloat(strings.TrimSpace(n), 64)
		if err != nil {
			return 0, &workerStoreError{"ValueError", fmt.Sprintf("could not convert string to float: %s", pyrepr.Repr(n))}
		}
		return f, nil
	}
	return 0, &workerStoreError{"TypeError", fmt.Sprintf("float() argument must be a string or a real number, not '%s'", pyrepr.TypeName(v))}
}

// Unexpired cooldown with its remaining seconds, nil when none is live
func (s *RuntimeSessionStore) GetCompressionFailureCooldown(sessionID string) (*pyjson.Object, error) {
	now := float64(time.Now().UnixNano()) / 1e9
	row, err := s.GetCompressionFailureCooldownRow(sessionID)
	if err != nil {
		return nil, err
	}
	if row.CooldownUntil == nil || *row.CooldownUntil <= now {
		return nil, nil
	}
	return pyjson.ObjectOf("cooldown_until", *row.CooldownUntil, "remaining_seconds", *row.CooldownUntil-now, "error", row.Error), nil
}

// Live cooldown in the compressor's typed shape
func (s *RuntimeSessionStore) CompressionCooldown(sessionID string) (state.CompressionCooldown, error) {
	row, err := s.GetCompressionFailureCooldown(sessionID)
	if err != nil || row == nil {
		return state.CompressionCooldown{}, err
	}
	out := state.CompressionCooldown{Until: row.GetOr("cooldown_until", 0.0).(float64), RemainingSeconds: row.GetOr("remaining_seconds", 0.0).(float64)}
	if v := row.GetOr("error", nil); v != nil {
		out.Error = pyrepr.Str(v)
	}
	return out, nil
}

// Clamped numeric column of the session row, zero when the value does not convert
func (s *RuntimeSessionStore) sessionNumber(sessionID, column string, integral bool) (float64, error) {
	row, err := s.sessionRow(context.Background(), sessionID)
	if err != nil {
		return 0, err
	}
	raw := row.GetOr(column, nil)
	if !pyrepr.Truthy(raw) {
		return 0, nil
	}
	var f float64
	switch n := raw.(type) {
	case float64:
		f = n
		if integral {
			f = math.Trunc(n)
		}
	case string:
		if !integral {
			parsed, err := pyFloatValue(n)
			if err != nil {
				return 0, nil
			}
			f = parsed
		} else {
			i, err := strconv.ParseInt(strings.TrimSpace(n), 10, 64)
			if err != nil {
				return 0, nil
			}
			f = float64(i)
		}
	default:
		parsed, err := pyFloatValue(raw)
		if err != nil {
			return 0, nil
		}
		f = parsed
	}
	if f > 0 {
		return f, nil
	}
	return 0, nil
}

// Persisted compression fallback streak
func (s *RuntimeSessionStore) CompressionFallbackStreak(sessionID string) (int, error) {
	v, err := s.sessionNumber(sessionID, "compression_fallback_streak", true)
	return int(v), err
}

// Persisted ineffective compression count
func (s *RuntimeSessionStore) CompressionIneffectiveCount(sessionID string) (int, error) {
	v, err := s.sessionNumber(sessionID, "compression_ineffective_count", true)
	return int(v), err
}

// Persisted compression recovery deadline
func (s *RuntimeSessionStore) CompressionRecoveryDeadline(sessionID string) (float64, error) {
	return s.sessionNumber(sessionID, "compression_recovery_deadline", false)
}

// One model_config key of the session row, the default when absent
func (s *RuntimeSessionStore) GetSessionModelConfigValue(ctx context.Context, sessionID, key string, def any) (any, error) {
	row, err := s.sessionRow(ctx, sessionID)
	if err != nil {
		return nil, err
	}
	config := state.ParseModelConfig(row.GetOr("model_config", nil))
	if v, ok := config[key]; ok {
		return v, nil
	}
	return def, nil
}

// One model_config key and whether the row holds it, the canonical store's error swallowing read
func (s *RuntimeSessionStore) GetModelConfigValue(sessionID, key string) (any, bool, error) {
	row, err := s.sessionRow(context.Background(), sessionID)
	if err != nil {
		return nil, false, err
	}
	v, ok := state.ParseModelConfig(row.GetOr("model_config", nil))[key]
	return v, ok, nil
}

// One model_config key for the rejected thinking reader, absent on any failure
func (s *RuntimeSessionStore) ModelConfigValue(sessionID, key string) (any, bool) {
	v, ok, err := s.GetModelConfigValue(sessionID, key)
	if err != nil {
		return nil, false
	}
	return v, ok
}

// Read modify write of one model_config key on the assigned row, the owner applying the patch
func (s *RuntimeSessionStore) UpdateConversationModelConfigValue(sessionID, key string, update func(current any) any) (string, error) {
	if err := s.session(sessionID); err != nil {
		return "", err
	}
	current, err := s.GetSessionModelConfigValue(context.Background(), sessionID, key, nil)
	if err != nil {
		return "", err
	}
	if err := s.PatchSessionModelConfig(context.Background(), sessionID, map[string]any{key: update(current)}); err != nil {
		return "", err
	}
	return sessionID, nil
}

// The assigned session when its model_config holds the key, the only row a worker may scan
func (s *RuntimeSessionStore) SessionIDsWithModelConfigKey(key string) ([]string, error) {
	sid := s.scopeSessionID()
	v, err := s.GetSessionModelConfigValue(context.Background(), sid, key, nil)
	if err != nil {
		return nil, err
	}
	if v == nil {
		return []string{}, nil
	}
	return []string{sid}, nil
}

// Stores the session's system prompt through the owner
func (s *RuntimeSessionStore) UpdateSystemPrompt(ctx context.Context, sessionID string, systemPrompt *string) error {
	if err := s.session(sessionID); err != nil {
		return err
	}
	_, err := s.apply(ctx, "session.prompt", pyjson.ObjectOf("system_prompt", noneIfNil(systemPrompt)))
	return err
}

// Merges model_config sidecar keys through the owner, a None value removing its key
func (s *RuntimeSessionStore) PatchSessionModelConfig(ctx context.Context, sessionID string, patch map[string]any) error {
	if err := s.session(sessionID); err != nil {
		return err
	}
	obj, _ := pyjson.AsObject(patch)
	_, err := s.apply(ctx, "session.sidecars", pyjson.ObjectOf("patch", obj))
	return err
}

// Model config patch in the rejected thinking store shape
func (s *RuntimeSessionStore) PatchModelConfig(sessionID string, patch map[string]any) error {
	return s.PatchSessionModelConfig(context.Background(), sessionID, patch)
}

// Freezes the tools pin through the owner, None clearing it
func (s *RuntimeSessionStore) UpdateSessionToolNames(sessionID string, pin any) error {
	if err := s.session(sessionID); err != nil {
		return err
	}
	_, err := s.apply(context.Background(), "session.tools", pyjson.ObjectOf("tool_names", pin))
	return err
}
