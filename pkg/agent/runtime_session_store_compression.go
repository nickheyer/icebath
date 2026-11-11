package agent

import (
	"context"
	"fmt"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/state"
	"github.com/nickheyer/icebath/pkg/tools"
)

// Live compaction dicts projected onto the owner's closed handoff fields, a None row passing through
func storeHandoffRows(messages []map[string]any) []any {
	out := make([]any, 0, len(messages))
	for _, m := range messages {
		if m == nil {
			out = append(out, nil)
			continue
		}
		out = append(out, storeProjectRow(m, false))
	}
	return out
}

// Held rows the owner re-verifies, the handoff fields plus the persistence marker of each dict
func storeCASRows(held []map[string]any) []any {
	out := make([]any, 0, len(held))
	for _, m := range held {
		if m != nil {
			out = append(out, storeProjectRow(m, true))
		}
	}
	return out
}

func storeProjectRow(m map[string]any, persisted bool) map[string]any {
	row := map[string]any{}
	for k, v := range m {
		if state.HandoffMessageFields[k] || (persisted && k == "_db_persisted") {
			row[k] = v
		}
	}
	return row
}

// Copies of the dict messages, None for a nil list
func storeDictCopies(messages []map[string]any) any {
	if messages == nil {
		return nil
	}
	out := make([]any, 0, len(messages))
	for _, m := range messages {
		if m == nil {
			continue
		}
		c := make(map[string]any, len(m))
		for k, v := range m {
			c[k] = v
		}
		out = append(out, c)
	}
	return out
}

