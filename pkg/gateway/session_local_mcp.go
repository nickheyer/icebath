package gateway

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strconv"
	"strings"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/state"
)

// Private secret path the editor MCP server list is held under in owner memory
var editorMCPPrivateKey = secretPath{"editor_mcp"}

// Sorted compact JSON every editor MCP fingerprint and size bound reads
func editorMCPJSON(value any) (string, error) {
	return pyjson.Dumps(value, pyjson.SortKeys(true), pyjson.Separators(",", ":"))
}

// Hex sha256 of the sorted compact JSON of a server list
func editorMCPFingerprint(servers any) (string, error) {
	text, err := editorMCPJSON(servers)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256([]byte(text))
	return hex.EncodeToString(sum[:]), nil
}

// One pydantic validation error, its location path and error type
type acpFieldError struct {
	loc []any
	typ string
}

// Collects pydantic model validation errors of the ACP McpServer and header or env models
type acpValidator struct{ errs []acpFieldError }

// Location extended by one key or index
func acpLoc(loc []any, part any) []any { return append(append([]any(nil), loc...), part) }

// Validates the optional _meta mapping a populate by name BaseModel reads by alias first
func (v *acpValidator) meta(fields map[string]any, loc []any) {
	for _, key := range []string{"_meta", "field_meta"} {
		value, has := fields[key]
		if !has {
			continue
		}
		if _, ok := value.(map[string]any); value != nil && !ok {
			v.errs = append(v.errs, acpFieldError{acpLoc(loc, key), "dict_type"})
		}
		return
	}
}

// Validates a required str field, missing or string_type on failure
func (v *acpValidator) str(fields map[string]any, key string, loc []any) (string, bool) {
	value, has := fields[key]
	if !has {
		v.errs = append(v.errs, acpFieldError{acpLoc(loc, key), "missing"})
		return "", false
	}
	text, ok := value.(string)
	if !ok {
		v.errs = append(v.errs, acpFieldError{acpLoc(loc, key), "string_type"})
	}
	return text, ok
}

// Validates a required list field, each item checked by item at its index
func (v *acpValidator) list(fields map[string]any, key string, loc []any, item func(value any, loc []any)) {
	value, has := fields[key]
	if !has {
		v.errs = append(v.errs, acpFieldError{acpLoc(loc, key), "missing"})
		return
	}
	items, ok := value.([]any)
	if !ok {
		v.errs = append(v.errs, acpFieldError{acpLoc(loc, key), "list_type"})
		return
	}
	for i, entry := range items {
		item(entry, acpLoc(acpLoc(loc, key), i))
	}
}

// Validates a str list item
func (v *acpValidator) strItem(value any, loc []any) {
	if _, ok := value.(string); !ok {
		v.errs = append(v.errs, acpFieldError{loc, "string_type"})
	}
}

// Validates an EnvVariable or HttpHeader item, both a _meta, name and value model
func (v *acpValidator) pairItem(value any, loc []any) {
	fields, ok := value.(map[string]any)
	if !ok {
		v.errs = append(v.errs, acpFieldError{loc, "model_type"})
		return
	}
	v.meta(fields, loc)
	v.str(fields, "name", loc)
	v.str(fields, "value", loc)
}

// Validates spec as McpServerStdio or McpServerHttp and McpServerSse, returning its name when valid
func (v *acpValidator) server(kind string, fields map[string]any) (string, bool) {
	v.meta(fields, nil)
	var name string
	if kind == "stdio" {
		v.list(fields, "args", nil, v.strItem)
		v.str(fields, "command", nil)
		v.list(fields, "env", nil, v.pairItem)
		name, _ = v.str(fields, "name", nil)
	} else {
		v.list(fields, "headers", nil, v.pairItem)
		name, _ = v.str(fields, "name", nil)
		v.str(fields, "url", nil)
	}
	return name, len(v.errs) == 0
}

// Joined loc and type of every validation error, never an input value
func (v *acpValidator) why() string {
	parts := make([]string, len(v.errs))
	for i, e := range v.errs {
		loc := make([]string, len(e.loc))
		for j, part := range e.loc {
			if n, ok := part.(int); ok {
				loc[j] = strconv.Itoa(n)
			} else {
				loc[j] = fmt.Sprint(part)
			}
		}
		parts[i] = strings.Join(loc, ".") + ": " + e.typ
	}
	return strings.Join(parts, "; ")
}

