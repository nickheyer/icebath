package provider

import (
	"context"
	"net/http"
	"testing"
)

func TestGeminiSchemaStripsUnknownTopLevelKeys(t *testing.T) {
	cleaned := sanitizeGeminiSchema(gemMap(`{"type": "object", "$schema": "https://json-schema.org/draft/2020-12/schema", "additionalProperties": false, "properties": {"foo": {"type": "string"}}}`))
	gemEq(t, cleaned, `{"type": "object", "properties": {"foo": {"type": "string"}}}`)
}

func TestGeminiSchemaStringifiesIntegerEnumToSatisfyGemini(t *testing.T) {
	cleaned := sanitizeGeminiSchema(gemMap(`{"type": "integer", "enum": [60, 1440, 4320, 10080], "description": "Minutes (60, 1440, 4320, 10080)."}`))
	gemEq(t, cleaned, `{"type": "integer", "enum": ["60", "1440", "4320", "10080"], "description": "Minutes (60, 1440, 4320, 10080)."}`)
}

func TestGeminiSchemaStringifiesNestedIntegerEnumInsideProperties(t *testing.T) {
	cleaned := sanitizeGeminiSchema(gemMap(`{"type": "object", "properties": {"auto_archive_duration": {"type": "integer", "enum": [60, 1440, 4320, 10080], "description": "Thread archive duration in minutes."},
		"status": {"type": "string", "enum": ["active", "archived"]}}}`))
	gemEq(t, gemAt(cleaned, "properties", "auto_archive_duration", "enum"), `["60", "1440", "4320", "10080"]`)
	gemEq(t, gemAt(cleaned, "properties", "auto_archive_duration", "type"), `"integer"`)
	gemEq(t, gemAt(cleaned, "properties", "status", "enum"), `["active", "archived"]`)
}

func TestGeminiSchemaNonDictInputReturnsEmpty(t *testing.T) {
	for _, in := range []any{nil, "not a schema", gemList(`[1, 2, 3]`)} {
		if out := sanitizeGeminiSchema(in); len(out) != 0 {
			t.Fatalf("%v -> %v", in, out)
		}
	}
}

func TestGeminiRequiredPruningInsideArrayItems(t *testing.T) {
	cleaned := sanitizeGeminiSchema(gemMap(`{"type": "object", "properties": {"issue_fields": {"type": "array", "items": {"type": "object", "required": ["field_id", "value"]}}}, "required": ["issue_fields"]}`))
	if _, ok := gemAt(cleaned, "properties", "issue_fields", "items").(map[string]any)["required"]; ok {
		t.Fatal("dangling required kept")
	}
	gemEq(t, cleaned["required"], `["issue_fields"]`)
}

func TestGeminiRequiredPruningValidRequiredUntouched(t *testing.T) {
	cleaned := sanitizeGeminiSchema(gemMap(`{"type": "object", "properties": {"a": {"type": "string"}, "b": {"type": "integer"}}, "required": ["a", "b"]}`))
	gemEq(t, cleaned["required"], `["a", "b"]`)
}

func TestGeminiRequiredPruningInsideAnyOfBranches(t *testing.T) {
	cleaned := sanitizeGeminiSchema(gemMap(`{"anyOf": [{"type": "object", "properties": {"x": {"type": "string"}}, "required": ["x", "ghost"]}, {"type": "object", "required": ["orphan"]}]}`))
	gemEq(t, gemAt(cleaned, "anyOf", 0, "required"), `["x"]`)
	if _, ok := gemAt(cleaned, "anyOf", 1).(map[string]any)["required"]; ok {
		t.Fatal("orphan required kept")
	}
}

func TestGeminiToolParametersEmptyReturnValidObjectSchema(t *testing.T) {
	gemEq(t, sanitizeGeminiToolParameters(map[string]any{}), `{"type": "object", "properties": {}}`)
}

func TestGeminiToolParametersDiscordCreateThreadNoLongerTripsGemini(t *testing.T) {
	cleaned := sanitizeGeminiToolParameters(gemMap(`{"type": "object", "properties": {"action": {"type": "string", "enum": ["create_thread"]},
		"auto_archive_duration": {"type": "integer", "enum": [60, 1440, 4320, 10080], "description": "Thread archive duration in minutes (create_thread, default 1440)."}},
		"required": ["action"]}`))
	gemEq(t, gemAt(cleaned, "properties", "auto_archive_duration"), `{"type": "integer", "enum": ["60", "1440", "4320", "10080"], "description": "Thread archive duration in minutes (create_thread, default 1440)."}`)
	gemEq(t, gemAt(cleaned, "properties", "action", "enum"), `["create_thread"]`)
}

