package tools

import (
	"cmp"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyjsonschema"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
)

// Schema object when usable, an error message when not, both empty when no schema was requested
func CoerceOutputSchema(raw any) (any, string) {
	if raw == nil {
		return nil, ""
	}
	if s, ok := raw.(string); ok {
		parsed, err := pyjson.Loads(s)
		if err != nil {
			return nil, "output_schema must be a JSON Schema object, got a non-JSON string."
		}
		if !isJSONObject(parsed) {
			return nil, "output_schema must be a JSON Schema object."
		}
		raw = parsed
	}
	if !isJSONObject(raw) {
		return nil, fmt.Sprintf("output_schema must be a JSON Schema object, got %s.", pyrepr.TypeName(raw))
	}
	if err := pyjsonschema.CheckSchema(raw); err != nil {
		return nil, "output_schema is not a valid JSON Schema: " + err.Error()
	}
	return raw, ""
}

func isJSONObject(v any) bool {
	switch v.(type) {
	case map[string]any, *pyjson.Object:
		return true
	}
	return false
}

// Child context with the machine validated output contract block appended
func AppendOutputContract(context string, schema any) string {
	schemaText, err := pyjson.Dumps(schema, pyjson.Indent(2), pyjson.EnsureASCII(false))
	if err != nil {
		schemaText = pyrepr.Str(schema)
	}
	block := "OUTPUT CONTRACT (machine-validated):\n" +
		"Your FINAL response must be ONLY the JSON value that validates against this JSON " +
		"Schema — no prose before or after it, no code fence, no explanation. Anything else " +
		"costs a correction turn and, if it fails again, is handed to the caller unvalidated.\n" +
		schemaText
	base := pystr.Rstrip(context)
	if base != "" {
		return base + "\n\n" + block
	}
	return block
}

// Outermost parseable object or array span with code fences and surrounding prose stripped
func ExtractJSONCandidate(text string) string {
	raw := pystr.Strip(text)
	if strings.HasPrefix(raw, "```") {
		if _, rest, ok := strings.Cut(raw, "\n"); ok {
			raw = rest
		}
		if strings.HasSuffix(pystr.Rstrip(raw), "```") {
			trimmed := pystr.Rstrip(raw)
			raw = trimmed[:len(trimmed)-3]
		}
		raw = pystr.Strip(raw)
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
		if _, err := pyjson.Loads(s.candidate); err == nil {
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
	if pystr.Strip(candidate) == "" {
		return false, []string{"Response was empty — expected a JSON object matching the schema."}, nil
	}
	parsed, err := pyjson.Loads(candidate)
	if err != nil {
		return false, []string{"Response is not valid JSON: " + err.Error()}, nil
	}
	errs, err := pyjsonschema.IterErrors(parsed, schema)
	if err != nil {
		return false, nil, err
	}
	var typeErr error
	slices.SortStableFunc(errs, func(a, b *pyjsonschema.IterError) int {
		c, err := comparePaths(a.AbsolutePath, b.AbsolutePath)
		if err != nil && typeErr == nil {
			typeErr = err
		}
		return c
	})
	if typeErr != nil {
		return false, nil, typeErr
	}
	rendered := []string{}
	for _, e := range errs[:min(len(errs), 10)] {
		var b strings.Builder
		b.WriteString("$")
		for _, p := range e.AbsolutePath {
			if n, ok := p.(int); ok {
				b.WriteString("[" + strconv.Itoa(n) + "]")
			} else {
				b.WriteString("." + pyrepr.Str(p))
			}
		}
		rendered = append(rendered, b.String()+": "+e.Message)
	}
	return len(rendered) == 0, rendered, nil
}

// Python list ordering of two absolute paths, a TypeError when an index meets a key
func comparePaths(a, b []any) (int, error) {
	for i := 0; i < min(len(a), len(b)); i++ {
		an, aInt := a[i].(int)
		bn, bInt := b[i].(int)
		switch {
		case aInt && bInt:
			if c := cmp.Compare(an, bn); c != 0 {
				return c, nil
			}
		case !aInt && !bInt:
			if c := strings.Compare(pyrepr.Str(a[i]), pyrepr.Str(b[i])); c != 0 {
				return c, nil
			}
		default:
			return 0, errors.New("'<' not supported between instances of 'int' and 'str'")
		}
	}
	return cmp.Compare(len(a), len(b)), nil
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
