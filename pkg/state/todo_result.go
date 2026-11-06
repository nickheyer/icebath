package state

import (
	"context"
	"encoding/json"
	"errors"
	"math"

	"github.com/nickheyer/icebath/pkg/internal/pyint"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
)

const todoToolsSQL = "tool_name IN ('todo_list', 'todo')"

// Largest Todo result the resume restores, the todo tool cap
const MaxTodoResultChars = 512_000

// Newest visible, paired and valid Todo result in the resume display scope, false when none
func (d *SessionDB) GetLatestTodoResult(ctx context.Context, sessionID string, maxChars *int) (string, bool, error) {
	if sessionID == "" {
		return "", false, nil
	}
	limit := MaxTodoResultChars
	if maxChars != nil {
		limit = *maxChars
	}
	sessionIDs, err := d.resumeLineageIDs(ctx, sessionID)
	if err != nil {
		return "", false, err
	}
	result, found := "", false
	err = d.readCtx(ctx, true, func(conn SQLiteConn) error {
		result, found = "", false
		removed, err := removedTodoCopies(ctx, conn, sessionIDs)
		if err != nil {
			return err
		}
		for i := len(sessionIDs) - 1; i >= 0; i-- {
			sid := sessionIDs[i]
			beforeID := int64(math.MaxInt64)
			for {
				rows, err := queryObjects(ctx, conn, "SELECT id, role, content, timestamp, tool_call_id, tool_calls, tool_name, message_uid, "+
					"absorbed_message_uids FROM messages INDEXED BY idx_messages_session_id "+
					"WHERE session_id = ? AND id < ? AND role = 'tool' AND "+todoToolsSQL+" AND (active = 1 OR compacted = 1) "+
					"AND length(content) <= ? ORDER BY id DESC LIMIT 100", sid, beforeID, limit)
				if err != nil {
					return err
				}
				if len(rows) == 0 {
					break
				}
				for _, row := range rows {
					callID := rowValue(row, "tool_call_id")
					if !pyTruthy(callID) {
						continue
					}
					gone, err := todoCopyRemoved(row, removed)
					if err != nil {
						return err
					}
					if gone {
						continue
					}
					id, _ := rowInt(row, "id")
					prior, err := queryObjects(ctx, conn, "SELECT role, tool_calls FROM messages INDEXED BY idx_messages_session_id "+
						"WHERE session_id = ? AND id < ? AND role IN ('assistant', 'user', 'system') "+
						"AND (active = 1 OR compacted = 1) ORDER BY id DESC LIMIT 1", sid, id)
					if err != nil {
						return err
					}
					if len(prior) == 0 || rowValue(prior[0], "role") != "assistant" {
						continue
					}
					valid, err := todoResultValid(rowValue(prior[0], "tool_calls"), callID, rowValue(row, "content"))
					if err != nil {
						return err
					}
					if valid {
						result, found = pyStr(rowValue(row, "content")), true
						return nil
					}
				}
				beforeID, _ = rowInt(rows[len(rows)-1], "id")
			}
		}
		return nil
	})
	return result, found, err
}

// Errors the Python ValueError and TypeError handler swallows while validating a Todo result
var errTodoSkip = errors.New("todo result skipped")

// JSON text of a stored cell decoded, failures reported as a skip
func todoLoads(raw any) (any, error) {
	var text string
	switch x := raw.(type) {
	case string:
		text = x
	case []byte:
		decoded, err := pyJSONDecodeBytes(x)
		if err != nil {
			return nil, errTodoSkip
		}
		text = decoded
	default:
		return nil, errTodoSkip
	}
	v, err := pyLoads(text)
	if err != nil {
		return nil, errTodoSkip
	}
	return v, nil
}

// Whether the prior assistant row pairs the call with a Todo tool and the content holds a todos list and integer revision
func todoResultValid(priorCalls, callID, content any) (bool, error) {
	ok, err := todoResultCheck(priorCalls, callID, content)
	if errors.Is(err, errTodoSkip) {
		return false, nil
	}
	return ok, err
}

