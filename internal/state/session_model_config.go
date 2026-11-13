package state

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/nickheyer/icebath/internal/fileutil"
)

// Error of a model config merge against a row that does not exist
var ErrSessionNotFound = errors.New("session not found")

// One member of a JSON object in its stored order
type configMember struct {
	key   string
	value json.RawMessage
}

// Members of model_config text in order, empty for anything that is not a JSON object
func configMembers(text string) []configMember {
	t := bytes.TrimSpace([]byte(text))
	if len(t) == 0 || t[0] != '{' {
		return nil
	}
	dec := json.NewDecoder(bytes.NewReader(t))
	dec.UseNumber()
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
		replaced := false
		for i := range out {
			if out[i].key == key {
				out[i].value, replaced = raw, true
			}
		}
		if !replaced {
			out = append(out, configMember{key, raw})
		}
	}
	if _, err := dec.Token(); err != nil {
		return nil
	}
	return out
}

// JSON text of a patch value, raw JSON kept in its order and other values encoded with sorted keys
func patchValueJSON(v any) (string, error) {
	if raw, ok := v.(json.RawMessage); ok {
		return pythonJSONText(raw)
	}
	enc, err := fileutil.EncodeJSON(v, fileutil.JSONStyle{Sorted: true, ASCII: true})
	return string(enc), err
}

// Members with patch applied, nil values deleting keys and new keys appended in sorted order
func applyConfigPatch(members []configMember, patch map[string]any) ([]configMember, error) {
	keys := make([]string, 0, len(patch))
	for k := range patch {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		v := patch[k]
		idx := -1
		for i := range members {
			if members[i].key == k {
				idx = i
			}
		}
		if v == nil {
			if idx >= 0 {
				members = append(members[:idx], members[idx+1:]...)
			}
			continue
		}
		text, err := patchValueJSON(v)
		if err != nil {
			return nil, err
		}
		if idx >= 0 {
			members[idx].value = json.RawMessage(text)
		} else {
			members = append(members, configMember{k, json.RawMessage(text)})
		}
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
		if err := writePythonString(&b, m.key); err != nil {
			return "", err
		}
		b.WriteString(": ")
		text, err := pythonJSONText(m.value)
		if err != nil {
			return "", err
		}
		b.WriteString(text)
	}
	b.WriteByte('}')
	return b.String(), nil
}

