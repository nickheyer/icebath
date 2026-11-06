package state

import (
	"fmt"
	"math"
	"strings"

	"github.com/nickheyer/icebath/pkg/internal/pystr"
)

// Python truth value that also treats typed Go slices by length
func seqTruthy(v any) bool {
	if items, ok := listItems(v); ok {
		return len(items) > 0
	}
	return pyTruthy(v)
}

// Integer value of a non bool Python int, false for every other type
func plainRowInt(v any) (int64, bool) {
	switch x := v.(type) {
	case int64:
		return x, true
	case int:
		return int64(x), true
	}
	return 0, false
}

// Python int() of a counter value that defaults to fallback when falsy
func seqCounter(v any, fallback int64) (int64, error) {
	if !seqTruthy(v) {
		return fallback, nil
	}
	n, err := pyIntOf(v)
	if err != nil {
		return 0, err
	}
	if !n.IsInt64() {
		return math.MaxInt64, nil
	}
	return n.Int64(), nil
}

// Elements of an iterable value, a TypeError for a value Python cannot iterate
func seqIter(v any) ([]any, error) {
	if items, ok := listItems(v); ok {
		return items, nil
	}
	switch v.(type) {
	case string, map[string]any, pyObject:
		return pyIter(v), nil
	}
	return nil, fmt.Errorf("TypeError: '%s' object is not iterable", seqTypeName(v))
}

// Python type name of a value for exception text
func seqTypeName(v any) string {
	switch v.(type) {
	case nil:
		return "NoneType"
	case bool:
		return "bool"
	case int, int64:
		return "int"
	case float64:
		return "float"
	case string:
		return "str"
	case map[string]any, pyObject:
		return "dict"
	}
	if _, ok := listItems(v); ok {
		return "list"
	}
	return fmt.Sprintf("%T", v)
}

// Codex Responses interim turn that carries its own continuation state and replays verbatim
func isCodexInterim(m map[string]any) bool {
	return seqTruthy(m["codex_reasoning_items"]) || seqTruthy(m["codex_message_items"]) || m["finish_reason"] == "incomplete"
}

// Whether content holds text, typed blocks or any value the API treats as non empty
func ContentHasPayload(content any) bool {
	if s, ok := content.(string); ok {
		return pystr.Strip(s) != ""
	}
	items, ok := listItems(content)
	if !ok {
		return content != nil
	}
	for _, block := range items {
		if isMapping(block) {
			typ, _ := mappingGet(block, "type")
			if typ != "text" {
				return true
			}
			text, _ := mappingGet(block, "text")
			if s, ok := text.(string); ok && pystr.Strip(s) != "" {
				return true
			}
			continue
		}
		if seqTruthy(block) {
			return true
		}
	}
	return false
}

// Whether a message carries text, blocks, tool calls, reasoning or codex replay items
func MsgHasPayload(msg map[string]any) bool {
	if ContentHasPayload(msg["content"]) || seqTruthy(msg["tool_calls"]) {
		return true
	}
	if s, ok := msg["reasoning_content"].(string); ok && pystr.Strip(s) != "" {
		return true
	}
	return seqTruthy(msg["reasoning"]) || seqTruthy(msg["reasoning_details"]) ||
		seqTruthy(msg["codex_message_items"]) || seqTruthy(msg["codex_reasoning_items"])
}

