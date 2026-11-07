package state

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
)

// Model pick made while a turn ran and committed at the next turn boundary
const DeferredPickPrefix = "gateway.deferred_pick.v1."

const mutationReceiptsSQL = "FROM state_meta WHERE key GLOB 'gateway.mutation.v1.*' AND json_valid(value) "

type policyHandler func(d *SessionDB, ctx context.Context, conn SQLiteConn, sessionID string, payload, prepared *pyjson.Object) ([]string, *pyjson.Object, error)

var policyHandlers = map[string]policyHandler{
	"model":       modelInTransaction,
	"reasoning":   reasoningInTransaction,
	"workspace":   workspaceInTransaction,
	"editor_mcp":  editorMCPInTransaction,
	"edit_policy": editPolicyInTransaction,
}

// Distinct ids in first seen order, the list of a Python set of session ids
func distinctSessionIDs(ids ...string) []string {
	seen := make(map[string]bool, len(ids))
	out := make([]string, 0, len(ids))
	for _, id := range ids {
		if !seen[id] {
			seen[id] = true
			out = append(out, id)
		}
	}
	return out
}

// First column of the first row and whether a row came back
func fetchScalar(ctx context.Context, conn SQLiteConn, query string, args ...any) (any, bool, error) {
	var value any
	err := conn.QueryRowContext(ctx, query, args...).Scan(&value)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	return value, true, nil
}

// First column of a row that must exist, a TypeError as subscripting a None fetch raises
func requiredScalar(ctx context.Context, conn SQLiteConn, query string, args ...any) (any, error) {
	value, ok, err := fetchScalar(ctx, conn, query, args...)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, &stateTypedError{"TypeError", "'NoneType' object is not subscriptable"}
	}
	return value, nil
}

// Decoded JSON object of a stored text value, ValueError on bad JSON and TypeError on a non dict
func loadsStoredObject(raw any) (*pyjson.Object, error) {
	text, ok := raw.(string)
	if !ok {
		return nil, &stateTypedError{"TypeError", fmt.Sprintf("the JSON object must be str, bytes or bytearray, not %s", pyTypeName(raw))}
	}
	decoded, err := pyjson.Loads(text)
	if err != nil {
		return nil, pyValueError(err.Error())
	}
	obj, isObj := decoded.(*pyjson.Object)
	if !isObj {
		return nil, &stateTypedError{"TypeError", fmt.Sprintf("'%s' object is not a mapping", pyTypeName(decoded))}
	}
	return obj, nil
}

// Frozen local policy receipt of a session, refused as runtime_coordination_required when absent
func savedLocalPolicy(ctx context.Context, conn SQLiteConn, sessionID string) (*pyjson.Object, error) {
	raw, ok, err := fetchScalar(ctx, conn, "SELECT value FROM state_meta WHERE key=?", LocalPolicyPrefix+sessionID)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, runtimeRefusal("runtime_coordination_required")
	}
	return loadsStoredObject(raw)
}

// Receipt, physical target and target revision a prepared mutation is revalidated against
func (d *SessionDB) LocalSnapshot(ctx context.Context, conn SQLiteConn, sessionID string) (*pyjson.Object, error) {
	saved, err := savedLocalPolicy(ctx, conn, sessionID)
	if err != nil {
		return nil, err
	}
	target, err := validateLocalLineage(ctx, conn, saved)
	if err != nil {
		return nil, err
	}
	if err := d.RequireTargetAdvanceable(ctx, conn, distinctSessionIDs(sessionID, target)); err != nil {
		return nil, err
	}
	revision, err := requiredScalar(ctx, conn, "SELECT runtime_revision FROM sessions WHERE id=?", target)
	if err != nil {
		return nil, err
	}
	snapshot := pyjson.NewObject()
	snapshot.Set("receipt", saved)
	snapshot.Set("target", target)
	snapshot.Set("target_revision", revision)
	return snapshot, nil
}

