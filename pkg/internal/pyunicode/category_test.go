package pyunicode

import "testing"

func TestCategoryMatchesUnicodedata(t *testing.T) {
	cases := map[rune]string{'A': "Lu", 'a': "Ll", '5': "Nd", ' ': "Zs", '\n': "Cc", '​': "Cf", '': "Co", 0xd800: "Cs", 0x0378: "Cn", 0xfdd0: "Cn", '́': "Mn", '-': "Pd", '$': "Sc"}
	for r, want := range cases {
		if got := Category(r); got != want {
			t.Errorf("Category(%U) = %s, want %s", r, got, want)
		}
	}
}
