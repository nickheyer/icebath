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