// Current snapshot when it still equals the prepared one, revision_conflict when anything moved
func (d *SessionDB) ValidatePrepared(ctx context.Context, conn SQLiteConn, sessionID string, prepared *pyjson.Object) (*pyjson.Object, error) {
	if prepared == nil {
		return nil, runtimeRefusal("runtime_coordination_required")
	}
	expected, has := prepared.Get("snapshot")
	if !has {
		return nil, runtimeRefusal("runtime_coordination_required")
	}
	current, err := d.LocalSnapshot(ctx, conn, sessionID)
	if err != nil {
		return nil, err
	}
	if !pyjson.Equal(current, expected) {
		return nil, runtimeRefusal("revision_conflict")
	}
	return current, nil
}

// Writes the prepared policy over its snapshot receipt and opens the next execution generation
func (d *SessionDB) commitPolicy(ctx context.Context, conn SQLiteConn, sessionID string, prepared *pyjson.Object) (*pyjson.Object, any, error) {
	snapshot, err := d.ValidatePrepared(ctx, conn, sessionID, prepared)
	if err != nil {
		return nil, nil, err
	}
	receipt, _ := snapshot.Get("receipt")
	saved := receipt.(*pyjson.Object)
	policy, err := receiptItem(prepared, "policy")
	if err != nil {
		return nil, nil, err
	}
	saved.Set("policy", policy)
	text, err := runtimeJSON(saved)
	if err != nil {
		return nil, nil, err
	}
	if _, err := conn.ExecContext(ctx, "UPDATE state_meta SET value=? WHERE key=?", text, LocalPolicyPrefix+sessionID); err != nil {
		return nil, nil, err
	}
	generation, err := bumpRuntimeGeneration(ctx, conn, sessionID)
	if err != nil {
		return nil, nil, err
	}
	return snapshot, generation, nil
}

// Increments the session's runtime generation and returns the new value
func bumpRuntimeGeneration(ctx context.Context, conn SQLiteConn, sessionID string) (any, error) {
	if _, err := conn.ExecContext(ctx, "UPDATE sessions SET runtime_generation=runtime_generation+1 WHERE id=?", sessionID); err != nil {
		return nil, err
	}
	return requiredScalar(ctx, conn, "SELECT runtime_generation FROM sessions WHERE id=?", sessionID)
}

func snapshotTarget(snapshot *pyjson.Object) string {
	target, _ := snapshot.Get("target")
	return target.(string)
}

// Nested item lookup with the KeyError or TypeError each subscript raises
func receiptPath(v any, keys ...string) (any, error) {
	var err error
	for _, key := range keys {
		if v, err = receiptItem(v, key); err != nil {
			return nil, err
		}
	}
	return v, nil
}

// Commits a model pick to the policy and target row, superseding a deferred model pick
func modelInTransaction(d *SessionDB, ctx context.Context, conn SQLiteConn, sessionID string, payload, prepared *pyjson.Object) ([]string, *pyjson.Object, error) {
	snapshot, generation, err := d.commitPolicy(ctx, conn, sessionID, prepared)
	if err != nil {
		return nil, nil, err
	}
	model, err := receiptPath(prepared, "policy", "model")
	if err != nil {
		return nil, nil, err
	}
	configJSON, err := receiptPath(prepared, "policy", "config_json")
	if err != nil {
		return nil, nil, err
	}
	configDoc, err := loadsAny(configJSON)
	if err != nil {
		return nil, nil, err
	}
	provider, err := receiptPath(configDoc, "model", "provider")
	if err != nil {
		return nil, nil, err
	}
	target := snapshotTarget(snapshot)
	raw, err := requiredScalar(ctx, conn, "SELECT model_config FROM sessions WHERE id=?", target)
	if err != nil {
		return nil, nil, err
	}
	if !pyTruthy(raw) {
		raw = "{}"
	}
	modelConfig, err := loadsStoredObject(raw)
	if err != nil {
		return nil, nil, err
	}
	modelConfig.Set("provider", provider)
	configText, err := runtimeJSON(modelConfig)
	if err != nil {
		return nil, nil, err
	}
	if _, err := conn.ExecContext(ctx, "UPDATE sessions SET model=?,model_config=? WHERE id=?", model, configText, target); err != nil {
		return nil, nil, err
	}
	key := DeferredPickPrefix + sessionID
	pending, ok, err := fetchScalar(ctx, conn, "SELECT value FROM state_meta WHERE key=?", key)
	if err != nil {
		return nil, nil, err
	}
	_, payloadReasoning := payload.Get("reasoning")
	var level any
	if ok && !payloadReasoning {
		pick, err := loadsStoredObject(pending)
		if err != nil {
			return nil, nil, err
		}
		level, _ = pick.Get("reasoning")
	}
	if level == nil {
		_, err = conn.ExecContext(ctx, "DELETE FROM state_meta WHERE key=?", key)
	} else {
		kept := pyjson.NewObject()
		kept.Set("reasoning", level)
		var text string
		if text, err = runtimeJSON(kept); err == nil {
			_, err = conn.ExecContext(ctx, "UPDATE state_meta SET value=? WHERE key=?", text, key)
		}
	}
	if err != nil {
		return nil, nil, err
	}
	result := pyjson.NewObject()
	result.Set("model", model)
	result.Set("provider", provider)
	result.Set("execution_generation", generation)
	if effort, has := payload.Get("reasoning"); has {
		result.Set("effort", effort)
	}
	return distinctSessionIDs(sessionID, target), result, nil
}

