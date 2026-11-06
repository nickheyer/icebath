package pyshlex

import (
	"reflect"
	"testing"
)

func TestSplitPOSIXMatchesShlexSplitWithComments(t *testing.T) {
	cases := []struct {
		in   string
		want []string
		err  string
	}{
		{"a b  c", []string{"a", "b", "c"}, ""},
		{`exec '/x y/icebath' acp "$@"`, []string{"exec", "/x y/icebath", "acp", "$@"}, ""},
		{"a#b c", []string{"a"}, ""},
		{"# full\nexec /r/icebath", []string{"exec", "/r/icebath"}, ""},
		{`x "a\"b" '' ""`, []string{"x", `a"b`, "", ""}, ""},
		{`a\ b`, []string{"a b"}, ""},
		{`"q\x"`, []string{`q\x`}, ""},
		{`tail\`, nil, "No escaped character"},
		{`"open`, nil, "No closing quotation"},
		{"", nil, ""},
	}
	for _, c := range cases {
		got, err := SplitPOSIX(c.in, true)
		if c.err != "" {
			if err == nil || err.Error() != c.err {
				t.Fatalf("%q err %v want %s", c.in, err, c.err)
			}
			continue
		}
		if err != nil || !reflect.DeepEqual(got, c.want) {
			t.Fatalf("%q got %q %v want %q", c.in, got, err, c.want)
		}
	}
}
