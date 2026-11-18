package state

import (
	"bytes"
	"encoding/json"
	"reflect"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
)

// Extra value recording that a key the struct always renders was absent from the source dict
type absentKey struct{}

// Dict keys of the Message fields that carry no json name
var messageBookkeepingKeys = map[string]string{"RowID": "id", "Active": "active", "Compacted": "compacted", "Persisted": DBPersistedMarker}

// Lossless dict form of a message, struct fields first and every Extra key on top
func MessageToDict(m *Message) map[string]any {
	if m == nil {
		return nil
	}
	return structToDict(reflect.ValueOf(m).Elem(), messageBookkeepingKeys)
}

// Message holding every dict key, keys whose value does not fit a field kept in Extra
func MessageFromDict(d map[string]any) *Message {
	m := &Message{}
	dictToStruct(d, reflect.ValueOf(m).Elem(), messageBookkeepingKeys)
	if keys, ok := m.Extra[WireKeyOrder].([]string); ok {
		m.Keys = append([]string(nil), keys...)
		delete(m.Extra, WireKeyOrder)
		if len(m.Extra) == 0 {
			m.Extra = nil
		}
	} else {
		m.Keys = restoredKeyOrderOf(d)
	}
	return m
}

// Go only key of a wire message copy holding its dict key order where the encoder default differs
const WireKeyOrder = "\x00wire_key_order"

// Dict key order of every restored message a conversion adopted, by durable message uid or row id
var restoredKeyOrders sync.Map

// Restored message dicts of the latest store loads with the key order each was built in
var restoredLoads struct {
	sync.Mutex
	loads []map[uintptr]restoredDict
}

// One restored dict kept alive so its identity stays unique while it is looked up
type restoredDict struct {
	dict map[string]any
	keys []string
}

// Number of store loads whose dicts keep their identity lookup
const restoredLoadsKept = 8

// Registry key of a message dict, its durable uid or else its row id
func restoredKeyOrderID(d map[string]any) string {
	if uid, ok := d[MessageUID].(string); ok && uid != "" {
		return "uid:" + uid
	}
	if id, ok := exactInt(d["_row_id"]); ok {
		return "row:" + strconv.FormatInt(id, 10)
	}
	return ""
}

// Remembers the dicts one store load returned with the key order each was built in, dropping the oldest load
func recordRestoredLoad(msgs []map[string]any, orderOf func(map[string]any) []string) {
	load := make(map[uintptr]restoredDict, len(msgs))
	for _, m := range msgs {
		if keys := orderOf(m); m != nil && keys != nil {
			load[reflect.ValueOf(m).Pointer()] = restoredDict{dict: m, keys: keys}
		}
	}
	restoredLoads.Lock()
	defer restoredLoads.Unlock()
	restoredLoads.loads = append(restoredLoads.loads, load)
	if len(restoredLoads.loads) > restoredLoadsKept {
		restoredLoads.loads = restoredLoads.loads[len(restoredLoads.loads)-restoredLoadsKept:]
	}
}

// Restore key order of a message dict, adopting it by uid when the dict itself came from a recent store load
func restoredKeyOrderOf(d map[string]any) []string {
	ptr := reflect.ValueOf(d).Pointer()
	restoredLoads.Lock()
	var keys []string
	for i := len(restoredLoads.loads) - 1; i >= 0 && keys == nil; i-- {
		if hit, ok := restoredLoads.loads[i][ptr]; ok && reflect.ValueOf(hit.dict).Pointer() == ptr {
			keys = hit.keys
		}
	}
	restoredLoads.Unlock()
	id := restoredKeyOrderID(d)
	if keys != nil {
		if id != "" {
			restoredKeyOrders.Store(id, keys)
		}
		return append([]string(nil), keys...)
	}
	if id == "" {
		return nil
	}
	if adopted, ok := restoredKeyOrders.Load(id); ok {
		return append([]string(nil), adopted.([]string)...)
	}
	return nil
}

// Keys of d in insertion order after the ones already listed, several new keys in sorted order
func appendNewKeys(order []string, d map[string]any) []string {
	listed := make(map[string]bool, len(order))
	for _, k := range order {
		listed[k] = true
	}
	var added []string
	for k := range d {
		if !listed[k] {
			added = append(added, k)
		}
	}
	sort.Strings(added)
	return append(order, added...)
}

