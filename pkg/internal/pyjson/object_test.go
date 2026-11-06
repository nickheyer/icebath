package pyjson

import (
	"reflect"
	"testing"
)

func TestObjectOfKeepsLiteralOrderAndDumpsInsertionOrder(t *testing.T) {
	o := ObjectOf("schema", 1, "started_at", "now", "argv", []any{"icebath"})
	o.Set("schema", 2)
	o.Set("update_id", "abc")
	got, err := Dumps(o, Indent(2))
	if err != nil {
		t.Fatal(err)
	}
	want := "{\n  \"schema\": 2,\n  \"started_at\": \"now\",\n  \"argv\": [\n    \"icebath\"\n  ],\n  \"update_id\": \"abc\"\n}"
	if got != want {
		t.Fatalf("dumps = %q", got)
	}
}

func TestObjectCopyUpdatePopFollowDictSemantics(t *testing.T) {
	o := ObjectOf("a", 1, "b", 2)
	c := o.Copy()
	c.Update(ObjectOf("c", 3, "a", 9))
	if !reflect.DeepEqual(c.Keys, []string{"a", "b", "c"}) || c.Vals["a"] != 9 || o.Vals["a"] != 1 {
		t.Fatalf("copy/update = %v %v, original %v", c.Keys, c.Vals, o.Vals)
	}
	if v, ok := c.Pop("b"); !ok || v != 2 || !reflect.DeepEqual(c.Keys, []string{"a", "c"}) {
		t.Fatalf("pop = %v %v keys %v", v, ok, c.Keys)
	}
	if _, ok := c.Pop("missing"); ok {
		t.Fatal("pop of a missing key reported present")
	}
}

func TestCloneDeepCopiesObjectsAndAsObjectSortsPlainMaps(t *testing.T) {
	inner := ObjectOf("z", []any{1})
	o := ObjectOf("inner", inner, "m", map[string]any{"k": []any{2}})
	c := Clone(o).(*Object)
	c.Vals["inner"].(*Object).Vals["z"].([]any)[0] = 5
	c.Vals["m"].(map[string]any)["k"].([]any)[0] = 6
	if inner.Vals["z"].([]any)[0] != 1 || o.Vals["m"].(map[string]any)["k"].([]any)[0] != 2 {
		t.Fatal("clone shares nested containers")
	}
	view, ok := AsObject(map[string]any{"b": 1, "a": 2})
	if !ok || !reflect.DeepEqual(view.Keys, []string{"a", "b"}) {
		t.Fatalf("AsObject = %v %v", view, ok)
	}
	if same, ok := AsObject(inner); !ok || same != inner {
		t.Fatal("AsObject of an Object is not the same Object")
	}
	if _, ok := AsObject([]any{}); ok {
		t.Fatal("AsObject accepted a list")
	}
}
