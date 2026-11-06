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
