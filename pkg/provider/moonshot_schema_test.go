package provider

import (
	"reflect"
	"testing"
)

func moonshotProps(t *testing.T, out map[string]any, names ...string) map[string]any {
	t.Helper()
	node := out
	for _, name := range names {
		props, ok := node["properties"].(map[string]any)
		if !ok {
			t.Fatalf("no properties under %v", node)
		}
		node, ok = props[name].(map[string]any)
		if !ok {
			t.Fatalf("property %s missing in %v", name, props)
		}
	}
	return node
}

func TestMoonshotModelDetectionPositiveMatches(t *testing.T) {
	for _, model := range []string{"kimi-k2.6", "kimi-k2-thinking", "k3", "K3", "moonshotai/k3", "k3.1-preview", "moonshotai/Kimi-K2.6", "moonshotai/kimi-k2.6",
		"nous/moonshotai/kimi-k2.6", "openrouter/moonshotai/kimi-k2-thinking", "MOONSHOTAI/KIMI-K2.6"} {
		if !isMoonshotModel(model) {
			t.Errorf("%q not detected", model)
		}
	}
}

func TestMoonshotModelDetectionNegativeMatches(t *testing.T) {
	for _, model := range []string{"", "anthropic/claude-sonnet-4.6", "openai/gpt-5.4", "google/gemini-3-flash-preview", "deepseek-chat"} {
		if isMoonshotModel(model) {
			t.Errorf("%q detected", model)
		}
	}
}

func TestMissingTypeFilledNestedPropertiesAreRepaired(t *testing.T) {
	out := sanitizeMoonshotToolParameters(map[string]any{"type": "object", "properties": map[string]any{
		"filter": map[string]any{"type": "object", "properties": map[string]any{"field": map[string]any{"description": "no type"}}}}})
	if got := moonshotProps(t, out, "filter", "field")["type"]; got != "string" {
		t.Fatalf("nested type %v", got)
	}
}

func TestMissingTypeFilledRefNodeIsNotGivenSyntheticType(t *testing.T) {
	out := sanitizeMoonshotToolParameters(map[string]any{"type": "object", "properties": map[string]any{"payload": map[string]any{"$ref": "#/$defs/Payload"}},
		"$defs": map[string]any{"Payload": map[string]any{"type": "object", "properties": map[string]any{}}}})
	payload := moonshotProps(t, out, "payload")
	if _, has := payload["type"]; has || payload["$ref"] != "#/$defs/Payload" {
		t.Fatalf("ref node %v", payload)
	}
}

func TestConditionalSchemaNotGivenSyntheticTypeIfThenNodeIsNotGivenSyntheticType(t *testing.T) {
	out := sanitizeMoonshotToolParameters(map[string]any{
		"oneOf": []any{map[string]any{"required": []any{"prompt"}, "not": map[string]any{"required": []any{"goal"}}},
			map[string]any{"required": []any{"goal"}, "not": map[string]any{"required": []any{"prompt"}}}},
		"allOf": []any{map[string]any{"if": map[string]any{"required": []any{"goal"}},
			"then": map[string]any{"properties": map[string]any{"mode": map[string]any{"enum": []any{"build", "edit"}}}}}}})
	conditional := out["allOf"].([]any)[0].(map[string]any)
	if _, has := conditional["type"]; has {
		t.Fatalf("conditional typed %v", conditional)
	}
	if got := moonshotProps(t, conditional["then"].(map[string]any), "mode")["type"]; got != "string" {
		t.Fatalf("then branch mode type %v", got)
	}
}

func TestAnyOfParentTypeAnyofNullBranchCollapsedToSingleType(t *testing.T) {
	out := sanitizeMoonshotToolParameters(map[string]any{"type": "object", "properties": map[string]any{
		"from_format": map[string]any{"type": "string", "anyOf": []any{map[string]any{"type": "string"}, map[string]any{"type": "null"}}}}})
	fromFormat := moonshotProps(t, out, "from_format")
	if _, has := fromFormat["anyOf"]; has || fromFormat["type"] != "string" {
		t.Fatalf("from_format %v", fromFormat)
	}
}

