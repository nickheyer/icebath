package gateway

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/state"
)

// State meta key and digest of a deferred pick's exact retry receipt, an absent fence fingerprinted as None
func deferredPickReceiptKey(principalID, sessionID, requestID, operation string, payload *pyjson.Object, expectedRevision, expectedGeneration any) (string, string, error) {
	keyDigest, err := state.AdmissionFingerprint(sessionID, map[string]any{"principal": principalID, "request": requestID})
	if err != nil {
		return "", "", err
	}
	fingerprint := map[string]any{"operation": operation, "payload": payload, "expected_revision": expectedRevision}
	if expectedGeneration != nil {
		fingerprint["expected_generation"] = expectedGeneration
	}
	digest, err := state.AdmissionFingerprint(sessionID, fingerprint)
	if err != nil {
		return "", "", err
	}
	return "gateway.mutation.v1." + keyDigest, digest, nil
}

// One model or reasoning pick refused only because a turn runs, held for the next turn
type deferredPickRequest struct {
	Operation          string
	RequestID          string
	Payload            *pyjson.Object
	ExpectedRevision   any
	ExpectedGeneration any
}

// Records a pick for the next turn with its receipt, the latest pick winning and a level beside it
func deferPick(ctx context.Context, a *SessionAuthority, principalID string, ref SessionRef, request deferredPickRequest) (*pyjson.Object, error) {
	operation := request.Operation
	if operation == "" {
		operation = "model"
	}
	if err := state.ValidateAction(operation, request.Payload); err != nil {
		return nil, err
	}
	var shown any
	if operation == "model" {
		reset, err := isModelReset(request.Payload)
		if err != nil {
			return nil, err
		}
		if reset {
			model, _, err := configuredDefault(ctx, a)
			if err != nil {
				return nil, err
			}
			shown = model
		}
	}
	key, digest, err := deferredPickReceiptKey(principalID, ref.SessionID, request.RequestID, operation, request.Payload, request.ExpectedRevision, request.ExpectedGeneration)
	if err != nil {
		return nil, err
	}
	pick := pyjson.ObjectOf("principal_id", principalID, "request_id", request.RequestID, "payload", request.Payload,
		"application_id", "deferred-pick:"+request.RequestID)
	var result *pyjson.Object
	err = a.DB.ExecuteWrite(ctx, func(conn state.SQLiteConn) error {
		if err := state.RuntimeEpoch(ctx, conn, a.Epoch); err != nil {
			return err
		}
		replay, err := state.ReplayMutationReceipt(ctx, conn, key, digest)
		if err != nil {
			return err
		}
		if replay != nil {
			stored, ok := replay.(*pyjson.Object)
			if !ok {
				return &PyError{Kind: "TypeError", Msg: "stored mutation result is not a dict"}
			}
			result = stored
			return nil
		}
		var revision, generation any
		if err := conn.QueryRowContext(ctx, "SELECT runtime_revision, runtime_generation FROM sessions WHERE id=?", ref.SessionID).Scan(&revision, &generation); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return runtimeRefusal("not_found")
			}
			return err
		}
		current := pyjson.NewObject()
		var stored string
		switch err := conn.QueryRowContext(ctx, "SELECT value FROM state_meta WHERE key=?", state.DeferredPickPrefix+ref.SessionID).Scan(&stored); {
		case err == nil:
			loaded, err := pyjson.LoadsDict(stored)
			if err != nil {
				return err
			}
			current = loaded
		case !errors.Is(err, sql.ErrNoRows):
			return err
		}
		var value, picked *pyjson.Object
		if operation == "reasoning" {
			if prior, ok := current.GetOr("payload", pyjson.NewObject()).(*pyjson.Object); ok {
				prior.Delete("reasoning")
			}
			value = preparedWith(current, "reasoning", pick)
			effort, _ := request.Payload.Get("effort")
			picked = pyjson.ObjectOf("effort", effort)
		} else {
			_, held := current.Get("reasoning")
			_, picks := request.Payload.Get("reasoning")
			value = preparedWith(pick)
			if held && !picks {
				level, _ := current.Get("reasoning")
				value.Set("reasoning", level)
			}
			model, _ := request.Payload.Get("model")
			if pyTruthy(shown) {
				model = shown
			}
			picked = pyjson.ObjectOf("model", model)
		}
		result = preparedWith(pyjson.ObjectOf("session_id", ref.SessionID, "operation", operation, "status", "deferred"), flattenObject(picked)...)
		result.Set("revision", sqlNumber(revision))
		result.Set("execution_generation", sqlNumber(generation))
		text, err := state.RuntimeJSON(value)
		if err != nil {
			return err
		}
		if _, err := conn.ExecContext(ctx, "INSERT INTO state_meta(key,value) VALUES(?,?) ON CONFLICT(key) DO UPDATE SET value=excluded.value",
			state.DeferredPickPrefix+ref.SessionID, text); err != nil {
			return err
		}
		receipt, err := state.RuntimeJSON(pyjson.ObjectOf("digest", digest, "result", result))
		if err != nil {
			return err
		}
		_, err = conn.ExecContext(ctx, "INSERT INTO state_meta(key,value) VALUES(?,?)", key, receipt)
		return err
	})
	if err != nil {
		return nil, err
	}
	return result, nil
}

