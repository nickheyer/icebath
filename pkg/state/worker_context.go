package state

import (
	"context"
	"unicode/utf8"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
)

// Handler of one worker receipt operation, run on the receipt transaction connection without committing
type WorkerHandler func(d *SessionDB, ctx context.Context, conn SQLiteConn, sessionID string, payload *pyjson.Object) (*pyjson.Object, error)

// model_config sidecar keys a worker may patch
var SidecarKeys = map[string]bool{"_usage_anchor": true, "_proactive_prune_rearm_tokens": true, "_anthropic_rejected_thinking": true}

// True when the payload holds exactly the given keys
func payloadKeysExactly(payload *pyjson.Object, keys ...string) bool {
	if payload.Len() != len(keys) {
		return false
	}
	for _, k := range keys {
		if _, has := payload.Get(k); !has {
			return false
		}
	}
	return true
}

// True when some payload key lies outside the allowed set
func payloadHasKeysOutside(payload *pyjson.Object, allowed map[string]bool) bool {
	for _, k := range payload.Keys {
		if !allowed[k] {
			return true
		}
	}
	return false
}

// Non empty string of at most 1024 characters, invalid_params for anything else
func runtimeTextOf(value any) (string, error) {
	text, ok := value.(string)
	if !ok || text == "" || utf8.RuneCountInString(text) > 1024 {
		return "", runtimeRefusal("invalid_params")
	}
	return text, nil
}

// First row object of a statement on the caller connection, nil when none matches
func connObject(ctx context.Context, conn SQLiteConn, query string, args ...any) (*pyjson.Object, error) {
	rows, err := queryObjects(ctx, conn, query, args...)
	if err != nil || len(rows) == 0 {
		return nil, err
	}
	return rows[0], nil
}

