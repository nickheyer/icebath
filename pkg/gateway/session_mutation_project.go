package gateway

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/state"
	"github.com/nickheyer/icebath/pkg/tools"
)

// Metadata operations that leave the transcript and policy alone
var mutationMetadata = map[string]bool{"rename": true, "archive": true, "sidebar": true}

// Policy operations prepared off the commit and published as a new frozen policy
var mutationPolicy = map[string]bool{"model": true, "reasoning": true, "workspace": true, "editor_mcp": true, "edit_policy": true}

// Fields of one session.mutate request
type mutationParams struct {
	SessionID          string
	RequestID          string
	ExpectedRevision   int64
	ExpectedGeneration *int64
	Operation          string
	Payload            any
}

// Runner that drops a typed reasoning override of a route
type reasoningOverrideRunner interface {
	SetSessionReasoningOverride(sessionKey string, reasoningConfig map[string]any)
}

// Drops the session's cached terminal environments and the cwd and workspace records that would win over a moved binding
func dropSessionTerminal(ctx context.Context, a *SessionAuthority, ref SessionRef) error {
	receipt, err := a.DB.LocalReceipt(ctx, ref.SessionID)
	if err != nil {
		return err
	}
	entrySID, err := pyItem(receipt, "entry", "session_id")
	if err != nil {
		return err
	}
	ids := []string{ref.SessionID}
	if other := pyrepr.Str(entrySID); other != ref.SessionID {
		ids = append(ids, other)
	}
	live, ok := a.live(ref.SessionID)
	if !ok {
		return &pyKeyError{ref.SessionID}
	}
	env := terminalEnv(ownerScope(ctx, a, false))
	keys := map[string]bool{tools.QualifyTaskKey(env, "session:"+live.Route): true}
	for _, sid := range ids {
		keys[tools.QualifyTaskKey(env, sid)] = true
	}
	for _, key := range append([]string{live.Route}, ids...) {
		tools.UnbindTurnWorkspace(env, key)
		tools.ClearSessionCwd(env, key)
	}
	outer := terminalEnv(ctx)
	for key := range keys {
		tools.CleanupVM(outer, key, false)
	}
	return nil
}

// Commit that ends the onboarding run its delete removed and tells every window, returning the receipt
func endingRunOnDelete(ctx context.Context, db *state.SessionDB, sessionID string, commit func() (*pyjson.Object, error)) (*pyjson.Object, error) {
	var receipt *pyjson.Object
	var commitErr error
	_, err := tools.DeleteOnboardingChat(ctx, db, sessionID, func() bool {
		receipt, commitErr = commit()
		if commitErr != nil {
			return false
		}
		deleted, _ := receipt.Get("deleted_ids")
		ids, _ := deleted.([]any)
		for _, id := range ids {
			if id == sessionID {
				return true
			}
		}
		return false
	})
	if commitErr != nil {
		return nil, commitErr
	}
	if err != nil {
		return nil, err
	}
	return receipt, nil
}

// Runtime projections of a committed receipt, idempotent reads repeated by an exact retry
func projectResult(ctx context.Context, a *SessionAuthority, ref SessionRef, live *LiveSession, params mutationParams, result *pyjson.Object, editorServers any, applied bool) error {
	operation := params.Operation
	current, err := a.DB.GetSession(ctx, ref.SessionID)
	if err != nil {
		return err
	}
	var generation any
	if params.ExpectedGeneration != nil {
		generation = *params.ExpectedGeneration
	}
	if value, has := result.Get("execution_generation"); has {
		generation = value
	}
	owns := current != nil && pyjson.Equal(rowValue(current, "runtime_generation"), generation)
	if operation == "compress" && applied && owns && live != nil {
		noteCompression(a, ref, live)
	} else if operation == "reset" && applied && live != nil {
		live.CompressionCount = 0
	}
	if editorServers != nil && (operation == "branch" || owns) {
		target := any(ref.SessionID)
		if branched, has := result.Get("branched_session_id"); has {
			target = branched
		}
		if err := installEditorMCP(ctx, a, pyrepr.Str(target), editorServers); err != nil {
			return err
		}
		if operation == "editor_mcp" && !pyTruthy(editorServers) {
			retireEditorMCP(ctx, a, ref.SessionID)
		}
	}
	if !(mutationPolicy[operation] || operation == "reset" || operation == "compress" || operation == "rewind") || owns {
		if operation == "workspace" {
			if err := dropSessionTerminal(ctx, a, ref); err != nil {
				slog.Warn(fmt.Sprintf("Session %s moved without dropping its terminal environment: %s", ref.SessionID, pyExceptionRepr(err)))
			}
		}
		if err := projectCommitted(ctx, a, ref, operation, result); err != nil {
			return err
		}
	}
	if live == nil {
		return nil
	}
	if operation == "rewind" && owns {
		evicting, ok := a.Runner.(agentEvictingRunner)
		if !ok {
			return &PyError{Kind: "AttributeError", Msg: fmt.Sprintf("'%T' object has no attribute '_evict_cached_agent'", a.Runner)}
		}
		evicting.EvictCachedAgent(live.Route)
	}
	if applied {
		live.EventStream.Publish(ref.SessionID, result, "session.updated", nil)
		announceToolsChanged(live, ref.SessionID, result)
	}
	return nil
}

