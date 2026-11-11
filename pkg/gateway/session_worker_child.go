package gateway

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"strings"

	"github.com/nickheyer/icebath/pkg/internal/pyint"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/state"
)

// Delegation caps a frozen policy falls back to
var delegationDefaults = map[string]int64{"max_concurrent_children": 10, "max_spawn_depth": 1}

// Positive cap of a delegation section key, its default when the value is not an int
func delegationLimit(section map[string]any, key string) int64 {
	value, ok := section[key]
	if !ok {
		value = delegationDefaults[key]
	}
	n, err := pyint.Int64(value)
	if err != nil {
		return delegationDefaults[key]
	}
	return max(1, n)
}

// Child caps from the root conversation's frozen policy, the profile config for a session with no local policy
func delegationLimits(ctx context.Context, a *SessionAuthority, executionID, sessionID string) (int64, int64, error) {
	root, _, _ := strings.Cut(executionID, state.ChildSeparator)
	if root != executionID {
		err := a.DB.ReadCtx(ctx, func(conn state.SQLiteConn) error {
			row, err := state.ConnRow(ctx, conn, "SELECT session_id FROM worker_executions WHERE execution_id=?", root)
			if err != nil {
				return err
			}
			if row != nil {
				sessionID = pyStrOf(row["session_id"])
			}
			return nil
		})
		if err != nil {
			return 0, 0, err
		}
	}
	owner, err := a.LogicalOwner(ctx, sessionID)
	if err != nil {
		return 0, 0, err
	}
	var policy *LocalSessionPolicy
	if live, ok := a.live(owner); ok {
		if policy, err = policyForSource(ctx, a.Runner, live.Source); err != nil {
			return 0, 0, err
		}
	}
	var config map[string]any
	if policy != nil {
		config, err = policy.config(ctx, a, true)
	} else {
		config, err = profileConfig(ctx, a)
	}
	if err != nil {
		return 0, 0, err
	}
	section, _ := config["delegation"].(map[string]any)
	if section == nil {
		section = map[string]any{}
	}
	return delegationLimit(section, "max_concurrent_children"), delegationLimit(section, "max_spawn_depth"), nil
}

// Random lowercase hex text of n bytes
func tokenHex(n int) string {
	raw := make([]byte, n)
	if _, err := rand.Read(raw); err != nil {
		panic(err)
	}
	return hex.EncodeToString(raw)
}

// Reserves a delegated child session and execution bound to the parent's verified identity, a refusal receipt past a cap
func reserveChildExecution(ctx context.Context, db *state.SessionDB, conn state.SQLiteConn, sessionID string, payload *pyjson.Object,
	epoch int64, executionID string, identity map[string]any, principal string, limits [2]int64, refused *string) (*pyjson.Object, error) {
	secret, isText := payload.GetOr("secret", nil).(string)
	if !frameKeysAre(payload, "secret") || !isText || pystr.Len(secret) < 16 || pystr.Len(secret) > 256 {
		return nil, runtimeRefusal("invalid_params")
	}
	prefix := executionID + state.ChildSeparator
	low, high := state.DescendantRange(executionID)
	row, err := state.ConnRow(ctx, conn, "SELECT COUNT(*) AS n FROM worker_executions WHERE execution_id>=? AND execution_id<? "+
		"AND instr(substr(execution_id,?),'/')=0 AND status!='terminal'", low, high, pystr.Len(prefix)+1)
	if err != nil {
		return nil, err
	}
	live, _ := pyint.Int64(row["n"])
	if refused == nil && int64(strings.Count(prefix, state.ChildSeparator)) > limits[1] {
		reason := "delegation_depth_exceeded"
		refused = &reason
	}
	if refused == nil && live >= limits[0] {
		reason := "delegation_capacity"
		refused = &reason
	}
	if refused != nil {
		return pyjson.ObjectOf("refused", *refused), nil
	}
	parent, err := state.ConnRow(ctx, conn, "SELECT profile_name FROM sessions WHERE id=?", sessionID)
	if err != nil {
		return nil, err
	}
	if parent == nil {
		return nil, &PyError{Kind: "TypeError", Msg: "'NoneType' object is not subscriptable"}
	}
	childSID, childEID := state.NewSessionID(), prefix+tokenHex(8)
	if err := db.InsertSessionRowInTransaction(ctx, conn, state.NewSession{ID: childSID, Source: "subagent",
		ModelConfig: map[string]any{"_delegate_from": sessionID}, ParentSessionID: sessionID, ProfileName: pyStrOrEmpty(parent["profile_name"])}); err != nil {
		return nil, err
	}
	scope := map[string]any{}
	for key, value := range identity {
		scope[key] = value
	}
	scope["session_id"], scope["execution_id"], scope["generation"], scope["secret"], scope["principal"] = childSID, childEID, 0, secret, principal
	claim, err := state.AdmissionFingerprint(childSID, scope)
	if err != nil {
		return nil, err
	}
	digest, err := state.SecretDigest(claim)
	if err != nil {
		return nil, err
	}
	if _, err := conn.ExecContext(ctx, "INSERT INTO worker_executions(execution_id,session_id,kind,owner_epoch,generation,status,adoption_digest) "+
		"VALUES(?,?,'child',?,0,'running',?)", childEID, childSID, epoch, digest); err != nil {
		return nil, err
	}
	return pyjson.ObjectOf("child", pyjson.ObjectOf("session_id", childSID, "execution_id", childEID, "generation", 0)), nil
}

