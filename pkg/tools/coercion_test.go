package tools

import (
	"context"
	"reflect"
	"testing"
)

func coercionRegistry(t *testing.T, parameters string) (*Registry, *Env) {
	t.Helper()
	reg := NewRegistry()
	reg.Register(Def{Name: "test_tool", Toolset: "test", Schema: map[string]any{"name": "test_tool", "description": "test", "parameters": jsonRound(t, parameters)},
		Handler: func(ctx context.Context, call *Call) string { return "{}" }})
	return reg, &Env{}
}

func TestCoerceNumber(t *testing.T) {
	v, changed := coerceNumber("42", false)
	if !changed || v != int64(42) {
		t.Fatalf("got %v %v", v, changed)
	}
	v, _ = coerceNumber("-7", false)
	if v != int64(-7) {
		t.Fatalf("got %v", v)
	}
	v, changed = coerceNumber("3.14", true)
	if changed || v != "3.14" {
		t.Fatalf("integer only accepted float: %v", v)
	}
	v, changed = coerceNumber("3.14", false)
	if !changed || v != 3.14 {
		t.Fatalf("got %v", v)
	}
	if _, changed := coerceNumber("inf", false); changed {
		t.Fatal("inf accepted")
	}
	if _, changed := coerceNumber("nan", false); changed {
		t.Fatal("nan accepted")
	}
	if _, changed := coerceNumber("0x10", false); changed {
		t.Fatal("hex accepted")
	}
}

func TestCoerceBoolean(t *testing.T) {
	v, changed := coerceBoolean("true")
	if !changed || v != true {
		t.Fatalf("got %v", v)
	}
	if v, changed := coerceBoolean("1"); changed || v != "1" {
		t.Fatalf("'1' coerced: %v", v)
	}
	if v, changed := coerceBoolean("0"); changed || v != "0" {
		t.Fatalf("'0' coerced: %v", v)
	}
}

func TestCoerceValue(t *testing.T) {
	v, _ := coerceValue("5", "integer", nil)
	if v != int64(5) {
		t.Fatalf("got %v", v)
	}
	v, _ = coerceValue(`["a", "b"]`, "array", nil)
	if !reflect.DeepEqual(v, []any{"a", "b"}) {
		t.Fatalf("got %v", v)
	}
	v, _ = coerceValue("[1, 2, 3]", "array", nil)
	if !reflect.DeepEqual(v, []any{1.0, 2.0, 3.0}) {
		t.Fatalf("got %v", v)
	}
	v, changed := coerceValue("null", "string", map[string]any{"type": "string", "nullable": true})
	if !changed || v != nil {
		t.Fatalf("nullable null: %v", v)
	}
	v, _ = coerceValue("7", []any{"integer", "null"}, nil)
	if v != int64(7) {
		t.Fatalf("union: %v", v)
	}
}

func TestCoerceToolArgsIntegration(t *testing.T) {
	reg, env := coercionRegistry(t, `{"type":"object","properties":{"limit":{"type":"integer"}}}`)
	ctx := context.Background()
	result := CoerceToolArgs(ctx, reg, env, "test_tool", map[string]any{"limit": "10"})
	if result["limit"] != int64(10) {
		t.Fatalf("got %v", result)
	}
	result = CoerceToolArgs(ctx, reg, env, "test_tool", map[string]any{"limit": 10})
	if result["limit"] != 10 {
		t.Fatalf("correct type changed: %v", result)
	}
	if got := CoerceToolArgs(ctx, reg, env, "test_tool", map[string]any{}); len(got) != 0 {
		t.Fatalf("empty args: %v", got)
	}
}

func TestSchemaAcceptsKind(t *testing.T) {
	if !coerceSchemaAcceptsKind(map[string]any{"type": "array"}, "array", nil, nil) {
		t.Fatal("array")
	}
	if !coerceSchemaAcceptsKind(map[string]any{"type": "object"}, "object", nil, nil) {
		t.Fatal("object")
	}
	if coerceSchemaAcceptsKind(map[string]any{"type": "string"}, "array", nil, nil) {
		t.Fatal("string accepts array")
	}
	if coerceSchemaAcceptsKind(nil, "array", nil, nil) {
		t.Fatal("nil")
	}
	for _, unionKey := range []string{"anyOf", "oneOf", "allOf"} {
		root := map[string]any{"$defs": map[string]any{"Node": map[string]any{unionKey: []any{map[string]any{"$ref": "#/$defs/Node"}, map[string]any{"type": "object"}}}}}
		node := root["$defs"].(map[string]any)["Node"].(map[string]any)
		if coerceSchemaAcceptsKind(node, "array", root, nil) {
			t.Fatalf("%s recursive union accepts array", unionKey)
		}
		schema := map[string]any{"type": "array", "items": map[string]any{"$ref": "#/$defs/Node"}}
		out, _ := coerceNormalizeJSONStrings([]any{`{"street": "Alpha Ave"}`}, schema, root)
		if !reflect.DeepEqual(out, []any{map[string]any{"street": "Alpha Ave"}}) {
			t.Fatalf("%s: %v", unionKey, out)
		}
	}
}

func TestNormalizeJSONStringsForSchema(t *testing.T) {
	schema := map[string]any{"type": "array", "items": map[string]any{"type": "string"}}
	out, _ := coerceNormalizeJSONStrings(`["git status", "bun test"]`, schema, nil)
	if !reflect.DeepEqual(out, []any{"git status", "bun test"}) {
		t.Fatalf("got %v", out)
	}
	value := []any{map[string]any{"id": "1"}}
	out, changed := coerceNormalizeJSONStrings(value, map[string]any{"type": "array", "items": map[string]any{"type": "object", "properties": map[string]any{}}}, nil)
	if changed || !reflect.DeepEqual(out, value) {
		t.Fatal("native list changed")
	}
	out, _ = coerceNormalizeJSONStrings("x", nil, nil)
	if out != "x" {
		t.Fatal("non dict schema changed value")
	}
}

