package state

import (
	"context"
	"sort"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
)

// Route, origin and routing entry of a local branch child, assigned by the gateway wiring
var BranchLocalRoute func(childID, principalID string, originProfile, routeProfile any) (route string, origin, entry *pyjson.Object, updatedAt float64, err error)

// Highest row id the branch copies, a tool run directly after an assistant boundary belonging to it
func branchBoundary(ctx context.Context, conn SQLiteConn, target string, through any) (any, error) {
	if through == nil {
		return requiredScalar(ctx, conn, "SELECT COALESCE(MAX(id), 0) FROM messages WHERE session_id=? AND active=1", target)
	}
	role, ok, err := fetchScalar(ctx, conn, "SELECT role FROM messages WHERE id=? AND session_id=? AND active=1", through, target)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, runtimeRefusal("invalid_params")
	}
	if role == "assistant" {
		rows, err := connRows(ctx, conn, "SELECT id, role FROM messages WHERE session_id=? AND active=1 AND id>? ORDER BY id", target, through)
		if err != nil {
			return nil, err
		}
		for _, later := range rows {
			if later["role"] != "tool" {
				break
			}
			through = later["id"]
		}
	}
	return through, nil
}

// The parent's frozen policy for the child, editor servers coming only from the gateway's prepared selection
func childPolicy(policy *pyjson.Object, child string, prepared *pyjson.Object) (*pyjson.Object, error) {
	out := policy.Copy()
	if _, has := out.Get("edit_approval_policy"); has {
		out.Set("edit_approval_policy", "ask")
	}
	editorJSON, _ := out.Get("editor_mcp_json")
	if prepared == nil {
		if pyTruthy(editorJSON) {
			return nil, runtimeRefusal("runtime_coordination_required")
		}
		return out, nil
	}
	preparedChild, err := receiptItem(prepared, "child")
	if err != nil {
		return nil, err
	}
	parentEditor, err := receiptItem(prepared, "parent_editor_mcp_json")
	if err != nil {
		return nil, err
	}
	if !pyjson.Equal(child, preparedChild) || !pyjson.Equal(editorJSON, parentEditor) {
		return nil, runtimeRefusal("revision_conflict")
	}
	for _, key := range []string{"editor_mcp_json", "toolsets", "config_secret_ref"} {
		value, err := receiptItem(prepared, key)
		if err != nil {
			return nil, err
		}
		out.Set(key, value)
	}
	return out, nil
}

