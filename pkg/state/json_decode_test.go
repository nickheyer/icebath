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
		v, err := loadsValue(in)
		if err != nil {
			t.Fatalf("%s: %v", in, err)
		}
		got, err := dumpsValue(v, jsonDefault)
		if err != nil || got != want {
			t.Fatalf("%s -> %s %v want %s", in, got, err, want)
		}
	}
	for in, msg := range map[string]string{
		"":               "invalid JSON at line 1 column 1: expected a value",
		"[1,]":           "invalid JSON at line 1 column 3: a comma ends the list",
		"{\"a\" 1}":      "invalid JSON at line 1 column 6: expected ':' after a key",
		"[1] x":          "invalid JSON at line 1 column 5: more text follows the value",
		"\ufeff[]":       "invalid JSON at line 1 column 1: the text starts with a UTF-8 byte order mark",
		"\"a\nb\"":       "invalid JSON at line 1 column 3: a string holds a control character",
		`"\x"`:           `invalid JSON at line 1 column 2: a string holds an invalid backslash escape`,
		`"\u12"`:         `invalid JSON at line 1 column 3: a string holds an invalid \uXXXX escape`,
		`"\u0041`:        `invalid JSON at line 1 column 3: a string holds an invalid \uXXXX escape`,
		`"\ud83d\uZZZZ"`: `invalid JSON at line 1 column 9: a string holds an invalid \uXXXX escape`,
		"[1,\n  ]":       "invalid JSON at line 1 column 3: a comma ends the list",
		`{"a": }`:        "invalid JSON at line 1 column 7: expected a value",
		`"unterminated`:  "invalid JSON at line 1 column 1: a string is never closed",
	} {
		if _, err := loadsValue(in); err == nil || err.Error() != msg {
			t.Fatalf("%q: %v want %s", in, err, msg)
		}
	}
	v, _ := loadsValue(`"\ud800"`)
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
		{[]byte("\xef\xbb\xbf{}"), orderedObject{}},
		{[]byte("\xed\xa0\x80"), "fallback"},
	} {
		got, err := jsonOr(tc.raw, "fallback", "warn")
		if err != nil || !reflect.DeepEqual(got, tc.want) {
			t.Fatalf("%q -> %#v %v", tc.raw, got, err)
		}
	}
	_, err := jsonOr([]byte("[\xff]"), nil, "warn")
	var conv *convError
	if !errors.As(err, &conv) || conv.Class != "UnicodeDecodeError" {
		t.Fatalf("bad utf-8 bytes raise %v", err)
	}
}

func TestTextAndNumberHelpers(t *testing.T) {
	for in, want := range map[string]string{
		"\xe2\x82": "�", "\xffA": "�A", "\xed\xa0\x80": "���", "\xf0\x9f\x98": "�", "ok\xc3\xa9": "oké",
	} {
		if got := decodeUTF8Replace([]byte(in)); got != want {
			t.Fatalf("decode %q -> %q", in, got)
		}
	}
	ints := map[any]int64{" 42 ": 42, "4_2": 42, "+7": 7, "٤٢": 42, 3.9: 3, -3.9: -3, true: 1, json.Number("-5"): -5}
	for in, want := range ints {
		if n, err := intOfValue(in); err != nil || n.Int64() != want {
			t.Fatalf("int(%v) -> %v %v", in, n, err)
		}
	}
	for in, class := range map[any]string{"_4": "ValueError", "4__2": "ValueError", "": "ValueError", math.NaN(): "ValueError", math.Inf(1): "OverflowError", nil: "TypeError", "x": "ValueError"} {
		var conv *convError
		if _, err := intOfValue(in); !errors.As(err, &conv) || conv.Class != class {
			t.Fatalf("int(%v) error %v", in, err)
		}
	}
	for in, want := range map[string]float64{"1_000.5": 1000.5, "1e500": math.Inf(1), " -inf ": math.Inf(-1), "Infinity": math.Inf(1), ".5": 0.5, "5.": 5, "1E-2": 0.01} {
		if f, ok := parseFloat(in); !ok || f != want {
			t.Fatalf("float(%q) -> %v %v", in, f, ok)
		}
	}
	for _, in := range []string{"0x10", "1__0", "e5", "", "1e", "_1", "1_", "nanx"} {
		if _, ok := parseFloat(in); ok {
			t.Fatalf("float(%q) must fail", in)
		}
	}
	if f, ok := parseFloat(" nan "); !ok || !math.IsNaN(f) {
		t.Fatal("nan parses")
	}
	if got := splitWhitespace(" a\x1cb　c  "); !reflect.DeepEqual(got, []string{"a\x1cb", "c"}) {
		t.Fatalf("split %q", got)
	}
}
