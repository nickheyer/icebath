package pyjson

import (
	"errors"
	"math"
	"math/big"
	"testing"

	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
)

// Python json loads then dumps of numbers, specials and DEL match byte for byte
func TestLoadsDumpsRoundTripMatchesPython(t *testing.T) {
	src := `{"a": 1e5, "b": 1.50, "c": -0, "d": NaN, "e": [Infinity, -Infinity], "f": 1e400, "g": 0.0001, "h": 1e-05, "i": 1e16, "j": 123456789012345678, "k": "\u007f<&>", "l": -1.5e-7}`
	v, err := Loads(src)
	if err != nil {
		t.Fatal(err)
	}
	compact, err := Dumps(v)
	if err != nil {
		t.Fatal(err)
	}
	want := `{"a": 100000.0, "b": 1.5, "c": 0, "d": NaN, "e": [Infinity, -Infinity], "f": Infinity, "g": 0.0001, "h": 1e-05, "i": 1e+16, "j": 123456789012345678, "k": "\u007f<&>", "l": -1.5e-07}`
	if compact != want {
		t.Fatalf("compact\n got %s\nwant %s", compact, want)
	}
	indented, err := Dumps(v, Indent(2))
	if err != nil {
		t.Fatal(err)
	}
	wantIndent := "{\n  \"a\": 100000.0,\n  \"b\": 1.5,\n  \"c\": 0,\n  \"d\": NaN,\n  \"e\": [\n    Infinity,\n    -Infinity\n  ],\n  \"f\": Infinity,\n  \"g\": 0.0001,\n  \"h\": 1e-05,\n  \"i\": 1e+16,\n  \"j\": 123456789012345678,\n  \"k\": \"\\u007f<&>\",\n  \"l\": -1.5e-07\n}"
	if indented != wantIndent {
		t.Fatalf("indented\n got %s\nwant %s", indented, wantIndent)
	}
}

// Ordered objects keep first key position with the last value and Python int and float types
func TestLoadsKeepsOrderAndPythonTypes(t *testing.T) {
	v, err := Loads(`{"z": 1.0e2, "a": NaN, "z": [2.50, 99999999999999999999, -0, 7]}`)
	if err != nil {
		t.Fatal(err)
	}
	obj := v.(*Object)
	if len(obj.Keys) != 2 || obj.Keys[0] != "z" || obj.Keys[1] != "a" {
		t.Fatalf("keys %v", obj.Keys)
	}
	if a, _ := obj.Get("a"); !math.IsNaN(a.(float64)) {
		t.Fatalf("a %v", a)
	}
	list := obj.Vals["z"].([]any)
	if _, ok := list[1].(*big.Int); !ok || list[2] != int64(0) || list[3] != int64(7) || list[0] != 2.5 {
		t.Fatalf("list %#v", list)
	}
	out, _ := Dumps(obj, Separators(",", ":"), EnsureASCII(false))
	if out != `{"z":[2.5,99999999999999999999,0,7],"a":NaN}` {
		t.Fatalf("got %s", out)
	}
	if !obj.Delete("z") || obj.Len() != 1 || obj.Delete("z") {
		t.Fatalf("delete %v", obj.Keys)
	}
}

// Decode errors carry the CPython message, line, column and char offset
func TestLoadsErrorsMatchPython(t *testing.T) {
	cases := map[string]string{
		"":            "Expecting value: line 1 column 1 (char 0)",
		"1 2":         "Extra data: line 1 column 3 (char 2)",
		"\ufeff{}":    "Unexpected UTF-8 BOM (decode using utf-8-sig): line 1 column 1 (char 0)",
		"{\n\"a\" 1}": "Expecting ':' delimiter: line 2 column 5 (char 6)",
	}
	for src, want := range cases {
		_, err := Loads(src)
		var de *DecodeError
		if !errors.As(err, &de) || err.Error() != want {
			t.Errorf("Loads(%q) = %v, want %s", src, err, want)
		}
	}
}