// Key order the agent builds a fresh assistant message in before its carriers and tool calls
var assistantBuildKeyOrder = []string{"role", "content", "reasoning", "finish_reason", "timestamp", "reasoning_content", "reasoning_details",
	"anthropic_content_blocks", "bedrock_content_blocks", "codex_reasoning_items", "codex_message_items"}

// Key order the agent builds a fresh tool result message in
var toolBuildKeyOrder = []string{"role", "name", "tool_name", "content", "tool_call_id", "_tool_output_risk", "effect_disposition"}

// Key order of every other fresh message
var plainBuildKeyOrder = []string{"role", "name", "content", "tool_calls", "tool_call_id"}

// Dict keys of a message the struct renders, recorded order first, then the build order of its role, then sorted
func MessageKeyOrder(m *Message, d map[string]any) []string {
	var build []string
	switch m.Role {
	case "assistant":
		build = append(build, assistantBuildKeyOrder...)
		known := map[string]bool{"tool_calls": true}
		for _, k := range assistantBuildKeyOrder {
			known[k] = true
		}
		rt := reflect.TypeOf(Message{})
		for i := range rt.NumField() {
			if key, _ := fieldKey(rt.Field(i), messageBookkeepingKeys); key != "" {
				known[key] = true
			}
		}
		var carriers []string
		for k := range d {
			if !known[k] && !strings.HasPrefix(k, "_") {
				carriers = append(carriers, k)
			}
		}
		sort.Strings(carriers)
		build = append(append(build, carriers...), "tool_calls")
	case "tool":
		build = toolBuildKeyOrder
	default:
		build = plainBuildKeyOrder
	}
	present := make(map[string]any, len(d))
	for k := range d {
		present[k] = nil
	}
	return orderedDict(present, m.Keys, build).Keys
}

// Key order the agent builds a tool call dict in
var toolCallKeyOrder = []string{"id", "call_id", "response_item_id", "type", "function", "extra_content"}

// Key order the agent builds a tool call function dict in
var toolCallFunctionKeyOrder = []string{"name", "arguments", "thought_signature"}

// Lossless dict form of a tool call struct in the key order of its source dict
func ToolCallToDict(tc ToolCall) *pyjson.Object {
	return orderedDict(structToDict(reflect.ValueOf(tc), nil), tc.Keys, toolCallKeyOrder)
}

// Tool call struct from an ordered dict entry, recording its key order when it differs from the agent build order
func ToolCallFromObject(o *pyjson.Object) ToolCall {
	tc := ToolCallFromDict(o.Vals)
	tc.Keys = sourceKeyOrder(o.Keys, toolCallKeyOrder)
	return tc
}

// Ordered dict of m, recorded keys first, then the build order, then any remaining key in sorted order
func orderedDict(m map[string]any, recorded, build []string) *pyjson.Object {
	o := pyjson.NewObject()
	for _, list := range [][]string{recorded, build} {
		for _, k := range list {
			if v, ok := m[k]; ok {
				o.Set(k, v)
			}
		}
	}
	rest := make([]string, 0, len(m)-o.Len())
	for k := range m {
		if _, ok := o.Vals[k]; !ok {
			rest = append(rest, k)
		}
	}
	sort.Strings(rest)
	for _, k := range rest {
		o.Set(k, m[k])
	}
	return o
}

// Source key order worth recording, nil when the build order already reproduces it
func sourceKeyOrder(keys, build []string) []string {
	present := make(map[string]any, len(keys))
	for _, k := range keys {
		present[k] = nil
	}
	if slices.Equal(orderedDict(present, nil, build).Keys, keys) {
		return nil
	}
	return append([]string(nil), keys...)
}

// Tool call struct from a dict entry, unfit keys kept in Extra
func ToolCallFromDict(d map[string]any) ToolCall {
	var tc ToolCall
	dictToStruct(d, reflect.ValueOf(&tc).Elem(), nil)
	return tc
}

// Dict key and omitempty flag of a struct field, empty key for fields with no dict form
func fieldKey(f reflect.StructField, bookkeeping map[string]string) (string, bool) {
	if f.Name == "Extra" || f.Name == "Keys" || !f.IsExported() {
		return "", false
	}
	tag := f.Tag.Get("json")
	if tag == "-" {
		return bookkeeping[f.Name], true
	}
	name, opts, _ := strings.Cut(tag, ",")
	return name, strings.Contains(opts, "omitempty")
}

