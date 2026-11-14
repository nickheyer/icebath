package pyjson

import (
	"reflect"
	"testing"
)

func TestLoadsPartialClosesPrefixes(t *testing.T) {
	cases := []struct {
		in   string
		want any
	}{
		{`{"a": 1, "b": [1, 2`, map[string]any{"a": int64(1), "b": []any{int64(1), int64(2)}}},
		{`{"a": "hel`, map[string]any{}},
		{`{"a"`, map[string]any{}},
		{`{"city": "Paris", "unit": tr`, map[string]any{"city": "Paris"}},
		{`{"x": {"y": null}}`, map[string]any{"x": map[string]any{"y": nil}}},
		{`[1, "two", {"k": fal`, []any{int64(1), "two", map[string]any{}}},
	}
	for _, c := range cases {
		got, err := LoadsPartial(c.in)
		if err != nil {
			t.Fatalf("LoadsPartial(%q) error %v", c.in, err)
		}
		encoded, _ := Dumps(c.want)
		if want, _ := Loads(encoded); !reflect.DeepEqual(got, Plain(want)) {
			t.Fatalf("LoadsPartial(%q) = %#v, want %#v", c.in, got, want)
		}
	}
	if _, err := LoadsPartial(`{"a" 1}`); err == nil {
		t.Fatal("malformed object parsed")
	}
}