// Bytes input follows json.detect_encoding and CPython codec errors
func TestLoadsBytesDetectsEncoding(t *testing.T) {
	v, err := LoadsBytes([]byte("\xff\xfe[\x001\x00]\x00"))
	if err != nil || v.([]any)[0] != int64(1) {
		t.Fatalf("utf-16 bom %v %v", v, err)
	}
	if v, err := LoadsBytes([]byte("\xef\xbb\xbf\"x\"")); err != nil || v != "x" {
		t.Fatalf("utf-8-sig %v %v", v, err)
	}
	_, err = LoadsBytes([]byte("\xff\xfe[\x00]"))
	if err == nil || err.Error() != "'utf-16-le' codec can't decode byte 0x5d in position 4: truncated data" {
		t.Fatalf("truncated %v", err)
	}
}

// Keyword arguments follow json.dumps, including key conversion, sort order and NaN refusal
func TestDumpsKeywordSurface(t *testing.T) {
	cases := []struct {
		v    any
		opts []Option
		want string
	}{
		{map[int]any{10: 1, 2: "é"}, nil, `{"2": "\u00e9", "10": 1}`},
		{"😀\x7f", nil, `"\ud83d\ude00\u007f"`},
		{"😀\x7f", []Option{EnsureASCII(false)}, "\"😀\x7f\""},
		{map[string]any{}, []Option{Indent(4)}, `{}`},
		{[]any{1, []any{}}, []Option{Indent(0)}, "[\n1,\n[]\n]"},
		{[]any{1, 2}, []Option{IndentText("\t"), Separators(", ", ": ")}, "[\n\t1, \n\t2\n]"},
		{map[float64]any{1.5: true, math.Inf(1): nil}, nil, `{"1.5": true, "Infinity": null}`},
		{map[bool]int{true: 1, false: 0}, []Option{SortKeys(true)}, `{"false": 0, "true": 1}`},
		{[]float64{0.1, 1e16, 1e-5, math.Copysign(0, -1)}, nil, `[0.1, 1e+16, 1e-05, -0.0]`},
		{map[[2]int]int{{1, 2}: 3}, []Option{SkipKeys(true)}, `{}`},
	}
	for _, c := range cases {
		got, err := Dumps(c.v, c.opts...)
		if err != nil || got != c.want {
			t.Errorf("Dumps(%#v) = %q %v, want %q", c.v, got, err, c.want)
		}
	}
	if _, err := Dumps(math.NaN(), AllowNaN(false)); err == nil || err.Error() != "Out of range float values are not JSON compliant: nan" {
		t.Errorf("allow_nan %v", err)
	}
	if _, err := Dumps(map[[2]int]int{{1, 2}: 3}); err == nil || err.Error() != "keys must be str, int, float, bool or None, not [2]int" {
		t.Errorf("bad key %v", err)
	}
	loop := []any{nil}
	loop[0] = loop
	if _, err := Dumps(loop); !errors.Is(err, ErrCircular) {
		t.Errorf("circular %v", err)
	}
	type point struct{ X int }
	got, err := Dumps([]any{point{3}}, Default(func(v any) (any, error) { return []any{v.(point).X}, nil }))
	if err != nil || got != "[[3]]" {
		t.Errorf("default %q %v", got, err)
	}
	if _, err := Dumps(point{1}); err == nil {
		t.Errorf("unsupported value encoded")
	}
}

func TestObjectReprKeepsOrder(t *testing.T) {
	v, err := Loads(`{"b": [1, 2.0, null], "a": {"x": "it's"}, "c": 1e400}`)
	if err != nil {
		t.Fatal(err)
	}
	if got := pyrepr.Repr(v); got != `{'b': [1, 2.0, None], 'a': {'x': "it's"}, 'c': inf}` {
		t.Errorf("repr %s", got)
	}
	if pyrepr.TypeName(v) != "dict" || !pyrepr.Truthy(v) || pyrepr.Truthy(NewObject()) {
		t.Error("dict type name or truth value")
	}
}
