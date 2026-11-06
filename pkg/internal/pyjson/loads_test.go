package pyjson

import (
	"strings"
	"testing"
)

func TestLoadsLenientAcceptsControlCharacters(t *testing.T) {
	text := "{\"a\": \"x\ty\nz\"}"
	if _, err := Loads(text); err == nil || !strings.Contains(err.Error(), "Invalid control character at") {
		t.Fatalf("strict decode accepted control characters: %v", err)
	}
	v, err := LoadsLenient(text)
	if err != nil {
		t.Fatal(err)
	}
	if got, _ := v.(*Object).Get("a"); got != "x\ty\nz" {
		t.Fatalf("got %q", got)
	}
}

func TestLoadsPairsHookSeesEveryPairInOrderAndPropagatesItsError(t *testing.T) {
	var seen [][]string
	hook := func(keys []string, vals []any) (any, error) {
		seen = append(seen, keys)
		for i, k := range keys {
			for _, prior := range keys[:i] {
				if prior == k {
					return nil, &DecodeError{Msg: "Duplicate"}
				}
			}
		}
		obj := NewObject()
		for i, k := range keys {
			obj.Set(k, vals[i])
		}
		return obj, nil
	}
	v, err := LoadsBytesPairsHook([]byte(`{"b": {}, "a": [1, {"c": 2}]}`), hook)
	if err != nil {
		t.Fatal(err)
	}
	if got, _ := Dumps(v); got != `{"b": {}, "a": [1, {"c": 2}]}` {
		t.Fatalf("decoded %s", got)
	}
	if len(seen) != 3 || len(seen[0]) != 0 || seen[1][0] != "c" || seen[2][0] != "b" || seen[2][1] != "a" {
		t.Fatalf("hook calls %v", seen)
	}
	if _, err := LoadsPairsHook(`{"a": 1, "a": 2}`, hook); err == nil || err.Error() != "Duplicate: line 0 column 0 (char 0)" {
		t.Fatalf("duplicate keys decoded: %v", err)
	}
}
