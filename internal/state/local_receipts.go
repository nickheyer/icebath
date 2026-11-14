package state

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
)

// Meta key prefixes of an API session binding, its declared conversation key and a legacy local binding
const (
	apiBindingPrefix  = "gateway.api.binding.v1."
	apiDeclaredPrefix = "gateway.api.conversation.v1."
	localLegacyPrefix = "gateway.local_legacy.v1:"
)

// Text member of a JSON object, false when absent or not a string
func memberText(members []configMember, key string) (string, bool) {
	v, ok := memberValue(members, key)
	if !ok || !jsonIsString(v) {
		return "", false
	}
	return jsonString(v), true
}

// String list a JSON array holds, false for anything else or any non-string element
func stringList(raw json.RawMessage) ([]string, bool) {
	elements, ok := arrayElements(raw)
	if !ok {
		return nil, false
	}
	out := make([]string, len(elements))
	for i, e := range elements {
		if !jsonIsString(e) {
			return nil, false
		}
		out[i] = jsonString(e)
	}
	return out, true
}

// Stored creation receipt of a local owner, nil when none is stored and a storage refusal when it is not an object
func storedLocalReceipt(ctx context.Context, c Conn, owner string) ([]configMember, error) {
	saved, ok, err := metaValue(ctx, c, localPolicyPrefix+owner)
	if err != nil || !ok {
		return nil, err
	}
	members, isObject := objectMembers(json.RawMessage(saved))
	if !isObject {
		return nil, runtimeErr(ReasonStorageUnavailable)
	}
	return members, nil
}

// Session id the routing entry of a receipt names, empty when it names none
func receiptEntryTarget(receipt []configMember) string {
	raw, _ := memberValue(receipt, "entry")
	entry, _ := objectMembers(raw)
	target, _ := memberText(entry, "session_id")
	return target
}

// Root segment of a receipt's lineage, a legacy binding having to match its stored record exactly
func legacyLineageRoot(ctx context.Context, c Conn, receipt []configMember) (string, error) {
	sessionID, ok := memberText(receipt, "session_id")
	if !ok {
		return "", runtimeErr(ReasonStorageUnavailable)
	}
	legacy, bound := memberValue(receipt, "legacy_session_id")
	if !bound {
		return sessionID, nil
	}
	if !jsonIsString(legacy) || jsonString(legacy) != sessionID {
		return "", runtimeErr(ReasonStorageUnavailable)
	}
	saved, found, err := metaValue(ctx, c, localLegacyPrefix+sessionID)
	if err != nil {
		return "", err
	}
	var expected []configMember
	for _, key := range []string{"session_id", "profile_id", "principal_id", "legacy_session_id"} {
		v, has := memberValue(receipt, key)
		if !has {
			return "", runtimeErr(ReasonStorageUnavailable)
		}
		expected = append(expected, configMember{key, v})
	}
	if !found || !json.Valid([]byte(saved)) || jsonCompareKey(json.RawMessage(saved)) != jsonCompareKey(rawObject(expected)) {
		return "", runtimeErr(ReasonStorageUnavailable)
	}
	return sessionID, nil
}

// Segment list of a receipt, the root alone when it records none, false when the member is not a list of strings
func receiptLineage(receipt []configMember, root string) ([]string, bool) {
	raw, has := memberValue(receipt, "lineage")
	if !has {
		return []string{root}, true
	}
	return stringList(raw)
}