// Todo pairing and payload validation, errTodoSkip standing for a swallowed ValueError or TypeError
func todoResultCheck(priorCalls, callID, content any) (bool, error) {
	var calls any = []any{}
	if pyTruthy(priorCalls) {
		v, err := todoLoads(priorCalls)
		if err != nil {
			return false, err
		}
		calls = v
	}
	isTodoCall := func(call any) (bool, error) {
		if !isMapping(call) {
			return false, nil
		}
		if id, _ := mappingGet(call, "id"); !pyrepr.Equal(id, callID) {
			return false, nil
		}
		function, _ := mappingGet(call, "function")
		if !isMapping(function) {
			return false, nil
		}
		name, _ := mappingGet(function, "name")
		if name == "todo_list" || name == "todo" {
			return true, nil
		}
		if name != "tool_call" {
			return false, nil
		}
		arguments, _ := mappingGet(function, "arguments")
		if s, ok := arguments.(string); ok {
			v, err := todoLoads(s)
			if err != nil {
				return false, err
			}
			arguments = v
		}
		if !isMapping(arguments) {
			return false, nil
		}
		inner, _ := mappingGet(arguments, "calls")
		list, ok := inner.([]any)
		if !ok {
			return false, nil
		}
		for _, c := range list {
			if n, _ := mappingGet(c, "name"); isMapping(c) && (n == "todo_list" || n == "todo") {
				return true, nil
			}
		}
		return false, nil
	}
	list, ok := calls.([]any)
	if !ok {
		return false, nil
	}
	paired := false
	for _, call := range list {
		hit, err := isTodoCall(call)
		if err != nil {
			return false, err
		}
		if hit {
			paired = true
			break
		}
	}
	if !paired {
		return false, nil
	}
	data, err := todoLoads(content)
	if err != nil {
		return false, err
	}
	todos, _ := mappingGet(data, "todos")
	if _, isList := todos.([]any); !isMapping(data) || !isList {
		return false, nil
	}
	revision, _ := mappingGet(data, "revision")
	if !pyTruthy(revision) {
		return true, nil
	}
	return true, pyIntConvertible(revision)
}

// Python int of a JSON value, errTodoSkip for ValueError and TypeError, an OverflowError for infinity
func pyIntConvertible(v any) error {
	switch x := v.(type) {
	case bool, int, int64, json.Number:
		return nil
	case float64:
		if math.IsInf(x, 0) {
			return &pyConvError{"OverflowError", "cannot convert float infinity to integer"}
		}
		if math.IsNaN(x) {
			return errTodoSkip
		}
		return nil
	case string:
		if _, ok := pyint.ParseBase(x, 10); !ok {
			return errTodoSkip
		}
		return nil
	}
	return errTodoSkip
}

// Copy identities the removal aware display dedupe gives a Todo result row
func todoCopies(row *pyjson.Object, identity any) ([]string, error) {
	hashed := ""
	if pyTruthy(identity) {
		hashed = "h:" + string(identityBytes(identity))
	} else {
		key, err := displayDedupeKey(objMap(row))
		if err != nil {
			return nil, err
		}
		hashed = "h:" + string(displayIdentityOf(key))
	}
	copies := []string{hashed}
	for _, uid := range append([]any{rowValue(row, "message_uid")}, anyStrings(uidList(rowValue(row, "absorbed_message_uids")))...) {
		if pyTruthy(uid) {
			copies = append(copies, "uid:"+pyStr(uid))
		}
	}
	return copies, nil
}

// Copy identity to the newest removed Todo result row id in the lineage
func removedTodoCopies(ctx context.Context, conn SQLiteConn, sessionIDs []string) (map[string]int64, error) {
	rows, err := queryObjects(ctx, conn, "SELECT id, role, timestamp, tool_call_id, tool_calls, tool_name, message_uid, absorbed_message_uids, "+
		"display_identity, CASE WHEN display_identity IS NULL THEN content END AS content "+
		"FROM messages WHERE session_id IN ("+placeholders(len(sessionIDs))+") AND active = 0 "+
		"AND compacted = 0 AND role = 'tool' AND "+todoToolsSQL, anyStrings(sessionIDs)...)
	if err != nil {
		return nil, err
	}
	removed := map[string]int64{}
	for _, row := range rows {
		copies, err := todoCopies(row, rowValue(row, "display_identity"))
		if err != nil {
			return nil, err
		}
		id, _ := rowInt(row, "id")
		for _, c := range copies {
			removed[c] = max(removed[c], id)
		}
	}
	return removed, nil
}

// Whether a newer copy of the row was removed so the resume must not restore it
func todoCopyRemoved(row *pyjson.Object, removed map[string]int64) (bool, error) {
	if len(removed) == 0 {
		return false, nil
	}
	copies, err := todoCopies(row, nil)
	if err != nil {
		return false, err
	}
	id, _ := rowInt(row, "id")
	for _, c := range copies {
		if removed[c] > id {
			return true, nil
		}
	}
	return false, nil
}