// Rows changed by one statement on the caller connection
func execRowcount(ctx context.Context, conn SQLiteConn, query string, args ...any) (int64, error) {
	res, err := conn.ExecContext(ctx, query, args...)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// Session row with its prompt and tools pin resolved, the receipt connection view of get_session
func workerSessionRow(d *SessionDB, ctx context.Context, conn SQLiteConn, sessionID string) (*pyjson.Object, error) {
	row, err := connObject(ctx, conn, "SELECT s.*, COALESCE(sp.prompt, s.system_prompt) AS _system_prompt_resolved, "+
		"COALESCE(tp.prompt, s.tool_names) AS _tool_names_resolved "+
		"FROM sessions s LEFT JOIN system_prompts sp ON sp.hash=s.system_prompt_hash "+
		"LEFT JOIN system_prompts tp ON tp.hash=s.tool_names WHERE s.id=?", sessionID)
	if err != nil {
		return nil, err
	}
	if row == nil {
		return nil, &stateTypedError{"TypeError", "'NoneType' object is not iterable"}
	}
	return d.sessionRowDict(row), nil
}

// Session row of the assignment, refusing any payload content
func workerContext(d *SessionDB, ctx context.Context, conn SQLiteConn, sessionID string, payload *pyjson.Object) (*pyjson.Object, error) {
	if payload.Len() > 0 {
		return nil, runtimeRefusal("invalid_params")
	}
	session, err := workerSessionRow(d, ctx, conn, sessionID)
	if err != nil {
		return nil, err
	}
	return pyjson.ObjectOf("session", session), nil
}

// Replaces the frozen system prompt through the content addressed prompt table
func workerPrompt(d *SessionDB, ctx context.Context, conn SQLiteConn, sessionID string, payload *pyjson.Object) (*pyjson.Object, error) {
	prompt, _ := payload.Get("system_prompt")
	if _, isStr := prompt.(string); !payloadKeysExactly(payload, "system_prompt") || (prompt != nil && !isStr) {
		return nil, runtimeRefusal("invalid_params")
	}
	hash, err := storeSystemPrompt(ctx, conn, prompt)
	if err != nil {
		return nil, err
	}
	if _, err := conn.ExecContext(ctx, "UPDATE sessions SET system_prompt_hash=?, system_prompt=NULL WHERE id=?", hash, sessionID); err != nil {
		return nil, err
	}
	if err := deleteUnreferencedSystemPrompts(ctx, conn); err != nil {
		return nil, err
	}
	return pyjson.ObjectOf("value", nil), nil
}

// Merges a sidecar only patch into the stored model_config
func workerSidecars(d *SessionDB, ctx context.Context, conn SQLiteConn, sessionID string, payload *pyjson.Object) (*pyjson.Object, error) {
	raw, _ := payload.Get("patch")
	patch, ok := raw.(*pyjson.Object)
	if !payloadKeysExactly(payload, "patch") || !ok || payloadHasKeysOutside(patch, SidecarKeys) {
		return nil, runtimeRefusal("invalid_params")
	}
	merged, _, err := mergeModelConfigJSON(ctx, conn, sessionID, patch, true)
	if err != nil {
		return nil, err
	}
	if _, err := conn.ExecContext(ctx, "UPDATE sessions SET model_config=? WHERE id=?", merged, sessionID); err != nil {
		return nil, err
	}
	return pyjson.ObjectOf("value", nil), nil
}

// Validates a tools pin, a name list or the version and tools dict with an optional delegation backend
func validToolNamesPin(pin any) bool {
	switch p := pin.(type) {
	case nil:
		return true
	case []any:
		for _, n := range p {
			s, ok := n.(string)
			if !ok || s == "" || utf8.RuneCountInString(s) > 256 {
				return false
			}
		}
		return true
	case *pyjson.Object:
		backend, has := p.Get("delegation_backend")
		if !has {
			backend = ""
		}
		_, hasVersion := p.Get("version")
		_, hasTools := p.Get("tools")
		if !hasVersion || !hasTools || payloadHasKeysOutside(p, map[string]bool{"version": true, "tools": true, "delegation_backend": true}) {
			return false
		}
		version, _ := p.Get("version")
		tools, _ := p.Get("tools")
		_, versionStr := version.(string)
		_, toolsList := tools.([]any)
		backendStr, backendIsStr := backend.(string)
		return versionStr && toolsList && backendIsStr && utf8.RuneCountInString(backendStr) <= 64
	}
	return false
}

// Persists the tools pin through the prompt table with a four megabyte bound, nil clearing it
func workerToolNames(d *SessionDB, ctx context.Context, conn SQLiteConn, sessionID string, payload *pyjson.Object) (*pyjson.Object, error) {
	if !payloadKeysExactly(payload, "tool_names") {
		return nil, runtimeRefusal("invalid_params")
	}
	pin, _ := payload.Get("tool_names")
	if !validToolNamesPin(pin) {
		return nil, runtimeRefusal("invalid_params")
	}
	var encoded any
	if pin != nil {
		text, err := pyjson.Dumps(pin)
		if err != nil {
			return nil, err
		}
		if len(text) > 4*1024*1024 {
			return nil, runtimeRefusal("invalid_params")
		}
		encoded = text
	}
	hash, err := storeSystemPrompt(ctx, conn, encoded)
	if err != nil {
		return nil, err
	}
	if _, err := conn.ExecContext(ctx, "UPDATE sessions SET tool_names=? WHERE id=?", hash, sessionID); err != nil {
		return nil, err
	}
	if err := deleteUnreferencedSystemPrompts(ctx, conn); err != nil {
		return nil, err
	}
	return pyjson.ObjectOf("value", nil), nil
}

// Role of the newest active conversation row, nil for an empty transcript
func workerTailRole(d *SessionDB, ctx context.Context, conn SQLiteConn, sessionID string, payload *pyjson.Object) (*pyjson.Object, error) {
	if payload.Len() > 0 {
		return nil, runtimeRefusal("invalid_params")
	}
	role, _, err := fetchScalar(ctx, conn, "SELECT role FROM messages WHERE session_id=? AND active=1 "+
		"AND role NOT IN ('session_meta','system') ORDER BY id DESC LIMIT 1", sessionID)
	if err != nil {
		return nil, err
	}
	return pyjson.ObjectOf("value", role), nil
}

// Role of one active row of this transcript, nil for any other row
func workerRowRole(d *SessionDB, ctx context.Context, conn SQLiteConn, sessionID string, payload *pyjson.Object) (*pyjson.Object, error) {
	raw, _ := payload.Get("row_id")
	rowID, ok := exactInt(raw)
	if !payloadKeysExactly(payload, "row_id") || !ok {
		return nil, runtimeRefusal("invalid_params")
	}
	role, _, err := fetchScalar(ctx, conn, "SELECT role FROM messages WHERE id=? AND session_id=? AND active=1", rowID, sessionID)
	if err != nil {
		return nil, err
	}
	return pyjson.ObjectOf("value", role), nil
}
