package state

import (
	"context"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
)

const LocalLegacyPrefix = "gateway.local_legacy.v1:"

// Refusal carrying only its reason
func runtimeRefusal(reason string) error { return NewRuntimeStoreError(reason, nil, nil) }

// Canonical runtime JSON of a dict, sorted compact and non ASCII kept, invalid_params for anything else
func runtimeJSON(value any) (string, error) {
	if _, ok := value.(*pyjson.Object); !ok {
		return "", runtimeRefusal("invalid_params")
	}
	text, err := pyjson.Dumps(value, pyjson.EnsureASCII(false), pyjson.SortKeys(true), pyjson.Separators(",", ":"), pyjson.AllowNaN(false))
	if err != nil {
		return "", runtimeRefusal("invalid_params")
	}
	return text, nil
}

// Item of a decoded receipt dict, a KeyError or TypeError as subscripting raises
func receiptItem(v any, key string) (any, error) {
	obj, ok := v.(*pyjson.Object)
	if !ok {
		return nil, &stateTypedError{"TypeError", "receipt is not subscriptable by key"}
	}
	item, ok := obj.Get(key)
	if !ok {
		return nil, &stateTypedError{"KeyError", pyRepr(key)}
	}
	return item, nil
}

// Lineage root of a receipt, the legacy session id only when its migration record matches exactly
func legacyLineageRoot(ctx context.Context, conn SQLiteConn, receipt *pyjson.Object) (any, error) {
	sessionID, err := receiptItem(receipt, "session_id")
	if err != nil {
		return nil, err
	}
	legacy, hasLegacy := receipt.Get("legacy_session_id")
	if !hasLegacy {
		return sessionID, nil
	}
	if !pyrepr.Equal(legacy, sessionID) {
		return nil, runtimeRefusal("storage_unavailable")
	}
	expected := pyjson.NewObject()
	for _, k := range []string{"session_id", "profile_id", "principal_id", "legacy_session_id"} {
		v, err := receiptItem(receipt, k)
		if err != nil {
			return nil, err
		}
		expected.Set(k, v)
	}
	row, err := connRow(ctx, conn, "SELECT value FROM state_meta WHERE key=?", LocalLegacyPrefix+pyStr(legacy))
	if err != nil {
		return nil, err
	}
	if row == nil {
		return nil, runtimeRefusal("storage_unavailable")
	}
	text, _ := row["value"].(string)
	stored, err := pyjson.Loads(text)
	if err != nil {
		return nil, pyValueError(err.Error())
	}
	if !pyrepr.Equal(stored, expected) {
		return nil, runtimeRefusal("storage_unavailable")
	}
	return legacy, nil
}