// Renders struct fields under their dict keys then applies Extra, dropping keys marked absent
func structToDict(rv reflect.Value, bookkeeping map[string]string) map[string]any {
	out := map[string]any{}
	rt := rv.Type()
	for i := range rt.NumField() {
		key, omitEmpty := fieldKey(rt.Field(i), bookkeeping)
		if key == "" {
			continue
		}
		fv := rv.Field(i)
		if omitEmpty && fv.IsZero() {
			continue
		}
		out[key] = fieldToDictValue(fv)
		if raw, isRaw := fv.Interface().(json.RawMessage); isRaw && key == "reasoning_details" && len(raw) > 0 {
			if ordered, err := pyjson.LoadsOrdered(raw); err == nil {
				out[key] = ordered
			}
		}
	}
	if extra, ok := rv.FieldByName("Extra").Interface().(map[string]any); ok {
		for k, v := range extra {
			if _, absent := v.(absentKey); absent {
				delete(out, k)
				continue
			}
			out[k] = v
		}
	}
	return out
}

// Python shaped value of one struct field
func fieldToDictValue(fv reflect.Value) any {
	switch v := fv.Interface().(type) {
	case json.RawMessage:
		if len(v) == 0 {
			return nil
		}
		dec := json.NewDecoder(bytes.NewReader(v))
		dec.UseNumber()
		var out any
		if dec.Decode(&out) != nil {
			return string(v)
		}
		return out
	case []ToolCall:
		out := make([]any, len(v))
		for i, tc := range v {
			out[i] = ToolCallToDict(tc)
		}
		return out
	case ToolCallFunction:
		return orderedDict(structToDict(fv, nil), v.Keys, toolCallFunctionKeyOrder)
	case []string:
		out := make([]any, len(v))
		for i, s := range v {
			out[i] = s
		}
		return out
	case map[string]string:
		out := make(map[string]any, len(v))
		for k, s := range v {
			out[k] = s
		}
		return out
	}
	return fv.Interface()
}

// Fills struct fields from their dict keys, sending unfit values, zero valued omitempty keys and absences to Extra
func dictToStruct(d map[string]any, rv reflect.Value, bookkeeping map[string]string) {
	extra := map[string]any{}
	known := map[string]bool{}
	rt := rv.Type()
	for i := range rt.NumField() {
		key, omitEmpty := fieldKey(rt.Field(i), bookkeeping)
		if key == "" {
			continue
		}
		known[key] = true
		value, present := d[key]
		if !present {
			if !omitEmpty {
				extra[key] = absentKey{}
			}
			continue
		}
		fv := rv.Field(i)
		if !assignDictValue(fv, value) {
			extra[key] = value
			continue
		}
		if omitEmpty && fv.IsZero() {
			extra[key] = value
		}
	}
	for k, v := range d {
		if !known[k] {
			extra[k] = v
		}
	}
	if len(extra) > 0 {
		rv.FieldByName("Extra").Set(reflect.ValueOf(extra))
	}
}

// Stores a dict value into a field when the Python value has the field's shape exactly
func assignDictValue(fv reflect.Value, value any) bool {
	switch fv.Interface().(type) {
	case json.RawMessage:
		if value == nil {
			return true
		}
		raw, err := rawJSONOf(value)
		if err != nil {
			return false
		}
		candidate := reflect.ValueOf(json.RawMessage(raw))
		if !reflect.DeepEqual(plainDictTree(fieldToDictValue(candidate)), plainDictTree(value)) {
			return false
		}
		fv.Set(candidate)
		return true
	case []ToolCall:
		items, ok := value.([]any)
		if !ok {
			return false
		}
		calls := make([]ToolCall, len(items))
		for i, item := range items {
			dict, keys, ok := orderedDictValue(item)
			if !ok {
				return false
			}
			calls[i] = ToolCallFromDict(dict)
			calls[i].Keys = sourceKeyOrder(keys, toolCallKeyOrder)
		}
		fv.Set(reflect.ValueOf(calls))
		return true
	case ToolCallFunction:
		dict, keys, ok := orderedDictValue(value)
		if !ok {
			return false
		}
		var fn ToolCallFunction
		dictToStruct(dict, reflect.ValueOf(&fn).Elem(), nil)
		fn.Keys = sourceKeyOrder(keys, toolCallFunctionKeyOrder)
		fv.Set(reflect.ValueOf(fn))
		return true
	case []string:
		items, ok := value.([]any)
		if !ok {
			return false
		}
		strs := make([]string, len(items))
		for i, item := range items {
			if strs[i], ok = item.(string); !ok {
				return false
			}
		}
		fv.Set(reflect.ValueOf(strs))
		return true
	case map[string]string:
		dict, ok := value.(map[string]any)
		if !ok {
			return false
		}
		out := make(map[string]string, len(dict))
		for k, item := range dict {
			s, ok := item.(string)
			if !ok {
				return false
			}
			out[k] = s
		}
		fv.Set(reflect.ValueOf(out))
		return true
	case int64:
		n, ok := exactInt(value)
		if ok {
			fv.SetInt(n)
		}
		return ok
	case int:
		n, ok := exactInt(value)
		if ok {
			fv.SetInt(n)
		}
		return ok
	case float64:
		f, ok := value.(float64)
		if ok {
			fv.SetFloat(f)
		}
		return ok
	}
	if value == nil {
		return fv.Kind() == reflect.Interface || fv.Kind() == reflect.Map && fv.IsNil()
	}
	val := reflect.ValueOf(value)
	if val.Type() != fv.Type() && fv.Kind() != reflect.Interface {
		return false
	}
	fv.Set(val)
	return true
}

