package pyjson

import "testing"

func TestLoadsDictReturnsObjectsAndTheGetAttributeErrorOtherwise(t *testing.T) {
	d, err := LoadsDict(`{"a": 1, "b": null}`)
	if err != nil || d.Len() != 2 {
		t.Fatalf("object %v %v", d, err)
	}
	if got := d.GetOr("a", "x"); got != int64(1) {
		t.Fatalf("present key %#v", got)
	}
	if got := d.GetOr("b", "x"); got != nil {
		t.Fatalf("present null %#v", got)
	}
	if got := d.GetOr("c", "x"); got != "x" {
		t.Fatalf("absent key %#v", got)
	}
	for raw, want := range map[string]string{`[]`: "'list' object has no attribute 'get'", `5`: "'int' object has no attribute 'get'",
		`"s"`: "'str' object has no attribute 'get'", `null`: "'NoneType' object has no attribute 'get'"} {
		if _, err := LoadsDict(raw); err == nil || err.Error() != want {
			t.Fatalf("LoadsDict(%s) error %v", raw, err)
		}
	}
	if _, err := LoadsDict(`{`); err == nil {
		t.Fatal("bad JSON decoded")
	}
}