// Commits a reasoning level, dropping any level still deferred beside or inside a model pick
func reasoningInTransaction(d *SessionDB, ctx context.Context, conn SQLiteConn, sessionID string, payload, prepared *pyjson.Object) ([]string, *pyjson.Object, error) {
	snapshot, generation, err := d.commitPolicy(ctx, conn, sessionID, prepared)
	if err != nil {
		return nil, nil, err
	}
	key := DeferredPickPrefix + sessionID
	pending, ok, err := fetchScalar(ctx, conn, "SELECT value FROM state_meta WHERE key=?", key)
	if err != nil {
		return nil, nil, err
	}
	if ok {
		pick, err := loadsStoredObject(pending)
		if err != nil {
			return nil, nil, err
		}
		pick.Pop("reasoning")
		if inner, has := pick.Get("payload"); has {
			innerObj, isObj := inner.(*pyjson.Object)
			if !isObj {
				return nil, nil, &stateTypedError{"AttributeError", fmt.Sprintf("'%s' object has no attribute 'pop'", pyTypeName(inner))}
			}
			innerObj.Pop("reasoning")
		}
		if _, has := pick.Get("application_id"); has {
			text, err := runtimeJSON(pick)
			if err != nil {
				return nil, nil, err
			}
			_, err = conn.ExecContext(ctx, "UPDATE state_meta SET value=? WHERE key=?", text, key)
			if err != nil {
				return nil, nil, err
			}
		} else if _, err := conn.ExecContext(ctx, "DELETE FROM state_meta WHERE key=?", key); err != nil {
			return nil, nil, err
		}
	}
	effort, err := receiptItem(payload, "effort")
	if err != nil {
		return nil, nil, err
	}
	result := pyjson.NewObject()
	result.Set("effort", effort)
	result.Set("execution_generation", generation)
	return distinctSessionIDs(sessionID, snapshotTarget(snapshot)), result, nil
}

// Moves the policy cwd and every row of the conversation to the new workspace, replacing git identity
func workspaceInTransaction(d *SessionDB, ctx context.Context, conn SQLiteConn, sessionID string, payload, prepared *pyjson.Object) ([]string, *pyjson.Object, error) {
	snapshot, generation, err := d.commitPolicy(ctx, conn, sessionID, prepared)
	if err != nil {
		return nil, nil, err
	}
	cwd, err := receiptPath(prepared, "policy", "cwd")
	if err != nil {
		return nil, nil, err
	}
	branch, err := receiptPath(prepared, "git", "branch")
	if err != nil {
		return nil, nil, err
	}
	root, err := receiptPath(prepared, "git", "root")
	if err != nil {
		return nil, nil, err
	}
	target := snapshotTarget(snapshot)
	ids, err := localConversationIDs(ctx, conn, sessionID)
	if err != nil {
		return nil, nil, err
	}
	for _, sid := range distinctSessionIDs(append(ids, target)...) {
		if _, err := conn.ExecContext(ctx, "UPDATE sessions SET cwd=?, git_branch=?, git_repo_root=?, "+
			"git_metadata_generation=COALESCE(git_metadata_generation, 0)+1 WHERE id=?", cwd, orNone(branch), orNone(root), sid); err != nil {
			return nil, nil, err
		}
	}
	result := pyjson.NewObject()
	result.Set("cwd", cwd)
	result.Set("branch", orNone(branch))
	result.Set("git_repo_root", orNone(root))
	result.Set("execution_generation", generation)
	return distinctSessionIDs(sessionID, target), result, nil
}

