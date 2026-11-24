package tools

import (
	"encoding/json"
	"math/big"
	"slices"
	"strings"

	anyutil "github.com/nickheyer/icebath/pkg/utils/any"
	jsonutil "github.com/nickheyer/icebath/pkg/utils/json"
)

var delegationParentEventFields = map[string]bool{
	"type": true, "delegation_id": true, "session_key": true, "origin_ui_session_id": true, "origin_session_id": true,
	"parent_session_id": true, "goal": true, "goals": true, "context": true, "toolsets": true, "role": true, "model": true,
	"group": true, "scope_id": true, "user_id": true, "user_name": true, "dispatched_at": true, "completed_at": true,
}

var delegationResultMetadata = map[string]bool{"task_index": true, "model": true, "group": true, "_child_role": true, "status": true, "exit_reason": true}

// Parent facing text carrying the forced projection persistent sinks read, serialized as its text
type ParentCopy struct {
	Text   string
	Forced string
}

func (p *ParentCopy) PyStr() string { return p.Text }

// JSON string of the parent text, the way json.dumps writes a str subclass
func (p *ParentCopy) MarshalJSON() ([]byte, error) { return jsonutil.Append(nil, p.Text) }

// Text for the parent remembering the forced projection, the plain string when both match
func NewParentCopy(text, forced string) any {
	if text == forced {
		return text
	}
	return &ParentCopy{Text: text, Forced: forced}
}

// Carrier itself for the opt out parent copy so persistent sinks still find its forced text, the forced text otherwise
func projectParentCopy(pc *ParentCopy, force bool) any {
	if force {
		return pc.Forced
	}
	return pc
}

// Scrubbed child produced string, force false honouring the user opt out while vault values stay scrubbed
func RedactChildText(env *Env, text any, force bool) (out string) {
	if pc, ok := text.(*ParentCopy); ok {
		if force {
			return pc.Forced
		}
		return pc.Text
	}
	s := ""
	if anyutil.IsTrue(text) {
		s = anyutil.String(text)
	}
	if force || redactionEnabled(env) {
		return RedactForEgress(env, s)
	}
	defer func() {
		if recover() != nil {
			out = RedactionUnavailable
		}
	}()
	return RedactSensitiveTextWith(env, s, RedactOptions{})
}

// String value redacted with its key as context so credential named fields get the key gated pass
func redactDelegationField(env *Env, key, value string, force bool) string {
	cleaned := RedactChildText(env, value, force)
	if cleaned != value || !IsSecretFieldName(key) {
		return cleaned
	}
	raw, err := jsonutil.Encode(map[string]any{key: value}, jsonutil.Spaced(), jsonutil.ASCII())
	if err != nil {
		return RedactionUnavailable
	}
	scrubbed := RedactChildText(env, raw, force)
	if scrubbed == raw {
		return cleaned
	}
	probe, err := jsonutil.Decode(scrubbed)
	if err != nil {
		return RedactionUnavailable
	}
	if obj, ok := probe.(*jsonutil.Object); ok && obj.Len() == 1 {
		if only, ok := obj.Value(obj.Keys()[0]).(string); ok {
			return only
		}
	}
	return RedactionUnavailable
}

// Copy of JSON shaped child data with unsupported or overly deep values withheld
func RedactChildValue(env *Env, value any, force bool) any {
	return redactChildValueAt(env, value, force, 0)
}

func redactChildValueAt(env *Env, value any, force bool, depth int) any {
	if depth > 64 {
		return RedactionUnavailable
	}
	switch v := value.(type) {
	case *ParentCopy:
		return projectParentCopy(v, force)
	case string:
		return RedactChildText(env, v, force)
	case *jsonutil.Object, map[string]any:
		keys, get, _ := delegationObject(v)
		out := jsonutil.NewObject()
		for _, k := range keys {
			item := get(k)
			rk := RedactChildText(env, k, force)
			var cleaned any
			if s, ok := item.(string); ok {
				cleaned = redactDelegationField(env, k, s, force)
			} else if pc, ok := item.(*ParentCopy); ok {
				cleaned = projectParentCopy(pc, force)
			} else {
				cleaned = redactChildValueAt(env, item, force, depth+1)
			}
			out.Set(rk, cleaned)
		}
		if _, isMap := v.(map[string]any); isMap {
			return out.Map()
		}
		return out
	case []any:
		out := make([]any, len(v))
		for i, item := range v {
			out[i] = redactChildValueAt(env, item, force, depth+1)
		}
		return out
	case []string:
		out := make([]any, len(v))
		for i, item := range v {
			out[i] = RedactChildText(env, item, force)
		}
		return out
	case map[string]string:
		out := make(map[string]any, len(v))
		for k, item := range v {
			out[RedactChildText(env, k, force)] = redactDelegationField(env, k, item, force)
		}
		return out
	case nil, bool, int, int8, int16, int32, int64, uint, uint8, uint16, uint32, uint64, float32, float64, json.Number, *big.Int:
		return v
	}
	return RedactionUnavailable
}

// Ordered keys and lookup of a map shaped value
func delegationObject(v any) ([]string, func(string) any, bool) {
	switch o := v.(type) {
	case *jsonutil.Object:
		return o.Keys(), func(k string) any { return o.Value(k) }, true
	case map[string]any:
		keys := make([]string, 0, len(o))
		for k := range o {
			keys = append(keys, k)
		}
		slices.Sort(keys)
		return keys, func(k string) any { return o[k] }, true
	}
	return nil, nil, false
}