// Folds a consecutive assistant turn into prev and reports whether its text survives
func mergeAssistantInto(prev, msg map[string]any) (bool, error) {
	var prevCalls, newCalls []any
	var err error
	if seqTruthy(prev["tool_calls"]) {
		if prevCalls, err = seqIter(prev["tool_calls"]); err != nil {
			return false, err
		}
	}
	if seqTruthy(msg["tool_calls"]) {
		if newCalls, err = seqIter(msg["tool_calls"]); err != nil {
			return false, err
		}
	}
	callsChanged := false
	switch {
	case len(newCalls) > 0:
		prev["tool_calls"] = append(append([]any{}, prevCalls...), newCalls...)
		if extra := msg[ToolCallUIDs]; isMapping(extra) && seqTruthy(extra) {
			own := prev[ToolCallUIDs]
			if !isMapping(own) {
				own = map[string]any{}
			}
			prev[ToolCallUIDs] = MergeToolCallUIDs(PerOccurrenceToolCallUIDs(own, prevCalls), PerOccurrenceToolCallUIDs(extra, newCalls))
		}
		callsChanged = true
	case len(prevCalls) > 0:
		prev["tool_calls"] = append([]any{}, prevCalls...)
	default:
		_, callsChanged = prev["tool_calls"]
		delete(prev, "tool_calls")
	}
	prevContent, newContent := prev["content"], msg["content"]
	contentRewritten := false
	textKept := !seqTruthy(newContent)
	ps, prevIsStr := prevContent.(string)
	ns, newIsStr := newContent.(string)
	if prevIsStr && newIsStr {
		textKept = true
		var parts []string
		for _, p := range []string{pystr.Strip(ps), pystr.Strip(ns)} {
			if p != "" {
				parts = append(parts, p)
			}
		}
		joined := strings.Join(parts, "\n")
		prev["content"] = joined
		contentRewritten = joined != ps
	} else if !seqTruthy(prevContent) && newContent != nil {
		prev["content"] = newContent
		contentRewritten = !pyValueEqual(newContent, prevContent)
		textKept = true
	}
	reasoningCarried := false
	if !seqTruthy(prev["reasoning_content"]) && seqTruthy(msg["reasoning_content"]) {
		prev["reasoning_content"] = msg["reasoning_content"]
		reasoningCarried = true
	}
	if contentRewritten {
		DropStaleAPIContent(prev)
	}
	if contentRewritten || callsChanged || reasoningCarried {
		delete(prev, DBPersistedMarker)
	}
	return textKept, nil
}

// Retires the dropped dict's row ids onto survivor, recording its uid only when its text was folded
func rememberAbsorbedRow(survivor, dropped map[string]any, folded bool) error {
	var ids []any
	if rowID, ok := plainRowInt(dropped["_row_id"]); ok && rowID > 0 && !pyValueEqual(rowID, survivor["_row_id"]) {
		ids = append(ids, rowID)
	}
	if seqTruthy(dropped["_absorbed_row_ids"]) {
		older, err := seqIter(dropped["_absorbed_row_ids"])
		if err != nil {
			return err
		}
		for _, v := range older {
			if id, ok := plainRowInt(v); ok && id > 0 && !seqContains(ids, id) {
				ids = append(ids, id)
			}
		}
	}
	if len(ids) > 0 {
		absorbed, _ := listItems(survivor["_absorbed_row_ids"])
		for _, id := range ids {
			if !seqContains(absorbed, id) {
				absorbed = append(absorbed, id)
			}
		}
		survivor["_absorbed_row_ids"] = absorbed
	}
	if seqTruthy(dropped[UnnamedDurableRows]) {
		have, err := seqCounter(survivor[UnnamedDurableRows], 0)
		if err != nil {
			return err
		}
		add, err := seqCounter(dropped[UnnamedDurableRows], 0)
		if err != nil {
			return err
		}
		survivor[UnnamedDurableRows] = have + add
	}
	if seqTruthy(dropped[RetiredDurableRows]) {
		rows, err := seqIter(dropped[RetiredDurableRows])
		if err != nil {
			return err
		}
		retired, _ := listItems(survivor[RetiredDurableRows])
		for _, row := range rows {
			if !isMapping(row) {
				return fmt.Errorf("AttributeError: '%s' object has no attribute 'items'", seqTypeName(row))
			}
			copied := map[string]any{}
			for _, kv := range seqItems(row) {
				if kv.Key != OwnRow {
					copied[kv.Key] = kv.Value
				}
			}
			retired = append(retired, copied)
		}
		survivor[RetiredDurableRows] = retired
	}
	if folded {
		RecordAbsorbedMessage(survivor, dropped, false)
	}
	return nil
}

// Key value pairs of a mapping row, empty for a non mapping
func seqItems(row any) pyObject {
	obj, _ := asPyObject(row)
	return obj
}

// Python list membership by value equality
func seqContains(items []any, v any) bool {
	for _, item := range items {
		if pyValueEqual(item, v) {
			return true
		}
	}
	return false
}