// Proves a receipt's lineage against the session rows and returns its current physical target
func validateLocalLineage(ctx context.Context, c Conn, receipt []configMember) (string, error) {
	unavailable := runtimeErr(ReasonStorageUnavailable)
	sessionID, ok := memberText(receipt, "session_id")
	if !ok {
		return "", unavailable
	}
	if _, legacy := memberValue(receipt, "legacy_session_id"); !legacy {
		profile, ok1 := memberText(receipt, "profile_id")
		principal, ok2 := memberText(receipt, "principal_id")
		request, ok3 := memberText(receipt, "request_id")
		identity, err := LocalIdentity(profile, principal, request)
		if err != nil {
			return "", err
		}
		if !ok1 || !ok2 || !ok3 || sessionID != identity {
			return "", unavailable
		}
	}
	root, err := legacyLineageRoot(ctx, c, receipt)
	if err != nil {
		return "", err
	}
	lineage, ok := receiptLineage(receipt, root)
	if !ok || len(lineage) == 0 || lineage[0] != root || lineage[len(lineage)-1] != receiptEntryTarget(receipt) {
		return "", unavailable
	}
	seen := map[string]bool{}
	for _, id := range lineage {
		if seen[id] {
			return "", unavailable
		}
		seen[id] = true
	}
	for i := 1; i < len(lineage); i++ {
		parentID, childID := lineage[i-1], lineage[i]
		var reason, parent, config sql.NullString
		err := c.QueryRowContext(ctx, "SELECT end_reason FROM sessions WHERE id=?", parentID).Scan(&reason)
		if err == nil {
			err = c.QueryRowContext(ctx, "SELECT parent_session_id,model_config FROM sessions WHERE id=?", childID).Scan(&parent, &config)
		}
		if errors.Is(err, sql.ErrNoRows) {
			return "", unavailable
		}
		if err != nil {
			return "", err
		}
		if parent.String != parentID {
			return "", unavailable
		}
		valid := false
		if reason.String == "session_reset" {
			text := config.String
			if text == "" {
				text = "{}"
			}
			members, isObject := objectMembers(json.RawMessage(text))
			if !isObject {
				return "", unavailable
			}
			marker, _ := memberText(members, "_reset_from")
			valid = marker == parentID
		} else {
			step, err := chainStep(ctx, c, parentID)
			if err != nil {
				return "", err
			}
			valid = reason.String == "compression" && step == childID
		}
		if !valid {
			return "", unavailable
		}
	}
	step, err := chainStep(ctx, c, lineage[len(lineage)-1])
	if err != nil {
		return "", err
	}
	if step != "" {
		return "", unavailable
	}
	return lineage[len(lineage)-1], nil
}

// Current transcript of a local owner read on the caller's transaction, the id itself outside a local receipt
func localPhysicalTarget(ctx context.Context, c Conn, sessionID string) (string, error) {
	receipt, err := storedLocalReceipt(ctx, c, sessionID)
	if err != nil || receipt == nil {
		return sessionID, err
	}
	if owner, _ := memberText(receipt, "session_id"); owner != sessionID {
		return "", runtimeErr(ReasonStorageUnavailable)
	}
	return validateLocalLineage(ctx, c, receipt)
}

// Current physical target a creation receipt routes to, empty for a malformed receipt
func policyTarget(raw string) string {
	members, ok := objectMembers(json.RawMessage(raw))
	if !ok {
		return ""
	}
	return receiptEntryTarget(members)
}

// Key and value of every meta entry whose key matches a GLOB pattern
func metaGlob(ctx context.Context, c Conn, pattern string) ([]MetaEntry, error) {
	rows, err := c.QueryContext(ctx, "SELECT key,value FROM state_meta WHERE key GLOB ?", pattern)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []MetaEntry
	for rows.Next() {
		var k, v sql.NullString
		if err := rows.Scan(&k, &v); err != nil {
			return nil, err
		}
		out = append(out, MetaEntry{Key: k.String, Value: v.String})
	}
	return out, rows.Err()
}

// Retires the owner receipts of deleted sessions, a creation policy with its current target and an API binding with its id
func retireLocalReceipts(ctx context.Context, c Conn, sessionIDs []string) error {
	if len(sessionIDs) == 0 {
		return nil
	}
	targets := map[string]bool{}
	for _, id := range sessionIDs {
		targets[id] = true
	}
	var gone []string
	policies, err := metaGlob(ctx, c, localPolicyPrefix+"*")
	if err != nil {
		return err
	}
	for _, e := range policies {
		if target := policyTarget(e.Value); target != "" && targets[target] {
			gone = append(gone, e.Key)
		}
	}
	declared, err := metaGlob(ctx, c, apiDeclaredPrefix+"*")
	if err != nil {
		return err
	}
	for _, e := range declared {
		if targets[e.Value] {
			gone = append(gone, e.Key)
		}
	}
	for _, id := range sessionIDs {
		gone = append(gone, apiBindingPrefix+id)
	}
	for _, key := range gone {
		if _, err := c.ExecContext(ctx, "DELETE FROM state_meta WHERE key=?", key); err != nil {
			return err
		}
	}
	return nil
}
