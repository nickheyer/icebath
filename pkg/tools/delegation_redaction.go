package tools

import (
	"encoding/json"
	"math/big"
	"slices"
	"strings"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
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

// Text for the parent remembering the forced projection, the plain string when both match
func NewParentCopy(text, forced string) any {
	if text == forced {
		return text
	}
	return &ParentCopy{Text: text, Forced: forced}
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
	if pyrepr.Truthy(text) {
		s = pyrepr.Str(text)
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
	raw, err := pyjson.Dumps(map[string]any{key: value})
	if err != nil {
		return RedactionUnavailable
	}
	scrubbed := RedactChildText(env, raw, force)
	if scrubbed == raw {
		return cleaned
	}
	probe, err := pyjson.Loads(scrubbed)
	if err != nil {
		return RedactionUnavailable
	}
	if obj, ok := probe.(*pyjson.Object); ok && len(obj.Keys) == 1 {
		if only, ok := obj.Vals[obj.Keys[0]].(string); ok {
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
	case string, *ParentCopy:
		return RedactChildText(env, v, force)
	case *pyjson.Object, map[string]any:
		keys, get, _ := delegationObject(v)
		out := pyjson.NewObject()
		for _, k := range keys {
			item := get(k)
			rk := RedactChildText(env, k, force)
			var cleaned any
			if s, ok := item.(string); ok {
				cleaned = redactDelegationField(env, k, s, force)
			} else if pc, ok := item.(*ParentCopy); ok {
				cleaned = RedactChildText(env, pc, force)
			} else {
				cleaned = redactChildValueAt(env, item, force, depth+1)
			}
			if _, seen := out.Vals[rk]; !seen {
				out.Keys = append(out.Keys, rk)
			}
			out.Vals[rk] = cleaned
		}
		if _, isMap := v.(map[string]any); isMap {
			return out.Vals
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
	case nil, bool, int, int8, int16, int32, int64, uint, uint8, uint16, uint32, uint64, float32, float64, json.Number, *big.Int:
		return v
	}
	return RedactionUnavailable
}

// Ordered keys and lookup of a map shaped value
func delegationObject(v any) ([]string, func(string) any, bool) {
	switch o := v.(type) {
	case *pyjson.Object:
		return o.Keys, func(k string) any { return o.Vals[k] }, true
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
		return RedactChildText(env, pc, force)
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
			if parsed, err = pyjson.Loads(candidate); err != nil {
				start = -1
			}
		}
		if start >= 0 {
			prefix, suffix := s[:start], s[start+len(candidate):]
			cleaned := RedactChildValue(env, parsed, force)
			body := candidate
			if !pyrepr.Equal(cleaned, parsed) {
				if text, err := pyjson.Dumps(cleaned, pyjson.EnsureASCII(false)); err == nil {
					body = text
				}
			}
			return RedactChildText(env, prefix, force) + body + RedactChildText(env, suffix, force)
		}
	}
	return RedactChildValue(env, summary, force)
}

// Text of a summary value with a parent copy compared by its text the way a str subclass compares
func summaryComparable(v any) any {
	if pc, ok := v.(*ParentCopy); ok {
		return pc.Text
	}
	return v
}

// Projection of a result or batch keeping accounting and host owned identity, revalidating a summary redaction changed
func RedactChildResult(env *Env, result any, outputSchema any, force bool) any {
	keys, get, ok := delegationObject(result)
	if !ok {
		return RedactChildValue(env, result, force)
	}
	_, hasValid := lookupKey(keys, "schema_valid")
	structured := isJSONObject(outputSchema) || hasValid
	cleaned := pyjson.NewObject()
	set := func(k string, v any) {
		if _, seen := cleaned.Vals[k]; !seen {
			cleaned.Keys = append(cleaned.Keys, k)
		}
		cleaned.Vals[k] = v
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
	if hasValid && !pyrepr.Equal(summaryComparable(cleaned.Vals["summary"]), summaryComparable(get("summary"))) {
		valid, errs := false, []string{"The redacted summary has not been validated against the original output_schema."}
		if isJSONObject(outputSchema) {
			summary, _ := cleaned.Vals["summary"].(string)
			v, e, err := ValidateOutput(summary, outputSchema)
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
		return cleaned.Vals
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

func deleteKey(o *pyjson.Object, k string) {
	if i := slices.Index(o.Keys, k); i >= 0 {
		o.Keys = slices.Delete(o.Keys, i, i+1)
		delete(o.Vals, k)
	}
}

// Completion event with child output scrubbed and parent instructions and routing left exact
func RedactCompletionEvent(env *Env, event any, force bool) any {
	keys, get, ok := delegationObject(event)
	if !ok {
		return RedactChildValue(env, event, force)
	}
	child := pyjson.NewObject()
	for _, k := range keys {
		if !delegationParentEventFields[k] {
			child.Keys = append(child.Keys, k)
			child.Vals[k] = get(k)
		}
	}
	cleaned := RedactChildResult(env, child, nil, force).(*pyjson.Object)
	for _, k := range keys {
		if delegationParentEventFields[k] {
			if _, seen := cleaned.Vals[k]; !seen {
				cleaned.Keys = append(cleaned.Keys, k)
			}
			cleaned.Vals[k] = get(k)
		}
	}
	if _, isMap := event.(map[string]any); isMap {
		return cleaned.Vals
	}
	return cleaned
}