func TestGeminiPrepareFullJSONSchemaSurvivesAndInputIsNotMutated(t *testing.T) {
	text := `{"$schema": "https://json-schema.org/draft/2020-12/schema", "type": "object", "properties": {
		"globs": {"anyOf": [{"type": "string"}, {"type": "array", "items": {"type": "string"}}]}, "bare_array": {"type": "array"},
		"opts": {"type": "object", "additionalProperties": {"type": "string"}}}, "required": ["globs"]}`
	params := gemMap(text)
	out := prepareGeminiToolParameters(params)
	gemEq(t, params, text)
	if _, ok := out["$schema"]; ok {
		t.Fatal("$schema kept")
	}
	gemEq(t, gemAt(out, "properties", "globs", "anyOf", 1), `{"type": "array", "items": {"type": "string"}}`)
	gemEq(t, gemAt(out, "properties", "bare_array"), `{"type": "array"}`)
	gemEq(t, gemAt(out, "properties", "opts", "additionalProperties"), `{"type": "string"}`)
	gemEq(t, prepareGeminiToolParameters(map[string]any{}), `{"type": "object", "properties": {}}`)
}

func TestGeminiPrepareLocalRefsInlinedWithSiblingOverrideAndDefsDropped(t *testing.T) {
	out := prepareGeminiToolParameters(gemMap(`{"type": "object", "properties": {"user": {"$ref": "#/$defs/User", "description": "who"}},
		"$defs": {"User": {"type": "object", "description": "generic", "properties": {"tags": {"type": "array", "items": {"$ref": "#/$defs/Tag"}}}}, "Tag": {"type": "string"}}}`))
	if _, ok := out["$defs"]; ok {
		t.Fatal("$defs kept")
	}
	gemEq(t, gemAt(out, "properties", "user", "description"), `"who"`)
	gemEq(t, gemAt(out, "properties", "user", "properties", "tags", "items"), `{"type": "string"}`)
}

func TestGeminiPrepareUnresolvableOrCircularRefPassesSchemaThroughUntouched(t *testing.T) {
	for _, defs := range []string{`{"Loop": {"type": "object", "properties": {"next": {"$ref": "#/$defs/Loop"}}}}`, `{}`} {
		out := prepareGeminiToolParameters(gemMap(`{"type": "object", "properties": {"p": {"$ref": "#/$defs/Loop"}}, "$defs": ` + defs + `}`))
		gemEq(t, gemAt(out, "properties", "p"), `{"$ref": "#/$defs/Loop"}`)
		gemEq(t, out["$defs"], defs)
	}
}

func TestGeminiV1betaSendsParametersJSONSchemaOtherVersionsSendLegacySubset(t *testing.T) {
	tools := []map[string]any{gemMap(`{"type": "function", "function": {"name": "t", "parameters": {"type": "object", "properties": {"g": {"anyOf": [{"type": "string"}, {"type": "array", "items": {"type": "string"}}]}}}}}`)}
	decl := func(baseURL string) map[string]any {
		f := newGemFake(t, func(w http.ResponseWriter, r *http.Request, n int) { gemWriteJSON(w, 200, `{"candidates": []}`) })
		c := gemClient(t, f, baseURL, "k", "gemini-2.5-flash")
		req := gemUserRequest("gemini-2.5-flash", "hi")
		req.Tools = tools
		if _, err := c.Complete(context.Background(), req); err != nil {
			t.Fatal(err)
		}
		return gemAt(f.all()[0].Body, "tools", 0, "functionDeclarations", 0).(map[string]any)
	}
	beta := decl("https://generativelanguage.googleapis.com/v1beta")
	if _, ok := beta["parameters"]; ok {
		t.Fatal("v1beta sent legacy parameters")
	}
	gemEq(t, gemAt(beta, "parametersJsonSchema", "properties", "g", "anyOf", 1, "items"), `{"type": "string"}`)
	v1 := decl("https://generativelanguage.googleapis.com/v1")
	if _, ok := v1["parametersJsonSchema"]; ok {
		t.Fatal("v1 sent parametersJsonSchema")
	}
	if branches, _ := gemAt(v1, "parameters", "properties", "g", "anyOf").([]any); len(branches) == 0 {
		t.Fatalf("legacy translator not applied: %v", v1)
	}
}

