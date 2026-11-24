package state

import (
	"context"
	"sort"

	jsonutil "github.com/nickheyer/icebath/pkg/utils/json"
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
	for _, k := range parsed.Keys() {
		out[k] = parsed.Value(k)
	}
	return out
}

// Ordered patch object of a Go map in sorted key order
func modelConfigPatchObject(patch map[string]any) *jsonutil.Object {
	keys := make([]string, 0, len(patch))
	for k := range patch {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	out := jsonutil.NewObject()
	for _, k := range keys {
		out.Set(k, patch[k])
	}
	return out
}

// Merges the patch into model_config then runs the statement with the merged text in one write, a missing row a no op
func (d *SessionDB) writeModelConfigPatch(ctx context.Context, sessionID string, patch *jsonutil.Object, query string, params func(merged any) []any) error {
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

// Plain model_config of a session row, nested objects as plain maps
func plainModelConfig(raw any) map[string]any {
	plain, _ := jsonutil.Plain(parseModelConfigValue(raw)).(map[string]any)
	if plain == nil {
		plain = map[string]any{}
	}
	return plain
}

// SessionModelConfig is a session's model_config with nested objects as plain maps, empty when the session or its config is missing
func (d *SessionDB) SessionModelConfig(ctx context.Context, sessionID string) (map[string]any, error) {
	session, err := d.GetSession(ctx, sessionID)
	if err != nil || session == nil {
		return map[string]any{}, err
	}
	return plainModelConfig(rowValue(session, "model_config")), nil
}

// UpdateSessionModelConfig rewrites a session's model_config through fn in one write, fn answering false leaving it untouched and a missing row a no op
func (d *SessionDB) UpdateSessionModelConfig(ctx context.Context, sessionID string, fn func(cfg map[string]any) bool) error {
	if sessionID == "" {
		return nil
	}
	return d.executeWrite(ctx, func(conn SQLiteConn) error {
		row, err := connRow(ctx, conn, "SELECT model_config FROM sessions WHERE id = ?", sessionID)
		if err != nil || row == nil {
			return err
		}
		cfg := plainModelConfig(row["model_config"])
		if !fn(cfg) {
			return nil
		}
		var text any
		if len(cfg) > 0 {
			dumped, err := jsonutil.Encode(cfg, jsonutil.SortKeys(), jsonutil.Spaced(), jsonutil.ASCII())
			if err != nil {
				return err
			}
			text = dumped
		}
		_, err = conn.ExecContext(ctx, "UPDATE sessions SET model_config = ? WHERE id = ?", text, sessionID)
		return err
	}, nil)
}

// model_config key holding the values a session set for itself in the settings session layer
const SessionSettingsKey = "session_settings"

// SessionsWithModelConfigKey lists the open sessions whose model_config holds a key
func (d *SessionDB) SessionsWithModelConfigKey(ctx context.Context, key string) ([]string, error) {
	rows, err := d.readRows(ctx, "SELECT id FROM sessions WHERE ended_at IS NULL AND "+sqlJSONExtract("model_config", "$."+key)+" IS NOT NULL")
	if err != nil {
		return nil, err
	}
	ids := make([]string, 0, len(rows))
	for _, row := range rows {
		if id, ok := rowValue(row, "id").(string); ok && id != "" {
			ids = append(ids, id)
		}
	}
	return ids, nil
}