// Commits the editor MCP selection the owner prepared and reports whether tools changed
func editorMCPInTransaction(d *SessionDB, ctx context.Context, conn SQLiteConn, sessionID string, payload, prepared *pyjson.Object) ([]string, *pyjson.Object, error) {
	snapshot, generation, err := d.commitPolicy(ctx, conn, sessionID, prepared)
	if err != nil {
		return nil, nil, err
	}
	toolsChanged, err := receiptItem(prepared, "tools_changed")
	if err != nil {
		return nil, nil, err
	}
	servers, err := receiptItem(prepared, "mcp_servers")
	if err != nil {
		return nil, nil, err
	}
	result := pyjson.NewObject()
	result.Set("tools_changed", toolsChanged)
	result.Set("mcp_servers", servers)
	result.Set("execution_generation", generation)
	return distinctSessionIDs(sessionID, snapshotTarget(snapshot)), result, nil
}

// Commits an ACP session's edit approval policy
func editPolicyInTransaction(d *SessionDB, ctx context.Context, conn SQLiteConn, sessionID string, payload, prepared *pyjson.Object) ([]string, *pyjson.Object, error) {
	snapshot, generation, err := d.commitPolicy(ctx, conn, sessionID, prepared)
	if err != nil {
		return nil, nil, err
	}
	policy, err := receiptItem(payload, "policy")
	if err != nil {
		return nil, nil, err
	}
	result := pyjson.NewObject()
	result.Set("edit_approval_policy", policy)
	result.Set("execution_generation", generation)
	return distinctSessionIDs(sessionID, snapshotTarget(snapshot)), result, nil
}

// Decoded JSON of a text value, any JSON type
func loadsAny(raw any) (any, error) {
	text, ok := raw.(string)
	if !ok {
		return nil, &stateTypedError{"TypeError", fmt.Sprintf("the JSON object must be str, bytes or bytearray, not %s", pyTypeName(raw))}
	}
	decoded, err := pyjson.Loads(text)
	if err != nil {
		return nil, pyValueError(err.Error())
	}
	return decoded, nil
}

// Policy edits committed for a session below an optional revision, plus the picks a branch copied
func picksOn(ctx context.Context, conn SQLiteConn, sessionID any, before any) (map[string]bool, error) {
	query := "SELECT DISTINCT json_extract(value, '$.result.operation') " + mutationReceiptsSQL +
		"AND json_extract(value, '$.result.operation') IN ('model', 'reasoning', 'workspace') " +
		"AND COALESCE(json_extract(value, '$.result.status'), '') != 'deferred' " +
		"AND json_extract(value, '$.result.session_id')=?"
	args := []any{sessionID}
	if before != nil {
		query += " AND json_extract(value, '$.result.revision')<?"
		args = append(args, before)
	}
	rows, err := connRows(ctx, conn, query, args...)
	if err != nil {
		return nil, err
	}
	picked := map[string]bool{}
	for _, row := range rows {
		for _, v := range row {
			picked[pyStr(v)] = true
		}
	}
	sid, isStr := sessionID.(string)
	if !isStr {
		return nil, &stateTypedError{"TypeError", fmt.Sprintf("can only concatenate str (not \"%s\") to str", pyTypeName(sessionID))}
	}
	raw, ok, err := fetchScalar(ctx, conn, "SELECT value FROM state_meta WHERE key=?", LocalPolicyPrefix+sid)
	if err != nil || !ok {
		return picked, err
	}
	decoded, err := loadsAny(raw)
	if err != nil {
		return nil, err
	}
	if hasPicksMember(decoded) {
		list, err := receiptItem(decoded, "picks")
		if err != nil {
			return nil, err
		}
		if err := addPickMembers(picked, list); err != nil {
			return nil, err
		}
		return picked, nil
	}
	branched, ok, err := fetchRow(ctx, conn, "SELECT json_extract(value, '$.result.session_id'), json_extract(value, '$.result.revision') "+
		mutationReceiptsSQL+"AND json_extract(value, '$.result.operation')='branch' "+
		"AND json_extract(value, '$.result.branched_session_id')=?", sid)
	if err != nil || !ok {
		return picked, err
	}
	parent, err := picksOn(ctx, conn, branched[0], branched[1])
	if err != nil {
		return nil, err
	}
	for name := range parent {
		picked[name] = true
	}
	return picked, nil
}

