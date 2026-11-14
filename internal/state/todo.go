package state

import (
	"context"
	"database/sql"
	"encoding/json"
	"math"
	"strconv"
	"strings"
)

const (
	todoToolsSQL        = "tool_name IN ('todo_list', 'todo')"
	maxTodoResultChars  = 512000
	promptRefreshKey    = "prompt_refresh.epoch"
	promptRefreshPrefix = "prompt_refresh.epoch:"
)

func todoCopies(r *msgRow, identity []byte) []string {
	if len(identity) == 0 {
		identity = displayIdentity(displayKeyOf(r))
	}
	copies := []string{"h:" + string(identity)}
	for _, uid := range append([]string{textCell(r.messageUID)}, uidListCell(r.absorbedMessageUIDs)...) {
		if uid != "" {
			copies = append(copies, "u:"+uid)
		}
	}
	return copies
}

func removedTodoCopies(ctx context.Context, c Conn, sessionIDs []string) (map[string]int64, error) {
	rows, err := queryMsgRows(c.QueryContext(ctx, "SELECT id, role, timestamp, tool_call_id, tool_calls, tool_name, message_uid, absorbed_message_uids, "+
		"display_identity, CASE WHEN display_identity IS NULL THEN content END AS content "+
		"FROM messages WHERE session_id IN ("+bindMarks(len(sessionIDs))+") AND active = 0 "+
		"AND compacted = 0 AND role = 'tool' AND "+todoToolsSQL, stringArgs(sessionIDs)...))
	if err != nil {
		return nil, err
	}
	removed := map[string]int64{}
	for _, r := range rows {
		identity, _ := r.displayIdentity.([]byte)
		for _, key := range todoCopies(r, identity) {
			removed[key] = max(removed[key], r.id)
		}
	}
	return removed, nil
}

func todoCopyRemoved(r *msgRow, removed map[string]int64) bool {
	if len(removed) == 0 {
		return false
	}
	for _, key := range todoCopies(r, nil) {
		if removed[key] > r.id {
			return true
		}
	}
	return false
}

func isTodoCall(call json.RawMessage, callID string) bool {
	members, ok := objectMembers(call)
	if !ok {
		return false
	}
	if id, _ := memberValue(members, "id"); !jsonIsString(id) || jsonString(id) != callID {
		return false
	}
	rawFunction, _ := memberValue(members, "function")
	function, ok := objectMembers(rawFunction)
	if !ok {
		return false
	}
	name, _ := memberText(function, "name")
	if name == "todo_list" || name == "todo" {
		return true
	}
	if name != "tool_call" {
		return false
	}
	arguments, _ := memberValue(function, "arguments")
	if jsonIsString(arguments) {
		arguments = json.RawMessage(jsonString(arguments))
	}
	args, ok := objectMembers(arguments)
	if !ok {
		return false
	}
	rawCalls, _ := memberValue(args, "calls")
	inner, ok := arrayElements(rawCalls)
	if !ok {
		return false
	}
	for _, e := range inner {
		if fields, isObject := objectMembers(e); isObject {
			if n, _ := memberText(fields, "name"); n == "todo_list" || n == "todo" {
				return true
			}
		}
	}
	return false
}

func todoRevisionValid(raw json.RawMessage) bool {
	if !jsonTruthy(raw) {
		return true
	}
	switch jsonKind(raw) {
	case "int", "bool":
		return true
	case "float":
		f, err := strconv.ParseFloat(strings.TrimSpace(string(raw)), 64)
		return err == nil && !math.IsInf(f, 0) && !math.IsNaN(f)
	case "string":
		_, err := strconv.ParseInt(strings.TrimSpace(jsonString(raw)), 10, 64)
		return err == nil
	}
	return false
}

func validTodoResult(content string) bool {
	if !json.Valid([]byte(content)) {
		return false
	}
	data, ok := objectMembers(json.RawMessage(content))
	if !ok {
		return false
	}
	todos, _ := memberValue(data, "todos")
	if jsonKind(todos) != "array" {
		return false
	}
	revision, _ := memberValue(data, "revision")
	return todoRevisionValid(revision)
}

