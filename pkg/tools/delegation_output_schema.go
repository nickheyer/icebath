package tools

import (
	"cmp"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"unicode"

	anyutil "github.com/nickheyer/icebath/pkg/utils/any"
	jsonutil "github.com/nickheyer/icebath/pkg/utils/json"
	jsonschemautil "github.com/nickheyer/icebath/pkg/utils/jsonschema"
)

// Schema object when usable, an error message when not, both empty when no schema was requested
func CoerceOutputSchema(raw any) (any, string) {
	if raw == nil {
		return nil, ""
	}
	if s, ok := raw.(string); ok {
		parsed, err := jsonutil.Decode(s)
		if err != nil {
			return nil, "output_schema must be a JSON Schema object, got a non-JSON string."
		}
		if !isJSONObject(parsed) {
			return nil, "output_schema must be a JSON Schema object."
		}
		raw = parsed
	}
	if !isJSONObject(raw) {
		return nil, fmt.Sprintf("output_schema must be a JSON Schema object, got %s.", anyutil.Kind(raw))
	}
	if _, err := jsonschemautil.Compile(raw); err != nil {
		return nil, "output_schema is not a valid JSON Schema: " + err.Error()
	}
	return raw, ""
}

// Task list with each task's output_schema swapped for its Python valued copy, the dict delegate_task parsed
func WithPythonTaskSchemas(tasks, tasksValue any) any {
	list, ok := tasks.([]any)
	listOf, okValue := tasksValue.([]any)
	if !ok || !okValue || len(list) != len(listOf) {
		return tasks
	}
	out := make([]any, len(list))
	for i, t := range list {
		out[i] = t
		taskValue, isObject := listOf[i].(*jsonutil.Object)
		m, isMap := t.(map[string]any)
		if !isObject || !isMap {
			continue
		}
		schema, has := taskValue.Get("output_schema")
		if !has {
			continue
		}
		cp := make(map[string]any, len(m))
		for k, v := range m {
			cp[k] = v
		}
		cp["output_schema"] = schema
		out[i] = cp
	}
	return out
}

func isJSONObject(v any) bool {
	switch v.(type) {
	case map[string]any, *jsonutil.Object:
		return true
	}
	return false
}

// Child context with the machine validated output contract block appended
func AppendOutputContract(context string, schema any) string {
	schemaText, err := jsonutil.Encode(schema, jsonutil.Indent(2))
	if err != nil {
		schemaText = anyutil.String(schema)
	}
	block := "OUTPUT CONTRACT (machine-validated):\n" +
		"Your FINAL response must be ONLY the JSON value that validates against this JSON " +
		"Schema — no prose before or after it, no code fence, no explanation. Anything else " +
		"costs a correction turn and, if it fails again, is handed to the caller unvalidated.\n" +
		schemaText
	base := strings.TrimRightFunc(context, unicode.IsSpace)
	if base != "" {
		return base + "\n\n" + block
	}
	return block
}

// Outermost parseable object or array span with code fences and surrounding prose stripped
func ExtractJSONCandidate(text string) string {
	raw := strings.TrimSpace(text)
	if strings.HasPrefix(raw, "```") {
		if _, rest, ok := strings.Cut(raw, "\n"); ok {
			raw = rest
		}
		if strings.HasSuffix(strings.TrimRightFunc(raw, unicode.IsSpace), "```") {
			trimmed := strings.TrimRightFunc(raw, unicode.IsSpace)
			raw = trimmed[:len(trimmed)-3]
		}
		raw = strings.TrimSpace(raw)
		if strings.HasPrefix(strings.ToLower(raw), "json\n") {
			_, raw, _ = strings.Cut(raw, "\n")
		}
	}
	type span struct {
		start     int
		candidate string
	}
	var spans []span
	for _, pair := range [][2]string{{"{", "}"}, {"[", "]"}} {
		start, end := strings.Index(raw, pair[0]), strings.LastIndex(raw, pair[1])
		if start >= 0 && end > start {
			spans = append(spans, span{start, raw[start : end+1]})
		}
	}
	slices.SortStableFunc(spans, func(a, b span) int {
		return cmp.Or(cmp.Compare(a.start, b.start), strings.Compare(a.candidate, b.candidate))
	})
	for _, s := range spans {
		if _, err := jsonutil.Decode(s.candidate); err == nil {
			return s.candidate
		}
	}
	if len(spans) > 0 {
		return spans[0].candidate
	}
	return raw
}

// Validity of a final answer against schema with retry ready error strings, at most ten
func ValidateOutput(text string, schema any) (bool, []string, error) {
	candidate := ExtractJSONCandidate(text)
	if strings.TrimSpace(candidate) == "" {
		return false, []string{"Response was empty — expected a JSON object matching the schema."}, nil
	}
	parsed, err := jsonutil.Decode(candidate)
	if err != nil {
		return false, []string{"Response is not valid JSON: " + err.Error()}, nil
	}
	sch, err := jsonschemautil.Compile(schema)
	if err != nil {
		return false, nil, err
	}
	errs := sch.Errors(parsed)
	slices.SortStableFunc(errs, func(a, b *jsonschemautil.ValidationError) int { return comparePaths(a.InstancePath, b.InstancePath) })
	rendered := []string{}
	for _, e := range errs[:min(len(errs), 10)] {
		var b strings.Builder
		b.WriteString("$")
		for _, p := range e.InstancePath {
			switch x := p.(type) {
			case int:
				b.WriteString("[" + strconv.Itoa(x) + "]")
			case string:
				b.WriteString("." + x)
			}
		}
		rendered = append(rendered, b.String()+": "+e.Message)
	}
	return len(rendered) == 0, rendered, nil
}

// Order of two paths into a value: positions by number, names by text, a position before a name, a prefix first
func comparePaths(a, b []any) int {
	for i := 0; i < min(len(a), len(b)); i++ {
		an, aInt := a[i].(int)
		bn, bInt := b[i].(int)
		switch {
		case aInt && bInt:
			if c := cmp.Compare(an, bn); c != 0 {
				return c
			}
		case aInt:
			return -1
		case bInt:
			return 1
		default:
			as, _ := a[i].(string)
			bs, _ := b[i].(string)
			if c := strings.Compare(as, bs); c != 0 {
				return c
			}
		}
	}
	return cmp.Compare(len(a), len(b))
}

// Single bounded retry turn carrying the errors verbatim without re-pasting the schema
func BuildRetryMessage(errs []string) string {
	lines := make([]string, len(errs))
	for i, e := range errs {
		lines[i] = "- " + e
	}
	return "Your previous final response was rejected by the output contract " +
		"validator. Validation errors:\n" + strings.Join(lines, "\n") + "\n\n" +
		"Reply with ONLY the corrected JSON object matching the OUTPUT " +
		"CONTRACT schema from your task context. No prose, no explanations."
}
