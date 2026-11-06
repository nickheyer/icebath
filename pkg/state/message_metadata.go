package state

import (
	"github.com/nickheyer/icebath/pkg/internal/pyjson"

	"reflect"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
)

// Transcript repair bookkeeping and durable record keys that never reach a provider
const (
	DBRowSnapshot           = "_db_row_snapshot"
	CanonicalRow            = "_canonical_row"
	RestoredFromStore       = "_restored_from_store"
	MergedTurnPrefix        = "_merged_turn_prefix"
	DataUse                 = "data_use"
	WithheldPrompt          = "withheld_prompt"
	PreLLMBlockWithheldText = "[Message withheld: a pre_llm_call policy hook blocked it.]"
	InflightTaskRestatement = "compaction_inflight_task_restatement"
)

// Repair bookkeeping keys riding on batch rows and live dicts
var RepairBookkeepingFields = map[string]bool{DBRowSnapshot: true, CanonicalRow: true, RestoredFromStore: true}

// Keys the request builder strips from every outgoing copy and the token estimator ignores
var PersistenceOnlyMessageFields = map[string]bool{
	"timestamp": true, "display_kind": true, "display_metadata": true, "_row_id": true, "_submit_row_session_id": true,
	MergedTurnPrefix: true, MessageUID: true, AbsorbedMessageUIDs: true, ToolCallUIDs: true, ToolCallUID: true,
	MergedDurableRows: true, UnnamedDurableRows: true, RetiredDurableRows: true,
	DBRowSnapshot: true, CanonicalRow: true, RestoredFromStore: true,
}

// Replay carriers the preflight estimate prices at zero before profile declared carrier keys join
var UnpricedCoreFields = []string{"_anthropic_content_blocks", "reasoning_details", "extra_content", "bedrock_content_blocks", DataUse}

// Dict view of a mapping value, false for anything that is not a Python dict
func mappingGet(value any, key string) (any, bool) {
	switch m := value.(type) {
	case map[string]any:
		v, ok := m[key]
		return v, ok
	case pyObject:
		return m.get(key)
	case *pyjson.Object:
		if m == nil {
			return nil, false
		}
		return m.Get(key)
	}
	return nil, false
}

func isMapping(value any) bool {
	switch value.(type) {
	case map[string]any, pyObject, *pyjson.Object:
		return true
	}
	return false
}

// Iteration of a Python value as for x in value, strings yield characters and dicts their keys
func pyIter(value any) []any {
	switch v := value.(type) {
	case string:
		out := make([]any, 0, utf8.RuneCountInString(v))
		for _, r := range v {
			out = append(out, string(r))
		}
		return out
	case map[string]any:
		obj, _ := asPyObject(v)
		return pyIter(obj)
	case pyObject:
		out := make([]any, len(v))
		for i, kv := range v {
			out[i] = kv.Key
		}
		return out
	}
	items, _ := listItems(value)
	return items
}

// Union of data_use over the dict messages, what the receiving model must guarantee
func DataUseRequirement(messages any) map[string]bool {
	out := map[string]bool{}
	if !pyTruthy(messages) {
		return out
	}
	for _, m := range pyIter(messages) {
		v, _ := mappingGet(m, DataUse)
		if !isMapping(m) || !pyTruthy(v) {
			continue
		}
		for _, r := range pyIter(v) {
			out[pyrepr.Str(r)] = true
		}
	}
	return out
}

// Copy of a display_metadata value when it is a dict, else a fresh empty dict
func metadataCopy(metadata any) map[string]any {
	out := map[string]any{}
	switch m := metadata.(type) {
	case map[string]any:
		for k, v := range m {
			out[k] = v
		}
	case pyObject:
		for _, kv := range m {
			out[kv.Key] = kv.Value
		}
	}
	return out
}

// Marks a tool message with its requirement in the message and its durable display metadata
func StampDataUse(message map[string]any, requires []string) {
	if len(requires) == 0 {
		return
	}
	message[DataUse] = stringsToAny(requires)
	metadata := metadataCopy(message["display_metadata"])
	metadata[DataUse] = stringsToAny(requires)
	message["display_metadata"] = metadata
}

// Typed text a transcript view shows for a vetoed user row, false when the row has none
func WithheldPromptText(message any) (string, bool) {
	metadata, _ := mappingGet(message, "display_metadata")
	text, _ := mappingGet(metadata, WithheldPrompt)
	s, ok := text.(string)
	return s, ok
}