func TestAnyOfParentTypeAnyofMultipleNonNullPreserved(t *testing.T) {
	out := sanitizeMoonshotToolParameters(map[string]any{"type": "object", "properties": map[string]any{
		"mode": map[string]any{"anyOf": []any{map[string]any{"type": "string"}, map[string]any{"type": "integer"}}}}})
	mode := moonshotProps(t, out, "mode")
	_, hasAnyOf := mode["anyOf"]
	_, hasType := mode["type"]
	if !hasAnyOf || hasType {
		t.Fatalf("mode %v", mode)
	}
}

func TestAnyOfParentTypeAnyofEnumWithNullCollapsed(t *testing.T) {
	out := sanitizeMoonshotToolParameters(map[string]any{"type": "object", "properties": map[string]any{
		"db_type": map[string]any{"anyOf": []any{map[string]any{"enum": []any{"mysql", "postgresql", ""}}, map[string]any{"type": "null"}}}}})
	dbType := moonshotProps(t, out, "db_type")
	if _, has := dbType["anyOf"]; has || dbType["type"] != "string" || !reflect.DeepEqual(dbType["enum"], []any{"mysql", "postgresql"}) {
		t.Fatalf("db_type %v", dbType)
	}
}

func TestTopLevelGuaranteesNonDictInputReturnsEmptyObject(t *testing.T) {
	empty := map[string]any{"type": "object", "properties": map[string]any{}, "required": []any{}}
	for _, in := range []any{nil, "garbage", []any{}} {
		if got := sanitizeMoonshotToolParameters(in); !reflect.DeepEqual(got, empty) {
			t.Fatalf("%v -> %v", in, got)
		}
	}
}

func TestTopLevelGuaranteesDoesNotMutateInput(t *testing.T) {
	q := map[string]any{"description": "no type"}
	params := map[string]any{"type": "object", "properties": map[string]any{"q": q}}
	sanitizeMoonshotToolParameters(params)
	if params["type"] != "object" {
		t.Fatalf("type mutated %v", params)
	}
	if _, has := q["type"]; has {
		t.Fatalf("input property mutated %v", q)
	}
}

func TestRequiredArrayDanglingRequiredPruned(t *testing.T) {
	out := sanitizeMoonshotToolParameters(map[string]any{"type": "object", "properties": map[string]any{"q": map[string]any{"type": "string"}}, "required": []any{"q", "ghost"}})
	if !reflect.DeepEqual(out["required"], []any{"q"}) {
		t.Fatalf("required %v", out["required"])
	}
}

func TestRequiredArrayNestedObjectPropertyGetsRequired(t *testing.T) {
	out := sanitizeMoonshotToolParameters(map[string]any{"type": "object", "properties": map[string]any{"filter": map[string]any{"type": "object", "properties": map[string]any{}}}})
	if !reflect.DeepEqual(moonshotProps(t, out, "filter")["required"], []any{}) || !reflect.DeepEqual(out["required"], []any{}) {
		t.Fatalf("required arrays %v", out)
	}
}

func TestRequiredArrayCoercedTopLevelGetsRequired(t *testing.T) {
	out := sanitizeMoonshotToolParameters(map[string]any{"type": "string"})
	if out["type"] != "object" || !reflect.DeepEqual(out["required"], []any{}) {
		t.Fatalf("coerced top level %v", out)
	}
}

func TestToolListSanitizerAppliesPerTool(t *testing.T) {
	tools := []map[string]any{
		{"type": "function", "function": map[string]any{"name": "search", "description": "Search",
			"parameters": map[string]any{"type": "object", "properties": map[string]any{"q": map[string]any{"description": "query"}}}}},
		{"type": "function", "function": map[string]any{"name": "noop", "description": "Does nothing",
			"parameters": map[string]any{"type": "object", "properties": map[string]any{}}}},
	}
	out := sanitizeMoonshotTools(tools)
	first := out[0]["function"].(map[string]any)["parameters"].(map[string]any)
	if got := moonshotProps(t, first, "q")["type"]; got != "string" {
		t.Fatalf("first tool q type %v", got)
	}
	second := out[1]["function"].(map[string]any)["parameters"]
	if !reflect.DeepEqual(second, map[string]any{"type": "object", "properties": map[string]any{}, "required": []any{}}) {
		t.Fatalf("second tool parameters %v", second)
	}
}