// Summary redacted per value when structured so JSON delimiters survive, wrapper text getting the text pass
func redactDelegationSummary(env *Env, summary any, structured, force bool) any {
	if pc, ok := summary.(*ParentCopy); ok {
		return projectParentCopy(pc, force)
	}
	if s, ok := summary.(string); ok && structured {
		candidate := ExtractJSONCandidate(s)
		start := -1
		if candidate != "" {
			start = strings.Index(s, candidate)
		}
		var parsed any
		if start >= 0 {
			var err error
			if parsed, err = jsonutil.Decode(candidate); err != nil {
				start = -1
			}
		}
		if start >= 0 {
			prefix, suffix := s[:start], s[start+len(candidate):]
			cleaned := RedactChildValue(env, parsed, force)
			body := candidate
			if !anyutil.Equal(cleaned, parsed) {
				if text, err := jsonutil.Encode(cleaned, jsonutil.Spaced()); err == nil {
					body = text
				}
			}
			return RedactChildText(env, prefix, force) + body + RedactChildText(env, suffix, force)
		}
	}
	return RedactChildValue(env, summary, force)
}

// Summary redacted per value when structured, exported for the delegation result pipeline
func RedactSummary(env *Env, summary any, structured, force bool) any {
	return redactDelegationSummary(env, summary, structured, force)
}

// Text of a summary value with a parent copy compared by its text the way a str subclass compares
func summaryComparable(v any) any {
	if pc, ok := v.(*ParentCopy); ok {
		return pc.Text
	}
	return v
}

// Python frames under validate_output when _finalize_child_results revalidates a redacted summary in the reference
const finalizeRedactionValidateDepth = 29

// Projection of a result or batch keeping accounting and host owned identity, revalidating a summary redaction changed
func RedactChildResult(env *Env, result any, outputSchema any, force bool) any {
	return RedactChildResultAt(env, result, outputSchema, force, finalizeRedactionValidateDepth)
}

// RedactChildResult whose revalidation runs validateDepth Python frames deep, the depth of the reference call site it mirrors
func RedactChildResultAt(env *Env, result any, outputSchema any, force bool, validateDepth int) any {
	keys, get, ok := delegationObject(result)
	if !ok {
		return RedactChildValue(env, result, force)
	}
	_, hasValid := lookupKey(keys, "schema_valid")
	structured := isJSONObject(outputSchema) || hasValid
	cleaned := jsonutil.NewObject()
	set := func(k string, v any) {
		cleaned.Set(k, v)
	}
	for _, k := range keys {
		value := get(k)
		switch {
		case k == "results" && isDelegationList(value):
			list := value.([]any)
			out := make([]any, len(list))
			for i, entry := range list {
				if _, _, isObj := delegationObject(entry); isObj {
					out[i] = RedactChildResult(env, entry, nil, force)
				} else {
					out[i] = RedactChildValue(env, entry, force)
				}
			}
			set(k, out)
		case k == "summary":
			set(k, redactDelegationSummary(env, value, structured, force))
		case delegationResultMetadata[k]:
			set(k, value)
		default:
			set(k, RedactChildValue(env, value, force))
		}
	}
	if hasValid && !anyutil.Equal(summaryComparable(cleaned.Value("summary")), summaryComparable(get("summary"))) {
		valid, errs := false, []string{"The redacted summary has not been validated against the original output_schema."}
		if isJSONObject(outputSchema) {
			summary, _ := summaryComparable(cleaned.Value("summary")).(string)
			v, e, err := ValidateOutputAt(summary, outputSchema, validateDepth)
			if err != nil {
				errs = []string{"Output validation was unavailable after secret redaction."}
			} else {
				valid, errs = v, e
			}
		}
		set("schema_valid", valid)
		deleteKey(cleaned, "schema_errors")
		deleteKey(cleaned, "schema_note")
		if !valid {
			list := make([]any, len(errs))
			for i, e := range errs {
				list[i] = e
			}
			set("schema_errors", RedactChildValue(env, list, force))
			set("schema_note", "Secret redaction changed the delivered summary; do not treat it as validated output.")
		}
	}
	if _, isMap := result.(map[string]any); isMap {
		return cleaned.Map()
	}
	return cleaned
}

func isDelegationList(v any) bool {
	_, ok := v.([]any)
	return ok
}

func lookupKey(keys []string, k string) (int, bool) {
	i := slices.Index(keys, k)
	return i, i >= 0
}

func deleteKey(o *jsonutil.Object, k string) {
	o.Delete(k)
}

// Completion event with child output scrubbed and parent instructions and routing left exact
func RedactCompletionEvent(env *Env, event any, force bool) any {
	keys, get, ok := delegationObject(event)
	if !ok {
		return RedactChildValue(env, event, force)
	}
	child := jsonutil.NewObject()
	for _, k := range keys {
		if !delegationParentEventFields[k] {
			child.Set(k, get(k))
		}
	}
	cleaned := RedactChildResult(env, child, nil, force).(*jsonutil.Object)
	for _, k := range keys {
		if delegationParentEventFields[k] {
			cleaned.Set(k, get(k))
		}
	}
	if _, isMap := event.(map[string]any); isMap {
		return cleaned.Map()
	}
	return cleaned
}