// Refuses a non list, oversized or malformed server list or one with a duplicate or empty name
func validateServers(servers any) error {
	servers = pyjson.Plain(servers)
	list, ok := servers.([]any)
	if !ok {
		return invalidParams("editor.mcp_servers", "must be a list, got "+pyrepr.TypeName(servers))
	}
	if len(list) > 16 {
		return invalidParams("editor.mcp_servers", fmt.Sprintf("at most 16 servers, got %d", len(list)))
	}
	text, err := editorMCPJSON(list)
	if err != nil {
		return err
	}
	if len(text) > 65536 {
		return invalidParams("editor.mcp_servers", "the serialized server list exceeds 65536 bytes")
	}
	names := map[string]bool{}
	for _, spec := range list {
		why := serverSpecProblem(spec, names)
		if why != "" {
			return invalidParams("editor.mcp_servers", "unusable server spec: "+why)
		}
	}
	return nil
}

// Problem of one server spec as validate_servers names it, empty when the spec is usable
func serverSpecProblem(spec any, names map[string]bool) string {
	fields, ok := spec.(map[string]any)
	if !ok {
		return "AttributeError"
	}
	kind, has := fields["type"]
	if !has {
		kind = "stdio"
	}
	switch k := kind.(type) {
	case []any, map[string]any:
		return "TypeError"
	case string:
		if k != "stdio" && k != "http" && k != "sse" {
			return "KeyError"
		}
		v := &acpValidator{}
		name, valid := v.server(k, fields)
		if !valid {
			return v.why()
		}
		if name == "" || names[name] {
			return "duplicate server"
		}
		names[name] = true
		return ""
	default:
		return "KeyError"
	}
}

// Moves a launch request's editor servers into private memory, leaving only their fingerprint in the request
func privateEditorRequest(request map[string]any, private *RecoveredSecrets) error {
	editor := map[string]any{}
	if raw := pyjson.Plain(request["editor"]); pyrepr.Truthy(raw) {
		m, ok := raw.(map[string]any)
		if !ok {
			return fmt.Errorf("AttributeError: '%s' object has no attribute 'get'", pyrepr.TypeName(raw))
		}
		editor = m
	}
	servers := editor["mcp_servers"]
	if !pyrepr.Truthy(servers) {
		return nil
	}
	if private == nil {
		return state.NewRuntimeStoreError("launch_credentials_unavailable", nil, nil)
	}
	copied, err := editorMCPRoundTrip(servers)
	if err != nil {
		return err
	}
	digest, err := editorMCPFingerprint(servers)
	if err != nil {
		return err
	}
	private.Set(editorMCPPrivateKey, copied)
	replaced := make(map[string]any, len(editor)+1)
	for k, v := range editor {
		replaced[k] = v
	}
	replaced["mcp_servers"] = map[string]any{"fingerprint": digest}
	request["editor"] = replaced
	return nil
}

// Deep copy of a server list through its sorted compact JSON
func editorMCPRoundTrip(servers any) (any, error) {
	text, err := editorMCPJSON(servers)
	if err != nil {
		return nil, err
	}
	loaded, err := pyjson.Loads(text)
	if err != nil {
		return nil, err
	}
	return pyjson.Plain(loaded), nil
}

// Store payload and owner held servers of an editor_mcp or server naming branch mutation
func privateMutationPayload(operation string, payload any) (any, any, error) {
	fields, isMap := pyjson.Plain(payload).(map[string]any)
	if operation == "editor_mcp" {
		if _, has := fields["mcp_servers"]; !isMap || len(fields) != 1 || !has {
			return nil, nil, state.NewRuntimeStoreError("invalid_params", nil, nil)
		}
	} else if _, has := fields["mcp_servers"]; operation != "branch" || !isMap || !has {
		return payload, nil, nil
	}
	if err := validateServers(fields["mcp_servers"]); err != nil {
		return nil, nil, err
	}
	servers, err := editorMCPRoundTrip(fields["mcp_servers"])
	if err != nil {
		return nil, nil, err
	}
	digest, err := editorMCPFingerprint(servers)
	if err != nil {
		return nil, nil, err
	}
	if object, ok := payload.(*pyjson.Object); ok {
		stored := object.Copy()
		stored.Set("mcp_servers", pyjson.ObjectOf("fingerprint", digest))
		return stored, servers, nil
	}
	stored := make(map[string]any, len(fields))
	for k, v := range fields {
		stored[k] = v
	}
	stored["mcp_servers"] = map[string]any{"fingerprint": digest}
	return stored, servers, nil
}
