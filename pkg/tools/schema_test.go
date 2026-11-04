package tools

import (
	"encoding/json"
	"reflect"
	"testing"
)

func schemaTool(name string, parameters any) map[string]any {
	return map[string]any{"type": "function", "function": map[string]any{"name": name, "parameters": parameters}}
}

func jsonRound(t *testing.T, src string) map[string]any {
	t.Helper()
	var out map[string]any
	if err := json.Unmarshal([]byte(src), &out); err != nil {
		t.Fatalf("bad json fixture: %v", err)
	}
	return out
}

func sanitizedParams(t *testing.T, tool map[string]any) map[string]any {
	t.Helper()
	out := SanitizeToolSchemas([]map[string]any{tool})
	return out[0]["function"].(map[string]any)["parameters"].(map[string]any)
}

func emptyObjectSchema() map[string]any {
	return map[string]any{"type": "object", "properties": map[string]any{}, "required": []any{}}
}

func TestObjectWithoutPropertiesGetsEmptyProperties(t *testing.T) {
	params := sanitizedParams(t, schemaTool("t", jsonRound(t, `{"type":"object"}`)))
	if !reflect.DeepEqual(params, emptyObjectSchema()) {
		t.Fatalf("got %v", params)
	}
}

func TestNestedObjectWithoutPropertiesGetsEmptyProperties(t *testing.T) {
	params := sanitizedParams(t, schemaTool("t", jsonRound(t, `{"type":"object","properties":{"name":{"type":"string"},"arguments":{"type":"object","description":"free-form"}},"required":["name"]}`)))
	args := params["properties"].(map[string]any)["arguments"].(map[string]any)
	if args["type"] != "object" || len(args["properties"].(map[string]any)) != 0 || args["description"] != "free-form" {
		t.Fatalf("got %v", args)
	}
}

func TestBareStringObjectValueReplacedWithSchemaDict(t *testing.T) {
	params := sanitizedParams(t, schemaTool("t", jsonRound(t, `{"type":"object","properties":{"payload":"object"}}`)))
	payload := params["properties"].(map[string]any)["payload"].(map[string]any)
	if payload["type"] != "object" || len(payload["properties"].(map[string]any)) != 0 {
		t.Fatalf("got %v", payload)
	}
}

func TestNullableTypeArrayCollapsedToSingleString(t *testing.T) {
	params := sanitizedParams(t, schemaTool("t", jsonRound(t, `{"type":"object","properties":{"maybe_name":{"type":["string","null"]}}}`)))
	prop := params["properties"].(map[string]any)["maybe_name"].(map[string]any)
	if prop["type"] != "string" || prop["nullable"] != true {
		t.Fatalf("got %v", prop)
	}
}

func TestMultitypeArrayBecomesAnyOfNoBranchDropped(t *testing.T) {
	params := sanitizedParams(t, schemaTool("t", jsonRound(t, `{"type":"object","properties":{"status":{"type":["number","string"],"description":"status filter"}}}`)))
	prop := params["properties"].(map[string]any)["status"].(map[string]any)
	if _, has := prop["type"]; has {
		t.Fatalf("type should be removed: %v", prop)
	}
	want := []any{map[string]any{"type": "number"}, map[string]any{"type": "string"}}
	if !reflect.DeepEqual(prop["anyOf"], want) || prop["nullable"] != nil || prop["description"] != "status filter" {
		t.Fatalf("got %v", prop)
	}
}

func TestAllNullTypeArrayBecomesNullType(t *testing.T) {
	params := sanitizedParams(t, schemaTool("t", jsonRound(t, `{"type":"object","properties":{"n":{"type":["null"]}}}`)))
	if params["properties"].(map[string]any)["n"].(map[string]any)["type"] != "null" {
		t.Fatalf("got %v", params)
	}
}

func TestSingleElementTypeArrayUnwrapped(t *testing.T) {
	params := sanitizedParams(t, schemaTool("t", jsonRound(t, `{"type":"object","properties":{"s":{"type":["string"]}}}`)))
	prop := params["properties"].(map[string]any)["s"].(map[string]any)
	if prop["type"] != "string" || prop["nullable"] != nil {
		t.Fatalf("got %v", prop)
	}
}

func TestAnyOfNestedObjectsSanitized(t *testing.T) {
	params := sanitizedParams(t, schemaTool("t", jsonRound(t, `{"type":"object","properties":{"opt":{"anyOf":[{"type":"object"},{"type":"string"}]}}}`)))
	variants := params["properties"].(map[string]any)["opt"].(map[string]any)["anyOf"].([]any)
	if !reflect.DeepEqual(variants[0], emptyObjectSchema()) || !reflect.DeepEqual(variants[1], map[string]any{"type": "string"}) {
		t.Fatalf("got %v", variants)
	}
}

