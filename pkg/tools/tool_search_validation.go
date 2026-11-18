package tools

import (
	"context"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyjsonschema"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
)

// Literal keywords hold instance data rather than schemas
var validationSchemaLiteralKeys = map[string]bool{"const": true, "default": true, "enum": true, "example": true, "examples": true}

var validationIdentifierRe = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// JSON Schema compatible copy honoring OpenAPI nullable true, key order kept
func validationSchemaForLocal(node any) any {
	switch t := node.(type) {
	case []any:
		out := make([]any, len(t))
		for i, item := range t {
			out[i] = validationSchemaForLocal(item)
		}
		return out
	case *pyjson.Object:
		normalized := pyjson.NewObject()
		for _, key := range t.Keys {
			if key == "nullable" {
				continue
			}
			if validationSchemaLiteralKeys[key] {
				normalized.Set(key, pyjson.Clone(t.Vals[key]))
			} else {
				normalized.Set(key, validationSchemaForLocal(t.Vals[key]))
			}
		}
		if nullable, ok := t.Vals["nullable"].(bool); !ok || !nullable {
			return normalized
		}
		var types []any
		switch st := normalized.Vals["type"].(type) {
		case string:
			types = []any{st}
		case []any:
			types = st
		}
		if types != nil {
			hasNull := false
			for _, v := range types {
				if pyrepr.Equal(v, "null") {
					hasNull = true
				}
			}
			if !hasNull {
				normalized.Set("type", append(append([]any{}, types...), "null"))
			}
			return normalized
		}
		return pyjson.ObjectOf("anyOf", []any{normalized, pyjson.ObjectOf("type", "null")})
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
	case *pyjson.Object:
		if ref, ok := t.Vals["$ref"].(string); ok && !strings.HasPrefix(ref, "#") {
			return true
		}
		for _, key := range t.Keys {
			if validationSchemaLiteralKeys[key] {
				continue
			}
			if validationSchemaHasExternalRef(t.Vals[key]) {
				return true
			}
		}
		return false
	default:
		return false
	}
}

// Python frames under validate_deferred_call_args in the reference, unwrapped by the tool executor or dispatched through handle_function_call
const (
	deferredValidationFrameDepth = 22
	dispatchValidationFrameDepth = 24
)

// Formats a jsonschema absolute path as a compact argument path
func validationPath(absolutePath []any) string {
	path := "arguments"
	for _, part := range absolutePath {
		switch p := part.(type) {
		case string:
			if validationIdentifierRe.MatchString(p) {
				path += "." + p
			} else {
				path += "[" + pyDumps(p, pyjson.EnsureASCII(false)) + "]"
			}
		case int:
			path += "[" + strconv.Itoa(p) + "]"
		}
	}
	return path
}

func validationError(message, path, constraint string, parameters any) string {
	return toolErrorJSON(message, Dict("path", path, "constraint", constraint, "parameters", parameters,
		"hint", "Retry tool_call with 'arguments' matching the parameters schema above."))
}

// re.sub of \s+ with one space over a message, whitespace as str.isspace reads it
func validationCollapseSpace(s string) string {
	var b strings.Builder
	inSpace := false
	for _, r := range pystr.Runes(s) {
		if pystr.IsSpace(r) {
			if !inSpace {
				b.WriteByte(' ')
			}
			inSpace = true
			continue
		}
		inSpace = false
		b.WriteString(pystr.FromRunes([]rune{r}))
	}
	return b.String()
}

