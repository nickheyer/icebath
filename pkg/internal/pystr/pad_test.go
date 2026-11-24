package pystr

import (
	"testing"
)

func TestCenter(t *testing.T) {
	cases := []struct{ got, want string }{
		{Center("abc", 6), " abc  "},
		{Center("ab", 5), "  ab "},
		{Center("abc", 8), "  abc   "},
		{Center("a", 4), " a  "},
		{Center("ab", 7), "   ab  "},
		{Center("toolong", 3), "toolong"},
	}
	for i, c := range cases {
		if c.got != c.want {
			t.Errorf("case %d got %q want %q", i, c.got, c.want)
		}
	}
}
