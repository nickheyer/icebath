package state

import (
	"encoding/json"
	"math/big"
	"reflect"
)

// Message keys that carry durable row coverage through an in place archive
const (
	AbsorbedRowIDs      = "_absorbed_row_ids"
	MergedDurableRows   = "_merged_durable_rows"
	UnnamedDurableRows  = "_unnamed_durable_rows"
	RetiredDurableRows  = "_retired_durable_rows"
	RetiredRow          = "_retired_row"
	OwnRow              = "_own_row"
	DBPersistedMarker   = "_db_persisted"
	archiveRowIDMessage = "_row_id"
)

// Session store lookup that reports a row role, false when the row is gone
type MessageRoleReader interface {
	GetMessageRole(sessionID string, messageID int64) (string, bool)
}

// Fields the commit matches a retired row without an id by
func RetiredRowPayload(message map[string]any) map[string]any {
	payload := map[string]any{"role": message["role"], "content": message["content"]}
	if pyTruthy(message["tool_call_id"]) {
		payload["tool_call_id"] = message["tool_call_id"]
	}
	var calls []any
	if items, ok := listItems(message["tool_calls"]); ok && pyTruthy(message["tool_calls"]) {
		for _, call := range items {
			if m, ok := call.(map[string]any); ok {
				calls = append(calls, m["id"])
			} else if o, ok := call.(pyObject); ok {
				v, _ := o.get("id")
				calls = append(calls, v)
			}
		}
	}
	if len(calls) > 0 {
		payload["tool_call_ids"] = calls
	}
	return payload
}

// Python int above zero as int64, false for bools, floats and everything else
func positiveID(value any) (int64, bool) {
	var n int64
	switch v := value.(type) {
	case bool, nil:
		return 0, false
	case json.Number:
		i, err := v.Int64()
		if err != nil {
			return 0, false
		}
		n = i
	case *big.Int:
		if !v.IsInt64() {
			return 0, false
		}
		n = v.Int64()
	default:
		rv := reflect.ValueOf(value)
		switch rv.Kind() {
		case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
			n = rv.Int()
		case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
			n = int64(rv.Uint())
		default:
			return 0, false
		}
	}
	if n > 0 {
		return n, true
	}
	return 0, false
}

// Covered row ids in first seen order and the dicts without an id from the held history
func HeldArchiveCoverage(messages, verbatimTail []map[string]any) ([]int64, []map[string]any) {
	var covered []int64
	var unresolved []map[string]any
	seen := map[int64]bool{}
	add := func(id int64) {
		if !seen[id] {
			seen[id] = true
			covered = append(covered, id)
		}
	}
	for _, batch := range [][]map[string]any{messages, verbatimTail} {
		for _, message := range batch {
			if message == nil {
				continue
			}
			if id, ok := positiveID(message[archiveRowIDMessage]); ok {
				add(id)
			} else {
				unresolved = append(unresolved, message)
			}
			if items, ok := listItems(message[AbsorbedRowIDs]); ok {
				for _, absorbed := range items {
					if id, ok := positiveID(absorbed); ok {
						add(id)
					}
				}
			}
		}
	}
	if covered == nil {
		covered = []int64{}
	}
	return covered, unresolved
}

// Newest held row id that still names the row the compressor saw, false when none does
func NewestExactHeldID(messages, verbatimTail []map[string]any) (int64, bool) {
	var newest int64
	found := false
	consider := func(message map[string]any) {
		if id, ok := positiveID(message[archiveRowIDMessage]); ok && (!found || id > newest) {
			newest, found = id, true
		}
	}
	for _, message := range messages {
		if message != nil && pyTruthy(message[DBPersistedMarker]) {
			consider(message)
		}
	}
	for _, message := range verbatimTail {
		if message != nil {
			consider(message)
		}
	}
	return newest, found
}

// Coverage for archive and compact, ok false keeps the watermark
func CoverageForCommit(sessionDB any, sessionID string, messages, verbatimTail []map[string]any) (covered []int64, unresolved []map[string]any, ok bool, err error) {
	newest, has := NewestExactHeldID(messages, verbatimTail)
	if reader, isReader := sessionDB.(MessageRoleReader); has && isReader {
		if _, live := reader.GetMessageRole(sessionID, newest); !live {
			return nil, nil, false, nil
		}
	}
	covered, unresolved = HeldArchiveCoverage(messages, verbatimTail)
	var retired []map[string]any
	for _, message := range unresolved {
		var named []map[string]any
		if items, isList := listItems(message[RetiredDurableRows]); isList {
			for _, row := range items {
				if m, isMap := row.(map[string]any); isMap {
					named = append(named, m)
				}
			}
		}
		unnamed := message[UnnamedDurableRows]
		if !pyTruthy(unnamed) {
			unnamed = 0
		}
		count, convErr := pyIntOf(unnamed)
		if convErr != nil {
			return nil, nil, false, convErr
		}
		own := 0
		for _, row := range named {
			if !pyTruthy(row[OwnRow]) {
				own++
			}
		}
		if count.Cmp(big.NewInt(int64(own))) > 0 {
			return nil, nil, false, nil
		}
		for _, row := range named {
			clone := make(map[string]any, len(row)+2)
			for k, v := range row {
				clone[k] = v
			}
			clone[DBPersistedMarker] = true
			clone[RetiredRow] = true
			retired = append(retired, clone)
		}
	}
	unresolved = append(append([]map[string]any{}, unresolved...), retired...)
	marked := false
	for _, message := range unresolved {
		if pyTruthy(message[DBPersistedMarker]) {
			marked = true
		}
	}
	if len(covered) == 0 && !marked {
		return nil, nil, false, nil
	}
	return covered, unresolved, true, nil
}
