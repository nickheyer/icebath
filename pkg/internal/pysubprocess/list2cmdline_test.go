package pysubprocess

import "testing"

func TestList2cmdlineMatchesCPython(t *testing.T) {
	cases := []struct {
		seq  []string
		want string
	}{
		{[]string{"a b c", "d", "e"}, `"a b c" d e`},
		{[]string{`ab"c`, `\`, "d"}, `ab\"c \ d`},
		{[]string{`ab"c`, ` \`, "d"}, `ab\"c " \\" d`},
		{[]string{`a\\\b`, "de fg", "h"}, `a\\\b "de fg" h`},
		{[]string{`a\"b`, "c", "d"}, `a\\\"b c d`},
		{[]string{`a\\b c`, "d", "e"}, `"a\\b c" d e`},
		{[]string{`a\\b\ c`, "d", "e"}, `"a\\b\ c" d e`},
		{[]string{"ab", ""}, `ab ""`},
		{[]string{"tab\there"}, "\"tab\there\""},
	}
	for _, c := range cases {
		if got := List2cmdline(c.seq); got != c.want {
			t.Errorf("List2cmdline(%q) = %q, want %q", c.seq, got, c.want)
		}
	}
}
