package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/santhosh-tekuri/jsonschema/v5"
)

// Literal keywords hold instance data rather than schemas
var validationSchemaLiteralKeys = map[string]bool{"const": true, "default": true, "enum": true, "example": true, "examples": true}

var validationIdentifierRe = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

var validationWhitespaceRe = regexp.MustCompile(`\s+`)

// JSON Schema compatible copy honoring OpenAPI nullable true
func validationSchemaForLocal(node any) any {
	switch t := node.(type) {
	case []any:
		out := make([]any, len(t))
		for i, item := range t {
			out[i] = validationSchemaForLocal(item)
		}
		return out
	case map[string]any:
		normalized := map[string]any{}
		for key, value := range t {
			if key == "nullable" {
				continue
			}
			if validationSchemaLiteralKeys[key] {
				normalized[key] = schemaDeepCopy(value)
			} else {
				normalized[key] = validationSchemaForLocal(value)
			}
		}
		if nullable, ok := t["nullable"].(bool); !ok || !nullable {
			return normalized
		}
		var types []any
		switch st := normalized["type"].(type) {
		case string:
			types = []any{st}
		case []any:
			types = st
		}
		if types != nil {
			hasNull := false
			for _, v := range types {
				if s, ok := v.(string); ok && s == "null" {
					hasNull = true
				}
			}
			if !hasNull {
				normalized["type"] = append(append([]any{}, types...), "null")
			}
			return normalized
		}
		return map[string]any{"anyOf": []any{normalized, map[string]any{"type": "null"}}}
	default:
		return node
	}
}

// Reports whether a node contains a non local $ref so validation never fetches anything
func validationSchemaHasExternalRef(node any) bool {
	switch t := node.(type) {
	case []any:
		for _, item := range t {
			if validationSchemaHasExternalRef(item) {
				return true
			}
		}
		return false
	case map[string]any:
		if ref, ok := t["$ref"].(string); ok && !strings.HasPrefix(ref, "#") {
			return true
		}
		for key, value := range t {
			if validationSchemaLiteralKeys[key] {
				continue
			}
			if validationSchemaHasExternalRef(value) {
				return true
			}
		}
		return false
	default:
		return false
	}
}

// Formats a JSON pointer instance location as a compact argument path
func validationPath(instanceLocation string) string {
	path := "arguments"
	if instanceLocation == "" {
		return path
	}
	for _, raw := range strings.Split(strings.TrimPrefix(instanceLocation, "/"), "/") {
		part := strings.ReplaceAll(strings.ReplaceAll(raw, "~1", "/"), "~0", "~")
		if _, err := strconv.Atoi(part); err == nil {
			path += "[" + part + "]"
		} else if validationIdentifierRe.MatchString(part) {
			path += "." + part
		} else {
			path += "[" + jsonCompact(part) + "]"
		}
	}
	return path
}

func validationError(message, path, constraint string, parameters any) string {
	return toolErrorJSON(message, map[string]any{"path": path, "constraint": constraint, "parameters": parameters,
		"hint": "Retry tool_call with 'arguments' matching the parameters schema above."})
}

// Weak keywords lose relevance ties the way jsonschema best_match treats anyOf and oneOf
var validationWeakKeywords = map[string]bool{"anyOf": true, "oneOf": true}

func validationKeyword(err *jsonschema.ValidationError) string {
	loc := err.KeywordLocation
	if idx := strings.LastIndex(loc, "/"); idx >= 0 {
		loc = loc[idx+1:]
	}
	if loc == "" {
		return "schema"
	}
	return loc
}

func validationPathDepth(err *jsonschema.ValidationError) int {
	if err.InstanceLocation == "" {
		return 0
	}
	return strings.Count(err.InstanceLocation, "/")
}

// Flattens nested causes into the leaf errors jsonschema iter_errors would yield
func validationLeaves(err *jsonschema.ValidationError) []*jsonschema.ValidationError {
	if len(err.Causes) == 0 {
		return []*jsonschema.ValidationError{err}
	}
	if validationWeakKeywords[validationKeyword(err)] {
		return []*jsonschema.ValidationError{err}
	}
	var out []*jsonschema.ValidationError
	for _, c := range err.Causes {
		out = append(out, validationLeaves(c)...)
	}
	return out
}

// Relevance ordering of jsonschema best_match with shallower paths and strong keywords first
func validationMoreRelevant(a, b *jsonschema.ValidationError) bool {
	da, db := validationPathDepth(a), validationPathDepth(b)
	if da != db {
		return da < db
	}
	wa, wb := validationWeakKeywords[validationKeyword(a)], validationWeakKeywords[validationKeyword(b)]
	if wa != wb {
		return !wa
	}
	return false
}

func validationSameRelevance(a, b *jsonschema.ValidationError) bool {
	return !validationMoreRelevant(a, b) && !validationMoreRelevant(b, a)
}

