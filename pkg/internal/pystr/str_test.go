package pystr

import (
	"reflect"
	"testing"
)

// Whitespace stripping and splitting use the CPython isspace set including the ASCII separators
func TestStripAndSplit(t *testing.T) {
	if got := Strip("\x1c a b　\u0085"); got != "a b" {
		t.Errorf("strip %q", got)
	}
	if got := StripChars("xxaxyx", "xy"); got != "a" {
		t.Errorf("strip chars %q", got)
	}
	if got := SplitWhitespace("  a b  c  ", 1); !reflect.DeepEqual(got, []string{"a", "b  c  "}) {
		t.Errorf("split max %q", got)
	}
	if got := SplitWhitespace(" a\x1fb ", -1); !reflect.DeepEqual(got, []string{"a", "b"}) {
		t.Errorf("split %q", got)
	}
	if got, _ := Split("a,b,c", ",", 1); !reflect.DeepEqual(got, []string{"a", "b,c"}) {
		t.Errorf("split sep %q", got)
	}
	if _, err := Split("a", "", -1); err != ErrEmptySeparator {
		t.Errorf("empty separator %v", err)
	}
	if got := Splitlines("a\r\nb c\x1d", false); !reflect.DeepEqual(got, []string{"a", "b", "c"}) {
		t.Errorf("splitlines %q", got)
	}
	if got := Splitlines("a\r\nb\n", true); !reflect.DeepEqual(got, []string{"a\r\n", "b\n"}) {
		t.Errorf("splitlines keepends %q", got)
	}
	if got := Len("a\xed\xa0\x80\xff"); got != 3 {
		t.Errorf("len %d", got)
	}
}

// Case mappings are the full Unicode mappings CPython applies
func TestCaseMappings(t *testing.T) {
	cases := []struct{ got, want string }{
		{Lower("ΑΣ İ"), "ας i̇"},
		{Upper("straße"), "STRASSE"},
		{Casefold("Straße"), "strasse"},
		{Title("hello wORLD's ǆx ßa"), "Hello World'S ǅx Ssa"},
		{Title("ΑΣ"), "Ας"},
		{Capitalize("hELLO"), "Hello"},
	}
	for _, c := range cases {
		if c.got != c.want {
			t.Errorf("got %q want %q", c.got, c.want)
		}
	}
}

// Character class predicates follow the str methods
func TestPredicates(t *testing.T) {
	if !IsDigit('²') || IsDecimal('²') || !All("١٢", IsDecimal) || All("", IsDigit) {
		t.Errorf("digits")
	}
	for s, want := range map[string]bool{"_a1": true, "1a": false, "ä": true, "a-b": false, "": false, "℘x": true, "à": true, "̀a": false} {
		if IsIdentifier(s) != want {
			t.Errorf("isidentifier %q", s)
		}
	}
	if !IsAlnum('½') || IsAlnum('-') || !IsSpace('\x1f') || IsPrintable(' ') || !IsPrintable(' ') {
		t.Errorf("classes")
	}
}
