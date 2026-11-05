package state

import (
	"encoding/json"
	"errors"
	"math"
	"reflect"
	"testing"
)

func TestPyLoadsRoundTripsThroughDumps(t *testing.T) {
	cases := map[string]string{
		`{"b": 1, "a": [1.5, NaN, -0, 12345678901234567890, -Infinity], "b": 2}`: `{"b": 2, "a": [1.5, NaN, 0, 12345678901234567890, -Infinity]}`,
		` [true, false, null, "\u00e9\ud83d\ude00", 1E2, 0.10] `:                 `[true, false, null, "\u00e9\ud83d\ude00", 100.0, 0.1]`,
		`"lone \ud800 surrogate"`:                                                `"lone \ud800 surrogate"`,
		`{}`:                                                                     `{}`,
	}
	for in, want := range cases {
		v, err := pyLoads(in)
		if err != nil {
			t.Fatalf("%s: %v", in, err)
		}
		got, err := pyDumps(v, pyJSONDefault)
		if err != nil || got != want {
			t.Fatalf("%s -> %s %v want %s", in, got, err, want)
		}
	}
	for in, msg := range map[string]string{
		"":               "Expecting value: line 1 column 1 (char 0)",
		"[1,]":           "Illegal trailing comma before end of array: line 1 column 3 (char 2)",
		"{\"a\" 1}":      "Expecting ':' delimiter: line 1 column 6 (char 5)",
		"[1] x":          "Extra data: line 1 column 5 (char 4)",
		"\ufeff[]":       "Unexpected UTF-8 BOM (decode using utf-8-sig): line 1 column 1 (char 0)",
		"\"a\nb\"":       "Invalid control character at: line 1 column 3 (char 2)",
		`"\x"`:           `Invalid \escape: line 1 column 2 (char 1)`,
		`"\u12"`:         `Invalid \uXXXX escape: line 1 column 3 (char 2)`,
		`"\u0041`:        `Invalid \uXXXX escape: line 1 column 3 (char 2)`,
		`"\ud83d\uZZZZ"`: `Invalid \uXXXX escape: line 1 column 9 (char 8)`,
		"[1,\n  ]":       "Illegal trailing comma before end of array: line 1 column 3 (char 2)",
		`{"a": }`:        "Expecting value: line 1 column 7 (char 6)",
		`"unterminated`:  "Unterminated string starting at: line 1 column 1 (char 0)",
	} {
		if _, err := pyLoads(in); err == nil || err.Error() != msg {
			t.Fatalf("%q: %v want %s", in, err, msg)
		}
	}
	v, _ := pyLoads(`"\ud800"`)
	if v != "\xed\xa0\x80" {
		t.Fatalf("lone surrogate bytes %q", v)
	}
}

func TestJSONOr(t *testing.T) {
	for _, tc := range []struct {
		raw  any
		want any
	}{
		{`[1, "a"]`, []any{json.Number("1"), "a"}},
		{"not json", "fallback"},
		{nil, "fallback"},
		{42, "fallback"},
		{[]byte("\xff\xfe[\x00]\x00"), []any{}},
		{[]byte("\x00[\x00]"), []any{}},
		{[]byte("\xef\xbb\xbf{}"), pyObject{}},
		{[]byte("\xed\xa0\x80"), "fallback"},
	} {
		got, err := jsonOr(tc.raw, "fallback", "warn")
		if err != nil || !reflect.DeepEqual(got, tc.want) {
			t.Fatalf("%q -> %#v %v", tc.raw, got, err)
		}
	}
	_, err := jsonOr([]byte("[\xff]"), nil, "warn")
	var conv *pyConvError
	if !errors.As(err, &conv) || conv.Class != "UnicodeDecodeError" {
		t.Fatalf("bad utf-8 bytes raise %v", err)
	}
}

func TestPythonStringAndNumberHelpers(t *testing.T) {
	for in, want := range map[string]string{
		"\xe2\x82": "�", "\xffA": "�A", "\xed\xa0\x80": "���", "\xf0\x9f\x98": "�", "ok\xc3\xa9": "oké",
	} {
		if got := pyDecodeUTF8Replace([]byte(in)); got != want {
			t.Fatalf("decode %q -> %q", in, got)
		}
	}
	if got := pyBytesRepr([]byte("a'b")); got != `b"a'b"` {
		t.Fatalf("bytes repr %s", got)
	}
	if got := pyBytesRepr([]byte("\x00\n\x7f\\")); got != `b'\x00\n\x7f\\'` {
		t.Fatalf("bytes repr %s", got)
	}
	if got := pyStr(pyObject{{"a", json.Number("1")}, {"b", []any{"x", nil, true, 1.5}}}); got != "{'a': 1, 'b': ['x', None, True, 1.5]}" {
		t.Fatalf("str %s", got)
	}
	ints := map[any]int64{" 42 ": 42, "4_2": 42, "+7": 7, "٤٢": 42, 3.9: 3, -3.9: -3, true: 1, json.Number("-5"): -5}
	for in, want := range ints {
		if n, err := pyIntOf(in); err != nil || n.Int64() != want {
			t.Fatalf("int(%v) -> %v %v", in, n, err)
		}
	}
	for in, class := range map[any]string{"_4": "ValueError", "4__2": "ValueError", "": "ValueError", math.NaN(): "ValueError", math.Inf(1): "OverflowError", nil: "TypeError", "x": "ValueError"} {
		var conv *pyConvError
		if _, err := pyIntOf(in); !errors.As(err, &conv) || conv.Class != class {
			t.Fatalf("int(%v) error %v", in, err)
		}
	}
	for in, want := range map[string]float64{"1_000.5": 1000.5, "1e500": math.Inf(1), " -inf ": math.Inf(-1), "Infinity": math.Inf(1), ".5": 0.5, "5.": 5, "1E-2": 0.01} {
		if f, ok := pyParseFloat(in); !ok || f != want {
			t.Fatalf("float(%q) -> %v %v", in, f, ok)
		}
	}
	for _, in := range []string{"0x10", "1__0", "e5", "", "1e", "_1", "1_", "nanx"} {
		if _, ok := pyParseFloat(in); ok {
			t.Fatalf("float(%q) must fail", in)
		}
	}
	if f, ok := pyParseFloat(" nan "); !ok || !math.IsNaN(f) {
		t.Fatal("nan parses")
	}
	if got := pySplitWhitespace(" a\x1cb　c  "); !reflect.DeepEqual(got, []string{"a", "b", "c"}) {
		t.Fatalf("split %q", got)
	}
}
