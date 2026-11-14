package pyshlex

import (
	"reflect"
	"testing"
)

func TestSplitPOSIXPunctuationMatchesShlexPunctuationChars(t *testing.T) {
	cases := []struct {
		in   string
		want []string
		err  string
	}{
		{"cd backend && ls", []string{"cd", "backend", "&&", "ls"}, ""},
		{"echo start; cd backend; ls", []string{"echo", "start", ";", "cd", "backend", ";", "ls"}, ""},
		{"cd backend;ls", []string{"cd", "backend", ";", "ls"}, ""},
		{"cd 'backend;'", []string{"cd", "backend;"}, ""},
		{"a|&b", []string{"a", "|&", "b"}, ""},
		{"(cd x)", []string{"(", "cd", "x", ")"}, ""},
		{"a>>b", []string{"a", ">>", "b"}, ""},
		{"a#b c", []string{"a"}, ""},
		{"a&&#c\nb", []string{"a", "&&", "b"}, ""},
		{"# full\ncd -P /tmp", []string{"cd", "-P", "/tmp"}, ""},
		{`x "a\"b" '' ""`, []string{"x", `a"b`, "", ""}, ""},
		{`a\ b "q\x" &"z"`, []string{"a b", `q\x`, "&", "z"}, ""},
		{`tail\`, nil, "No escaped character"},
		{`"open`, nil, "No closing quotation"},
		{"", nil, ""},
	}
	for _, c := range cases {
		got, err := SplitPOSIXPunctuation(c.in)
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
