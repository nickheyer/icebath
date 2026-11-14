package state

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/nickheyer/icebath/internal/fileutil"
)

// Error of a model config merge against a row that does not exist
var ErrSessionNotFound = errors.New("Session not found")

// One member of a model_config object, a nil Value in a patch removing the key
type ConfigMember struct {
	Key   string
	Value any
}

// One member of a stored JSON object in its stored order
type configMember struct {
	key   string
	value json.RawMessage
}

// Members of model_config text in order, a repeated key keeping its first place, empty for anything but one JSON object
func configMembers(text string) []configMember {
	t := bytes.TrimSpace([]byte(text))
	if len(t) == 0 || t[0] != '{' {
		return nil
	}
	dec := json.NewDecoder(bytes.NewReader(t))
	if _, err := dec.Token(); err != nil {
		return nil
	}
	var out []configMember
	for dec.More() {
		tok, err := dec.Token()
		if err != nil {
			return nil
		}
		key, _ := tok.(string)
		var raw json.RawMessage
		if err := dec.Decode(&raw); err != nil {
			return nil
		}
		out = setConfigMember(out, key, raw)
	}
	if _, err := dec.Token(); err != nil {
		return nil
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return nil
	}
	return out
}

// Members with key set to value, an existing key keeping its place and a new one appended
func setConfigMember(members []configMember, key string, value json.RawMessage) []configMember {
	for i := range members {
		if members[i].key == key {
			members[i].value = value
			return members
		}
	}
	return append(members, configMember{key, value})
}

// JSON text of a member value, raw JSON and struct fields in their own order and map keys sorted
func patchValueJSON(v any) (string, error) {
	if raw, ok := v.(json.RawMessage); ok {
		return spacedJSON(raw)
	}
	enc, err := fileutil.EncodeJSON(v, fileutil.JSONStyle{ASCII: true})
	return string(enc), err
}

// Members with patch applied in its order, nil values deleting keys and new keys appended
func applyConfigPatch(members []configMember, patch []ConfigMember) ([]configMember, error) {
	for _, p := range patch {
		if p.Value == nil {
			for i := range members {
				if members[i].key == p.Key {
					members = append(members[:i], members[i+1:]...)
					break
				}
			}
			continue
		}
		text, err := patchValueJSON(p.Value)
		if err != nil {
			return nil, err
		}
		members = setConfigMember(members, p.Key, json.RawMessage(text))
	}
	return members, nil
}

// JSON object text of members in the spaced layout
func encodeConfigMembers(members []configMember) (string, error) {
	var b strings.Builder
	b.WriteByte('{')
	for i, m := range members {
		if i > 0 {
			b.WriteString(", ")
		}
		key, err := asciiJSONString(m.key)
		if err != nil {
			return "", err
		}
		b.WriteString(key + ": ")
		text, err := spacedJSON(m.value)
		if err != nil {
			return "", err
		}
		b.WriteString(text)
	}
	b.WriteByte('}')
	return b.String(), nil
}

// Merged model_config of the row on c, NULL when the merge leaves it empty, false when the row is missing
func mergeModelConfigJSON(ctx context.Context, c Conn, sessionID string, patch []ConfigMember) (sql.NullString, bool, error) {
	var raw sql.NullString
	err := c.QueryRowContext(ctx, "SELECT model_config FROM sessions WHERE id = ?", sessionID).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return sql.NullString{}, false, nil
	}
	if err != nil {
		return sql.NullString{}, false, err
	}
	members, err := applyConfigPatch(configMembers(raw.String), patch)
	if err != nil || len(members) == 0 {
		return sql.NullString{}, true, err
	}
	text, err := encodeConfigMembers(members)
	return sql.NullString{String: text, Valid: err == nil}, true, err
}

// Merge for authorities inside their own transaction, ErrSessionNotFound naming the id when the row is missing
func mergeModelConfigIn(ctx context.Context, c Conn, sessionID string, patch []ConfigMember) (sql.NullString, error) {
	merged, ok, err := mergeModelConfigJSON(ctx, c, sessionID, patch)
	if err == nil && !ok {
		return merged, fmt.Errorf("%w: %s", ErrSessionNotFound, sessionID)
	}
	return merged, err
}

// Merges patch into model_config and runs query with args from the merge in one write, nothing for a missing row
func (s *Store) writeModelConfigPatch(ctx context.Context, sessionID string, patch []ConfigMember, query string, args func(merged sql.NullString) []any) error {
	return s.Write(ctx, WritePatience, func(c Conn) error {
		merged, ok, err := mergeModelConfigJSON(ctx, c, sessionID, patch)
		if err != nil || !ok {
			return err
		}
		_, err = c.ExecContext(ctx, query, args(merged)...)
		return err
	})
}

// Merges patch into model_config atomically in its order, a nil value removing its key
func (s *Store) PatchSessionModelConfig(ctx context.Context, sessionID string, patch []ConfigMember) error {
	if sessionID == "" || len(patch) == 0 {
		return nil
	}
	return s.writeModelConfigPatch(ctx, sessionID, patch, "UPDATE sessions SET model_config = ? WHERE id = ?",
		func(merged sql.NullString) []any { return []any{merged, sessionID} })
}

// JSON object text of members in the given order, nil values written as null
func objectJSON(members ...ConfigMember) (json.RawMessage, error) {
	var stored []configMember
	for _, m := range members {
		text, err := patchValueJSON(m.Value)
		if err != nil {
			return nil, err
		}
		stored = setConfigMember(stored, m.Key, json.RawMessage(text))
	}
	text, err := encodeConfigMembers(stored)
	return json.RawMessage(text), err
}

