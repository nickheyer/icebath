package pyshlex

import "testing"

func TestQuoteMatchesShlexQuote(t *testing.T) {
	cases := map[string]string{
		"":                "''",
		"plain/path-1.0":  "plain/path-1.0",
		"a b":             "'a b'",
		"it's":            `'it'"'"'s'`,
		"/s/old\nname":    "'/s/old\nname'",
		"caf\u00e9":       "'caf\u00e9'",
		"k=v,x:y@h%+":     "k=v,x:y@h%+",
		"$HOME":           "'$HOME'",
		"/tmp/x:/s":       "/tmp/x:/s",
		"semi;colon":      "'semi;colon'",
		"under_score.TXT": "under_score.TXT",
	}
	for in, want := range cases {
		if got := Quote(in); got != want {
			t.Errorf("Quote(%q) = %q, want %q", in, got, want)
		}
	}
}