func TestGeminiNativeTypeArraysPreserveAlternatives(t *testing.T) {
	typeCases := []any{gemList(`["string", "null"]`), gemList(`["integer", "null"]`), gemList(`["string", "integer"]`), gemList(`["string", "integer", "boolean", "null"]`),
		gemList(`["array", "object", "string"]`), gemList(`["null"]`), gemList(`[]`), "string"}
	for _, types := range typeCases {
		for _, location := range []string{"properties", "items", "anyOf"} {
			for _, constrained := range []bool{false, true} {
				node := map[string]any{"type": types, "nullable": false, "description": "Keep this guidance"}
				list, isList := types.([]any)
				has := func(want string) bool {
					for _, x := range list {
						if x == want {
							return true
						}
					}
					return false
				}
				if has("array") {
					node["items"], node["minItems"] = map[string]any{"type": "integer"}, 1
				}
				if has("object") {
					node["properties"], node["required"] = map[string]any{"name": map[string]any{"type": "string"}}, []any{"name", "undefined"}
				}
				if constrained {
					node["anyOf"] = gemList(`[{"enum": ["one"]}, {"enum": ["two"]}]`)
				}
				var wrapper map[string]any
				switch location {
				case "properties":
					wrapper = map[string]any{"properties": map[string]any{"value": node}}
				case "items":
					wrapper = map[string]any{"items": node}
				default:
					wrapper = map[string]any{"anyOf": []any{node}}
				}
				params := map[string]any{"type": "object", "properties": map[string]any{"nested": wrapper}}
				original := gemCanon(t, params)
				tools := []any{map[string]any{"type": "function", "function": map[string]any{"name": "probe", "parameters": params}}}
				out := gemAt(translateToolsToGemini(tools, false), 0, "functionDeclarations", 0, "parameters", "properties", "nested", location)
				switch location {
				case "properties":
					out = gemAt(out, "value")
				case "anyOf":
					out = gemAt(out, 0)
				}
				om := out.(map[string]any)
				expected := map[string]bool{}
				if isList {
					for _, x := range list {
						if x != "null" {
							expected[x.(string)] = true
						}
					}
				} else {
					expected[types.(string)] = true
				}
				if len(expected) == 0 {
					if has("null") {
						expected["null"] = true
					} else {
						expected["object"] = true
					}
				}
				var branches []any
				if _, typed := om["type"]; typed {
					branches = []any{om}
				} else {
					branches = om["anyOf"].([]any)
				}
				actual := map[string]bool{}
				for _, raw := range branches {
					b := raw.(map[string]any)
					bt, ok := b["type"].(string)
					if !ok {
						t.Fatalf("%v/%s: branch type %v", types, location, b["type"])
					}
					actual[bt] = true
					if bt == "array" {
						gemEq(t, b["items"], `{"type": "integer"}`)
						gemEq(t, b["minItems"], `1`)
					}
					if bt == "object" && has("object") {
						gemEq(t, b["properties"], `{"name": {"type": "string"}}`)
						gemEq(t, b["required"], `["name"]`)
					}
					if _, ok := b["items"]; ok && has("array") && bt != "array" {
						t.Fatalf("%v: items leaked onto %s branch", types, bt)
					}
					if constrained {
						gemEq(t, b["anyOf"], `[{"enum": ["one"]}, {"enum": ["two"]}]`)
					}
				}
				gemEq(t, actual, gemCanon(t, expected))
				if isList && has("null") && len(expected) > 0 && !(len(expected) == 1 && expected["null"]) && om["nullable"] != true {
					t.Fatalf("%v: nullable %v", types, om["nullable"])
				}
				if om["description"] != "Keep this guidance" {
					t.Fatalf("%v: description %v", types, om["description"])
				}
				if gemCanon(t, params) != original {
					t.Fatalf("%v: input mutated", types)
				}
			}
		}
	}
}

func TestGeminiNativeTypeArrayEnumsRemainWireStrings(t *testing.T) {
	for _, tc := range [][3]string{{`["integer", "null"]`, `[1, 2]`, `["1", "2"]`}, {`["string", "integer"]`, `["one", 2]`, `["one", "2"]`}, {`["boolean", "string"]`, `[true, "other"]`, `["true", "other"]`}} {
		tools := []any{gemMap(`{"type": "function", "function": {"name": "probe", "parameters": {"type": "object", "properties": {"value": {"type": ` + tc[0] + `, "enum": ` + tc[1] + `}}}}}`)}
		gemEq(t, gemAt(translateToolsToGemini(tools, false), 0, "functionDeclarations", 0, "parameters", "properties", "value", "enum"), tc[2])
	}
}
