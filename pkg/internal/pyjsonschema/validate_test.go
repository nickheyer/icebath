package pyjsonschema

import (
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
)

func TestValidateMessagesFollowPythonJsonschema(t *testing.T) {
	for _, tc := range []struct{ instance, schema, want string }{
		{`{"language": "French"}`, `{"type": "object", "properties": {"language": {"type": "string"}}, "required": ["language"]}`, ""},
		{`{}`, `{"type": "object", "required": ["language", "x"]}`, "'language' is a required property"},
		{`{"language": 1}`, `{"properties": {"language": {"type": "string"}}}`, "1 is not of type 'string'"},
		{`{"a": 1, "c": 2, "b": 3}`, `{"properties": {"a": {}}, "additionalProperties": false}`, "Additional properties are not allowed ('b', 'c' were unexpected)"},
		{`"x"`, `{"enum": ["a", "b"]}`, "'x' is not one of ['a', 'b']"},
		{`""`, `{"minLength": 1}`, "'' should be non-empty"},
		{`5`, `{"maximum": 3}`, "5 is greater than the maximum of 3"},
		{`[1, 2, 3]`, `{"prefixItems": [{}], "items": false}`, "Expected at most 1 item but found 2 extra: [2, 3]"},
	} {
		instance, _ := pyjson.Loads(tc.instance)
		schema, _ := pyjson.Loads(tc.schema)
		err := Validate(instance, schema)
		got := ""
		if err != nil {
			got = err.Error()
		}
		if got != tc.want {
			t.Fatalf("%s against %s: %q want %q", tc.instance, tc.schema, got, tc.want)
		}
	}
}

func TestIterErrorsYieldEveryErrorInSchemaOrder(t *testing.T) {
	instance, _ := pyjson.Loads(`{"zip": 5, "tags": ["a", 1]}`)
	schema, _ := pyjson.Loads(`{"type": "object", "required": ["city", "country"], "properties": {"zip": {"type": "string"}, "tags": {"type": "array", "items": {"type": "string"}}}}`)
	errs, err := IterErrors(instance, schema)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, e := range errs {
		got = append(got, fmt.Sprint(e.AbsolutePath)+" "+e.Message)
	}
	want := []string{"[] 'city' is a required property", "[] 'country' is a required property", "[zip] 5 is not of type 'string'", "[tags 1] 1 is not of type 'string'"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("errors %q", got)
	}
	if _, isInt := errs[3].AbsolutePath[1].(int); !isInt {
		t.Fatalf("list index kept as %T", errs[3].AbsolutePath[1])
	}
}

func TestCheckSchemaRendersTheFirstMetaschemaError(t *testing.T) {
	schema, _ := pyjson.Loads(`{"type": 42}`)
	err := CheckSchema(schema)
	want := "42 is not valid under any of the given schemas\n\n" +
		"Failed validating 'anyOf' in metaschema['allOf'][3]['properties']['type']:\n" +
		"    {'anyOf': [{'$ref': '#/$defs/simpleTypes'},\n" +
		"               {'type': 'array',\n" +
		"                'items': {'$ref': '#/$defs/simpleTypes'},\n" +
		"                'minItems': 1,\n" +
		"                'uniqueItems': True}]}\n\n" +
		"On schema['type']:\n    42"
	if err == nil || err.Error() != want {
		t.Fatalf("check_schema error\n%v", err)
	}
	good, _ := pyjson.Loads(`{"type": "object", "properties": {"a": {"pattern": "^x"}}}`)
	if err := CheckSchema(good); err != nil {
		t.Fatal(err)
	}
	badPattern, _ := pyjson.Loads(`{"pattern": "["}`)
	if err := CheckSchema(badPattern); err == nil || !strings.HasPrefix(err.Error(), "'[' is not a 'regex'") {
		t.Fatalf("regex format error %v", err)
	}
}

func TestPformatWrapsLongValuesAtWidthSeventyTwo(t *testing.T) {
	long := strings.Repeat("word ", 20)
	if got := Pformat(long); !strings.HasPrefix(got, "('word word") || !strings.Contains(got, "\n ") || !strings.HasSuffix(got, "')") {
		t.Fatalf("string wrap %q", got)
	}
	if got := Pformat([]any{"a", int64(1)}); got != "['a', 1]" {
		t.Fatalf("short list %q", got)
	}
}