// Whether any recorded retired row is flagged as the dict's own row, non mappings skipped or refused
func seqHasOwnRow(recorded any, requireMapping bool) (bool, error) {
	items, _ := listItems(recorded)
	for _, row := range items {
		if !isMapping(row) {
			if requireMapping {
				continue
			}
			return false, fmt.Errorf("AttributeError: '%s' object has no attribute 'get'", seqTypeName(row))
		}
		if v, _ := mappingGet(row, OwnRow); seqTruthy(v) {
			return true, nil
		}
	}
	return false, nil
}

// Counts the retired dict's durable row on survivor when a reload without row ids names neither
func countUnnamedRow(survivor, retired map[string]any) error {
	loaded := func(m map[string]any) bool {
		return seqTruthy(m[DBPersistedMarker]) || seqTruthy(m[UnnamedDurableRows])
	}
	if _, isInt := pyIntID(retired["_row_id"]); !loaded(survivor) || !loaded(retired) || isInt {
		return nil
	}
	have, err := seqCounter(survivor[UnnamedDurableRows], 0)
	if err != nil {
		return err
	}
	survivor[UnnamedDurableRows] = have + 1
	hasOwn, err := seqHasOwnRow(retired[RetiredDurableRows], false)
	if err != nil {
		return err
	}
	if !hasOwn {
		recorded, _ := listItems(survivor[RetiredDurableRows])
		survivor[RetiredDurableRows] = append(recorded, RetiredRowPayload(retired))
	}
	return nil
}

// Records the loaded fields of a survivor without a row id once, before a fold rewrites its text
func rememberOwnRow(survivor map[string]any) {
	_, isInt := pyIntID(survivor["_row_id"])
	if !seqTruthy(survivor[DBPersistedMarker]) || isInt {
		return
	}
	if hasOwn, _ := seqHasOwnRow(survivor[RetiredDurableRows], true); hasOwn {
		return
	}
	payload := RetiredRowPayload(survivor)
	payload[OwnRow] = true
	recorded, _ := listItems(survivor[RetiredDurableRows])
	survivor[RetiredDurableRows] = append(recorded, payload)
}

// Lets the survivor before a dropped durable row stand for it, parking drops with no survivor yet
func retireDroppedRow(kept []map[string]any, dropped map[string]any, leading *[]map[string]any) error {
	if len(kept) == 0 {
		*leading = append(*leading, dropped)
		return nil
	}
	last := kept[len(kept)-1]
	if last == nil {
		return nil
	}
	if err := countUnnamedRow(last, dropped); err != nil {
		return err
	}
	return rememberAbsorbedRow(last, dropped, false)
}

// Lets the first survivor stand for the rows dropped ahead of it
func retireLeadingDrops(kept []map[string]any, leading []map[string]any) error {
	if len(kept) == 0 || kept[0] == nil {
		return nil
	}
	for _, dropped := range leading {
		if err := countUnnamedRow(kept[0], dropped); err != nil {
			return err
		}
		if err := rememberAbsorbedRow(kept[0], dropped, false); err != nil {
			return err
		}
	}
	return nil
}

// Merges consecutive assistant turns, a provisional verification candidate being superseded instead
func mergeConsecutiveAssistants(messages []map[string]any) ([]map[string]any, int, error) {
	repairs := 0
	var collapsed []map[string]any
	for _, msg := range messages {
		var prev map[string]any
		if len(collapsed) > 0 {
			prev = collapsed[len(collapsed)-1]
		}
		if prev != nil && prev["role"] == "assistant" && msg != nil && msg["role"] == "assistant" &&
			!isCodexInterim(msg) && !isCodexInterim(prev) {
			if fr := prev["finish_reason"]; fr == "verification_required" || fr == "verify_hook_continue" {
				if err := countUnnamedRow(msg, prev); err != nil {
					return nil, 0, err
				}
				if err := rememberAbsorbedRow(msg, prev, false); err != nil {
					return nil, 0, err
				}
				collapsed[len(collapsed)-1] = msg
			} else {
				if err := countUnnamedRow(prev, msg); err != nil {
					return nil, 0, err
				}
				rememberOwnRow(prev)
				folded, err := mergeAssistantInto(prev, msg)
				if err != nil {
					return nil, 0, err
				}
				if err := rememberAbsorbedRow(prev, msg, folded); err != nil {
					return nil, 0, err
				}
			}
			repairs++
			continue
		}
		collapsed = append(collapsed, msg)
	}
	return collapsed, repairs, nil
}