func TestMissingParametersGetsDefaultObjectSchema(t *testing.T) {
	out := SanitizeToolSchemas([]map[string]any{{"type": "function", "function": map[string]any{"name": "t"}}})
	if !reflect.DeepEqual(out[0]["function"].(map[string]any)["parameters"], emptyObjectSchema()) {
		t.Fatalf("got %v", out)
	}
	params := sanitizedParams(t, schemaTool("t", "object"))
	if !reflect.DeepEqual(params, emptyObjectSchema()) {
		t.Fatalf("got %v", params)
	}
}

func TestRequiredPrunedToExistingProperties(t *testing.T) {
	params := sanitizedParams(t, schemaTool("t", jsonRound(t, `{"type":"object","properties":{"name":{"type":"string"}},"required":["name","missing_field"]}`)))
	if !reflect.DeepEqual(params["required"], []any{"name"}) {
		t.Fatalf("got %v", params["required"])
	}
}

func TestEmptyRequiredKeySurvivesSanitization(t *testing.T) {
	out := SanitizeToolSchemas([]map[string]any{
		schemaTool("t", jsonRound(t, `{"type":"object","properties":{},"required":[]}`)),
		schemaTool("u", jsonRound(t, `{"type":"object","properties":{"name":{"type":"string"}},"required":["missing_field"]}`)),
	})
	for i := range out {
		if !reflect.DeepEqual(out[i]["function"].(map[string]any)["parameters"].(map[string]any)["required"], []any{}) {
			t.Fatalf("tool %d required not empty list", i)
		}
	}
}

func TestWellFormedSchemaUnchanged(t *testing.T) {
	schema := jsonRound(t, `{"type":"object","properties":{"path":{"type":"string","description":"File path"},"offset":{"type":"integer","minimum":1}},"required":["path"]}`)
	params := sanitizedParams(t, schemaTool("read_file", schemaDeepCopy(schema)))
	if !reflect.DeepEqual(params, schema) {
		t.Fatalf("got %v want %v", params, schema)
	}
}

func TestAdditionalPropertiesAndItemsSanitized(t *testing.T) {
	params := sanitizedParams(t, schemaTool("t", jsonRound(t, `{"type":"object","properties":{"dict_field":{"type":"object","additionalProperties":{"type":"object"}},"bag":{"type":"array","items":{"type":"object"}}}}`)))
	props := params["properties"].(map[string]any)
	if !reflect.DeepEqual(props["dict_field"].(map[string]any)["additionalProperties"], emptyObjectSchema()) {
		t.Fatalf("additionalProperties: %v", props["dict_field"])
	}
	if !reflect.DeepEqual(props["bag"].(map[string]any)["items"], emptyObjectSchema()) {
		t.Fatalf("items: %v", props["bag"])
	}
}

func TestStripResponsesMixedFormats(t *testing.T) {
	tools := []map[string]any{
		jsonRound(t, `{"type":"function","function":{"name":"search","parameters":{"type":"object","properties":{"query":{"type":"string","pattern":"^[a-z]+$"}}}}}`),
		jsonRound(t, `{"name":"get_time","parameters":{"type":"object","properties":{"tz":{"type":"string","format":"date-time"}}},"type":"function"}`),
	}
	result, stripped := StripPatternAndFormat(tools)
	if stripped != 2 {
		t.Fatalf("stripped %d", stripped)
	}
	q := result[0]["function"].(map[string]any)["parameters"].(map[string]any)["properties"].(map[string]any)["query"].(map[string]any)
	if _, has := q["pattern"]; has {
		t.Fatalf("pattern kept: %v", q)
	}
	tz := result[1]["parameters"].(map[string]any)["properties"].(map[string]any)["tz"].(map[string]any)
	if _, has := tz["format"]; has {
		t.Fatalf("format kept: %v", tz)
	}
}

func TestStripSlashEnum(t *testing.T) {
	tools := []map[string]any{schemaTool("t", jsonRound(t, `{"type":"object","properties":{"model":{"type":"string","enum":["Qwen/Qwen3.5-0.8B","b"]},"ok":{"type":"string","enum":["x","y"]}}}`))}
	_, stripped := StripSlashEnum(tools)
	if stripped != 1 {
		t.Fatalf("stripped %d", stripped)
	}
	props := tools[0]["function"].(map[string]any)["parameters"].(map[string]any)["properties"].(map[string]any)
	if _, has := props["model"].(map[string]any)["enum"]; has {
		t.Fatal("slash enum kept")
	}
	if _, has := props["ok"].(map[string]any)["enum"]; !has {
		t.Fatal("plain enum dropped")
	}
}