// Copies the transcript through the boundary into an independent local route, policy and receipt
func branchInTransaction(d *SessionDB, ctx context.Context, conn SQLiteConn, sessionID string, payload, prepared *pyjson.Object) ([]string, *pyjson.Object, error) {
	saved, target, err := savedLocalPolicyTarget(ctx, conn, sessionID)
	if err != nil {
		return nil, nil, err
	}
	if err := d.RequireIdle(ctx, conn, distinctSessionIDs(sessionID, target)); err != nil {
		return nil, nil, err
	}
	var requestID any = "branch:" + NewUID()
	if prepared != nil {
		if requestID, err = receiptItem(prepared, "request_id"); err != nil {
			return nil, nil, err
		}
	}
	profileID, err := receiptItem(saved, "profile_id")
	if err != nil {
		return nil, nil, err
	}
	principalID, err := receiptItem(saved, "principal_id")
	if err != nil {
		return nil, nil, err
	}
	child, err := LocalIdentity(profileID, principalID, requestID)
	if err != nil {
		return nil, nil, err
	}
	savedPolicy, err := receiptItem(saved, "policy")
	if err != nil {
		return nil, nil, err
	}
	policyObj, ok := savedPolicy.(*pyjson.Object)
	if !ok {
		return nil, nil, &stateTypedError{"TypeError", "policy must be a dict"}
	}
	policy, err := childPolicy(policyObj, child, prepared)
	if err != nil {
		return nil, nil, err
	}
	origin, err := receiptPath(saved, "entry", "origin")
	if err != nil {
		return nil, nil, err
	}
	originObj, ok := origin.(*pyjson.Object)
	if !ok {
		return nil, nil, &stateTypedError{"TypeError", "'" + pyTypeName(origin) + "' object has no attribute 'get'"}
	}
	originProfile, _ := originObj.Get("profile")
	savedRoute, err := receiptItem(saved, "route")
	if err != nil {
		return nil, nil, err
	}
	var routeProfile any
	if text, ok := savedRoute.(string); ok {
		if profile, found := ProfileFromSessionKey(text); found {
			routeProfile = profile
		}
	}
	principalText, _ := principalID.(string)
	if BranchLocalRoute == nil {
		panic("unassigned wiring: state.BranchLocalRoute")
	}
	route, source, entry, updatedAt, err := BranchLocalRoute(child, principalText, originProfile, routeProfile)
	if err != nil {
		return nil, nil, err
	}
	parent, err := connRow(ctx, conn, "SELECT * FROM sessions WHERE id=?", target)
	if err != nil {
		return nil, nil, err
	}
	if parent == nil {
		return nil, nil, &stateTypedError{"TypeError", "'NoneType' object is not subscriptable"}
	}
	policySource, err := receiptItem(policy, "source")
	if err != nil {
		return nil, nil, err
	}
	policyModel, err := receiptItem(policy, "model")
	if err != nil {
		return nil, nil, err
	}
	policyCwd, err := receiptItem(policy, "cwd")
	if err != nil {
		return nil, nil, err
	}
	sourceText, _ := policySource.(string)
	if err := d.publishChildSessionRow(ctx, conn, parent, CompressionChildPublish{ParentSessionID: target, ChildSessionID: child,
		Source: sourceText, Model: optStringOf(policyModel), ModelConfig: pyjson.ObjectOf("_branched_from", target), Cwd: optStringOf(policyCwd),
		ProfileName: optStringOf(parent["profile_name"])}); err != nil {
		return nil, nil, err
	}
	originJSON, err := runtimeJSON(source)
	if err != nil {
		return nil, nil, err
	}
	if _, err := conn.ExecContext(ctx, "UPDATE sessions SET session_key=?,chat_id=?,origin_json=?,system_prompt_hash=? WHERE id=?",
		route, child, originJSON, parent["system_prompt_hash"], child); err != nil {
		return nil, nil, err
	}
	through, _ := payload.Get("through_message_id")
	boundary, err := branchBoundary(ctx, conn, target, through)
	if err != nil {
		return nil, nil, err
	}
	ids, tools, err := tailRowsAfterWatermark(ctx, conn, "SELECT id,tool_calls FROM messages WHERE session_id=? AND active=1 AND id<=? ORDER BY id", target, boundary)
	if err != nil {
		return nil, nil, err
	}
	if err := d.cloneMessageRows(ctx, conn, ids, &child); err != nil {
		return nil, nil, err
	}
	if _, err := conn.ExecContext(ctx, "UPDATE sessions SET message_count=?,tool_call_count=? WHERE id=?", int64(len(ids)), int64(tools), child); err != nil {
		return nil, nil, err
	}
	if title, _ := payload.Get("title"); pyTruthy(title) {
		if _, err := d.setSessionTitleInTransaction(ctx, conn, child, pyStr(title), TitleSourceUser); err != nil {
			return nil, nil, err
		}
	} else {
		base := "branch"
		if parentTitle := parent["title"]; pyTruthy(parentTitle) {
			base = pyStr(parentTitle)
		}
		next, err := LineageTitleOnConn(ctx, conn, base)
		if err != nil {
			return nil, nil, err
		}
		if _, err := d.setSessionTitleInTransaction(ctx, conn, child, next, TitleSourceDerived); err != nil {
			return nil, nil, err
		}
	}
	receipt := pyjson.ObjectOf("profile_id", profileID, "principal_id", principalID, "request_id", requestID,
		"session_id", child, "route", route, "entry", entry, "policy", policy)
	picks, err := picksOn(ctx, conn, sessionID, nil)
	if err != nil {
		return nil, nil, err
	}
	pickList := make([]string, 0, len(picks))
	for pick := range picks {
		pickList = append(pickList, pick)
	}
	sort.Strings(pickList)
	receipt.Set("picks", anyStrings(pickList))
	entryJSON, err := runtimeJSON(entry)
	if err != nil {
		return nil, nil, err
	}
	if _, err := conn.ExecContext(ctx, "INSERT INTO gateway_routing(scope,session_key,entry_json,updated_at) VALUES('',?,?,?)", route, entryJSON, updatedAt); err != nil {
		return nil, nil, err
	}
	receiptJSON, err := runtimeJSON(receipt)
	if err != nil {
		return nil, nil, err
	}
	if _, err := conn.ExecContext(ctx, "INSERT INTO state_meta(key,value) VALUES(?,?)", LocalPolicyPrefix+child, receiptJSON); err != nil {
		return nil, nil, err
	}
	return []string{sessionID}, pyjson.ObjectOf("branched_session_id", child, "copied_messages", int64(len(ids))), nil
}