// SQLite integer column value as a JSON number, None kept
func sqlNumber(v any) any {
	if n, ok := v.(int64); ok {
		return n
	}
	return v
}

// Commits a deferred entry as a mutation, nil while a worker still holds the transcript
func commitDeferredEntry(ctx context.Context, a *SessionAuthority, ref SessionRef, entry *pyjson.Object, operation string) (*pyjson.Object, error) {
	principal := rowString(entry, "principal_id")
	application := rowString(entry, "application_id")
	committed, err := a.DB.HasMutationReceipt(ctx, principal, ref.SessionID, application)
	if err != nil {
		return nil, err
	}
	if committed {
		return pyjson.ObjectOf("status", "replayed"), nil
	}
	row, err := a.DB.GetSession(ctx, ref.SessionID)
	if err != nil {
		return nil, err
	}
	payload, _ := entry.Get("payload")
	params, err := fencedMutation(row, ref.SessionID, application, operation, payload)
	if err != nil {
		return nil, err
	}
	result, err := writeMutation(ctx, a, ref, params, nil, principal, nil, nil)
	if err == nil {
		return result, nil
	}
	if state.IsMutationGuardRefusal(err) {
		return nil, nil
	}
	var refusal *state.RuntimeStoreError
	if errors.As(err, &refusal) {
		if refusal.Reason == "session_busy" {
			return nil, nil
		}
		return pyjson.ObjectOf("session_id", ref.SessionID, "operation", operation, "status", "refused", "reason", refusal.Reason), nil
	}
	slog.Error(fmt.Sprintf("Deferred %s pick on %s failed to resolve: %s", operation, ref.SessionID, pyExceptionRepr(err)))
	return pyjson.ObjectOf("session_id", ref.SessionID, "operation", operation, "status", "refused", "reason", operation+"_resolution_failed"), nil
}

// Commits the level deferred beside a pick and drops it, returning the remaining slot text when any is left
func commitLevel(ctx context.Context, a *SessionAuthority, ref SessionRef, key, raw string, pick *pyjson.Object) (string, bool, error) {
	level, _ := pick.Get("reasoning")
	pick.Delete("reasoning")
	entry, ok := level.(*pyjson.Object)
	if !ok {
		return "", false, &PyError{Kind: "TypeError", Msg: "deferred reasoning pick is not a dict"}
	}
	result, err := commitDeferredEntry(ctx, a, ref, entry, "reasoning")
	if err != nil || result == nil {
		return "", false, err
	}
	if rowString(result, "status") == "refused" {
		slog.Warn(fmt.Sprintf("Deferred reasoning pick on %s not applied: %s", ref.SessionID, rowString(result, "reason")))
	}
	rest := ""
	if _, has := pick.Get("application_id"); has {
		if rest, err = state.RuntimeJSON(pick); err != nil {
			return "", false, err
		}
	}
	err = a.DB.ExecuteWrite(ctx, func(conn state.SQLiteConn) error {
		var err error
		if rest == "" {
			_, err = conn.ExecContext(ctx, "DELETE FROM state_meta WHERE key=? AND value=?", key, raw)
		} else {
			_, err = conn.ExecContext(ctx, "UPDATE state_meta SET value=? WHERE key=? AND value=?", rest, key, raw)
		}
		return err
	})
	if err != nil || rest == "" {
		return "", false, err
	}
	return rest, true, nil
}

