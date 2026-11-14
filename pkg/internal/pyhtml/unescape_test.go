package pyhtml

import "testing"

func TestUnescapeMatchesPython(t *testing.T) {
	cases := map[string]string{
		"plain":                   "plain",
		"&amp;&lt;&gt;":           "&<>",
		"&amp":                    "&",
		"&notit;":                 "¬it;",
		"&ampx":                   "&x",
		"&nGt;":                   "≫⃒",
		"&#65;&#x42;&#X43":        "ABC",
		"&#0;":                    "�",
		"&#x80;":                  "€",
		"&#1;a":                   "a",
		"&#xD800;":                "�",
		"&#99999999999999999999;": "�",
		"&#xfffe;":                "",
		"&#x;":                    "&#x;",
		"&bogus;":                 "&bogus;",
		"&#13;":                   "\r",
		"a & b":                   "a & b",
	}
	for in, want := range cases {
		if got := Unescape(in); got != want {
			t.Errorf("Unescape(%q) = %q, want %q", in, got, want)
		}
	}
	if len(html5) != 2231 {
		t.Fatalf("html5 table has %d names, Python has 2231", len(html5))
	}
}

// The longest table name wins, legacy names match without a semicolon and unknown names match nothing
func TestLongestEntity(t *testing.T) {
	cases := []struct {
		in   string
		want string
		n    int
	}{
		{"amp;rest", "&", 4},
		{"ampx", "&", 3},
		{"notin;", "∉", 6},
		{"notit;", "¬", 3},
		{"nGt;", "≫⃒", 4},
		{"zzz;", "", 0},
		{"", "", 0},
		{"CounterClockwiseContourIntegral;x", "∳", 32},
	}
	for _, c := range cases {
		if v, n := LongestEntity(c.in); v != c.want || n != c.n {
			t.Errorf("LongestEntity(%q) = %q, %d, want %q, %d", c.in, v, n, c.want, c.n)
		}
	}
}