// Content with the withheld placeholder swapped back for what the user typed
func TypedPromptContent(content, message any) any {
	typed, ok := WithheldPromptText(message)
	if !ok {
		return content
	}
	if s, isStr := content.(string); isStr {
		return strings.ReplaceAll(s, PreLLMBlockWithheldText, typed)
	}
	parts, isList := listItems(content)
	if !isList {
		return content
	}
	out := make([]any, len(parts))
	for i, part := range parts {
		out[i] = part
		if obj, isObj := part.(pyObject); isObj {
			if text, isText := mappingText(obj); isText {
				out[i] = setPyKey(append(pyObject{}, obj...), "text", strings.ReplaceAll(text, PreLLMBlockWithheldText, typed))
			}
			continue
		}
		m, isMap := part.(map[string]any)
		if !isMap {
			continue
		}
		text, isText := m["text"].(string)
		if !isText {
			continue
		}
		clone := make(map[string]any, len(m))
		for k, v := range m {
			clone[k] = v
		}
		clone["text"] = strings.ReplaceAll(text, PreLLMBlockWithheldText, typed)
		out[i] = clone
	}
	return out
}

// Messages with the vetoed text dropped from each row display metadata, copying only rows that hold it
func WithoutWithheldPrompts(messages any) any {
	rows, isList := messages.([]map[string]any)
	if !isList {
		items, ok := messages.([]any)
		if !ok {
			return messages
		}
		found := false
		for _, m := range items {
			if _, has := WithheldPromptText(m); has {
				found = true
			}
		}
		if !found {
			return messages
		}
		out := make([]any, len(items))
		for i, m := range items {
			out[i] = m
			if _, has := WithheldPromptText(m); has {
				out[i] = withoutWithheldAny(m)
			}
		}
		return out
	}
	found := false
	for _, m := range rows {
		if _, has := WithheldPromptText(m); has {
			found = true
		}
	}
	if !found {
		return messages
	}
	out := make([]map[string]any, len(rows))
	for i, m := range rows {
		out[i] = m
		if _, has := WithheldPromptText(m); has {
			out[i] = withoutWithheld(m)
		}
	}
	return out
}

func withoutWithheld(m map[string]any) map[string]any {
	clone := make(map[string]any, len(m))
	for k, v := range m {
		clone[k] = v
	}
	metadata := metadataCopy(m["display_metadata"])
	delete(metadata, WithheldPrompt)
	clone["display_metadata"] = metadata
	return clone
}

// Restores a replayed row data_use from its durable display metadata copy
func LiftDataUse(message map[string]any) {
	metadata := message["display_metadata"]
	if !isMapping(metadata) {
		return
	}
	if v, _ := mappingGet(metadata, DataUse); pyTruthy(v) {
		message[DataUse] = append([]any{}, pyIter(v)...)
	}
}

// The message itself without persistence only fields, a shallow copy only when one is present
func WithoutPersistenceFields(msg map[string]any) map[string]any {
	present := false
	for k := range msg {
		if PersistenceOnlyMessageFields[k] {
			present = true
			break
		}
	}
	if !present {
		return msg
	}
	out := make(map[string]any, len(msg))
	for k, v := range msg {
		if !PersistenceOnlyMessageFields[k] {
			out[k] = v
		}
	}
	return out
}

// Fresh random durable id for a message or tool call occurrence
func MintUID() string { return uuid4Hex() }

// Message uid when it is a non empty string, false otherwise
func MessageUIDOrNone(msg map[string]any) (string, bool) {
	uid, ok := msg[MessageUID].(string)
	return uid, ok && uid != ""
}

// Message uid, minting and stamping one on the dict when it carries none
func StampMessageUID(msg map[string]any) string {
	uid, ok := MessageUIDOrNone(msg)
	if !ok {
		uid = MintUID()
		msg[MessageUID] = uid
	}
	return uid
}

// Flags the compressor in flight task restatement in display metadata
func MarkInflightRestatement(msg map[string]any) {
	metadata := metadataCopy(msg["display_metadata"])
	metadata[InflightTaskRestatement] = true
	msg["display_metadata"] = metadata
}

// Compaction provenance fields replay keeps, the summary flag and the restatement flag
func ScaffoldingReplayFields(msg map[string]any) map[string]any {
	fields := map[string]any{}
	if pyTruthy(msg["_compressed_summary"]) {
		fields["_compressed_summary"] = true
	}
	metadata := msg["display_metadata"]
	if v, _ := mappingGet(metadata, InflightTaskRestatement); isMapping(metadata) && pyTruthy(v) {
		fields["display_metadata"] = map[string]any{InflightTaskRestatement: true}
	}
	return fields
}