// Text of a nullable column, empty for NULL
func pyStrOrEmpty(value any) string {
	if value == nil {
		return ""
	}
	return pyStrOf(value)
}

// Worker persist child.reserve, the parent's full claim and assignment verified first and caps answered as receipts
func workerChildRequest(ctx context.Context, c *AuthorityConnection, ref SessionRef, params *pyjson.Object) (*pyjson.Object, error) {
	if !workerParamsAre(params, "epoch", "sequence", "operation", "payload") {
		return nil, runtimeRefusal("invalid_params")
	}
	claim, err := workerClaim(ctx, c, params)
	if err != nil {
		return nil, err
	}
	if !c.actor.Capabilities["worker:adopt"] {
		return nil, runtimeRefusal("permission_denied")
	}
	if err := verifyWorkerClaim(ctx, c, params, claim); err != nil {
		return nil, err
	}
	a := c.authority
	executionID, err := ledgerText(params.Vals["execution_id"])
	if err != nil {
		return nil, err
	}
	sessionID, err := ledgerText(params.Vals["session_id"])
	if err != nil {
		return nil, err
	}
	var refused *string
	var limits [2]int64
	if err := a.requireAdmissionOpen(false); err != nil {
		var refusal *state.RuntimeStoreError
		if !errors.As(err, &refusal) {
			return nil, err
		}
		refused = &refusal.Reason
	} else if children, depth, err := delegationLimits(ctx, a, executionID, sessionID); err != nil {
		var refusal *state.RuntimeStoreError
		if !errors.As(err, &refusal) {
			return nil, err
		}
		refused = &refusal.Reason
	} else {
		limits = [2]int64{children, depth}
	}
	epoch := workerLedgerInt(params.Vals["epoch"])
	identity := map[string]any{"profile_id": params.Vals["profile_id"], "pid": params.Vals["pid"], "birth": params.Vals["birth"]}
	handler := func(db *state.SessionDB, ctx context.Context, conn state.SQLiteConn, sessionID string, payload *pyjson.Object) (*pyjson.Object, error) {
		return reserveChildExecution(ctx, db, conn, sessionID, payload, epoch, executionID, identity, c.actor.Subject, limits, refused)
	}
	named, _ := params.Vals["operation"].(string)
	return a.DB.ApplyWorkerReceipt(ctx, state.WorkerReceipt{Epoch: epoch, ExecutionID: executionID, SessionID: sessionID,
		Generation: workerLedgerInt(params.Vals["generation"]), Sequence: max(workerLedgerInt(params.Vals["sequence"]), 0), Operation: named,
		Payload: params.Vals["payload"]}, map[string]state.WorkerHandler{"child.reserve": handler})
}