// Integer of a Python int value, bool excluded since it renders as True and False
func exactInt(value any) (int64, bool) {
	switch v := value.(type) {
	case int:
		return int64(v), true
	case int64:
		return v, true
	case json.Number:
		n, err := v.Int64()
		return n, err == nil && !strings.ContainsAny(v.String(), ".eE")
	}
	return 0, false
}

// Plain map of a dict value, an insertion ordered object from the row decoder counting as its dict
func dictValue(value any) (map[string]any, bool) {
	d, _, ok := orderedDictValue(value)
	return d, ok
}

// Plain map of a dict value with the key order of an ordered source, nil keys for a plain map
func orderedDictValue(value any) (map[string]any, []string, bool) {
	switch d := value.(type) {
	case map[string]any:
		return d, nil, true
	case *pyjson.Object:
		if d == nil {
			return nil, nil, false
		}
		out := make(map[string]any, len(d.Keys))
		for _, k := range d.Keys {
			out[k] = d.Vals[k]
		}
		return out, append([]string(nil), d.Keys...), true
	case pyObject:
		out := make(map[string]any, len(d))
		keys := make([]string, 0, len(d))
		for _, kv := range d {
			if _, seen := out[kv.Key]; !seen {
				keys = append(keys, kv.Key)
			}
			out[kv.Key] = kv.Value
		}
		return out, keys, true
	}
	return nil, nil, false
}

// Value with every ordered object at any depth turned into the plain map a json decode yields
func plainDictTree(v any) any {
	switch t := v.(type) {
	case *pyjson.Object:
		if t == nil {
			return v
		}
		out := make(map[string]any, len(t.Keys))
		for _, k := range t.Keys {
			out[k] = plainDictTree(t.Vals[k])
		}
		return out
	case pyObject:
		out := make(map[string]any, len(t))
		for _, kv := range t {
			out[kv.Key] = plainDictTree(kv.Value)
		}
		return out
	case map[string]any:
		out := make(map[string]any, len(t))
		for k, item := range t {
			out[k] = plainDictTree(item)
		}
		return out
	case []any:
		out := make([]any, len(t))
		for i, item := range t {
			out[i] = plainDictTree(item)
		}
		return out
	}
	return v
}

// Value with every decoded ordered object at any depth turned into a pyjson Object of the same key order
func orderedDictTree(v any) any {
	switch t := v.(type) {
	case pyObject:
		o := pyjson.NewObject()
		for _, kv := range t {
			o.Set(kv.Key, orderedDictTree(kv.Value))
		}
		return o
	case []any:
		out := make([]any, len(t))
		for i, item := range t {
			out[i] = orderedDictTree(item)
		}
		return out
	}
	return v
}

// Compact JSON bytes of a value, ordered objects keeping their key order
func rawJSONOf(v any) ([]byte, error) {
	if !reflect.DeepEqual(plainDictTree(v), v) {
		text, err := pyDumps(v, pyJSON{itemSep: ",", keySep: ":"})
		return []byte(text), err
	}
	return json.Marshal(v)
}
