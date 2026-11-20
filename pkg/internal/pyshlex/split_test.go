package pyshlex

import (
	"errors"
	"reflect"
	"testing"
)

func TestSplitNonPOSIXMatchesCPython(t *testing.T) {
	cases := []struct {
		in   string
		want []string
	}{
		{"", nil},
		{"  a  b\tc\n", []string{"a", "b", "c"}},
		{`"C:\Program Files\x.exe" -m mod`, []string{`"C:\Program Files\x.exe"`, "-m", "mod"}},
		{`foo"bar baz" q`, []string{`foo"bar`, `baz"`, "q"}},
		{`"abc"def`, []string{`"abc"`, "def"}},
		{`'it''s' #x`, []string{`'it'`, `'s'`, "#x"}},
		{`a\ b`, []string{`a\`, "b"}},
	}
	for _, c := range cases {
		got, err := SplitNonPOSIX(c.in)
		if err != nil || !reflect.DeepEqual(got, c.want) {
			t.Errorf("SplitNonPOSIX(%q) = %q, %v want %q", c.in, got, err, c.want)
		}
	}
	if _, err := SplitNonPOSIX(`a "open`); !errors.Is(err, ErrNoClosingQuotation) {
		t.Errorf("unterminated quote error = %v", err)
	}
}