// Appends under the compression lock or a custom turn lease through the owner
func (s *RuntimeSessionStore) appendCompressionMessages(ctx context.Context, sessionID string, messages []map[string]any, lockHolder, turnLeaseHolder *string, ttl float64) (int, error) {
	if err := s.session(sessionID); err != nil {
		return 0, err
	}
	result, err := s.apply(ctx, "compression.append", pyjson.ObjectOf("messages", messagesPayload(messages),
		"compression_lock_holder", noneIfNil(lockHolder), "turn_lease_holder", noneIfNil(turnLeaseHolder), "turn_lease_ttl_seconds", ttl))
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

// One lineage receipt field for the target
func (s *RuntimeSessionStore) lineageField(ctx context.Context, sessionID, field string) (any, error) {
	result, err := s.apply(ctx, "compression.lineage", pyjson.ObjectOf("target", sessionID))
	if err != nil {
		return nil, err
	}
	return receiptItem(result, field)
}

// Session row readable from this assignment, a lineage member checked before any foreign read
func (s *RuntimeSessionStore) GetSession(ctx context.Context, sessionID string) (*pyjson.Object, error) {
	if !pyjson.Equal(sessionID, s.scope.GetOr("session_id", nil)) {
		ids, err := s.lineageField(ctx, s.scopeSessionID(), "readable_ids")
		if err != nil {
			return nil, err
		}
		list, _ := ids.([]any)
		readable := false
		for _, id := range list {
			if pyjson.Equal(id, sessionID) {
				readable = true
				break
			}
		}
		if !readable {
			return nil, tools.NewWorkerPersistenceError("permission_denied")
		}
	}
	result, err := s.apply(ctx, "compression.context", pyjson.ObjectOf("target", sessionID))
	if err != nil {
		return nil, err
	}
	row, err := receiptItem(result, "session")
	if err != nil || row == nil {
		return nil, err
	}
	obj, ok := row.(*pyjson.Object)
	if !ok {
		return nil, &workerStoreError{"TypeError", fmt.Sprintf("session receipt is a %s, not a dict", pyrepr.TypeName(row))}
	}
	return obj, nil
}

// Text items of a receipt list
func receiptStrings(raw any) ([]string, error) {
	list, ok := raw.([]any)
	if !ok {
		return nil, &workerStoreError{"TypeError", fmt.Sprintf("'%s' object is not a list", pyrepr.TypeName(raw))}
	}
	out := make([]string, 0, len(list))
	for _, item := range list {
		text, err := receiptText(item)
		if err != nil {
			return nil, err
		}
		out = append(out, text)
	}
	return out, nil
}

// Compression lineage of the target
func (s *RuntimeSessionStore) GetCompressionLineage(ctx context.Context, sessionID string) ([]string, error) {
	raw, err := s.lineageField(ctx, sessionID, "lineage")
	if err != nil {
		return nil, err
	}
	return receiptStrings(raw)
}

// Conversation root of the target
func (s *RuntimeSessionStore) GetConversationRoot(sessionID string) (string, error) {
	raw, err := s.lineageField(context.Background(), sessionID, "root")
	if err != nil {
		return "", err
	}
	return receiptText(raw)
}

// Live compression tip of the target
func (s *RuntimeSessionStore) GetCompressionTip(ctx context.Context, sessionID string) (string, error) {
	raw, err := s.lineageField(ctx, sessionID, "tip")
	if err != nil {
		return "", err
	}
	return receiptText(raw)
}

// Session a resume of the target lands on
func (s *RuntimeSessionStore) ResolveResumeSessionID(sessionID string) (string, error) {
	raw, err := s.lineageField(context.Background(), sessionID, "resume")
	if err != nil {
		return "", err
	}
	return receiptText(raw)
}

// Declared fork flag and scope root of the target
func (s *RuntimeSessionStore) DeclaredScopeIdentity(ctx context.Context, sessionID string) (bool, string, error) {
	raw, err := s.lineageField(ctx, sessionID, "identity")
	if err != nil {
		return false, "", err
	}
	list, ok := raw.([]any)
	if !ok || len(list) != 2 {
		return false, "", &workerStoreError{"ValueError", "identity receipt is not a pair"}
	}
	root, err := receiptText(list[1])
	return pyrepr.Truthy(list[0]), root, err
}

// Whether the target is an explicit fork child
func (s *RuntimeSessionStore) IsExplicitForkChild(ctx context.Context, sessionID string) (bool, error) {
	fork, _, err := s.DeclaredScopeIdentity(ctx, sessionID)
	return fork, err
}

// Newest conversation boundary of the routing key and source
func (s *RuntimeSessionStore) LatestConversationBoundary(ctx context.Context, sessionKey, source string) (int64, bool, error) {
	value, err := s.applyValue(ctx, "compression.boundary", pyjson.ObjectOf("session_key", sessionKey, "source", source))
	if err != nil || value == nil {
		return 0, false, err
	}
	n, err := receiptInt(value)
	return n, err == nil, err
}

// Durable conversation of the target as the owner reads it
func (s *RuntimeSessionStore) GetMessagesAsConversation(ctx context.Context, sessionID string, o state.ConversationOptions) ([]map[string]any, error) {
	result, err := s.apply(ctx, "compression.history", pyjson.ObjectOf("target", sessionID, "include_ancestors", o.IncludeAncestors,
		"include_inactive", o.IncludeInactive, "repair_alternation", o.RepairAlternation, "include_row_ids", o.IncludeRowIDs,
		"include_compacted", o.IncludeCompacted))
	if err != nil {
		return nil, err
	}
	raw, err := receiptItem(result, "messages")
	if err != nil {
		return nil, err
	}
	return receiptMessages(raw)
}

// Newest active message id of the assigned transcript
func (s *RuntimeSessionStore) GetActiveMessageWatermark(ctx context.Context, sessionID string) (int64, error) {
	if err := s.session(sessionID); err != nil {
		return 0, err
	}
	value, err := s.applyValue(ctx, "compression.watermark", pyjson.NewObject())
	if err != nil {
		return 0, err
	}
	return receiptInt(value)
}

// Archives the compacted transcript through the owner, which proves carried and held rows in its commit
func (s *RuntimeSessionStore) ArchiveAndCompact(ctx context.Context, sessionID string, compacted []map[string]any, o state.ArchiveCompactOptions) (int, error) {
	if err := s.session(sessionID); err != nil {
		return 0, err
	}
	carried := storeDictCopies(o.CarriedMessages)
	if carried == nil {
		carried = []any{}
	}
	var cas any
	if o.HeldForCAS != nil {
		cas = storeCASRows(o.HeldForCAS)
	}
	var covered any
	if o.CoveredIDs != nil {
		ids := make([]any, len(o.CoveredIDs))
		for i, id := range o.CoveredIDs {
			ids[i] = id
		}
		covered = ids
	}
	var patch any
	if o.ModelConfigPatch != nil {
		patch = o.ModelConfigPatch
	}
	result, err := s.apply(ctx, "compression.archive", pyjson.ObjectOf("messages", storeHandoffRows(compacted),
		"model_config_patch", patch, "watermark", noneIfNil(o.Watermark), "lock_holder", noneIfNil(o.LockHolder),
		"tail_count", int64(o.TailCount), "carried_messages", carried, "covered_ids", covered,
		"unresolved_held", storeDictCopies(o.UnresolvedHeld), "held_for_cas", cas))
	if err != nil {
		return 0, err
	}
	annotations, err := receiptItem(result, "annotations")
	if err != nil {
		return 0, err
	}
	if err := ApplyRowAnnotations(compacted, annotations); err != nil {
		return 0, err
	}
	value, err := receiptItem(result, "value")
	if err != nil {
		return 0, err
	}
	n, err := receiptInt(value)
	return int(n), err
}

// Publishes the compression child through the owner, mirroring the stamps onto the handed messages
func (s *RuntimeSessionStore) PublishCompressionChild(ctx context.Context, p state.CompressionChildPublish) error {
	if err := s.session(p.ParentSessionID); err != nil {
		return err
	}
	var config any
	if p.ModelConfig != nil {
		config = p.ModelConfig
	}
	result, err := s.apply(ctx, "compression.publish", pyjson.ObjectOf("child_session_id", p.ChildSessionID, "source", p.Source,
		"messages", storeHandoffRows(p.Messages), "model", noneIfNil(p.Model), "model_config", config, "system_prompt", noneIfNil(p.SystemPrompt),
		"cwd", noneIfNil(p.Cwd), "profile_name", noneIfNil(p.ProfileName), "compression_lock_holder", noneIfNil(p.CompressionLockHolder),
		"require_compression_lease", p.RequireCompressionLease, "require_lease_refresh", p.RequireLeaseRefresh,
		"lease_ttl_seconds", p.LeaseTTLSeconds, "watermark", noneIfNil(p.Watermark), "watermark_ceiling", noneIfNil(p.WatermarkCeiling)))
	if err != nil {
		return err
	}
	annotations, err := receiptItem(result, "annotations")
	if err != nil {
		return err
	}
	return ApplyRowAnnotations(p.Messages, annotations)
}

// Acquires the compression lock through the owner, raising what the owner refused
func (s *RuntimeSessionStore) TryAcquireCompressionLockChecked(sessionID, holder string, ttlSeconds float64) (bool, error) {
	if err := s.session(sessionID); err != nil {
		return false, err
	}
	value, err := s.applyValue(context.Background(), "compression.lock.acquire", pyjson.ObjectOf("holder", holder, "ttl_seconds", ttlSeconds))
	return pyrepr.Truthy(value), err
}

// Renews the compression lock through the owner
func (s *RuntimeSessionStore) RefreshCompressionLockChecked(sessionID, holder string, ttlSeconds float64) (bool, error) {
	if err := s.session(sessionID); err != nil {
		return false, err
	}
	value, err := s.applyValue(context.Background(), "compression.lock.renew", pyjson.ObjectOf("holder", holder, "ttl_seconds", ttlSeconds))
	return pyrepr.Truthy(value), err
}

// Reopens an orphaned compression session through the owner
func (s *RuntimeSessionStore) ReopenOrphanedCompressionSession(ctx context.Context, sessionID string) (bool, error) {
	if err := s.session(sessionID); err != nil {
		return false, err
	}
	value, err := s.applyValue(ctx, "compression.reopen", pyjson.NewObject())
	return pyrepr.Truthy(value), err
}

// Releases the compression lock under the admission target
func (s *RuntimeSessionStore) ReleaseCompressionLockChecked(sessionID, holder string) error {
	_, err := s.apply(context.Background(), "compression.cleanup", pyjson.ObjectOf("target", sessionID, "holder", holder))
	return err
}

// Live compression lock holder of the assigned session
func (s *RuntimeSessionStore) GetCompressionLockHolder(sessionID string) (*string, error) {
	if err := s.session(sessionID); err != nil {
		return nil, err
	}
	value, err := s.applyValue(context.Background(), "compression.lock.holder", pyjson.NewObject())
	if err != nil || value == nil {
		return nil, err
	}
	text, err := receiptText(value)
	return &text, err
}

// Records a compression failure cooldown through the owner
func (s *RuntimeSessionStore) RecordCompressionFailureCooldownChecked(sessionID string, cooldownUntil float64, errValue *string) error {
	if err := s.session(sessionID); err != nil {
		return err
	}
	_, err := s.apply(context.Background(), "compression.cooldown.record", pyjson.ObjectOf("cooldown_until", cooldownUntil, "error", noneIfNil(errValue)))
	return err
}

// Cooldown write in the compressor's typed shape, an empty reason meaning None
func (s *RuntimeSessionStore) SetCompressionCooldown(sessionID string, until float64, reason string) error {
	var errValue *string
	if reason != "" {
		errValue = &reason
	}
	return s.RecordCompressionFailureCooldownChecked(sessionID, until, errValue)
}

// Restores exact cooldown columns through the owner
func (s *RuntimeSessionStore) RestoreCompressionFailureCooldownRow(sessionID string, snapshot state.CompressionCooldownRow) error {
	if err := s.session(sessionID); err != nil {
		return err
	}
	_, err := s.apply(context.Background(), "compression.cooldown.restore", pyjson.ObjectOf("snapshot", pyjson.ObjectOf(
		"session_exists", snapshot.SessionExists, "cooldown_until", noneIfNil(snapshot.CooldownUntil), "error", snapshot.Error)))
	return err
}

// Clears the cooldown through the owner
func (s *RuntimeSessionStore) ClearCompressionFailureCooldownChecked(sessionID string) error {
	if err := s.session(sessionID); err != nil {
		return err
	}
	_, err := s.apply(context.Background(), "compression.cooldown.clear", pyjson.NewObject())
	return err
}

// Cooldown clear in the compressor's typed shape
func (s *RuntimeSessionStore) ClearCompressionCooldown(sessionID string) error {
	return s.ClearCompressionFailureCooldownChecked(sessionID)
}

// Persists the fallback streak, never below zero
func (s *RuntimeSessionStore) SetCompressionFallbackStreak(sessionID string, streak int) error {
	if err := s.session(sessionID); err != nil {
		return err
	}
	_, err := s.apply(context.Background(), "compression.fallback", pyjson.ObjectOf("value", int64(max(0, streak))))
	return err
}

// Persists the ineffective count, never below zero
func (s *RuntimeSessionStore) SetCompressionIneffectiveCount(sessionID string, count int) error {
	if err := s.session(sessionID); err != nil {
		return err
	}
	_, err := s.apply(context.Background(), "compression.ineffective", pyjson.ObjectOf("value", int64(max(0, count))))
	return err
}

// Persists the recovery deadline, a zero deadline clearing it
func (s *RuntimeSessionStore) SetCompressionRecoveryDeadline(sessionID string, deadline float64) error {
	if err := s.session(sessionID); err != nil {
		return err
	}
	value := max(0.0, deadline)
	var payload any = value
	if value == 0 {
		payload = nil
	}
	_, err := s.apply(context.Background(), "compression.recovery", pyjson.ObjectOf("value", payload))
	return err
}