// Arguments a dict holds as an ordered Python object, false for anything that is not a dict
func validationArgsObject(args any) (*pyjson.Object, bool) {
	switch a := args.(type) {
	case *pyjson.Object:
		return a, a != nil
	case map[string]any:
		keys := make([]string, 0, len(a))
		for k := range a {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		o := pyjson.NewObject()
		for _, k := range keys {
			o.Set(k, a[k])
		}
		return o, true
	}
	return nil, false
}

// Copy of the arguments after the schema guided coercion dispatch applies, in the key order coerce_tool_args leaves
func deferredCandidateArgs(ctx context.Context, env *Env, name string, args *pyjson.Object, parametersRaw any) *pyjson.Object {
	vals := make(map[string]any, len(args.Keys))
	for _, k := range args.Keys {
		vals[k] = args.Vals[k]
	}
	coerced := coercePythonToolArgs(ctx, Default, env, name, vals)
	order := args.Keys
	parameters, _ := pyjson.Dict(parametersRaw)
	if properties, _ := pyjson.Dict(parameters["properties"]); len(properties) > 0 {
		if unrenamed, ok := UnrenameToolArgs(parametersRaw, args, nil).(*pyjson.Object); ok {
			order = unrenamed.Keys
		}
	}
	out := pyjson.NewObject()
	for _, k := range order {
		if v, ok := coerced[k]; ok {
			out.Set(k, v)
		}
	}
	return out
}

// Python valued arguments of the entry a tool_call resolves to, nil when the call holds no entry
func DeferredCallArguments(toolCallArgs map[string]any) *pyjson.Object {
	entries, errMsg := NormalizeToolCallEntries(toolCallArgs)
	if errMsg != "" {
		return nil
	}
	return entries[0].Arguments
}

// Checks tool_call arguments, a dict as a map or ordered object, against the deferred tool schema, failing open
func ValidateDeferredCallArgs(ctx context.Context, env *Env, name string, args any) string {
	return validateDeferredCallArgsAt(ctx, env, name, args, deferredValidationFrameDepth)
}

// Deferred argument validation whose validator runs callerDepth Python frames deep, the depth of the reference call site it mirrors
func validateDeferredCallArgsAt(ctx context.Context, env *Env, name string, args any, callerDepth int) string {
	argsObj, isDict := validationArgsObject(args)
	if !isDict {
		return ""
	}
	def := Default.Entry(ctx, env, name)
	if def == nil || def.Schema == nil {
		return ""
	}
	schema, _ := pyjson.Ordered(def.Schema).(*pyjson.Object)
	fn := schema
	if t, _ := schema.Vals["type"].(string); t == "function" {
		fn, _ = schema.Vals["function"].(*pyjson.Object)
	}
	if fn == nil {
		return ""
	}
	params, _ := fn.Vals["parameters"].(*pyjson.Object)
	if params == nil {
		return ""
	}
	var missing []string
	if required, ok := params.Vals["required"].([]any); ok {
		for _, r := range required {
			if s, ok := r.(string); ok {
				if _, has := argsObj.Vals[s]; !has {
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
		pkgLog().Debug("Skipping local deferred-argument validation: external $ref", "tool", name)
		return ""
	}
	candidate := deferredCandidateArgs(ctx, env, name, argsObj, def.Schema.Vals["parameters"])
	best, err := pyjsonschema.BestMatch(candidate, validationSchema, callerDepth)
	if err != nil {
		pkgLog().Debug("validate_deferred_call_args failed", "tool", name, "error", err)
		return ""
	}
	if best == nil {
		return ""
	}
	path := validationPath(best.AbsolutePath)
	constraint := "schema"
	if s, ok := best.Validator.(string); ok && s != "" {
		constraint = s
	}
	detail := pystr.Strip(validationCollapseSpace(best.Message))
	if pystr.Len(detail) > 600 {
		detail = pystr.Slice(detail, 0, 597) + "..."
	}
	return validationError(
		fmt.Sprintf("tool_call to '%s' failed argument validation at %s (%s): %s. The tool was NOT invoked.", name, path, constraint, detail),
		path, constraint, params)
}

// ToolCallEntry is one normalized tool_call batch entry, its arguments the dict Python holds
type ToolCallEntry struct {
	Name      string
	Arguments *pyjson.Object
}

// Arguments of an entry in the DecodeWTF8 shapes tool handlers read
func (e ToolCallEntry) HandlerArgs() map[string]any {
	return pyjson.WTF8Shape(e.Arguments).(map[string]any)
}

// Python str of a value with its falsy forms read as empty and surrounding whitespace stripped
func toolCallEntryName(v any) string {
	if v == nil || !pyrepr.Truthy(v) {
		return ""
	}
	return pystr.Strip(pyrepr.Str(v))
}

// NormalizeToolCallEntries turns tool_call arguments holding Python values into entries, accepting the legacy single shape
func NormalizeToolCallEntries(args map[string]any) ([]ToolCallEntry, string) {
	rawCalls := args["calls"]
	if rawCalls == nil {
		if toolCallEntryName(args["name"]) == "" {
			return nil, "tool_call requires 'calls' (an array of {name, arguments})"
		}
		rawCalls = []any{pyjson.ObjectOf("name", args["name"], "arguments", args["arguments"])}
	}
	if s, ok := rawCalls.(string); ok {
		parsed, err := pyjson.Loads(s)
		if err != nil {
			return nil, fmt.Sprintf("tool_call 'calls' is not valid JSON: %s", err.Error())
		}
		rawCalls = parsed
	}
	if _, isDict := pyjson.AsObject(rawCalls); isDict {
		rawCalls = []any{rawCalls}
	}
	list, ok := rawCalls.([]any)
	if !ok || len(list) == 0 {
		return nil, "tool_call 'calls' must be a non-empty array of {name, arguments}"
	}
	entries := make([]ToolCallEntry, 0, len(list))
	for position, raw := range list {
		m, ok := pyjson.AsObject(raw)
		if !ok {
			return nil, fmt.Sprintf("tool_call calls[%d] must be an object with 'name' and 'arguments'", position)
		}
		name := toolCallEntryName(m.Vals["name"])
		if name == "" {
			return nil, fmt.Sprintf("tool_call calls[%d] requires a 'name'", position)
		}
		if BridgeToolNames[name] {
			return nil, fmt.Sprintf("tool_call cannot invoke '%s' (it is itself a bridge tool)", name)
		}
		rawArgs := m.Vals["arguments"]
		if s, isStr := rawArgs.(string); rawArgs == nil || (isStr && pystr.Strip(s) == "") {
			rawArgs = pyjson.NewObject()
		}
		if s, isStr := rawArgs.(string); isStr {
			parsed, err := pyjson.Loads(s)
			if err != nil {
				return nil, fmt.Sprintf("tool_call calls[%d].arguments is not valid JSON: %s", position, err.Error())
			}
			rawArgs = parsed
		}
		argObj, ok := pyjson.AsObject(rawArgs)
		if !ok {
			return nil, fmt.Sprintf("tool_call calls[%d].arguments must be an object", position)
		}
		entries = append(entries, ToolCallEntry{Name: name, Arguments: argObj})
	}
	return entries, ""
}

const toolSearchEchoArgsMaxChars = 1500

// LocalBatchError rejects a multi entry batch naming a local tool while echoing the first entry
func LocalBatchError(entries []ToolCallEntry) string {
	return localBatchError(entries[0].Name, entries[0].Arguments, len(entries))
}

// Batch rejection restating the retry shape with the first entry's own name and arguments in json.dumps form
func localBatchError(name, arguments any, count int) string {
	args := pyDumps(arguments, pyjson.EnsureASCII(false), pyjson.Separators(",", ":"))
	if pystr.Len(args) > toolSearchEchoArgsMaxChars {
		args = "{...}"
	}
	retry := fmt.Sprintf(`{"calls":[{"name":%s,"arguments":%s}]}`, pyDumps(name, pyjson.EnsureASCII(false)), args)
	remaining := ""
	if count > 1 {
		remaining = fmt.Sprintf(" then issue the remaining %d call(s) as separate tool_call invocations", count-1)
	}
	return fmt.Sprintf("tool_call takes exactly one entry for local tools; you sent %d. Retry with only: %s%s. Only connectors__ names may be batched together.",
		count, retry, remaining)
}

// UnavailableInSessionReason explains why a registered tool absent from this session cannot be reached
func UnavailableInSessionReason(name string) string {
	return fmt.Sprintf("'%s' is not available in this execution context. ", name) +
		"Don't retry it — state the assumption and proceed with the work."
}

// DeferredDirectCallReason answers a direct call of a deferred tool, whose schema the request does not carry
func DeferredDirectCallReason(name string) string {
	return fmt.Sprintf("'%s' is a deferred tool: its schema is not loaded, so it cannot be called directly. The tool was NOT invoked. ", name) +
		fmt.Sprintf(`Load its schema with %s {"names": ["%s"]}, then invoke it with %s {"calls": [{"name": "%s", "arguments": {...}}]}.`,
			ToolDescribeName, name, ToolCallName, name)
}

// DisabledToolReason answers a call of a tool whose toolset is switched off in this session
func DisabledToolReason(name, toolset string) string {
	return fmt.Sprintf("'%s' is disabled in this session: its toolset '%s' is switched off. The tool was NOT invoked. ", name, toolset) +
		"Don't retry it — continue without it, or tell the user the task needs that toolset enabled."
}

// Toolset switching a registered non MCP tool off under the session selection, empty when the selection keeps it
func switchedOffToolset(ctx context.Context, env *Env, def *Def, enabled, disabled []string) string {
	if strings.HasPrefix(def.Toolset, "mcp-") {
		return ""
	}
	s := newSearchScope(ctx, env)
	if selected, _ := s.reg.SelectToolNames(s.ctx, env, enabled, disabled, definitionSelectionContext(s.ctx, env)); selected[def.Name] {
		return ""
	}
	for _, raw := range disabled {
		probe := map[string]bool{def.Name: true}
		s.reg.applyToolsetSelection(s.ctx, env, probe, []string{raw}, true)
		if !probe[def.Name] {
			return raw
		}
	}
	return def.Toolset
}

// UnofferedToolReason explains a call naming a registered tool the request did not offer, empty for an unregistered name
func UnofferedToolReason(ctx context.Context, env *Env, name string, deferred map[string]bool, enabled, disabled []string) string {
	if deferred[name] {
		return DeferredDirectCallReason(name)
	}
	def := newSearchScope(ctx, env).entry(name)
	if def == nil {
		return ""
	}
	if toolset := switchedOffToolset(ctx, env, def, enabled, disabled); toolset != "" {
		return DisabledToolReason(name, toolset)
	}
	if reason := OutOfScopeReason(ctx, env, name); reason != "" {
		return reason
	}
	return UnavailableInSessionReason(name)
}

// SessionScopeBlockReason blocks a tool_call naming a tool outside the session scope, naming a switched off toolset
func SessionScopeBlockReason(ctx context.Context, env *Env, name string, enabled, disabled []string) string {
	if def := newSearchScope(ctx, env).entry(name); def != nil {
		if toolset := switchedOffToolset(ctx, env, def, enabled, disabled); toolset != "" {
			return DisabledToolReason(name, toolset)
		}
	}
	return ScopeBlockReason(ctx, env, name)
}

// RegisteredToolNames lists every tool the registry holds for the session's profile scope
func RegisteredToolNames(ctx context.Context, env *Env) map[string]bool {
	s := newSearchScope(ctx, env)
	out := map[string]bool{}
	for _, d := range s.reg.Snapshot(s.ctx, s.scopeKey()) {
		out[d.Name] = true
	}
	return out
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