// Committed model and provider fields of a session row for a pick's viewers
func committedRoute(ctx context.Context, a *SessionAuthority, sessionID string) (any, any, error) {
	current, err := a.DB.GetSession(ctx, sessionID)
	if err != nil {
		return nil, nil, err
	}
	if current == nil {
		current = pyjson.NewObject()
	}
	config := pyjson.NewObject()
	if text, _ := current.Get("model_config"); pyTruthy(text) {
		if config, err = pyjson.LoadsDict(pyrepr.Str(text)); err != nil {
			return nil, nil, err
		}
	}
	providerName, _ := config.Get("provider")
	model, _ := current.Get("model")
	return model, providerName, nil
}

// Commits a deferred pick at the claim boundary, a pick that no longer resolves dropped and announced
func applyDeferredPick(ctx context.Context, a *SessionAuthority, ref SessionRef, live *LiveSession) error {
	key := state.DeferredPickPrefix + ref.SessionID
	raw, found, err := a.DB.GetMeta(ctx, key)
	if err != nil || !found {
		return err
	}
	pick, err := pyjson.LoadsDict(raw)
	if err != nil {
		return err
	}
	if _, has := pick.Get("reasoning"); has {
		rest, left, err := commitLevel(ctx, a, ref, key, raw, pick)
		if err != nil || !left {
			return err
		}
		raw = rest
	}
	if _, asked := pick.Get("awaiting_confirm"); asked {
		a.mu.Lock()
		reask := live.ReaskPick
		a.mu.Unlock()
		if !reask {
			return nil
		}
		pick.Delete("awaiting_confirm")
	}
	a.mu.Lock()
	live.ReaskPick = false
	a.mu.Unlock()
	result, err := commitDeferredEntry(ctx, a, ref, pick, "model")
	if err != nil || result == nil {
		return err
	}
	if rowString(result, "status") == "confirmation_required" {
		confirm, _ := result.Get("confirm")
		asked := preparedWith(pick, "awaiting_confirm", confirm)
		text, err := state.RuntimeJSON(asked)
		if err != nil {
			return err
		}
		if err := a.DB.ExecuteWrite(ctx, func(conn state.SQLiteConn) error {
			_, err := conn.ExecContext(ctx, "UPDATE state_meta SET value=? WHERE key=? AND value=?", text, key, raw)
			return err
		}); err != nil {
			return err
		}
		model, providerName, err := committedRoute(ctx, a, ref.SessionID)
		if err != nil {
			return err
		}
		payload, _ := pick.Get("payload")
		event := preparedWith(result, "payload", payload, "committed_model", model)
		if pyTruthy(providerName) {
			event.Set("committed_provider", providerName)
		}
		live.EventStream.Publish(ref.SessionID, event, "session.updated", nil)
		return nil
	}
	if err := a.DB.ExecuteWrite(ctx, func(conn state.SQLiteConn) error {
		_, err := conn.ExecContext(ctx, "DELETE FROM state_meta WHERE key=? AND value=?", key, raw)
		return err
	}); err != nil {
		return err
	}
	if rowString(result, "status") == "refused" {
		reason, _ := result.Get("reason")
		slog.Warn(fmt.Sprintf("Deferred model pick on %s not applied: %s", ref.SessionID, pyrepr.Str(reason)))
		model, providerName, err := committedRoute(ctx, a, ref.SessionID)
		if err != nil {
			return err
		}
		target, err := pyItem(pick, "payload", "model")
		if err != nil {
			return err
		}
		event := preparedWith(result, "target_model", target, "model", model)
		if pyTruthy(providerName) {
			event.Set("provider", providerName)
		}
		live.EventStream.Publish(ref.SessionID, event, "session.updated", nil)
	}
	return nil
}