// Value of a row column, nil when absent
func rowValue(row *pyjson.Object, key string) any {
	value, _ := row.Get(key)
	return value
}

// Live session of a projected id, a KeyError when the owner no longer holds it
func projectedLive(a *SessionAuthority, sessionID string) (*LiveSession, error) {
	live, ok := a.live(sessionID)
	if !ok {
		return nil, &pyKeyError{sessionID}
	}
	return live, nil
}

// Publishes a committed receipt's runtime projections, safe to repeat on exact retry
func projectCommitted(ctx context.Context, a *SessionAuthority, ref SessionRef, operation string, result *pyjson.Object) error {
	if mutationPolicy[operation] {
		if err := publishLocalPolicy(ctx, a, ref.SessionID, operation != "edit_policy"); err != nil {
			return err
		}
		if err := scheduleSession(ctx, a, ref); err != nil {
			return err
		}
	}
	_, effort := result.Get("effort")
	if operation == "reasoning" || (operation == "model" && effort) {
		live, err := projectedLive(a, ref.SessionID)
		if err != nil {
			return err
		}
		runner, ok := a.Runner.(reasoningOverrideRunner)
		if !ok {
			return &PyError{Kind: "AttributeError", Msg: fmt.Sprintf("'%T' object has no attribute '_set_session_reasoning_override'", a.Runner)}
		}
		runner.SetSessionReasoningOverride(live.Route, nil)
		live.PendingReasoning = nil
	}
	if operation == "model" || operation == "reasoning" || operation == "workspace" || operation == "edit_policy" {
		live, err := projectedLive(a, ref.SessionID)
		if err != nil {
			return err
		}
		info, err := localSessionInfo(ctx, a, ref)
		if err != nil {
			return err
		}
		live.EventStream.Publish(ref.SessionID, info, "session.info", nil)
	}
	if operation == "branch" {
		branched, err := pyItem(result, "branched_session_id")
		if err != nil {
			return err
		}
		if _, err := restoreLocalSession(ctx, a, pyrepr.Str(branched)); err != nil {
			return err
		}
	}
	if operation == "reset" || operation == "compress" {
		if err := projectBoundary(ctx, a, ref); err != nil {
			return err
		}
	}
	if operation == "delete" {
		deleted, err := pyItem(result, "deleted_ids")
		if err != nil {
			return err
		}
		list, _ := deleted.([]any)
		ids := make([]string, len(list))
		for i, sid := range list {
			ids[i] = pyrepr.Str(sid)
		}
		return retireLiveSessions(ctx, a, ids, true)
	}
	return nil
}

// Restores the replacement transcript, evicts the agent at the session boundary and publishes the prepared entry
func projectBoundary(ctx context.Context, a *SessionAuthority, ref SessionRef) error {
	if _, err := restoreLocalSession(ctx, a, ref.SessionID); err != nil {
		return err
	}
	live, err := projectedLive(a, ref.SessionID)
	if err != nil {
		return err
	}
	switch runner := a.Runner.(type) {
	case boundaryEvictingRunner:
		runner.EvictCachedAgentAtBoundary(live.Route)
	case agentEvictingRunner:
		runner.EvictCachedAgent(live.Route)
	default:
		return &PyError{Kind: "AttributeError", Msg: fmt.Sprintf("'%T' object has no attribute '_evict_cached_agent'", a.Runner)}
	}
	receipt, err := a.DB.LocalReceipt(ctx, ref.SessionID)
	if err != nil {
		return err
	}
	raw, err := pyItem(receipt, "entry")
	if err != nil {
		return err
	}
	data, ok := pyjson.AsObject(raw)
	if !ok {
		return &PyError{Kind: "AttributeError", Msg: fmt.Sprintf("'%s' object has no attribute 'get'", pyTypeName(raw))}
	}
	entry, err := SessionEntryFromDict(ctx, data)
	if err != nil {
		return err
	}
	entry.Origin = live.Source
	store := a.Runner.SessionStore()
	store.mu.Lock()
	store.entries.set(entry.SessionKey, entry)
	store.mu.Unlock()
	return nil
}
