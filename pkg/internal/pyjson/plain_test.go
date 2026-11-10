package pyjson

import (
	"reflect"
	"testing"
)

func TestPlainTurnsNestedObjectsIntoMaps(t *testing.T) {
	v, err := Loads(`{"calls": [{"name": "x", "arguments": {"a": [1, {"b": null}]}}], "n": 2.5}`)
	if err != nil {
		t.Fatal(err)
	}
	inner, _ := Loads(`1`)
	want := map[string]any{"calls": []any{map[string]any{"name": "x", "arguments": map[string]any{"a": []any{inner, map[string]any{"b": nil}}}}}, "n": 2.5}
	if got := Plain(v); !reflect.DeepEqual(got, want) {
		t.Fatalf("plain %#v", got)
	}
	if Plain((*Object)(nil)) != nil || Plain("s") != "s" {
		t.Fatal("scalars")
	}
}

func TestWTF8ShapeMatchesTheDecodeWTF8Shapes(t *testing.T) {
	text := `{"calls": [{"name": "x", "arguments": {"a": [1, 1.0, -0, 12345678901234567890123, {"b": null}]}}], "n": 2.5, "s": "\ud800"}`
	v, err := Loads(text)
	if err != nil {
		t.Fatal(err)
	}
	want, err := DecodeWTF8([]byte(text))
	if err != nil {
		t.Fatal(err)
	}
	if got := WTF8Shape(v); !reflect.DeepEqual(got, want) {
		t.Fatalf("shape %#v, want %#v", got, want)
	}
	if WTF8Shape((*Object)(nil)) != nil || WTF8Shape(int64(3)) != 3.0 || WTF8Shape("s") != "s" || WTF8Shape(true) != true {
		t.Fatal("scalars")
	}
}
