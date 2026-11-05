package pyrepr

import (
	"encoding/json"
	"errors"
	"iter"
	"math"
	"math/big"
	"strings"
	"testing"
)

type orderedPairs [][2]any

func (o orderedPairs) PyItems() iter.Seq2[any, any] {
	return func(yield func(any, any) bool) {
		for _, p := range o {
			if !yield(p[0], p[1]) {
				return
			}
		}
	}
}

func TestStrReprMatchesCPython(t *testing.T) {
	cases := []struct{ in, repr, ascii string }{
		{"", `''`, `''`},
		{"it's", `"it's"`, `"it's"`},
		{`say "hi"`, `'say "hi"'`, `'say "hi"'`},
		{`both ' "`, `'both \' "'`, `'both \' "'`},
		{"tab\tnl\nret\r\\", `'tab\tnl\nret\r\\'`, `'tab\tnl\nret\r\\'`},
		{"\x00\x1f\x7f\u0080\u009f\u00a0\u00ad\u00ff", `'\x00\x1f\x7f\x80\x9f\xa0\xad` + "\u00ff'", `'\x00\x1f\x7f\x80\x9f\xa0\xad\xff'`},
		{"\u0100\u2028\u200b\ufeff\ue000", "'\u0100" + `\u2028\u200b\ufeff\ue000'`, `'\u0100\u2028\u200b\ufeff\ue000'`},
		{"\U0001f600\U000e0001\U0010ffff", "'\U0001f600" + `\U000e0001\U0010ffff'`, `'\U0001f600\U000e0001\U0010ffff'`},
		{"\xed\xb2\x80\xed\xa0\x80", `'\udc80\ud800'`, `'\udc80\ud800'`},
		{"\xff", `'\udcff'`, `'\udcff'`},
		{"\u00e9\u4e2d", "'\u00e9\u4e2d'", `'\xe9\u4e2d'`},
	}
	for _, c := range cases {
		if got := StrRepr(c.in); got != c.repr {
			t.Errorf("StrRepr(%q) = %s want %s", c.in, got, c.repr)
		}
		if got := ASCII(c.in); got != c.ascii {
			t.Errorf("ASCII(%q) = %s want %s", c.in, got, c.ascii)
		}
	}
	if got := BytesRepr([]byte("a'\"\x80\t\\")); got != `b'a\'"\x80\t\\'` {
		t.Errorf("BytesRepr = %s", got)
	}
	if got := BytesRepr([]byte("it's")); got != `b"it's"` {
		t.Errorf("BytesRepr quote = %s", got)
	}
}

func TestReprStrTypeNameTruthy(t *testing.T) {
	big1, _ := new(big.Int).SetString("123456789012345678901234567890", 10)
	ordered := orderedPairs{{"z", 1}, {"a", []any{1.5, nil, true}}}
	cases := []struct {
		v                any
		repr, str, tname string
		truthy           bool
	}{
		{nil, "None", "None", "NoneType", false},
		{true, "True", "True", "bool", true},
		{"x'y", `"x'y"`, "x'y", "str", true},
		{"", "''", "", "str", false},
		{0, "0", "0", "int", false},
		{int64(-7), "-7", "-7", "int", true},
		{uint8(9), "9", "9", "int", true},
		{big1, "123456789012345678901234567890", "123456789012345678901234567890", "int", true},
		{json.Number("12"), "12", "12", "int", true},
		{json.Number("1e2"), "100.0", "100.0", "float", true},
		{1e16, "1e+16", "1e+16", "float", true},
		{math.Copysign(0, -1), "-0.0", "-0.0", "float", false},
		{math.NaN(), "nan", "nan", "float", true},
		{[]byte("ab"), "b'ab'", "b'ab'", "bytes", true},
		{[]any{"a", 1, 2.0}, "['a', 1, 2.0]", "['a', 1, 2.0]", "list", true},
		{[]string{}, "[]", "[]", "list", false},
		{Tuple{"a"}, "('a',)", "('a',)", "tuple", true},
		{Tuple{}, "()", "()", "tuple", false},
		{Tuple{1, "b"}, "(1, 'b')", "(1, 'b')", "tuple", true},
		{ordered, "{'z': 1, 'a': [1.5, None, True]}", "{'z': 1, 'a': [1.5, None, True]}", "dict", true},
		{orderedPairs{}, "{}", "{}", "dict", false},
		{map[string]any{"b": 2, "a": "x"}, "{'a': 'x', 'b': 2}", "{'a': 'x', 'b': 2}", "dict", true},
		{SortedMap(map[string]int{"y": 1, "x": 2}), "{'x': 2, 'y': 1}", "{'x': 2, 'y': 1}", "dict", true},
	}
	for _, c := range cases {
		if got := Repr(c.v); got != c.repr {
			t.Errorf("Repr(%#v) = %s want %s", c.v, got, c.repr)
		}
		if got := Str(c.v); got != c.str {
			t.Errorf("Str(%#v) = %s want %s", c.v, got, c.str)
		}
		if got := TypeName(c.v); got != c.tname {
			t.Errorf("TypeName(%#v) = %s want %s", c.v, got, c.tname)
		}
		if got := Truthy(c.v); got != c.truthy {
			t.Errorf("Truthy(%#v) = %v want %v", c.v, got, c.truthy)
		}
	}
	if got := Str(errors.New("boom")); got != "boom" {
		t.Errorf("Str(error) = %s", got)
	}
}