// Reports rows compaction sent unstamped, the summary and the restatement
func IsCompactionScaffolding(msg map[string]any) bool {
	return len(ScaffoldingReplayFields(msg)) > 0
}

// Copies the non empty uid, merge witness and tool call uid fields from src onto dst
func CopyIdentityFields(src, dst map[string]any) {
	if s, ok := src[MessageUID].(string); ok && s != "" {
		dst[MessageUID] = s
	}
	if items, ok := src[AbsorbedMessageUIDs].([]any); ok && len(items) > 0 {
		dst[AbsorbedMessageUIDs] = append([]any{}, items...)
	} else if items, ok := src[AbsorbedMessageUIDs].([]string); ok && len(items) > 0 {
		dst[AbsorbedMessageUIDs] = stringsToAny(items)
	}
	switch m := src[ToolCallUIDs].(type) {
	case pyObject:
		if len(m) > 0 {
			dst[ToolCallUIDs] = append(pyObject{}, m...)
		}
	case map[string]any:
		if len(m) > 0 {
			obj, _ := asPyObject(m)
			dst[ToolCallUIDs] = obj
		}
	}
	if s, ok := src[ToolCallUID].(string); ok && s != "" {
		dst[ToolCallUID] = s
	}
}

// Identity fields a row copied from msg must carry, minting the msg uid first
func MessageIdentity(msg map[string]any, withToolUIDs bool) map[string]any {
	StampMessageUID(msg)
	identity := map[string]any{}
	CopyIdentityFields(msg, identity)
	if !withToolUIDs {
		delete(identity, ToolCallUIDs)
		delete(identity, ToolCallUID)
	}
	return identity
}

// Records the uid of a dict folded into survivor, the leading constituent keeping its uid
func RecordAbsorbedMessage(survivor, dropped map[string]any, droppedLeads bool) {
	survivorUID, hasSurvivor := MessageUIDOrNone(survivor)
	droppedUID, hasDropped := MessageUIDOrNone(dropped)
	own := UIDList(survivor[AbsorbedMessageUIDs])
	theirs := UIDList(dropped[AbsorbedMessageUIDs])
	var ordered []string
	if droppedLeads && hasDropped {
		survivor[MessageUID] = droppedUID
		ordered = append(ordered, theirs...)
		if hasSurvivor {
			ordered = append(ordered, survivorUID)
		}
		ordered = append(ordered, own...)
	} else {
		ordered = append(ordered, own...)
		if hasDropped {
			ordered = append(ordered, droppedUID)
		}
		ordered = append(ordered, theirs...)
	}
	current := survivor[MessageUID]
	seen := map[string]bool{}
	var absorbed []any
	for _, uid := range ordered {
		if seen[uid] {
			continue
		}
		seen[uid] = true
		if cur, ok := current.(string); ok && cur == uid {
			continue
		}
		absorbed = append(absorbed, uid)
	}
	if len(absorbed) > 0 {
		survivor[AbsorbedMessageUIDs] = absorbed
	}
}

// Union of two tool call uid maps keeping one uid per occurrence for a repeated provider id
func MergeToolCallUIDs(into, extra any) PyObject {
	base, _ := asPyObject(into)
	merged := append(pyObject{}, base...)
	add, _ := asPyObject(extra)
	for _, kv := range add {
		if prev, ok := merged.get(kv.Key); ok {
			merged = setPyKey(merged, kv.Key, append(uidOccurrences(prev), uidOccurrences(kv.Value)...))
		} else {
			merged = setPyKey(merged, kv.Key, kv.Value)
		}
	}
	return merged
}

func uidOccurrences(value any) []any {
	if items, ok := value.([]any); ok {
		return append([]any{}, items...)
	}
	if items, ok := value.([]string); ok {
		return stringsToAny(items)
	}
	return []any{value}
}

// Uid map with every provider id named more than once spelled out as one uid per occurrence
func PerOccurrenceToolCallUIDs(uids any, toolCalls []any) PyObject {
	var order []string
	counts := map[string]int{}
	for _, tc := range toolCalls {
		if id := CoalesceToolCallID(tc); id != "" {
			if counts[id] == 0 {
				order = append(order, id)
			}
			counts[id]++
		}
	}
	base, _ := asPyObject(uids)
	expanded := append(pyObject{}, base...)
	for _, id := range order {
		uid, _ := base.get(id)
		if s, ok := uid.(string); ok && counts[id] > 1 {
			repeated := make([]any, counts[id])
			for i := range repeated {
				repeated[i] = s
			}
			expanded = setPyKey(expanded, id, repeated)
		}
	}
	return expanded
}

