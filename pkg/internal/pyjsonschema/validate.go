package pyjsonschema

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"sort"
	"strconv"
	"strings"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/santhosh-tekuri/jsonschema/v5"
)

// Best matching schema violation with the message Python jsonschema gives it
type ValidationError struct {
	Message string
	Keyword string
	Path    []string
}

func (e *ValidationError) Error() string { return e.Message }

var weakKeywords = map[string]bool{"anyOf": true, "oneOf": true}

// Raises the best_match error of instance against schema the way jsonschema.validate does
func Validate(instance, schema any) error {
	compiled, err := compile(schema)
	if err != nil {
		return err
	}
	plain, err := decodePlain(instance)
	if err != nil {
		return err
	}
	verr := compiled.Validate(plain)
	if verr == nil {
		return nil
	}
	var ve *jsonschema.ValidationError
	if !errors.As(verr, &ve) {
		return verr
	}
	best := bestMatch(ve)
	w := newWalker(map[string]any{schemaURL: schema}, schemaURL)
	msg, path := describe(best, instance, w)
	return &ValidationError{Message: msg, Keyword: keyword(best), Path: path}
}

// Draft 2020-12 compilation with every remote reference refused
func compile(schema any) (*jsonschema.Schema, error) {
	text, err := pyjson.Dumps(schema)
	if err != nil {
		return nil, err
	}
	compiler := jsonschema.NewCompiler()
	compiler.Draft = jsonschema.Draft2020
	compiler.LoadURL = func(url string) (io.ReadCloser, error) { return nil, fmt.Errorf("remote schema load refused: %s", url) }
	if err := compiler.AddResource(schemaURL, bytes.NewReader([]byte(text))); err != nil {
		return nil, err
	}
	return compiler.Compile(schemaURL)
}

// Unescaped segments of a JSON pointer
func pointer(location string) []string {
	if location == "" || location == "/" {
		return nil
	}
	parts := strings.Split(strings.TrimPrefix(location, "/"), "/")
	for i, p := range parts {
		parts[i] = strings.ReplaceAll(strings.ReplaceAll(p, "~1", "/"), "~0", "~")
	}
	return parts
}

// Value a pointer path reaches inside a decoded document, nil when absent
func at(doc any, path []string) any {
	for _, seg := range path {
		switch v := doc.(type) {
		case *pyjson.Object:
			doc = v.Vals[seg]
		case map[string]any:
			doc = v[seg]
		case []any:
			i, err := strconv.Atoi(seg)
			if err != nil || i < 0 || i >= len(v) {
				return nil
			}
			doc = v[i]
		default:
			return nil
		}
	}
	return doc
}

func keyword(e *jsonschema.ValidationError) string {
	parts := pointer(e.KeywordLocation)
	if len(parts) == 0 {
		return ""
	}
	return parts[len(parts)-1]
}

func leaves(e *jsonschema.ValidationError) []*jsonschema.ValidationError {
	if len(e.Causes) == 0 || weakKeywords[keyword(e)] {
		return []*jsonschema.ValidationError{e}
	}
	var out []*jsonschema.ValidationError
	for _, c := range e.Causes {
		out = append(out, leaves(c)...)
	}
	return out
}

func moreRelevant(a, b *jsonschema.ValidationError) bool {
	da, db := len(pointer(a.InstanceLocation)), len(pointer(b.InstanceLocation))
	if da != db {
		return da < db
	}
	wa, wb := weakKeywords[keyword(a)], weakKeywords[keyword(b)]
	return wa != wb && !wa
}

// Single most relevant error, descending into anyOf and oneOf context like best_match
func bestMatch(root *jsonschema.ValidationError) *jsonschema.ValidationError {
	all := leaves(root)
	sort.SliceStable(all, func(i, j int) bool { return moreRelevant(all[i], all[j]) })
	best := all[0]
	for len(best.Causes) > 0 {
		var context []*jsonschema.ValidationError
		for _, c := range best.Causes {
			context = append(context, leaves(c)...)
		}
		sort.SliceStable(context, func(i, j int) bool { return moreRelevant(context[j], context[i]) })
		if len(context) >= 2 && !moreRelevant(context[0], context[1]) && !moreRelevant(context[1], context[0]) {
			return best
		}
		best = context[0]
	}
	return best
}

func joinRepr(items []any) string {
	parts := make([]string, len(items))
	for i, item := range items {
		parts[i] = pyrepr.Repr(item)
	}
	return strings.Join(parts, ", ")
}

func number(v any) float64 {
	f, _ := strconv.ParseFloat(pyrepr.Repr(v), 64)
	return f
}

func size(v any) int {
	switch x := v.(type) {
	case string:
		return len([]rune(x))
	case []any:
		return len(x)
	case *pyjson.Object:
		return len(x.Keys)
	}
	return 0
}

