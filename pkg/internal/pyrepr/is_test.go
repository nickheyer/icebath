package pyrepr

import "testing"

func TestIs(t *testing.T) {
	type box struct{ n int }
	p, q := &box{1}, &box{1}
	m1, m2 := map[string]any{}, map[string]any{}
	cases := []struct {
		a, b any
		want bool
	}{
		{nil, nil, true}, {p, p, true}, {p, q, false}, {m1, m1, true}, {m1, m2, false},
		{nil, p, false}, {"x", "x", true}, {1, int64(1), false}, {[]int{1}, []int{1}, false},
	}
	for i, c := range cases {
		if got := Is(c.a, c.b); got != c.want {
			t.Fatalf("case %d got %v", i, got)
		}
	}
}