// Drops tool results that answer no known assistant call, consuming each call's whole alias group
func dropStrayToolResults(messages []map[string]any) ([]map[string]any, int, error) {
	repairs := 0
	known := map[string]int{}
	matched := map[int]bool{}
	nextGroup := 0
	var filtered, leading []map[string]any
	for _, msg := range messages {
		var role any
		if msg != nil {
			role = msg["role"]
		}
		switch role {
		case "assistant", "user":
			known = map[string]int{}
			matched = map[int]bool{}
			if role == "assistant" && seqTruthy(msg["tool_calls"]) {
				calls, err := seqIter(msg["tool_calls"])
				if err != nil {
					return nil, 0, err
				}
				for _, tc := range calls {
					variants := ToolCallIDVariants(tc)
					if len(variants) == 0 {
						continue
					}
					for id := range variants {
						if _, ok := known[id]; !ok {
							known[id] = nextGroup
						}
					}
					nextGroup++
				}
			}
		case "tool":
			resultVariants := ToolResultIDVariants(msg["tool_call_id"])
			best, found := 0, false
			for id := range resultVariants {
				if g, ok := known[id]; ok && !matched[g] && (!found || g < best) {
					best, found = g, true
				}
			}
			if len(resultVariants) > 0 && !found {
				if err := retireDroppedRow(filtered, msg, &leading); err != nil {
					return nil, 0, err
				}
				repairs++
				continue
			}
			if found {
				matched[best] = true
			}
		}
		filtered = append(filtered, msg)
	}
	if err := retireLeadingDrops(filtered, leading); err != nil {
		return nil, 0, err
	}
	return filtered, repairs, nil
}

// Prunes tool calls the immediately following tool run leaves unanswered, dropping turns left empty
func pruneUnansweredToolCalls(messages []map[string]any) ([]map[string]any, int, error) {
	repairs := 0
	var pruned, leading []map[string]any
	for i, msg := range messages {
		if msg == nil || msg["role"] != "assistant" || !seqTruthy(msg["tool_calls"]) || isCodexInterim(msg) {
			pruned = append(pruned, msg)
			continue
		}
		answered := map[string]bool{}
		for _, follower := range messages[i+1:] {
			if follower == nil || follower["role"] != "tool" {
				break
			}
			raw := follower["tool_call_id"]
			if !seqTruthy(raw) {
				raw = ""
			}
			s, ok := raw.(string)
			if !ok {
				return nil, 0, fmt.Errorf("AttributeError: '%s' object has no attribute 'strip'", seqTypeName(raw))
			}
			if tid := pystr.Strip(s); tid != "" {
				for id := range ToolResultIDVariants(tid) {
					answered[id] = true
				}
			}
		}
		calls, err := seqIter(msg["tool_calls"])
		if err != nil {
			return nil, 0, err
		}
		var keptCalls []any
		for _, tc := range calls {
			for id := range ToolCallIDVariants(tc) {
				if answered[id] {
					keptCalls = append(keptCalls, tc)
					break
				}
			}
		}
		if len(keptCalls) != len(calls) {
			repairs++
			rest := make(map[string]any, len(msg))
			for k, v := range msg {
				if k != "tool_calls" {
					rest[k] = v
				}
			}
			if len(keptCalls) == 0 && !MsgHasPayload(rest) {
				if err := retireDroppedRow(pruned, msg, &leading); err != nil {
					return nil, 0, err
				}
				continue
			}
			if len(keptCalls) > 0 {
				msg["tool_calls"] = keptCalls
			} else {
				delete(msg, "tool_calls")
			}
			delete(msg, DBPersistedMarker)
		}
		pruned = append(pruned, msg)
	}
	if err := retireLeadingDrops(pruned, leading); err != nil {
		return nil, 0, err
	}
	return pruned, repairs, nil
}

