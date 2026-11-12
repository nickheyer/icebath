package app

import "testing"

func TestIntOrParsesLikePythonInt(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	cases := []struct {
		in   any
		want int
	}{
		{"1_000", 1000}, {" -7 ", -7}, {"+5", 5}, {"٣", 3}, {"007", 7},
		{"1__0", -1}, {"_1", -1}, {"1_", -1}, {"3.5", -1}, {"- 5", -1}, {"", -1}, {nil, -1}, {12, 12},
	}
	for _, c := range cases {
		if got := intOr(c.in, -1); got != c.want {
			t.Fatalf("intOr(%#v) = %d, want %d", c.in, got, c.want)
		}
	}
}
