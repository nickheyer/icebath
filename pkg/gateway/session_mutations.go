package gateway

import (
	"context"
	"fmt"
	"sync"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/state"
)

// Prepare step of a policy or compress mutation over its fenced snapshot
type mutationPreparer func(ctx context.Context, a *SessionAuthority, live *LiveSession, payload *pyjson.Object, prepared *pyjson.Object) (*pyjson.Object, error)

// Prepare step of an operation, the editor servers bound into the editor_mcp one
func mutationPrepareStep(operation string, editorServers any) mutationPreparer {
	switch operation {
	case "model":
		return prepareModel
	case "compress":
		return prepareCompress
	case "editor_mcp":
		return func(ctx context.Context, a *SessionAuthority, live *LiveSession, payload, prepared *pyjson.Object) (*pyjson.Object, error) {
			return prepareEditorMCP(ctx, a, live, payload, prepared, editorServers)
		}
	}
	return mutationPreparer(policyPreparers[operation])
}

// Writer phase of session.mutate and of the editor MCP refreeze, the fenced prepare, receipt commit and its projections
func writeMutation(ctx context.Context, a *SessionAuthority, ref SessionRef, params mutationParams, editorServers any, principalID string,
	authorizeWrite func(ctx context.Context, conn state.SQLiteConn) error, commitLock sync.Locker) (*pyjson.Object, error) {
	operation := params.Operation
	live, _ := a.live(ref.SessionID)
	applied := false
	liveGuard := func(targets []string) error {
		applied = true
		if mutationMetadata[operation] || operation == "import" {
			return nil
		}
		if operation == "delete" {
			if store := a.Runner.SessionStore(); store != nil && (store.routingDB(ctx) == nil || !runtimeOwnsAuthority(a)) {
				return runtimeRefusal("runtime_coordination_required")
			}
		}
		if operation == "rewind" || operation == "reset" {
			if _, ok := a.Runner.(agentEvictingRunner); !ok {
				return runtimeRefusal("runtime_coordination_required")
			}
		}
		return nil
	}
	fields := state.RuntimeMutation{Epoch: a.Epoch, PrincipalID: principalID, SessionID: ref.SessionID, RequestID: params.RequestID,
		ExpectedRevision: params.ExpectedRevision, ExpectedGeneration: params.ExpectedGeneration, Operation: operation,
		Payload: params.Payload, LiveGuard: liveGuard}
	prepareOnly := func() (*pyjson.Object, error) {
		gate := fields
		gate.PrepareOnly = true
		return a.DB.MutateRuntimeSession(ctx, gate)
	}
	var prepared *pyjson.Object
	var err error
	if operation == "branch" {
		if prepared, editorServers, err = prepareBranchEditor(ctx, a, ref, editorServers, prepareOnly); err != nil {
			return nil, err
		}
	}
	if mutationPolicy[operation] || operation == "compress" {
		if prepared, err = prepareOnly(); err != nil {
			return nil, err
		}
		if _, has := prepared.Get("snapshot"); has {
			if live == nil && (operation == "model" || operation == "compress") {
				return nil, &PyError{Kind: "AttributeError", Msg: "'NoneType' object has no attribute 'source'"}
			}
			payload, ok := pyjson.AsObject(params.Payload)
			if !ok {
				return nil, &PyError{Kind: "TypeError", Msg: fmt.Sprintf("'%s' object is not subscriptable", pyTypeName(params.Payload))}
			}
			if prepared, err = mutationPrepareStep(operation, editorServers)(ctx, a, live, payload, prepared); err != nil {
				return nil, err
			}
			if status, _ := prepared.Get("status"); status == "preview" || status == "noop" || status == "confirmation_required" {
				return preparedWith(pyjson.ObjectOf("session_id", ref.SessionID, "operation", operation), flattenObject(prepared)...), nil
			}
		}
	}
	applied = false
	result, err := commitMutation(ctx, a, ref, live, params, prepared, fields, authorizeWrite, editorServers, &applied, commitLock)
	if err != nil {
		return nil, err
	}
	if operation == "delete" {
		if err := a.DB.CollectRetiredMedia(ctx); err != nil {
			return nil, err
		}
	}
	return result, nil
}

// Key value pairs of an object in order
func flattenObject(o *pyjson.Object) []any {
	pairs := make([]any, 0, 2*len(o.Keys))
	for _, key := range o.Keys {
		value, _ := o.Get(key)
		pairs = append(pairs, key, value)
	}
	return pairs
}

// Commits the receipt under the commit lock and publishes its projections, an exact retry answering its stored result
func commitMutation(ctx context.Context, a *SessionAuthority, ref SessionRef, live *LiveSession, params mutationParams, prepared *pyjson.Object,
	fields state.RuntimeMutation, authorizeWrite func(ctx context.Context, conn state.SQLiteConn) error, editorServers any, applied *bool, commitLock sync.Locker) (*pyjson.Object, error) {
	operation := params.Operation
	if commitLock != nil {
		commitLock.Lock()
		defer commitLock.Unlock()
	}
	var result *pyjson.Object
	snapshotted := false
	if prepared != nil {
		_, snapshotted = prepared.Get("snapshot")
	}
	if prepared != nil && !snapshotted {
		result = prepared
	} else {
		fields.Prepared, fields.AuthorizeWrite = prepared, authorizeWrite
		commit := func() (*pyjson.Object, error) { return a.DB.MutateRuntimeSession(ctx, fields) }
		var err error
		if operation == "delete" {
			result, err = endingRunOnDelete(ctx, a.DB, ref.SessionID, commit)
		} else {
			result, err = commit()
		}
		if operation == "compress" {
			var committed *pyjson.Object
			if *applied {
				committed = result
			}
			finishCompressEngine(ctx, a, prepared, committed)
		}
		if err != nil {
			return nil, err
		}
	}
	projectErr := projectResult(ctx, a, ref, live, params, result, editorServers, *applied)
	if operation == "compress" && *applied && prepared != nil {
		if hook, _ := prepared.Get("post_compaction"); pyTruthy(hook) {
			firePostCompactionHook(ctx, a, prepared, result)
		}
	}
	if projectErr != nil {
		return nil, projectErr
	}
	return result, nil
}

// Fenced params of an owner mutation on the row's current revision and generation
func fencedMutation(row *pyjson.Object, sessionID, requestID, operation string, payload any) (mutationParams, error) {
	if row == nil {
		return mutationParams{}, &PyError{Kind: "TypeError", Msg: "'NoneType' object is not subscriptable"}
	}
	revision, ok := pathIndex(rowValue(row, "runtime_revision"))
	if !ok {
		return mutationParams{}, &PyError{Kind: "TypeError", Msg: "runtime_revision is not an int: " + pyrepr.Repr(rowValue(row, "runtime_revision"))}
	}
	params := mutationParams{SessionID: sessionID, RequestID: requestID, ExpectedRevision: int64(revision), Operation: operation, Payload: payload}
	if generation, ok := pathIndex(rowValue(row, "runtime_generation")); ok {
		g := int64(generation)
		params.ExpectedGeneration = &g
	}
	return params, nil
}