func TestSanitizePropertyKey(t *testing.T) {
	if SanitizePropertyKey("~~~") != "___" || SanitizePropertyKey("") != "param" {
		t.Fatal("bad fallback")
	}
	params := sanitizedParams(t, schemaTool("t", jsonRound(t, `{"type":"object","properties":{"issue_class~neq":{"type":"string"},"issue_class_neq":{"type":"string"}},"required":["issue_class~neq"]}`)))
	props := params["properties"].(map[string]any)
	if _, has := props["issue_class_neq_2"]; !has {
		t.Fatalf("collision suffix missing: %v", props)
	}
	if !reflect.DeepEqual(params["required"], []any{"issue_class_neq_2"}) {
		t.Fatalf("required not remapped: %v", params["required"])
	}
}

func TestDependentRequiredPreservedAndSchemasSanitized(t *testing.T) {
	schema := jsonRound(t, `{"type":"object","properties":{"owner":{"type":"string"},"repo":{"type":"string"},"organization":{"type":"string"}},"dependentRequired":{"owner":["repo","organization"],"repo":["owner"]},"dependentSchemas":{"owner":{"type":"object"}}}`)
	saved := schemaDeepCopy(schema)
	params := sanitizedParams(t, schemaTool("t", schema))
	dep := params["dependentRequired"].(map[string]any)
	if !reflect.DeepEqual(dep["owner"], []any{"repo", "organization"}) || !reflect.DeepEqual(dep["repo"], []any{"owner"}) {
		t.Fatalf("dependentRequired changed: %v", dep)
	}
	if !reflect.DeepEqual(params["dependentSchemas"].(map[string]any)["owner"], emptyObjectSchema()) {
		t.Fatalf("dependentSchemas not sanitized: %v", params["dependentSchemas"])
	}
	if !reflect.DeepEqual(schema, saved) {
		t.Fatal("input mutated")
	}
}

func TestCollapseConstUnions(t *testing.T) {
	out := CollapseConstUnions(jsonRound(t, `{"anyOf":[{"const":"red"},{"const":"green"},{"const":"blue"}]}`))
	if !reflect.DeepEqual(out, map[string]any{"type": "string", "enum": []any{"red", "green", "blue"}}) {
		t.Fatalf("got %v", out)
	}
	out = CollapseConstUnions(jsonRound(t, `{"oneOf":[{"const":1},{"const":2},{"const":3}]}`))
	if !reflect.DeepEqual(out, map[string]any{"type": "integer", "enum": []any{1.0, 2.0, 3.0}}) {
		t.Fatalf("got %v", out)
	}
	mixed := jsonRound(t, `{"anyOf":[{"const":"a"},{"type":"string","minLength":3}]}`)
	if !reflect.DeepEqual(CollapseConstUnions(schemaDeepCopy(mixed)), mixed) {
		t.Fatal("mixed union changed")
	}
	nonUniform := jsonRound(t, `{"anyOf":[{"const":"a"},{"const":1}]}`)
	if !reflect.DeepEqual(CollapseConstUnions(schemaDeepCopy(nonUniform)), nonUniform) {
		t.Fatal("non uniform union changed")
	}
	boolInt := jsonRound(t, `{"anyOf":[{"const":true},{"const":1}]}`)
	if !reflect.DeepEqual(CollapseConstUnions(schemaDeepCopy(boolInt)), boolInt) {
		t.Fatal("bool merged with integer")
	}
	collapsed := CollapseConstUnions(jsonRound(t, `{"anyOf":[{"const":true},{"const":false}]}`))
	if !reflect.DeepEqual(collapsed, map[string]any{"type": "boolean", "enum": []any{true, false}}) {
		t.Fatalf("got %v", collapsed)
	}
	meta := CollapseConstUnions(jsonRound(t, `{"title":"Color","description":"Pick a color","default":"red","anyOf":[{"const":"red"},{"const":"blue"}]}`))
	if !reflect.DeepEqual(meta, map[string]any{"type": "string", "enum": []any{"red", "blue"}, "title": "Color", "description": "Pick a color", "default": "red"}) {
		t.Fatalf("got %v", meta)
	}
	branchMeta := CollapseConstUnions(jsonRound(t, `{"anyOf":[{"const":"a","title":"A","description":"first"},{"const":"b","type":"string"}]}`))
	if !reflect.DeepEqual(branchMeta, map[string]any{"type": "string", "enum": []any{"a", "b"}}) {
		t.Fatalf("got %v", branchMeta)
	}
	mismatch := jsonRound(t, `{"anyOf":[{"const":"a","type":"integer"},{"const":"b"}]}`)
	if !reflect.DeepEqual(CollapseConstUnions(schemaDeepCopy(mismatch)), mismatch) {
		t.Fatal("mismatched declared type collapsed")
	}
	nested := CollapseConstUnions(jsonRound(t, `{"type":"object","properties":{"mode":{"anyOf":[{"const":"fast"},{"const":"slow"}]},"inner":{"type":"object","properties":{"level":{"oneOf":[{"const":1},{"const":2}]}}}}}`)).(map[string]any)
	if !reflect.DeepEqual(nested["properties"].(map[string]any)["mode"], map[string]any{"type": "string", "enum": []any{"fast", "slow"}}) {
		t.Fatalf("nested mode: %v", nested)
	}
	withNull := CollapseConstUnions(jsonRound(t, `{"anyOf":[{"const":"fast"},{"const":"slow"},{"type":"null"}],"default":null}`)).(map[string]any)
	if withNull["type"] != "string" || withNull["nullable"] != true {
		t.Fatalf("null branch: %v", withNull)
	}
}

