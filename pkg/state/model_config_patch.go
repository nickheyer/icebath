package state

import (
	"context"
	"sort"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
)

// Tolerant model_config decode into a map, JSON text or a dict copied, anything else empty
func ParseModelConfig(raw any) map[string]any {
	out := map[string]any{}
	if m, ok := raw.(map[string]any); ok {
		for k, v := range m {
			out[k] = v
		}
		return out
	}
	parsed := parseModelConfigValue(raw)
	for _, k := range parsed.Keys {
		out[k] = parsed.Vals[k]
	}
	return out
}

// Ordered patch object of a Go map in sorted key order
func modelConfigPatchObject(patch map[string]any) *pyjson.Object {
	keys := make([]string, 0, len(patch))
	for k := range patch {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	out := pyjson.NewObject()
	for _, k := range keys {
		out.Set(k, patch[k])
	}
	return out
}

// Merges the patch into model_config then runs the statement with the merged text in one write, a missing row a no op
func (d *SessionDB) writeModelConfigPatch(ctx context.Context, sessionID string, patch *pyjson.Object, query string, params func(merged any) []any) error {
	return d.executeWrite(ctx, func(conn SQLiteConn) error {
		merged, missing, err := mergeModelConfigJSON(ctx, conn, sessionID, patch, false)
		if err != nil || missing {
			return err
		}
		args := []any{merged, sessionID}
		if params != nil {
			args = params(merged)
		}
		_, err = conn.ExecContext(ctx, query, args...)
		return err
	}, nil)
}

// Merges the patch into model_config atomically, nil removing a key, a no op for an empty patch or missing row
func (d *SessionDB) PatchSessionModelConfig(ctx context.Context, sessionID string, patch map[string]any) error {
	if sessionID == "" || len(patch) == 0 {
		return nil
	}
	return d.writeModelConfigPatch(ctx, sessionID, modelConfigPatchObject(patch), "UPDATE sessions SET model_config = ? WHERE id = ?", nil)
}

// Merges keys into the model config of a session, a nil value removing the key
func (d *DB) PatchModelConfig(id string, patch map[string]any) error {
	return d.PatchSessionModelConfig(context.Background(), id, patch)
}

// One key of a session model_config through the tolerant parse, def when the session or key is absent
func (d *SessionDB) GetSessionModelConfigValue(ctx context.Context, sessionID, key string, def any) (any, error) {
	session, err := d.GetSession(ctx, sessionID)
	if err != nil {
		return nil, err
	}
	if v, ok := parseModelConfigValue(rowValue(session, "model_config")).Get(key); ok {
		return v, nil
	}
	return def, nil
}

// One key of a session model_config, false when the session, the key or the read is missing
func (d *DB) ModelConfigValue(sessionID, key string) (any, bool) {
	session, err := d.GetSession(context.Background(), sessionID)
	if err != nil {
		return nil, false
	}
	return parseModelConfigValue(rowValue(session, "model_config")).Get(key)
}

// SessionYoloEnabled reads the persisted approval bypass of a session row, false on any parse failure
func SessionYoloEnabled(sessionMeta map[string]any) bool {
	return pyTruthy(ParseModelConfig(sessionMeta["model_config"])["yolo_mode"])
}
