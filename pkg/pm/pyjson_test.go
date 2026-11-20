package pm

import "testing"

// Python json loads then dumps of numbers, specials and DEL match byte for byte
func TestPyJSONRoundTripMatchesPython(t *testing.T) {
	src := `{"a": 1e5, "b": 1.50, "c": -0, "d": NaN, "e": [Infinity, -Infinity], "f": 1e400, "g": 0.0001, "h": 1e-05, "i": 1e16, "j": 123456789012345678, "k": "\u007f<&>", "l": -1.5e-7}`
	v, err := loadJSON([]byte(src))
	if err != nil {
		t.Fatal(err)
	}
	compact, err := dumpsJSON(v, false)
	if err != nil {
		t.Fatal(err)
	}
	want := `{"a": 100000.0, "b": 1.5, "c": 0, "d": NaN, "e": [Infinity, -Infinity], "f": Infinity, "g": 0.0001, "h": 1e-05, "i": 1e+16, "j": 123456789012345678, "k": "\u007f<&>", "l": -1.5e-07}`
	if string(compact) != want {
		t.Fatalf("compact\n got %s\nwant %s", compact, want)
	}
	indented, err := dumpsJSON(v, true)
	if err != nil {
		t.Fatal(err)
	}
	wantIndent := "{\n  \"a\": 100000.0,\n  \"b\": 1.5,\n  \"c\": 0,\n  \"d\": NaN,\n  \"e\": [\n    Infinity,\n    -Infinity\n  ],\n  \"f\": Infinity,\n  \"g\": 0.0001,\n  \"h\": 1e-05,\n  \"i\": 1e+16,\n  \"j\": 123456789012345678,\n  \"k\": \"\\u007f<&>\",\n  \"l\": -1.5e-07\n}"
	if string(indented) != wantIndent {
		t.Fatalf("indented\n got %s\nwant %s", indented, wantIndent)
	}
}

// Ordered objects keep key order and share the number rules
func TestOrderedJSONKeepsOrderAndPythonNumbers(t *testing.T) {
	v, err := loadOrderedJSON([]byte(`{"z": 1.0e2, "a": NaN, "m": [2.50]}`))
	if err != nil {
		t.Fatal(err)
	}
	out, err := dumpsJSON(v, false)
	if err != nil {
		t.Fatal(err)
	}
	if string(out) != `{"z": 100.0, "a": NaN, "m": [2.5]}` {
		t.Fatalf("got %s", out)
	}
}