// Picks the single most useful error the way jsonschema best_match does
func validationBestMatch(root *jsonschema.ValidationError) *jsonschema.ValidationError {
	leaves := validationLeaves(root)
	if len(leaves) == 0 {
		return root
	}
	sort.SliceStable(leaves, func(i, j int) bool { return validationMoreRelevant(leaves[i], leaves[j]) })
	best := leaves[0]
	for len(best.Causes) > 0 {
		context := make([]*jsonschema.ValidationError, 0, len(best.Causes))
		for _, c := range best.Causes {
			context = append(context, validationLeaves(c)...)
		}
		if len(context) == 0 {
			break
		}
		sort.SliceStable(context, func(i, j int) bool { return validationMoreRelevant(context[j], context[i]) })
		if len(context) >= 2 && validationSameRelevance(context[0], context[1]) {
			return best
		}
		best = context[0]
	}
	return best
}

// ValidateDeferredCallArgs validates tool_call arguments against the deferred tool's schema, failing open
func ValidateDeferredCallArgs(ctx context.Context, env *Env, name string, args map[string]any) string {
	def := Default.Entry(ctx, env, name)
	if def == nil || def.Schema == nil {
		return ""
	}
	schema := def.Schema
	fn := schema
	if t, _ := schema["type"].(string); t == "function" {
		fn, _ = schema["function"].(map[string]any)
	}
	params, _ := fn["parameters"].(map[string]any)
	if params == nil {
		return ""
	}
	var missing []string
	if required, ok := params["required"].([]any); ok {
		for _, r := range required {
			if s, ok := r.(string); ok {
				if _, has := args[s]; !has {
					missing = append(missing, s)
				}
			}
		}
	}
	if len(missing) > 0 {
		return validationError(
			fmt.Sprintf("tool_call to '%s' is missing required argument(s): %s. The tool was NOT invoked.", name, strings.Join(missing, ", ")),
			"arguments", "required", params)
	}
	validationSchema := validationSchemaForLocal(params)
	if validationSchemaHasExternalRef(validationSchema) {
		slog.Debug("Skipping local deferred-argument validation: external $ref", "tool", name)
		return ""
	}
	candidate := make(map[string]any, len(args))
	for k, v := range args {
		candidate[k] = v
	}
	candidate = CoerceToolArgs(ctx, Default, env, name, candidate)
	compiled, err := validationCompile(validationSchema)
	if err != nil {
		slog.Debug("validate_deferred_call_args schema compile failed", "tool", name, "error", err)
		return ""
	}
	var instance any
	raw, err := json.Marshal(candidate)
	if err != nil {
		return ""
	}
	if err := json.Unmarshal(raw, &instance); err != nil {
		return ""
	}
	verr := compiled.Validate(instance)
	if verr == nil {
		return ""
	}
	var ve *jsonschema.ValidationError
	if !errors.As(verr, &ve) {
		slog.Debug("validate_deferred_call_args failed", "tool", name, "error", verr)
		return ""
	}
	best := validationBestMatch(ve)
	path := validationPath(best.InstanceLocation)
	constraint := validationKeyword(best)
	detail := strings.TrimSpace(validationWhitespaceRe.ReplaceAllString(best.Message, " "))
	if len(detail) > 600 {
		detail = detail[:597] + "..."
	}
	return validationError(
		fmt.Sprintf("tool_call to '%s' failed argument validation at %s (%s): %s. The tool was NOT invoked.", name, path, constraint, detail),
		path, constraint, params)
}

// Compiles a parameters schema as draft 2020-12 with every remote load refused
func validationCompile(schema any) (*jsonschema.Schema, error) {
	raw, err := json.Marshal(schema)
	if err != nil {
		return nil, err
	}
	compiler := jsonschema.NewCompiler()
	compiler.Draft = jsonschema.Draft2020
	compiler.LoadURL = func(url string) (io.ReadCloser, error) {
		return nil, fmt.Errorf("remote schema load refused: %s", url)
	}
	const url = "icebath://tool-parameters.json"
	if err := compiler.AddResource(url, strings.NewReader(string(raw))); err != nil {
		return nil, err
	}
	return compiler.Compile(url)
}

// ToolCallEntry is one normalized tool_call batch entry
type ToolCallEntry struct {
	Name      string
	Arguments map[string]any
}