// Whether user content is plain text the merge may rewrite, never an undecodable sentinel string
func plainUserText(content any) bool {
	s, ok := content.(string)
	return ok && !strings.HasPrefix(s, contentJSONPrefix)
}

// User content with the missing key read as an empty string
func userContent(m map[string]any) any {
	if v, ok := m["content"]; ok {
		return v
	}
	return ""
}

// Merges consecutive plain text user turns so no user input is lost
func mergeConsecutiveUsers(messages []map[string]any) ([]map[string]any, int, error) {
	repairs := 0
	var merged []map[string]any
	for _, msg := range messages {
		var prev map[string]any
		if len(merged) > 0 {
			prev = merged[len(merged)-1]
		}
		if prev == nil || prev["role"] != "user" || msg == nil || msg["role"] != "user" {
			merged = append(merged, msg)
			continue
		}
		if handoff, _ := SplitUserOriginatedTurn(prev); handoff != nil || prev["display_kind"] == SteerDisplayKind ||
			!plainUserText(userContent(prev)) || !plainUserText(userContent(msg)) {
			merged = append(merged, msg)
			continue
		}
		prevContent, newContent := userContent(prev).(string), userContent(msg).(string)
		mergedContent := prevContent + newContent
		if prevContent != "" && newContent != "" {
			mergedContent = prevContent + "\n\n" + newContent
		}
		_, hadAPISidecar := prev["api_content"]
		if (seqTruthy(prev[DBPersistedMarker]) || seqTruthy(prev[MergedDurableRows])) && seqTruthy(msg[DBPersistedMarker]) {
			n, err := seqCounter(prev[MergedDurableRows], 1)
			if err != nil {
				return nil, 0, err
			}
			prev[MergedDurableRows] = n + 1
		} else {
			delete(prev, MergedDurableRows)
		}
		prev["content"] = mergedContent
		if prevContent != "" {
			prev[MergedTurnPrefix] = prevContent
		}
		DropStaleAPIContent(prev)
		if seqTruthy(prev["display_kind"]) && !seqTruthy(msg["display_kind"]) {
			markerRowID := prev["_row_id"]
			delete(prev, "display_kind")
			if msg["_row_id"] != nil {
				if id, ok := plainRowInt(markerRowID); ok {
					absorbed, _ := listItems(prev["_absorbed_row_ids"])
					if !seqContains(absorbed, id) {
						absorbed = append(absorbed, id)
					}
					prev["_absorbed_row_ids"] = absorbed
				}
				prev["_row_id"] = msg["_row_id"]
			}
			delete(prev, DBPersistedMarker)
		}
		if mergedContent != prevContent || hadAPISidecar {
			delete(prev, DBPersistedMarker)
		}
		if err := rememberAbsorbedRow(prev, msg, true); err != nil {
			return nil, 0, err
		}
		repairs++
	}
	return merged, repairs, nil
}

// Repair passes in order, the assistant merge first so the merged tool call ids are known
var sequenceRepairPasses = []func([]map[string]any) ([]map[string]any, int, error){
	mergeConsecutiveAssistants, dropStrayToolResults, pruneUnansweredToolCalls, mergeConsecutiveUsers,
}

// Decodes sentinel encoded row content in place so an image bearing turn is never merged as text
func normalizeSentinelEncodedContent(messages []map[string]any) {
	for _, msg := range messages {
		if msg == nil {
			continue
		}
		if s, ok := msg["content"].(string); ok && strings.HasPrefix(s, contentJSONPrefix) {
			msg["content"] = decodeMessageContent(s)
		}
	}
}

// Collapses malformed role alternation in place and returns the repair count
func RepairMessageSequence(messages *[]map[string]any) (int, error) {
	if messages == nil || len(*messages) == 0 {
		return 0, nil
	}
	normalizeSentinelEncodedContent(*messages)
	repairs := 0
	current := *messages
	for _, pass := range sequenceRepairPasses {
		next, made, err := pass(current)
		if err != nil {
			return repairs, err
		}
		current = next
		repairs += made
	}
	if repairs > 0 {
		*messages = current
	}
	return repairs, nil
}
