package state

import (
	"context"
	"sort"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
)

// Routing entry of a fresh local reset child built from the parent's stored entry, assigned by the gateway wiring
var ResetSessionEntry func(previous *pyjson.Object, childID string) (*pyjson.Object, error)

// Sorted ids of a set of session ids
func sortedIDs(ids map[string]bool) []string {
	out := make([]string, 0, len(ids))
	for sid := range ids {
		out = append(out, sid)
	}
	sort.Strings(out)
	return out
}

// Frozen local policy receipt and its validated physical target
func savedLocalPolicyTarget(ctx context.Context, conn SQLiteConn, sessionID string) (*pyjson.Object, string, error) {
	raw, ok, err := fetchScalar(ctx, conn, "SELECT value FROM state_meta WHERE key=?", LocalPolicyPrefix+sessionID)
	if err != nil {
		return nil, "", err
	}
	if !ok {
		return nil, "", runtimeRefusal("runtime_coordination_required")
	}
	receipt, err := loadsStoredObject(raw)
	if err != nil {
		return nil, "", err
	}
	target, err := validateLocalLineage(ctx, conn, receipt)
	if err != nil {
		return nil, "", err
	}
	return receipt, target, nil
}

// Prepares and publishes a local reset child inside the mutation receipt transaction
func resetInTransaction(d *SessionDB, ctx context.Context, conn SQLiteConn, sessionID string, payload *pyjson.Object) ([]string, *pyjson.Object, error) {
	receipt, target, err := savedLocalPolicyTarget(ctx, conn, sessionID)
	if err != nil {
		return nil, nil, err
	}
	lineage, err := runtimeLineage(ctx, conn, sessionID)
	if err != nil {
		return nil, nil, err
	}
	if err := DiscardOrphanWorkersOnReset(ctx, conn, sortedIDs(lineage)); err != nil {
		return nil, nil, err
	}
	if err := d.RequireTargetAdvanceable(ctx, conn, distinctSessionIDs(sessionID, target)); err != nil {
		return nil, nil, err
	}
	parent, err := connRow(ctx, conn, "SELECT * FROM sessions WHERE id=?", target)
	if err != nil {
		return nil, nil, err
	}
	policy, err := receiptItem(receipt, "policy")
	if err != nil {
		return nil, nil, err
	}
	previous, err := receiptItem(receipt, "entry")
	if err != nil {
		return nil, nil, err
	}
	previousEntry, ok := previous.(*pyjson.Object)
	if !ok {
		return nil, nil, &stateTypedError{"TypeError", "entry must be a dict"}
	}
	if ResetSessionEntry == nil {
		panic("unassigned wiring: state.ResetSessionEntry")
	}
	childID := NewUID()
	entry, err := ResetSessionEntry(previousEntry, childID)
	if err != nil {
		return nil, nil, err
	}
	if parent == nil {
		return nil, nil, &stateTypedError{"TypeError", "'NoneType' object is not subscriptable"}
	}
	source, err := receiptItem(policy, "source")
	if err != nil {
		return nil, nil, err
	}
	model, err := receiptItem(policy, "model")
	if err != nil {
		return nil, nil, err
	}
	cwd, err := receiptItem(policy, "cwd")
	if err != nil {
		return nil, nil, err
	}
	sourceText, _ := source.(string)
	if err := d.publishChildSessionRow(ctx, conn, parent, CompressionChildPublish{ParentSessionID: target, ChildSessionID: childID,
		Source: sourceText, Model: optStringOf(model), ModelConfig: pyjson.ObjectOf("_reset_from", target), Cwd: optStringOf(cwd),
		ProfileName: optStringOf(parent["profile_name"])}); err != nil {
		return nil, nil, err
	}
	if _, err := conn.ExecContext(ctx, "UPDATE sessions SET ended_at=?,end_reason='session_reset' WHERE id=?", now(), target); err != nil {
		return nil, nil, err
	}
	if err := bumpConversationGeneration(ctx, conn, target, "session_reset"); err != nil {
		return nil, nil, err
	}
	if err := advanceLocalTarget(ctx, conn, target, childID, entry); err != nil {
		return nil, nil, err
	}
	generation, err := bumpRuntimeGeneration(ctx, conn, sessionID)
	if err != nil {
		return nil, nil, err
	}
	return []string{}, pyjson.ObjectOf("target_session_id", childID, "previous_target_session_id", target, "execution_generation", generation), nil
}
