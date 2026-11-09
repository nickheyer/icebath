package pyunicode

import "testing"

func TestWcsWidthMatchesWcwidth(t *testing.T) {
	cases := []struct {
		in   string
		want int
	}{
		{"", 0},
		{"abc", 3},
		{"配置", 4},
		{"通义 1", 6},
		{"é", 1},
		{"é", 1},
		{"‍", 0},
		{"a\x1bb", -1},
		{"\x7f", -1},
		{"\u0000", 0},
		{"\U0001F600", 2},
		{"⚠️", 1},
	}
	for _, c := range cases {
		if got := WcsWidth(c.in); got != c.want {
			t.Errorf("WcsWidth(%q) = %d, want %d", c.in, got, c.want)
		}
	}
}

func TestWcWidthControlIsNegative(t *testing.T) {
	for _, r := range []rune{0x01, 0x1f, 0x7f, 0x9f} {
		if WcWidth(r) != -1 {
			t.Errorf("WcWidth(%#x) = %d, want -1", r, WcWidth(r))
		}
	}
	for r, want := range map[rune]int{'a': 1, 0xa0: 1, '配': 2, '́': 0} {
		if WcWidth(r) != want {
			t.Errorf("WcWidth(%#x) = %d, want %d", r, WcWidth(r), want)
		}
	}
}