// Merged model_config of the row on c, NULL when the merge leaves it empty, false when the row is missing
func mergeModelConfigJSON(ctx context.Context, c Conn, sessionID string, patch map[string]any) (sql.NullString, bool, error) {
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

// Exported merge for authorities inside their own transaction, ErrSessionNotFound when the row is missing
func MergeModelConfigIn(ctx context.Context, c Conn, sessionID string, patch map[string]any) (sql.NullString, error) {
	merged, ok, err := mergeModelConfigJSON(ctx, c, sessionID, patch)
	if err == nil && !ok {
		return merged, fmt.Errorf("%w: %s", ErrSessionNotFound, sessionID)
	}
	return merged, err
}

// Merges patch into model_config and runs query with args from the merge in one write, nothing for a missing row
func (s *Store) writeModelConfigPatch(ctx context.Context, sessionID string, patch map[string]any, query string, args func(merged sql.NullString) []any) error {
	return s.Write(ctx, WritePatience, func(c Conn) error {
		merged, ok, err := mergeModelConfigJSON(ctx, c, sessionID, patch)
		if err != nil || !ok {
			return err
		}
		_, err = c.ExecContext(ctx, query, args(merged)...)
		return err
	})
}

// Merges patch into model_config atomically, a nil value removing its key, nothing for an empty patch or id
func (s *Store) PatchSessionModelConfig(ctx context.Context, sessionID string, patch map[string]any) error {
	if sessionID == "" || len(patch) == 0 {
		return nil
	}
	return s.writeModelConfigPatch(ctx, sessionID, patch, "UPDATE sessions SET model_config = ? WHERE id = ?",
		func(merged sql.NullString) []any { return []any{merged, sessionID} })
}

// JSON object text with members in the given order
func orderedJSON(pairs ...any) (json.RawMessage, error) {
	members := make([]configMember, 0, len(pairs)/2)
	for i := 0; i+1 < len(pairs); i += 2 {
		text, err := patchValueJSON(pairs[i+1])
		if err != nil {
			return nil, err
		}
		members = append(members, configMember{pairs[i].(string), json.RawMessage(text)})
	}
	text, err := encodeConfigMembers(members)
	return json.RawMessage(text), err
}

// Sets the model after a mid-session switch, the whole route in both resume shapes when the provider is known
func (s *Store) UpdateSessionModel(ctx context.Context, sessionID, model, provider, baseURL, apiMode string) error {
	s.tokenBarrier(ctx, false)
	patch := map[string]any{"browser_model_lock": nil}
	if model != "" {
		patch["model"] = model
	}
	if provider != "" {
		route, err := orderedJSON("provider", provider, "base_url", emptyNull(baseURL), "api_mode", emptyNull(apiMode))
		if err != nil {
			return err
		}
		patch["provider"], patch["base_url"], patch["api_mode"], patch["gateway_runtime"] = provider, emptyNull(baseURL), emptyNull(apiMode), route
	}
	return s.writeModelConfigPatch(ctx, sessionID, patch, "UPDATE sessions SET model = ?, model_config = ? WHERE id = ?",
		func(merged sql.NullString) []any { return []any{model, merged, sessionID} })
}

// Persists a Browser or API client runtime lock into model_config, the lineage markers surviving
func (s *Store) UpdateSessionRuntimeLock(ctx context.Context, sessionID string, model, provider *string, modelOptions map[string]any, routeSource string, confirmed bool) error {
	options := modelOptions
	if options == nil {
		options = map[string]any{}
	}
	lock, err := orderedJSON("provider", derefText(provider), "model", derefText(model), "model_options", options,
		"route_source", routeSource, "confirmed", confirmed, "updated_at", nowEpoch())
	if err != nil {
		return err
	}
	modelArg := nullable(model)
	return s.writeModelConfigPatch(ctx, sessionID, map[string]any{"browser_model_lock": lock}, `UPDATE sessions SET
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
	return s.writeModelConfigPatch(ctx, sessionID, map[string]any{"yolo_mode": enabled}, "UPDATE sessions SET model_config = ? WHERE id = ?",
		func(merged sql.NullString) []any { return []any{merged, sessionID} })
}

// Reports the persisted YOLO flag, false on any parse failure so a resume never enables the bypass
func SessionYoloEnabled(modelConfig sql.Null[string]) bool {
	return jsonTruthy(parseModelConfigRaw(modelConfig.V)["yolo_mode"])
}

// Raw JSON of one model_config key of a session, false when the row or key is absent
func (s *Store) GetSessionModelConfigValue(ctx context.Context, sessionID, key string) (json.RawMessage, bool, error) {
	row, ok, err := s.GetSession(ctx, sessionID)
	if err != nil || !ok {
		return nil, false, err
	}
	v, ok := parseModelConfigRaw(row.ModelConfig.V)[key]
	return v, ok, nil
}

// Writes update of a conversation wide key at the compression tip in one write, returning the id written
func (s *Store) UpdateConversationModelConfigValue(ctx context.Context, sessionID, key string, update func(current json.RawMessage, present bool) any) (string, bool, error) {
	written, ok := "", false
	err := s.Write(ctx, WritePatience, func(c Conn) error {
		written, ok = "", false
		tip, err := compressionTipOn(ctx, c, sessionID)
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
		if members, err = applyConfigPatch(members, map[string]any{key: update(current, present)}); err != nil {
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
		ids, err = queryStrings(ctx, c, "SELECT id FROM sessions WHERE "+jsonExtractSQL("model_config", "$."+key)+" IS NOT NULL ORDER BY started_at DESC")
		return err
	})
	return ids, err
}