// Validates a receipt's recorded lineage edge by edge, returning its tip
func validateLocalLineage(ctx context.Context, conn SQLiteConn, receipt *pyjson.Object) (string, error) {
	if _, legacy := receipt.Get("legacy_session_id"); !legacy {
		var ids [4]any
		for i, k := range []string{"session_id", "profile_id", "principal_id", "request_id"} {
			v, err := receiptItem(receipt, k)
			if err != nil {
				return "", err
			}
			ids[i] = v
		}
		identity, err := LocalIdentity(ids[1], ids[2], ids[3])
		if err != nil {
			return "", err
		}
		if !pyEquals(ids[0], identity) {
			return "", runtimeRefusal("storage_unavailable")
		}
	}
	root, err := legacyLineageRoot(ctx, conn, receipt)
	if err != nil {
		return "", err
	}
	rawLineage, ok := receipt.Get("lineage")
	if !ok {
		rawLineage = []any{root}
	}
	entry, err := receiptItem(receipt, "entry")
	if err != nil {
		return "", err
	}
	entrySession, err := receiptItem(entry, "session_id")
	if err != nil {
		return "", err
	}
	lineage, ok := rawLineage.([]any)
	if !ok || len(lineage) == 0 || !pyrepr.Equal(lineage[0], root) || !pyrepr.Equal(lineage[len(lineage)-1], entrySession) {
		return "", runtimeRefusal("storage_unavailable")
	}
	seen := map[string]bool{}
	for _, id := range lineage {
		key := pyrepr.Repr(id)
		if seen[key] {
			return "", runtimeRefusal("storage_unavailable")
		}
		seen[key] = true
	}
	for i := 0; i+1 < len(lineage); i++ {
		parentID, childID := lineage[i], lineage[i+1]
		parent, err := connRow(ctx, conn, "SELECT end_reason FROM sessions WHERE id=?", parentID)
		if err != nil {
			return "", err
		}
		child, err := connRow(ctx, conn, "SELECT parent_session_id,model_config FROM sessions WHERE id=?", childID)
		if err != nil {
			return "", err
		}
		if parent == nil || child == nil || !pyrepr.Equal(child["parent_session_id"], parentID) {
			return "", runtimeRefusal("storage_unavailable")
		}
		valid := false
		if pyEquals(parent["end_reason"], "session_reset") {
			text := "{}"
			if s, ok := child["model_config"].(string); ok && s != "" {
				text = s
			}
			cfg, err := pyjson.Loads(text)
			if err != nil {
				return "", pyValueError(err.Error())
			}
			obj, ok := cfg.(*pyjson.Object)
			if !ok {
				return "", &stateTypedError{"AttributeError", "model_config has no get"}
			}
			from, _ := obj.Get("_reset_from")
			valid = pyrepr.Equal(from, parentID)
		} else {
			tip, err := connRow(ctx, conn, chainStepSQL, parentID)
			if err != nil {
				return "", err
			}
			valid = pyEquals(parent["end_reason"], "compression") && tip != nil && pyrepr.Equal(tip["id"], childID)
		}
		if !valid {
			return "", runtimeRefusal("storage_unavailable")
		}
	}
	next, err := connRow(ctx, conn, chainStepSQL, lineage[len(lineage)-1])
	if err != nil {
		return "", err
	}
	if next != nil {
		return "", runtimeRefusal("storage_unavailable")
	}
	return pyStr(lineage[len(lineage)-1]), nil
}

// Current transcript of a local owner resolved on the caller's transaction
func localPhysicalTarget(ctx context.Context, conn SQLiteConn, sessionID string) (string, error) {
	saved, err := connRow(ctx, conn, "SELECT value FROM state_meta WHERE key=?", LocalPolicyPrefix+sessionID)
	if err != nil {
		return "", err
	}
	if saved == nil {
		return sessionID, nil
	}
	text, _ := saved["value"].(string)
	decoded, err := pyjson.Loads(text)
	if err != nil {
		return "", pyValueError(err.Error())
	}
	receipt, ok := decoded.(*pyjson.Object)
	if !ok {
		return "", &stateTypedError{"TypeError", "receipt is not subscriptable by key"}
	}
	owner, err := receiptItem(receipt, "session_id")
	if err != nil {
		return "", err
	}
	if !pyEquals(owner, sessionID) {
		return "", runtimeRefusal("storage_unavailable")
	}
	return validateLocalLineage(ctx, conn, receipt)
}

// Routing entry of a route while it still targets the tip, nil otherwise
func routedEntry(ctx context.Context, conn SQLiteConn, route, tip any) (*pyjson.Object, error) {
	row, err := connRow(ctx, conn, "SELECT entry_json FROM gateway_routing WHERE scope='' AND session_key=?", route)
	if err != nil || row == nil {
		return nil, err
	}
	text, _ := row["entry_json"].(string)
	decoded, err := pyjson.Loads(text)
	if err != nil {
		return nil, pyValueError(err.Error())
	}
	if decoded == nil {
		return nil, nil
	}
	sessionID, err := receiptItem(decoded, "session_id")
	if err != nil {
		return nil, err
	}
	if !pyrepr.Equal(sessionID, tip) {
		return nil, nil
	}
	return decoded.(*pyjson.Object), nil
}

// Stores the receipt and its route entry and bumps the logical revision in one transaction
func commitLocalTarget(ctx context.Context, conn SQLiteConn, receipt, routed *pyjson.Object) error {
	routedText, err := runtimeJSON(routed)
	if err != nil {
		return err
	}
	route, _ := receipt.Get("route")
	if _, err := conn.ExecContext(ctx, "UPDATE gateway_routing SET entry_json=? WHERE scope='' AND session_key=?", routedText, route); err != nil {
		return err
	}
	receiptText, err := runtimeJSON(receipt)
	if err != nil {
		return err
	}
	sessionID, _ := receipt.Get("session_id")
	if _, err := conn.ExecContext(ctx, "UPDATE state_meta SET value=? WHERE key=?", receiptText, LocalPolicyPrefix+pyStr(sessionID)); err != nil {
		return err
	}
	_, err = conn.ExecContext(ctx, "UPDATE sessions SET runtime_revision=runtime_revision+1 WHERE id=?", sessionID)
	return err
}

