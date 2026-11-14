package pyint

import "testing"

func TestParseBaseMatchesPythonInt(t *testing.T) {
	cases := []struct {
		text string
		base int
		want int64
		ok   bool
	}{
		{"1f", 16, 31, true},
		{" 0x1F ", 16, 31, true},
		{"0x_1f", 16, 31, true},
		{"-a_b", 16, -171, true},
		{"deadbeef", 16, 0xdeadbeef, true},
		{"١٢", 10, 12, true},
		{"0b101", 2, 5, true},
		{"0x", 16, 0, false},
		{"", 16, 0, false},
		{"1__f", 16, 0, false},
		{"1f_", 16, 0, false},
		{"g", 16, 0, false},
		{"0o7", 16, 0, false},
		{"12", 1, 0, false},
	}
	for _, c := range cases {
		got, ok := ParseBase(c.text, c.base)
		if ok != c.ok || (ok && got.Int64() != c.want) {
			t.Errorf("ParseBase(%q, %d) = %v %v, want %d %v", c.text, c.base, got, ok, c.want, c.ok)
		}
	}
}
