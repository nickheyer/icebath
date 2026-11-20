package pystr

import "testing"

func TestStrIsDigitMatchesCPython(t *testing.T) {
	cases := map[string]bool{"": false, "123": true, "12a": false, "²": true, "٣": true, " 1": false, "-1": false, "⅕": false}
	for in, want := range cases {
		if got := StrIsDigit(in); got != want {
			t.Fatalf("%q -> %v want %v", in, got, want)
		}
	}
}
