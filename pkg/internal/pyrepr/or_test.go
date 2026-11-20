package pyrepr

import "testing"

func TestOrReturnsTheFirstTruthyOperand(t *testing.T) {
	for _, c := range []struct{ v, def, want any }{
		{"x", "d", "x"}, {"", "d", "d"}, {nil, "d", "d"}, {int64(0), int64(5), int64(5)},
		{int64(3), int64(5), int64(3)}, {0.0, 1.5, 1.5}, {[]any{}, "d", "d"}, {false, true, true},
	} {
		if got := Or(c.v, c.def); got != c.want {
			t.Fatalf("Or(%#v, %#v) = %#v", c.v, c.def, got)
		}
	}
	list := []any{1}
	if got := Or(list, "d").([]any); len(got) != 1 {
		t.Fatalf("non empty list %#v", got)
	}
}