func TestCoerceToolArgsNested(t *testing.T) {
	ctx := context.Background()
	arrayOfObjects := `{"type":"object","properties":{"items":{"type":"array","items":{"type":"object","properties":{"id":{"type":"string"},"content":{"type":"string"}}}}}}`
	reg, env := coercionRegistry(t, arrayOfObjects)
	result := CoerceToolArgs(ctx, reg, env, "test_tool", map[string]any{"items": []any{`{"id": "1", "content": "x"}`}})
	if !reflect.DeepEqual(result["items"], []any{map[string]any{"id": "1", "content": "x"}}) {
		t.Fatalf("elements: %v", result)
	}
	result = CoerceToolArgs(ctx, reg, env, "test_tool", map[string]any{"items": []any{map[string]any{"id": "1", "content": `{"not": "parsed"}`}}})
	if result["items"].([]any)[0].(map[string]any)["content"] != `{"not": "parsed"}` {
		t.Fatalf("string subfield parsed: %v", result)
	}

	reg, env = coercionRegistry(t, `{"type":"object","$defs":{"Address":{"type":"object","properties":{"street":{"type":"string"},"zip":{"type":"string"}}}},"properties":{"address":{"$ref":"#/$defs/Address"}}}`)
	result = CoerceToolArgs(ctx, reg, env, "test_tool", map[string]any{"address": `{"street": "Alpha Ave", "zip": "A-001"}`})
	if !reflect.DeepEqual(result["address"], map[string]any{"street": "Alpha Ave", "zip": "A-001"}) {
		t.Fatalf("ref object: %v", result)
	}

	reg, env = coercionRegistry(t, `{"type":"object","$defs":{"Count":{"type":"integer"},"Flag":{"type":"boolean"}},"properties":{"count":{"$ref":"#/$defs/Count"},"verbose":{"$ref":"#/$defs/Flag","description":"a keyword beside $ref"}}}`)
	result = CoerceToolArgs(ctx, reg, env, "test_tool", map[string]any{"count": "5", "verbose": "true"})
	if result["count"] != int64(5) || result["verbose"] != true {
		t.Fatalf("scalars behind ref: %v", result)
	}

	reg, env = coercionRegistry(t, `{"type":"object","$defs":{"My Type":{"type":"integer"}},"properties":{"limit":{"anyOf":[{"type":"integer"},{"type":"null"}]},"offset":{"$ref":"#/properties/limit/anyOf/0"},"page":{"$ref":"#/$defs/My%20Type"}}}`)
	result = CoerceToolArgs(ctx, reg, env, "test_tool", map[string]any{"offset": "7", "page": "3"})
	if result["offset"] != int64(7) || result["page"] != int64(3) {
		t.Fatalf("pointer refs: %v", result)
	}

	reg, env = coercionRegistry(t, `{"type":"object","$defs":{"a~2b":{"type":"integer"}},"properties":{"count":{"$ref":"#/$defs/a~2b"}}}`)
	result = CoerceToolArgs(ctx, reg, env, "test_tool", map[string]any{"count": "5"})
	if result["count"] != "5" {
		t.Fatalf("malformed escape resolved: %v", result)
	}

	reg, env = coercionRegistry(t, `{"type":"object","$defs":{"Person":{"type":"object","properties":{"first name":{"type":"string"}}}},"properties":{"person":{"$ref":"#/$defs/Person"}}}`)
	result = CoerceToolArgs(ctx, reg, env, "test_tool", map[string]any{"person": map[string]any{"first_name": "Ada"}})
	if !reflect.DeepEqual(result, map[string]any{"person": map[string]any{"first name": "Ada"}}) {
		t.Fatalf("unrename: %v", result)
	}

	reg, env = coercionRegistry(t, `{"type":"object","$defs":{"Address":{"type":"object","properties":{"street":{"type":"string"}}}},"properties":{"addresses":{"type":"array","items":{"$ref":"#/$defs/Address"}}}}`)
	result = CoerceToolArgs(ctx, reg, env, "test_tool", map[string]any{"addresses": []any{`{"street": "Alpha Ave"}`}})
	if !reflect.DeepEqual(result["addresses"], []any{map[string]any{"street": "Alpha Ave"}}) {
		t.Fatalf("array item ref: %v", result)
	}

	for _, fixture := range []string{
		`{"type":"object","$defs":{},"properties":{"address":{"$ref":"https://example.com/address.json"}}}`,
		`{"type":"object","$defs":{"Address":{"$ref":"#/$defs/Address"}},"properties":{"address":{"$ref":"#/$defs/Address"}}}`,
	} {
		reg, env = coercionRegistry(t, fixture)
		raw := `{"street": "Alpha Ave"}`
		result = CoerceToolArgs(ctx, reg, env, "test_tool", map[string]any{"address": raw})
		if result["address"] != raw {
			t.Fatalf("external or cyclic ref resolved: %v", result)
		}
	}

	reg, env = coercionRegistry(t, `{"type":"object","properties":{"tags":{"type":"array","items":{"type":"string"}}}}`)
	result = CoerceToolArgs(ctx, reg, env, "test_tool", map[string]any{"tags": "one"})
	if !reflect.DeepEqual(result["tags"], []any{"one"}) {
		t.Fatalf("bare string not wrapped: %v", result)
	}
}