func (s *Store) GetLatestTodoResult(ctx context.Context, sessionID string, maxChars int) (string, bool, error) {
	if sessionID == "" {
		return "", false, nil
	}
	if maxChars <= 0 {
		maxChars = maxTodoResultChars
	}
	sessionIDs, err := s.ResumeLineageIDs(ctx, sessionID)
	if err != nil {
		return "", false, err
	}
	result, found := "", false
	err = s.Read(ctx, func(c Conn) error {
		result, found = "", false
		removed, err := removedTodoCopies(ctx, c, sessionIDs)
		if err != nil {
			return err
		}
		for i := len(sessionIDs) - 1; i >= 0; i-- {
			sid := sessionIDs[i]
			before := int64(math.MaxInt64)
			for {
				rows, err := queryMsgRows(c.QueryContext(ctx, "SELECT id, role, content, timestamp, tool_call_id, tool_calls, tool_name, message_uid, "+
					"absorbed_message_uids FROM messages INDEXED BY idx_messages_session_id "+
					"WHERE session_id = ? AND id < ? AND role = 'tool' AND "+todoToolsSQL+" AND (active = 1 OR compacted = 1) "+
					"AND length(content) <= ? ORDER BY id DESC LIMIT 100", sid, before, maxChars))
				if err != nil {
					return err
				}
				if len(rows) == 0 {
					break
				}
				for _, r := range rows {
					callID := textCell(r.toolCallID)
					if callID == "" || todoCopyRemoved(r, removed) {
						continue
					}
					var role, calls sql.NullString
					err := c.QueryRowContext(ctx, "SELECT role, tool_calls FROM messages INDEXED BY idx_messages_session_id "+
						"WHERE session_id = ? AND id < ? AND role IN ('assistant', 'user', 'system') "+
						"AND (active = 1 OR compacted = 1) ORDER BY id DESC LIMIT 1", sid, r.id).Scan(&role, &calls)
					if err == sql.ErrNoRows {
						continue
					}
					if err != nil {
						return err
					}
					if role.String != "assistant" {
						continue
					}
					paired := false
					if calls.String != "" {
						if !json.Valid([]byte(calls.String)) {
							continue
						}
						list, _ := arrayElements(json.RawMessage(calls.String))
						for _, call := range list {
							paired = paired || isTodoCall(call, callID)
						}
					}
					content, isText := r.content.(string)
					if !paired || !isText || !validTodoResult(content) {
						continue
					}
					result, found = content, true
					return nil
				}
				before = rows[len(rows)-1].id
			}
		}
		return nil
	})
	return result, found, err
}

func (s *Store) RequestPromptRefresh(ctx context.Context, sessionID string) (int64, error) {
	key := promptRefreshKey
	if sessionID != "" {
		key = promptRefreshPrefix + sessionID
	}
	var epoch int64
	err := s.Write(ctx, WritePatience, func(c Conn) error {
		var current sql.Null[int64]
		if err := c.QueryRowContext(ctx, "SELECT MAX(CAST(value AS INTEGER)) FROM state_meta WHERE key = ? OR substr(key, 1, ?) = ?",
			promptRefreshKey, len(promptRefreshPrefix), promptRefreshPrefix).Scan(&current); err != nil {
			return err
		}
		epoch = current.V + 1
		_, err := c.ExecContext(ctx, "INSERT INTO state_meta (key, value) VALUES (?, ?) "+
			"ON CONFLICT(key) DO UPDATE SET value = excluded.value", key, strconv.FormatInt(epoch, 10))
		return err
	})
	return epoch, err
}

func (s *Store) PromptRefreshEpoch(ctx context.Context, sessionID string) (int64, error) {
	var epoch sql.Null[int64]
	err := s.Read(ctx, func(c Conn) error {
		return c.QueryRowContext(ctx, "SELECT MAX(CAST(value AS INTEGER)) FROM state_meta WHERE key IN (?, ?)",
			promptRefreshKey, promptRefreshPrefix+sessionID).Scan(&epoch)
	})
	return epoch.V, err
}

func (s *Store) CountSessionsWithStoredPrompt(ctx context.Context) (int64, error) {
	var count int64
	err := s.Read(ctx, func(c Conn) error {
		return c.QueryRowContext(ctx, "SELECT COUNT(*) FROM sessions WHERE system_prompt_hash IS NOT NULL OR system_prompt IS NOT NULL").Scan(&count)
	})
	return count, err
}
