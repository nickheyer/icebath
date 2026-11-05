package state

import (
	"database/sql"
	"encoding/json"
	"errors"
)

// Non throwing JSON lookup over a JSON text column
func sqlJSONExtract(expression, path string) string {
	return "json_extract((CASE WHEN json_valid(" + expression + ") THEN " + expression + " ELSE json_object() END), '" + path + "')"
}

// End reasons of a reset boundary a legacy reset child rides on
const resetEndReasonsSQL = "'session_reset', 'session_switch', 'idle', 'daily', 'suspended', 'resume_pending_expired'"

// Pre marker reset continuation heuristic over the alias of a child row
func legacyResetChildSQL(alias string) string {
	return "EXISTS (SELECT 1 FROM sessions p WHERE p.id = " + alias + ".parent_session_id AND p.end_reason IN (" + resetEndReasonsSQL + ")" +
		" AND " + alias + ".session_key IS NOT NULL AND " + alias + ".session_key != '' AND " + alias + ".session_key = p.session_key)"
}

// Tolerant parse of a model config column
func parseModelConfig(raw sql.NullString) map[string]any {
	cfg := map[string]any{}
	if raw.Valid && raw.String != "" {
		_ = json.Unmarshal([]byte(raw.String), &cfg)
	}
	return cfg
}

// GetModelConfigValue reads one key of a session's model config, false when the session or key is absent
func (d *DB) GetModelConfigValue(id, key string) (any, bool, error) {
	var raw sql.NullString
	err := d.read(func(q querier) error {
		return q.QueryRow("SELECT model_config FROM sessions WHERE id = ?", id).Scan(&raw)
	})
	if errors.Is(err, sql.ErrNoRows) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	v, ok := parseModelConfig(raw)[key]
	return v, ok, nil
}

// Compression tip of a session resolved inside one transaction
func compressionTipTx(tx *Tx, id string) (string, error) {
	cur := id
	seen := map[string]bool{id: true}
	for {
		var parent string
		err := tx.QueryRow(`SELECT parent.id FROM sessions child JOIN sessions parent ON parent.id = child.parent_session_id
			WHERE child.id = ? AND parent.end_reason = 'compression'
			AND COALESCE(json_extract(child.model_config, '$._branched_from'), '') = ''
			AND COALESCE(json_extract(child.model_config, '$._delegate_from'), '') = ''`, cur).Scan(&parent)
		if errors.Is(err, sql.ErrNoRows) || (err == nil && seen[parent]) {
			break
		}
		if err != nil {
			return "", err
		}
		seen[parent] = true
		cur = parent
	}
	seen = map[string]bool{cur: true}
	for {
		var child string
		err := tx.QueryRow(`SELECT child.id FROM sessions child JOIN sessions parent ON parent.id = child.parent_session_id
			WHERE parent.id = ? AND parent.end_reason = 'compression'
			AND COALESCE(json_extract(child.model_config, '$._branched_from'), '') = ''
			AND COALESCE(json_extract(child.model_config, '$._delegate_from'), '') = ''
			ORDER BY child.started_at DESC LIMIT 1`, cur).Scan(&child)
		if errors.Is(err, sql.ErrNoRows) || (err == nil && seen[child]) {
			return cur, nil
		}
		if err != nil {
			return "", err
		}
		seen[child] = true
		cur = child
	}
}

// UpdateConversationModelConfigValue rewrites one conversation wide key at the compression tip in one transaction, a nil result removing it
func (d *DB) UpdateConversationModelConfigValue(id, key string, update func(current any) any) (string, error) {
	written := ""
	err := d.write(func(tx *Tx) error {
		tip, err := compressionTipTx(tx, id)
		if err != nil {
			return err
		}
		var raw sql.NullString
		if err := tx.QueryRow("SELECT model_config FROM sessions WHERE id = ?", tip).Scan(&raw); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return nil
			}
			return err
		}
		cfg := parseModelConfig(raw)
		if value := update(cfg[key]); value == nil {
			delete(cfg, key)
		} else {
			cfg[key] = value
		}
		encoded, err := json.Marshal(cfg)
		if err != nil {
			return err
		}
		if _, err := tx.Exec("UPDATE sessions SET model_config = ? WHERE id = ?", string(encoded), tip); err != nil {
			return err
		}
		written = tip
		return nil
	})
	return written, err
}

// SessionIDsWithModelConfigKey lists the sessions whose model config carries the key, newest first
func (d *DB) SessionIDsWithModelConfigKey(key string) ([]string, error) {
	var ids []string
	err := d.read(func(q querier) error {
		rows, err := q.Query("SELECT id FROM sessions WHERE " + sqlJSONExtract("model_config", "$."+key) + " IS NOT NULL ORDER BY started_at DESC")
		if err != nil {
			return err
		}
		defer rows.Close()
		ids = nil
		for rows.Next() {
			var id string
			if err := rows.Scan(&id); err != nil {
				return err
			}
			ids = append(ids, id)
		}
		return rows.Err()
	})
	return ids, err
}

// ResolveResumeSessionID follows the compression chain to its tip then the deepest continuation holding messages
func (d *DB) ResolveResumeSessionID(id string) string {
	if id == "" {
		return id
	}
	if tip, err := d.CompressionTip(id); err == nil && tip != "" {
		id = tip
	}
	current, best := id, ""
	seen := map[string]bool{current: true}
	for i := 0; i < 32; i++ {
		var one int
		err := d.read(func(q querier) error {
			return q.QueryRow("SELECT 1 FROM messages WHERE session_id = ? LIMIT 1", current).Scan(&one)
		})
		if err == nil {
			best = current
		} else if !errors.Is(err, sql.ErrNoRows) {
			return id
		}
		var child string
		err = d.read(func(q querier) error {
			return q.QueryRow("SELECT id FROM sessions AS child WHERE child.parent_session_id = ?"+
				" AND "+sqlJSONExtract("child.model_config", "$._branched_from")+" IS NULL"+
				" AND "+sqlJSONExtract("child.model_config", "$._delegate_from")+" IS NULL"+
				" AND "+sqlJSONExtract("child.model_config", "$._reset_from")+" IS NULL"+
				" AND NOT "+legacyResetChildSQL("child")+
				" AND COALESCE(child.source, '') != 'tool'"+
				" ORDER BY child.started_at DESC, child.id DESC LIMIT 1", current).Scan(&child)
		})
		if errors.Is(err, sql.ErrNoRows) || (err == nil && (child == "" || seen[child])) {
			break
		}
		if err != nil {
			return id
		}
		current = child
		seen[current] = true
	}
	if best != "" {
		return best
	}
	return id
}