// Whether the receipt holds picks, the Python in operator over a dict, list or str
func hasPicksMember(decoded any) bool {
	switch v := decoded.(type) {
	case *pyjson.Object:
		_, has := v.Get("picks")
		return has
	case []any:
		for _, item := range v {
			if item == "picks" {
				return true
			}
		}
	case string:
		return strings.Contains(v, "picks")
	}
	return false
}

// Adds the operation names a receipt's picks list holds, a TypeError for a non iterable or non str member
func addPickMembers(picked map[string]bool, list any) error {
	items, ok := list.([]any)
	if !ok {
		return &stateTypedError{"TypeError", fmt.Sprintf("'%s' object is not iterable", pyTypeName(list))}
	}
	for _, item := range items {
		name, isStr := item.(string)
		if !isStr {
			return &stateTypedError{"TypeError", fmt.Sprintf("'<' not supported between instances of '%s' and 'str'", pyTypeName(item))}
		}
		picked[name] = true
	}
	return nil
}

// Columns of the first row in select order and whether a row came back
func fetchRow(ctx context.Context, conn SQLiteConn, query string, args ...any) ([]any, bool, error) {
	rows, err := conn.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, false, err
	}
	defer rows.Close()
	if !rows.Next() {
		return nil, false, rows.Err()
	}
	cols, err := rows.Columns()
	if err != nil {
		return nil, false, err
	}
	values := make([]any, len(cols))
	ptrs := make([]any, len(cols))
	for i := range values {
		ptrs[i] = &values[i]
	}
	if err := rows.Scan(ptrs...); err != nil {
		return nil, false, err
	}
	return values, true, nil
}

// Sorted policy edits a session.mutate committed for the session, the durable record of the user's picks
func (d *SessionDB) PolicyPicks(ctx context.Context, sessionID string) ([]string, error) {
	var names []string
	err := d.readCtx(ctx, false, func(conn SQLiteConn) error {
		picked, err := picksOn(ctx, conn, sessionID, nil)
		if err != nil {
			return err
		}
		names = sortedPicks(picked)
		return nil
	})
	return names, err
}

func sortedPicks(picked map[string]bool) []string {
	names := make([]string, 0, len(picked))
	for name := range picked {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// Replaces exactly the old policy of an idle session at its generation and returns the next generation
func (d *SessionDB) CommitRefreshedPolicy(ctx context.Context, epoch int64, sessionID string, old, new any, generation int64) (any, error) {
	var next any
	err := d.executeWrite(ctx, func(conn SQLiteConn) error {
		if err := runtimeEpoch(ctx, conn, epoch); err != nil {
			return err
		}
		snapshot, err := d.LocalSnapshot(ctx, conn, sessionID)
		if err != nil {
			return err
		}
		if err := d.RequireIdle(ctx, conn, distinctSessionIDs(sessionID, snapshotTarget(snapshot))); err != nil {
			return err
		}
		current, ok, err := fetchScalar(ctx, conn, "SELECT runtime_generation FROM sessions WHERE id=?", sessionID)
		if err != nil {
			return err
		}
		policy, err := receiptPath(snapshot, "receipt", "policy")
		if err != nil {
			return err
		}
		if !pyjson.Equal(policy, old) || !ok || !pyjson.Equal(current, generation) {
			return runtimeRefusal("revision_conflict")
		}
		prepared := pyjson.NewObject()
		prepared.Set("snapshot", snapshot)
		prepared.Set("policy", new)
		_, next, err = d.commitPolicy(ctx, conn, sessionID, prepared)
		return err
	}, nil)
	return next, err
}