func TestFormatMatchesCPython(t *testing.T) {
	cases := []struct {
		v          any
		spec, want string
	}{
		{1234567, ",", "1,234,567"}, {1234567, "_x", "12_d687"}, {255, "#06x", "0x00ff"},
		{-42, "+08_d", "-000_042"}, {3.14159, ".2f", "3.14"}, {math.Copysign(0, -1), "", "-0.0"},
		{math.Copysign(0, -1), "z.1f", "0.0"}, {1e16, "", "1e+16"}, {0.5, ".0%", "50%"},
		{1234.5, ">12,.1f", "     1,234.5"}, {1.0, "g", "1"}, {123456789.0, ".3g", "1.23e+08"},
		{1e-5, ".2e", "1.00e-05"}, {2.5, ".0f", "2"}, {math.NaN(), "+f", "+nan"},
		{math.Inf(-1), "08f", "-0000inf"}, {"ab", "^7", "  ab   "}, {true, ">5", "    1"},
		{65, "c", "A"}, {0.1, ".17g", "0.10000000000000001"}, {100, ".2%", "10000.00%"},
		{-1e-9, "z,.3f", "0.000"}, {12, "#o", "0o14"}, {12, "#b", "0b1100"}, {1.5, "#.0e", "2.e+00"},
		{1e22, "n", "1e+22"}, {nil, "", "None"},
		{1e300, "f", "1000000000000000052504760255204420248704468581108159154915854115511802457988908195786371375080447864043704443832883878176942523235360430575644792184786706982848387200926575803737830233794788090059368953234970799945081119038967640880074652742780142494579258788820056842838115669472196386865459400540160.000000"},
	}
	for _, c := range cases {
		got, err := Format(c.v, c.spec)
		if err != nil || got != c.want {
			t.Errorf("Format(%v, %q) = %q, %v want %q", c.v, c.spec, got, err, c.want)
		}
	}
	errCases := []struct {
		v                any
		spec, class, msg string
	}{
		{1, "xx", "ValueError", "Invalid format specifier 'xx' for object of type 'int'"},
		{"a", "xx", "ValueError", "Invalid format specifier 'xx' for object of type 'str'"},
		{1, "z", "ValueError", "Negative zero coercion (z) not allowed in integer format specifier"},
		{"a", "z", "ValueError", "Negative zero coercion (z) not allowed in string format specifier"},
		{"a", "d", "ValueError", "Unknown format code 'd' for object of type 'str'"},
		{nil, "5", "TypeError", "unsupported format string passed to NoneType.__format__"},
		{3, ".2d", "ValueError", "Precision not allowed in integer format specifier"},
	}
	for _, c := range errCases {
		_, err := Format(c.v, c.spec)
		var fe *FormatError
		if !errors.As(err, &fe) || fe.Class != c.class || fe.Msg != c.msg {
			t.Errorf("Format(%v, %q) err %v want %s: %s", c.v, c.spec, err, c.class, c.msg)
		}
	}
	if s := Float(12345678901234567.0); !strings.HasPrefix(s, "1.2345678901234568e+16") {
		t.Errorf("Float = %s", s)
	}
}