// Moves a local owner's physical target from the parent to its published child inside the publication transaction
func advanceLocalTarget(ctx context.Context, conn SQLiteConn, parentSessionID, childSessionID string, entry *pyjson.Object) error {
	parent, err := connRow(ctx, conn, "SELECT * FROM sessions WHERE id=?", parentSessionID)
	if err != nil {
		return err
	}
	var logicalID string
	if parent != nil {
		logicalID, _ = parent["chat_id"].(string)
	}
	if logicalID == "" {
		return nil
	}
	raw, err := connRow(ctx, conn, "SELECT value FROM state_meta WHERE key=?", LocalPolicyPrefix+logicalID)
	if err != nil {
		return err
	}
	if raw == nil {
		legacy, err := connRow(ctx, conn, "SELECT 1 AS one FROM state_meta WHERE key=?", LocalLegacyPrefix+logicalID)
		if err != nil {
			return err
		}
		if len(logicalID) >= 6 && logicalID[:6] == "local-" || legacy != nil {
			return runtimeRefusal("storage_unavailable")
		}
		return nil
	}
	receipt, candidate, err := advancedReceipt(ctx, conn, raw["value"], logicalID, parent, parentSessionID, childSessionID, entry)
	if err != nil {
		if _, ok := err.(*RuntimeStoreError); ok {
			return err
		}
		switch err.(type) {
		case *stateTypedError, *stateValueError:
			return runtimeRefusal("storage_unavailable")
		}
		return err
	}
	return commitLocalTarget(ctx, conn, receipt, candidate)
}

// Receipt with its entry and lineage advanced to the child, validated before commit
func advancedReceipt(ctx context.Context, conn SQLiteConn, raw any, logicalID string, parent map[string]any, parentSessionID, childSessionID string,
	entry *pyjson.Object) (*pyjson.Object, *pyjson.Object, error) {
	text, ok := raw.(string)
	if !ok {
		return nil, nil, &stateTypedError{"TypeError", "the JSON object must be str"}
	}
	decoded, err := pyjson.Loads(text)
	if err != nil {
		return nil, nil, pyValueError(err.Error())
	}
	receipt, ok := decoded.(*pyjson.Object)
	if !ok {
		return nil, nil, &stateTypedError{"TypeError", "receipt is not subscriptable by key"}
	}
	checks := []struct {
		got  func() (any, error)
		want any
	}{
		{func() (any, error) { return receiptItem(receipt, "session_id") }, logicalID},
		{func() (any, error) {
			e, err := receiptItem(receipt, "entry")
			if err != nil {
				return nil, err
			}
			return receiptItem(e, "session_id")
		}, parentSessionID},
		{func() (any, error) { return receiptItem(receipt, "principal_id") }, parent["user_id"]},
		{func() (any, error) { return receiptItem(receipt, "route") }, parent["session_key"]},
	}
	for _, c := range checks {
		got, err := c.got()
		if err != nil {
			return nil, nil, err
		}
		if !pyrepr.Equal(got, c.want) {
			return nil, nil, runtimeRefusal("storage_unavailable")
		}
	}
	route, _ := receipt.Get("route")
	routed, err := routedEntry(ctx, conn, route, parentSessionID)
	if err != nil {
		return nil, nil, err
	}
	if routed == nil {
		return nil, nil, runtimeRefusal("admission_conflict")
	}
	source := routed
	if entry != nil && entry.Len() > 0 {
		source = entry
	}
	candidate := pyjson.NewObject()
	for _, k := range source.Keys {
		candidate.Set(k, source.Vals[k])
	}
	candidate.Set("session_id", childSessionID)
	receipt.Set("entry", candidate)
	var lineage []any
	if prior, ok := receipt.Get("lineage"); ok {
		list, ok := prior.([]any)
		if !ok {
			return nil, nil, &stateTypedError{"TypeError", "lineage is not iterable"}
		}
		lineage = append(lineage, list...)
	} else {
		lineage = []any{logicalID}
	}
	receipt.Set("lineage", append(lineage, childSessionID))
	if _, err := validateLocalLineage(ctx, conn, receipt); err != nil {
		return nil, nil, err
	}
	return receipt, candidate, nil
}
