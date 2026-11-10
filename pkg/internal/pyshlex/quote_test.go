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

func TestJoinMatchesShlexJoin(t *testing.T) {
	cases := []struct {
		args []string
		want string
	}{
		{nil, ""},
		{[]string{"/usr/bin/python3", "-m", "playwright", "install-deps", "chromium"}, "/usr/bin/python3 -m playwright install-deps chromium"},
		{[]string{"/opt/my env/python", "-c", "print('x')"}, `'/opt/my env/python' -c 'print('"'"'x'"'"')'`},
		{[]string{"", "a b"}, "'' 'a b'"},
	}
	for _, c := range cases {
		if got := Join(c.args); got != c.want {
			t.Errorf("Join(%q) = %q, want %q", c.args, got, c.want)
		}
	}
}
