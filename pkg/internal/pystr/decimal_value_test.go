package pystr

import "testing"

func TestDecimalValueMatchesPythonInt(t *testing.T) {
	cases := map[rune]int{'0': 0, '7': 7, '٣': 3, '९': 9, '\U0001D7D9': 1, '\U0001D7FF': 9, '５': 5}
	for r, want := range cases {
		if got, ok := DecimalValue(r); !ok || got != want {
			t.Fatalf("%U got %d %v", r, got, ok)
		}
	}
	for _, r := range []rune{'²', 'a', '⅕'} {
		if _, ok := DecimalValue(r); ok {
			t.Fatalf("%U is not a decimal digit", r)
		}
	}
}
