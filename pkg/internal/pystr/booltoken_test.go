package pystr

import "testing"

func TestBoolToken(t *testing.T) {
	cases := []struct {
		in        string
		value, ok bool
	}{
		{"1", true, true}, {"true", true, true}, {" YES ", true, true}, {"On", true, true},
		{"0", false, true}, {"FALSE", false, true}, {"no", false, true}, {"\toff\n", false, true},
		{"", false, false}, {"maybe", false, false}, {"2", false, false}, {" yes ", true, true},
		{"None", false, false}, {"t", false, false},
	}
	for _, c := range cases {
		v, ok := BoolToken(c.in)
		if v != c.value || ok != c.ok {
			t.Errorf("BoolToken(%q) = %v, %v want %v, %v", c.in, v, ok, c.value, c.ok)
		}
	}
}
