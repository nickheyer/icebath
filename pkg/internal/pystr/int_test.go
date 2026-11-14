package pystr

import "testing"

func TestIntTextMatchesPythonInt(t *testing.T) {
	ok := map[string]string{"3": "3", " -12 ": "-12", "+7": "7", "1_000": "1000", "٣٤": "34", "\U0001D7D9": "1", "007": "7"}
	for in, want := range ok {
		got, valid := IntText(in)
		if !valid || got.String() != want {
			t.Errorf("IntText(%q) = %v %v want %s", in, got, valid, want)
		}
	}
	for _, in := range []string{"", "x", "1.5", "_1", "1_", "1__0", "--1", "0x10", "+"} {
		if _, valid := IntText(in); valid {
			t.Errorf("IntText(%q) accepted", in)
		}
	}
}