// Registers the assistant tool call uids under every pairing id variant and returns the variants it names
func IndexToolCallUIDs(index map[string]string, assistant map[string]any) map[string]bool {
	type call struct {
		tc       any
		variants map[string]bool
	}
	var calls []call
	if pyTruthy(assistant["tool_calls"]) {
		for _, tc := range pyIter(assistant["tool_calls"]) {
			calls = append(calls, call{tc, ToolCallIDVariants(tc)})
		}
	}
	named := map[string]bool{}
	for _, c := range calls {
		for v := range c.variants {
			named[v] = true
		}
	}
	for v := range named {
		delete(index, v)
	}
	uids, ok := asPyObject(assistant[ToolCallUIDs])
	if !ok || len(uids) == 0 || !isMapping(assistant[ToolCallUIDs]) {
		return named
	}
	occurrence := map[string]int{}
	for _, c := range calls {
		callID := CoalesceToolCallID(c.tc)
		uid, _ := uids.get(callID)
		if list, isList := uid.([]any); isList {
			nth := -1
			if n, seen := occurrence[callID]; seen {
				nth = n
			}
			nth++
			occurrence[callID] = nth
			uid = nil
			if nth < len(list) {
				uid = list[nth]
			}
		}
		if s, isStr := uid.(string); isStr && s != "" {
			for v := range c.variants {
				index[v] = s
			}
		}
	}
	return named
}

// Uid an indexed assistant minted for any variant of the tool call id, false when none
func ResolveToolCallUID(index map[string]string, toolCallID any) (string, bool) {
	id, ok := toolCallID.(string)
	if len(index) == 0 || !ok || id == "" {
		return "", false
	}
	for v := range ToolResultIDVariants(id) {
		if uid := index[v]; uid != "" {
			return uid, true
		}
	}
	return "", false
}

// Memo of one assistant dict named variants and uid index across a flush
type ToolCallOwner struct {
	Named map[string]bool
	Index map[string]string
}

// Uid of a tool result resolved from the nearest preceding assistant that named its id
func ToolCallUIDFromHistory(messages []map[string]any, toolIndex int, owners map[uintptr]*ToolCallOwner) (string, bool) {
	id, ok := messages[toolIndex]["tool_call_id"].(string)
	if !ok || id == "" {
		return "", false
	}
	resultVariants := ToolResultIDVariants(id)
	for i := toolIndex - 1; i >= 0; i-- {
		prior := messages[i]
		if prior == nil {
			continue
		}
		role := prior["role"]
		if role == "user" {
			return "", false
		}
		if role != "assistant" {
			continue
		}
		key := reflect.ValueOf(prior).Pointer()
		entry := owners[key]
		if entry == nil {
			index := map[string]string{}
			entry = &ToolCallOwner{Named: IndexToolCallUIDs(index, prior), Index: index}
			owners[key] = entry
		}
		disjoint := true
		for v := range resultVariants {
			if entry.Named[v] {
				disjoint = false
				break
			}
		}
		if disjoint {
			continue
		}
		return ResolveToolCallUID(entry.Index, id)
	}
	return "", false
}

// Wall clock in float seconds, swapped by tests
var metadataWallTime = func() float64 { return float64(time.Now().UnixNano()) / 1e9 }

// Attaches a creation timestamp unless the message already carries one, then returns the message
func StampMessageTimestamp(message map[string]any, timestamp *float64) map[string]any {
	if message["timestamp"] == nil {
		if timestamp != nil {
			message["timestamp"] = *timestamp
		} else {
			message["timestamp"] = metadataWallTime()
		}
	}
	return message
}

// Stamps and appends one live transcript message
func AppendMessage(messages *[]map[string]any, message map[string]any, timestamp *float64) map[string]any {
	*messages = append(*messages, StampMessageTimestamp(message, timestamp))
	return message
}

// Copy of a dict row of either shape without the withheld prompt in its display metadata
func withoutWithheldAny(m any) any {
	if obj, ok := m.(pyObject); ok {
		clone := append(pyObject{}, obj...)
		metadata, _ := obj.get("display_metadata")
		var kept pyObject
		if mo, isObj := metadata.(pyObject); isObj {
			for _, kv := range mo {
				if kv.Key != WithheldPrompt {
					kept = append(kept, kv)
				}
			}
			return setPyKey(clone, "display_metadata", kept)
		}
		return setPyKey(clone, "display_metadata", withoutWithheld(map[string]any{"display_metadata": metadata})["display_metadata"])
	}
	return withoutWithheld(m.(map[string]any))
}

func mappingText(obj pyObject) (string, bool) {
	v, _ := obj.get("text")
	s, ok := v.(string)
	return s, ok
}
