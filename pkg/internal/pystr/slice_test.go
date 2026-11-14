package pystr

import "testing"

func TestSliceMatchesPythonSubscript(t *testing.T) {
	cases := []struct {
		s          string
		start, end int
		want       string
	}{
		{"héllo wörld", 0, 5, "héllo"},
		{"héllo", 0, 120, "héllo"},
		{"héllo", -3, 5, "llo"},
		{"héllo", 2, 1, ""},
		{"héllo", -99, 2, "hé"},
		{"", 0, 3, ""},
		{"a\xed\xa0\x80b", 1, 2, "\xed\xa0\x80"},
	}
	for _, c := range cases {
		if got := Slice(c.s, c.start, c.end); got != c.want {
			t.Errorf("Slice(%q, %d, %d) = %q, want %q", c.s, c.start, c.end, got, c.want)
		}
	}
}