func TestToolListSanitizerEmptyListIsPassthrough(t *testing.T) {
	if got := sanitizeMoonshotTools([]map[string]any{}); len(got) != 0 {
		t.Fatalf("empty list %v", got)
	}
	if got := sanitizeMoonshotTools(nil); got != nil {
		t.Fatalf("nil list %v", got)
	}
}

func TestToolListSanitizerSkipsMalformedEntries(t *testing.T) {
	tools := []map[string]any{{"type": "function"}, {"not": "a tool"}}
	out := sanitizeMoonshotTools(tools)
	if !reflect.DeepEqual(out, tools) || &out[0] != &tools[0] {
		t.Fatalf("malformed entries rewritten %v", out)
	}
}

func TestRealWorldMCPShapeCombinedRewrites(t *testing.T) {
	out := sanitizeMoonshotToolParameters(map[string]any{"type": "object", "properties": map[string]any{
		"query":  map[string]any{"description": "search text"},
		"filter": map[string]any{"type": "string", "anyOf": []any{map[string]any{"type": "string"}, map[string]any{"type": "null"}}},
		"tags":   map[string]any{"type": "array", "items": map[string]any{"description": "tag"}}},
		"required": []any{"query"}})
	if moonshotProps(t, out, "query")["type"] != "string" {
		t.Fatalf("query %v", out)
	}
	filter := moonshotProps(t, out, "filter")
	if _, has := filter["anyOf"]; has || filter["type"] != "string" {
		t.Fatalf("filter %v", filter)
	}
	if moonshotProps(t, out, "tags")["items"].(map[string]any)["type"] != "string" {
		t.Fatalf("tags items %v", out)
	}
	if !reflect.DeepEqual(out["required"], []any{"query"}) {
		t.Fatalf("required %v", out["required"])
	}
}

func TestEnumNullStrippingDataslayerDbTypeAfterMcpNormalize(t *testing.T) {
	out := sanitizeMoonshotToolParameters(map[string]any{"type": "object", "properties": map[string]any{
		"datasource": map[string]any{"type": "string"},
		"db_type":    map[string]any{"enum": []any{"mysql", "mariadb", "postgresql", "sqlserver", "oracle", "", nil}, "type": "string", "nullable": true, "default": nil}},
		"required": []any{"datasource"}})
	dbType := moonshotProps(t, out, "db_type")
	if _, has := dbType["nullable"]; has {
		t.Fatalf("nullable keyword kept %v", dbType)
	}
	if !reflect.DeepEqual(dbType["enum"], []any{"mysql", "mariadb", "postgresql", "sqlserver", "oracle"}) || dbType["type"] != "string" {
		t.Fatalf("db_type %v", dbType)
	}
}

func TestUnionTypeListUnionTypeListNormalizesToFirstConcreteType(t *testing.T) {
	out := sanitizeMoonshotToolParameters(map[string]any{"type": "object", "properties": map[string]any{"limit": map[string]any{"type": []any{"number", "string"}, "description": "Max results"}}})
	if got := moonshotProps(t, out, "limit")["type"]; got != "number" {
		t.Fatalf("limit type %v", got)
	}
}

func TestUnionTypeListUnionTypeListSkipsNullType(t *testing.T) {
	out := sanitizeMoonshotToolParameters(map[string]any{"type": "object", "properties": map[string]any{"name": map[string]any{"type": []any{"null", "string"}}}})
	if got := moonshotProps(t, out, "name")["type"]; got != "string" {
		t.Fatalf("name type %v", got)
	}
}

func TestUnionTypeListUnionTypeListWithEnumDoesNotCrashOrMutateInput(t *testing.T) {
	sort := map[string]any{"type": []any{"string", "null"}, "enum": []any{"asc", "desc", nil, ""}}
	params := map[string]any{"type": "object", "properties": map[string]any{"sort": sort}}
	out := sanitizeMoonshotToolParameters(params)
	got := moonshotProps(t, out, "sort")
	if got["type"] != "string" || !reflect.DeepEqual(got["enum"], []any{"asc", "desc"}) {
		t.Fatalf("sort %v", got)
	}
	if !reflect.DeepEqual(sort["type"], []any{"string", "null"}) {
		t.Fatalf("input mutated %v", sort)
	}
}
