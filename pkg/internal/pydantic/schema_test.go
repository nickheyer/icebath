package pydantic

import (
	"testing"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
)

// Serialization schema of a closed model with an enum, a tagged union, a factory list and a nullable model
func TestSchemaGeneratorSerializationShapes(t *testing.T) {
	color := Enum("Color", "red", "red", "blue", "blue").Described("A colour.")
	a := NewModel("A", Config{Extra: ExtraForbid}, Req("kind", Literal("a")))
	b := NewModel("B", Config{Extra: ExtraAllow}, Req("kind", Literal("b")))
	m := NewModel("Thing", Config{Extra: ExtraForbid},
		Req("session_id", Str),
		Def("color", Nullable(color), nil),
		Fac("tags", List(Str), NewList),
		Def("ratio", Float, 0),
		Req("owner", Tagged("kind", a, b)),
		Def("child", Nullable(a), nil),
		Req("extra", Dict(Any)),
	).Described("One thing.")
	g := NewSchemaGenerator("#/$defs/{model}")
	if ref, _ := pyjson.Dumps(g.ModelRef(m)); ref != `{"$ref": "#/$defs/Thing"}` {
		t.Fatalf("ref = %s", ref)
	}
	got, _ := pyjson.Dumps(g.Definitions())
	want := `{"A": {"additionalProperties": false, "properties": {"kind": {"const": "a", "title": "Kind", "type": "string"}}, "required": ["kind"], "title": "A", "type": "object"}, ` +
		`"B": {"additionalProperties": true, "properties": {"kind": {"const": "b", "title": "Kind", "type": "string"}}, "required": ["kind"], "title": "B", "type": "object"}, ` +
		`"Color": {"description": "A colour.", "enum": ["red", "blue"], "title": "Color", "type": "string"}, ` +
		`"Thing": {"additionalProperties": false, "description": "One thing.", "properties": {"session_id": {"title": "Session Id", "type": "string"}, ` +
		`"color": {"anyOf": [{"$ref": "#/$defs/Color"}, {"type": "null"}], "default": null}, "tags": {"items": {"type": "string"}, "title": "Tags", "type": "array"}, ` +
		`"ratio": {"default": 0, "title": "Ratio", "type": "number"}, ` +
		`"owner": {"discriminator": {"mapping": {"a": "#/$defs/A", "b": "#/$defs/B"}, "propertyName": "kind"}, "oneOf": [{"$ref": "#/$defs/A"}, {"$ref": "#/$defs/B"}], "title": "Owner"}, ` +
		`"child": {"anyOf": [{"$ref": "#/$defs/A"}, {"type": "null"}], "default": null}, "extra": {"additionalProperties": true, "title": "Extra", "type": "object"}}, ` +
		`"required": ["session_id", "owner", "extra"], "title": "Thing", "type": "object"}}`
	if got != want {
		t.Fatalf("definitions\n got %s\nwant %s", got, want)
	}
}

// A RootModel takes its root schema with the class title and description and no extra key
func TestSchemaGeneratorRootModel(t *testing.T) {
	x := NewModel("X", Config{Extra: ExtraForbid}, Req("n", Int))
	root := NewRootModel("Either", Union(x, Nullable(Str))).Described("X or text.")
	g := NewSchemaGenerator("#/components/schemas/{model}")
	g.ModelRef(root)
	defs := g.Definitions()
	got, _ := pyjson.Dumps(defs.Vals["Either"])
	want := `{"anyOf": [{"$ref": "#/components/schemas/X"}, {"type": "string"}, {"type": "null"}], "description": "X or text.", "title": "Either"}`
	if got != want {
		t.Fatalf("root\n got %s\nwant %s", got, want)
	}
}