// Sets the model after a mid-session switch, the whole route in both resume shapes when the provider is known
func (s *Store) UpdateSessionModel(ctx context.Context, sessionID, model, provider, baseURL, apiMode string) error {
	s.tokenBarrier(ctx, false)
	patch := []ConfigMember{{Key: "browser_model_lock"}}
	if model != "" {
		patch = append(patch, ConfigMember{"model", model})
	}
	if provider != "" {
		route, err := objectJSON(ConfigMember{"provider", provider}, ConfigMember{"base_url", emptyNull(baseURL)}, ConfigMember{"api_mode", emptyNull(apiMode)})
		if err != nil {
			return err
		}
		patch = append(patch, ConfigMember{"provider", provider}, ConfigMember{"base_url", emptyNull(baseURL)},
			ConfigMember{"api_mode", emptyNull(apiMode)}, ConfigMember{"gateway_runtime", route})
	}
	return s.writeModelConfigPatch(ctx, sessionID, patch, "UPDATE sessions SET model = ?, model_config = ? WHERE id = ?",
		func(merged sql.NullString) []any { return []any{model, merged, sessionID} })
}

// Persists a Browser or API client runtime lock into model_config, the lineage markers surviving
func (s *Store) UpdateSessionRuntimeLock(ctx context.Context, sessionID string, model, provider *string, modelOptions json.RawMessage, routeSource string, confirmed bool) error {
	options := modelOptions
	if !jsonTruthy(options) {
		options = json.RawMessage("{}")
	}
	lock, err := objectJSON(ConfigMember{"provider", derefText(provider)}, ConfigMember{"model", derefText(model)},
		ConfigMember{"model_options", options}, ConfigMember{"route_source", routeSource},
		ConfigMember{"confirmed", confirmed}, ConfigMember{"updated_at", nowEpoch()})
	if err != nil {
		return err
	}
	modelArg := nullable(model)
	return s.writeModelConfigPatch(ctx, sessionID, []ConfigMember{{"browser_model_lock", lock}}, `UPDATE sessions SET
                   model_config = ?,
                   model = COALESCE(?, model)
                   WHERE id = ?`, func(merged sql.NullString) []any { return []any{merged, modelArg, sessionID} })
}

// Text a pointer holds, empty for nil
func derefText(v *string) string {
	if v == nil {
		return ""
	}
	return *v
}

// Persists the per-session YOLO flag so it survives a resume, nothing without a row
func (s *Store) SetSessionYolo(ctx context.Context, sessionID string, enabled bool) error {
	if sessionID == "" {
		return nil
	}
	return s.writeModelConfigPatch(ctx, sessionID, []ConfigMember{{"yolo_mode", enabled}}, "UPDATE sessions SET model_config = ? WHERE id = ?",
		func(merged sql.NullString) []any { return []any{merged, sessionID} })
}

// Reports the persisted YOLO flag, false on any parse failure so a resume never enables the bypass
func SessionYoloEnabled(modelConfig sql.Null[string]) bool {
	return jsonTruthy(ParseModelConfig(modelConfig.V)["yolo_mode"])
}

// Raw JSON of one model_config key of a session, false when the row or key is absent
func (s *Store) GetSessionModelConfigValue(ctx context.Context, sessionID, key string) (json.RawMessage, bool, error) {
	row, ok, err := s.GetSession(ctx, sessionID)
	if err != nil || !ok {
		return nil, false, err
	}
	v, ok := ParseModelConfig(row.ModelConfig.V)[key]
	return v, ok, nil
}

// Writes update of a conversation wide key at the compression tip in one write, returning the id written
func (s *Store) UpdateConversationModelConfigValue(ctx context.Context, sessionID, key string, update func(current json.RawMessage, present bool) any) (string, bool, error) {
	written, ok := "", false
	err := s.Write(ctx, WritePatience, func(c Conn) error {
		written, ok = "", false
		tip, err := CompressionTipIn(ctx, c, sessionID)
		if err != nil {
			return err
		}
		var raw sql.NullString
		err = c.QueryRowContext(ctx, "SELECT model_config FROM sessions WHERE id = ?", tip).Scan(&raw)
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		members := configMembers(raw.String)
		var current json.RawMessage
		present := false
		for _, m := range members {
			if m.key == key {
				current, present = m.value, true
			}
		}
		if members, err = applyConfigPatch(members, []ConfigMember{{key, update(current, present)}}); err != nil {
			return err
		}
		text, err := encodeConfigMembers(members)
		if err != nil {
			return err
		}
		if _, err := c.ExecContext(ctx, "UPDATE sessions SET model_config = ? WHERE id = ?", text, tip); err != nil {
			return err
		}
		written, ok = tip, true
		return nil
	})
	return written, ok, err
}

// Ids of sessions whose model_config carries key, newest first
func (s *Store) SessionIDsWithModelConfigKey(ctx context.Context, key string) ([]string, error) {
	var ids []string
	err := s.Read(ctx, func(c Conn) error {
		var err error
		ids, err = queryStrings(ctx, c, "SELECT id FROM sessions WHERE "+JSONExtractSQL("model_config", "$."+key)+" IS NOT NULL ORDER BY started_at DESC")
		return err
	})
	return ids, err
}