func TestStripNullableUnions(t *testing.T) {
	out := StripNullableUnions(jsonRound(t, `{"type":"object","properties":{"x":{"anyOf":[{"type":"string"},{"type":"null"}],"description":"d","default":"a"}}}`), true).(map[string]any)
	x := out["properties"].(map[string]any)["x"].(map[string]any)
	if x["type"] != "string" || x["nullable"] != true || x["description"] != "d" || x["default"] != "a" {
		t.Fatalf("got %v", x)
	}
	refOut := StripNullableUnions(jsonRound(t, `{"anyOf":[{"$ref":"#/$defs/A"},{"type":"null"}],"default":1}`), true).(map[string]any)
	if _, has := refOut["default"]; has {
		t.Fatalf("default kept beside $ref: %v", refOut)
	}
}

func TestBooleanRequiredPreservesParentIntent(t *testing.T) {
	for _, required := range []string{`[]`, `["existing"]`} {
		schema := jsonRound(t, `{"type":"object","required":`+required+`,"properties":{"existing":{"type":"string"},"nested":{"type":"array","items":{"properties":{"url path":{"required":true},"flag":{"type":"boolean","required":false}}}}}}`)
		original := schemaDeepCopy(schema)
		params := sanitizedParams(t, map[string]any{"function": map[string]any{"name": "probe", "parameters": schema}})
		item := params["properties"].(map[string]any)["nested"].(map[string]any)["items"].(map[string]any)
		if !reflect.DeepEqual(item["required"], []any{"url_path"}) {
			t.Fatalf("lifted required: %v", item["required"])
		}
		var want []any
		json.Unmarshal([]byte(required), &want)
		if !reflect.DeepEqual(params["required"], want) {
			t.Fatalf("parent required %v want %v", params["required"], want)
		}
		if !reflect.DeepEqual(schema, original) {
			t.Fatal("input mutated")
		}
	}
}

func TestBooleanRequiredDoesNotModifyLiteralData(t *testing.T) {
	literal := `{"type":"object","required":true,"properties":{"x":{"required":false}}}`
	prop := jsonRound(t, `{"const":`+literal+`,"default":`+literal+`,"example":`+literal+`,"x-metadata":`+literal+`,"enum":[`+literal+`],"examples":[`+literal+`]}`)
	schema := map[string]any{"type": "object", "properties": map[string]any{"value": schemaDeepCopy(prop)}}
	params := sanitizedParams(t, map[string]any{"function": map[string]any{"name": "probe", "parameters": schema}})
	if !reflect.DeepEqual(params["properties"].(map[string]any)["value"], prop) {
		t.Fatalf("literal data changed: %v", params["properties"])
	}
}

func TestTopLevelCombinatorsStripped(t *testing.T) {
	params := sanitizedParams(t, schemaTool("t", jsonRound(t, `{"type":"object","properties":{"a":{"type":"string"}},"anyOf":[{"required":["a"]}],"not":{"required":["b"]}}`)))
	for _, key := range []string{"anyOf", "not"} {
		if _, has := params[key]; has {
			t.Fatalf("%s kept at top level", key)
		}
	}
}

func TestBuiltinToolWithoutRequiredGetsEmptyRequiredList(t *testing.T) {
	params := sanitizedParams(t, schemaTool("t", jsonRound(t, `{"type":"object","properties":{"opts":{"type":"object","properties":{"k":{"type":"string"}}}}}`)))
	if !reflect.DeepEqual(params["required"], []any{}) {
		t.Fatalf("root required: %v", params["required"])
	}
	if !reflect.DeepEqual(params["properties"].(map[string]any)["opts"].(map[string]any)["required"], []any{}) {
		t.Fatalf("nested required missing")
	}
}
