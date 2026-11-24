package pystr

import (
	"testing"
)

// A surrogate written in three bytes and a stray byte each count as one character
func TestLen(t *testing.T) {
	if got := Len("a\xed\xa0\x80\xff"); got != 3 {
		t.Errorf("len %d", got)
	}
}

// Case mappings are the full Unicode mappings CPython applies
func TestCaseMappings(t *testing.T) {
	cases := []struct{ got, want string }{
		{Casefold("Straße"), "strasse"},
		{Casefold("ᎠꭰᏸᏰ"), "ᎠᎠᏰᏰ"},
		{Casefold("İﬓᾀ\u212a"), "i̇մնἀιk"},
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
	if !IsDigit('²') || !All("١٢", IsDigit) || All("", IsDigit) {
		t.Errorf("digits")
	}
	for s, want := range map[string]bool{"_a1": true, "1a": false, "ä": true, "a-b": false, "": false, "℘x": true, "à": true, "̀a": false} {
		if IsIdentifier(s) != want {
			t.Errorf("isidentifier %q", s)
		}
	}
	if !IsAlnum('½') || IsAlnum('-') || IsPrintable(' ') || !IsPrintable(' ') {
		t.Errorf("classes")
	}
}