// NormalizeToolCallEntries turns tool_call arguments into a calls list accepting the legacy single shape
func NormalizeToolCallEntries(args map[string]any) ([]ToolCallEntry, string) {
	rawCalls, has := args["calls"]
	if !has || rawCalls == nil {
		name := ""
		if n, ok := args["name"]; ok && n != nil {
			name = strings.TrimSpace(fmt.Sprint(n))
		}
		if name == "" {
			return nil, "tool_call requires 'calls' (an array of {name, arguments})"
		}
		rawCalls = []any{map[string]any{"name": args["name"], "arguments": args["arguments"]}}
	}
	if s, ok := rawCalls.(string); ok {
		var parsed any
		if err := json.Unmarshal([]byte(s), &parsed); err != nil {
			return nil, fmt.Sprintf("tool_call 'calls' is not valid JSON: %s", err.Error())
		}
		rawCalls = parsed
	}
	if m, ok := rawCalls.(map[string]any); ok {
		rawCalls = []any{m}
	}
	list, ok := rawCalls.([]any)
	if !ok || len(list) == 0 {
		return nil, "tool_call 'calls' must be a non-empty array of {name, arguments}"
	}
	entries := make([]ToolCallEntry, 0, len(list))
	for position, raw := range list {
		m, ok := raw.(map[string]any)
		if !ok {
			return nil, fmt.Sprintf("tool_call calls[%d] must be an object with 'name' and 'arguments'", position)
		}
		name := ""
		if n, ok := m["name"]; ok && n != nil {
			name = strings.TrimSpace(fmt.Sprint(n))
		}
		if name == "" {
			return nil, fmt.Sprintf("tool_call calls[%d] requires a 'name'", position)
		}
		if BridgeToolNames[name] {
			return nil, fmt.Sprintf("tool_call cannot invoke '%s' (it is itself a bridge tool)", name)
		}
		rawArgs := m["arguments"]
		if s, isStr := rawArgs.(string); rawArgs == nil || (isStr && strings.TrimSpace(s) == "") {
			rawArgs = map[string]any{}
		}
		if s, isStr := rawArgs.(string); isStr {
			var parsed any
			if err := json.Unmarshal([]byte(s), &parsed); err != nil {
				return nil, fmt.Sprintf("tool_call calls[%d].arguments is not valid JSON: %s", position, err.Error())
			}
			rawArgs = parsed
		}
		argMap, ok := rawArgs.(map[string]any)
		if !ok {
			return nil, fmt.Sprintf("tool_call calls[%d].arguments must be an object", position)
		}
		entries = append(entries, ToolCallEntry{Name: name, Arguments: argMap})
	}
	return entries, ""
}

const toolSearchEchoArgsMaxChars = 1500

// LocalBatchError rejects a multi entry batch naming a local tool while echoing the first entry
func LocalBatchError(entries []ToolCallEntry) string {
	first := entries[0]
	args := jsonCompact(first.Arguments)
	if first.Arguments == nil {
		args = "{}"
	}
	if len(args) > toolSearchEchoArgsMaxChars {
		args = "{...}"
	}
	retry := fmt.Sprintf(`{"calls":[{"name":%s,"arguments":%s}]}`, jsonCompact(first.Name), args)
	remaining := ""
	if len(entries) > 1 {
		remaining = fmt.Sprintf(" then issue the remaining %d call(s) as separate tool_call invocations", len(entries)-1)
	}
	return fmt.Sprintf("tool_call takes exactly one entry for local tools; you sent %d. Retry with only: %s%s. Only connectors__ names may be batched together.",
		len(entries), retry, remaining)
}

// UnavailableInSessionReason explains why a registered tool absent from this session cannot be reached
func UnavailableInSessionReason(name string) string {
	return fmt.Sprintf("'%s' is not available in this execution context. ", name) +
		"Don't retry it — state the assumption and proceed with the work."
}

// SessionUnavailableError reports a core tool missing from the session's own tool list
func SessionUnavailableError(ctx context.Context, env *Env, name string, sessionToolNames map[string]bool) string {
	if sessionToolNames == nil || sessionToolNames[name] {
		return ""
	}
	if !toolSearchCoreToolNames()[name] || newSearchScope(ctx, env).entry(name) == nil {
		return ""
	}
	if reason := OutOfScopeReason(ctx, env, name); reason != "" {
		return reason
	}
	return UnavailableInSessionReason(name)
}

// NotDeferrableError rejects a tool_call naming something that is not a deferred tool
func NotDeferrableError(ctx context.Context, env *Env, name string, sessionToolNames map[string]bool) string {
	s := newSearchScope(ctx, env)
	if toolSearchCoreToolNames()[name] || s.entry(name) != nil {
		if sessionToolNames != nil && !sessionToolNames[name] {
			return UnavailableInSessionReason(name)
		}
		return fmt.Sprintf("'%s' is a directly-listed tool, not a deferred one. Call it directly instead of via tool_call.", name)
	}
	suffix := "__" + name
	var candidates []string
	for _, d := range s.reg.Snapshot(s.ctx, s.scopeKey()) {
		if strings.HasSuffix(d.Name, suffix) {
			candidates = append(candidates, d.Name)
		}
	}
	sort.Strings(candidates)
	hint := " Use tool_search to find the exact name."
	if len(candidates) > 0 {
		quoted := make([]string, len(candidates))
		for i, c := range candidates {
			quoted[i] = "'" + c + "'"
		}
		hint = fmt.Sprintf(" Did you mean %s?", strings.Join(quoted, ", "))
	}
	return fmt.Sprintf("'%s' is not a known tool name. Deferred tools must be invoked through tool_call by the exact name tool_search returns (e.g. mcp__<server>__{tool}).%s", name, hint)
}