func keysOf(v any) []string {
	switch x := v.(type) {
	case *pyjson.Object:
		return x.Keys
	case map[string]any:
		keys := make([]string, 0, len(x))
		for k := range x {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		return keys
	}
	return nil
}

// Wording the Python jsonschema keyword function yields for one failed keyword
func message(e *jsonschema.ValidationError, instance, value, parent any) string {
	r := pyrepr.Repr(instance)
	switch kw := keyword(e); kw {
	case "type":
		types, ok := value.([]any)
		if !ok {
			types = []any{value}
		}
		return r + " is not of type " + joinRepr(types)
	case "required":
		for _, name := range asList(value) {
			if s, ok := name.(string); ok && !hasKey(instance, s) {
				return pyrepr.Repr(s) + " is a required property"
			}
		}
	case "enum":
		return r + " is not one of " + pyrepr.Repr(value)
	case "const":
		return pyrepr.Repr(value) + " was expected"
	case "minimum":
		return r + " is less than the minimum of " + pyrepr.Repr(value)
	case "maximum":
		return r + " is greater than the maximum of " + pyrepr.Repr(value)
	case "exclusiveMinimum":
		return r + " is less than or equal to the minimum of " + pyrepr.Repr(value)
	case "exclusiveMaximum":
		return r + " is greater than or equal to the maximum of " + pyrepr.Repr(value)
	case "multipleOf":
		return r + " is not a multiple of " + pyrepr.Repr(value)
	case "minLength", "minItems":
		if number(value) == 1 {
			return r + " should be non-empty"
		}
		return r + " is too short"
	case "maxLength", "maxItems":
		if number(value) == 0 {
			return r + " is expected to be empty"
		}
		return r + " is too long"
	case "minProperties":
		if number(value) == 1 {
			return r + " should be non-empty"
		}
		return r + " does not have enough properties"
	case "maxProperties":
		if number(value) == 0 {
			return r + " is expected to be empty"
		}
		return r + " has too many properties"
	case "uniqueItems":
		return r + " has non-unique elements"
	case "pattern":
		return r + " does not match " + pyrepr.Repr(value)
	case "format":
		return r + " is not a " + pyrepr.Repr(value)
	case "anyOf":
		return r + " is not valid under any of the given schemas"
	case "oneOf":
		if len(e.Causes) == 0 {
			return r + " is valid under each of " + joinRepr(asList(value))
		}
		return r + " is not valid under any of the given schemas"
	case "not":
		return r + " should not be valid under " + pyrepr.Repr(value)
	case "contains", "minContains":
		return r + " does not contain items matching the given schema"
	case "maxContains":
		return r + " contains too many items matching the given schema"
	case "dependentRequired":
		for prop, deps := range mapOf(value) {
			if hasKey(instance, prop) {
				for _, dep := range asList(deps) {
					if s, ok := dep.(string); ok && !hasKey(instance, s) {
						return pyrepr.Repr(s) + " is a dependency of " + pyrepr.Repr(prop)
					}
				}
			}
		}
	case "additionalProperties", "unevaluatedProperties", "propertyNames":
		if b, ok := value.(bool); ok && !b && kw != "propertyNames" {
			return extrasMessage(kw, instance, parent)
		}
	case "items", "prefixItems", "unevaluatedItems":
		if b, ok := value.(bool); ok && !b {
			return itemsMessage(instance, parent)
		}
	}
	if b, ok := value.(bool); ok && !b {
		return "False schema does not allow " + r
	}
	return e.Message
}

// Message for properties a false additionalProperties or unevaluatedProperties rejects
func extrasMessage(kw string, instance, parent any) string {
	declared := mapOf(at(parent, []string{"properties"}))
	patterns := mapOf(at(parent, []string{"patternProperties"}))
	var extras []any
	for _, k := range keysOf(instance) {
		if _, ok := declared[k]; ok {
			continue
		}
		matched := false
		for p := range patterns {
			if re, err := compilePattern(p); err == nil && re(k) {
				matched = true
			}
		}
		if !matched {
			extras = append(extras, k)
		}
	}
	sort.SliceStable(extras, func(i, j int) bool { return extras[i].(string) < extras[j].(string) })
	if kw == "additionalProperties" && len(patterns) > 0 {
		names := make([]any, 0, len(patterns))
		for p := range patterns {
			names = append(names, p)
		}
		sort.SliceStable(names, func(i, j int) bool { return names[i].(string) < names[j].(string) })
		verb := "do"
		if len(extras) == 1 {
			verb = "does"
		}
		return joinRepr(extras) + " " + verb + " not match any of the regexes: " + joinRepr(names)
	}
	verb := "were"
	if len(extras) == 1 {
		verb = "was"
	}
	if kw == "unevaluatedProperties" {
		return "Unevaluated properties are not allowed (" + joinRepr(extras) + " " + verb + " unexpected)"
	}
	return "Additional properties are not allowed (" + joinRepr(extras) + " " + verb + " unexpected)"
}

func asList(v any) []any {
	l, _ := v.([]any)
	return l
}

func mapOf(v any) map[string]any {
	switch x := v.(type) {
	case *pyjson.Object:
		return x.Vals
	case map[string]any:
		return x
	}
	return nil
}

func hasKey(v any, k string) bool {
	_, ok := mapOf(v)[k]
	return ok
}

// Message for array items past prefixItems that a false items schema rejects
func itemsMessage(instance, parent any) string {
	prefix, list := len(asList(at(parent, []string{"prefixItems"}))), asList(instance)
	extra := len(list) - prefix
	plural := "s"
	if prefix == 1 {
		plural = ""
	}
	var rest any = list[min(prefix, len(list)):]
	if extra == 1 {
		rest = list[prefix]
	}
	return "Expected at most " + strconv.Itoa(prefix) + " item" + plural + " but found " + strconv.Itoa(extra) + " extra: " + pyrepr.Repr(rest)
}
