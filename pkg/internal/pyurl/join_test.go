package pyurl

import "testing"

func TestJoinMatchesCPython(t *testing.T) {
	cases := [][3]string{
		{"https://a.com/x/y?q=1#f", "/z", "https://a.com/z"},
		{"https://a.com/x/y", "../w", "https://a.com/w"},
		{"https://a.com/x/y/", "./", "https://a.com/x/y/"},
		{"https://a.com/x/y?q#f", "", "https://a.com/x/y?q#f"},
		{"https://a.com/x/y?q#f", "?r", "https://a.com/x/y?r"},
		{"https://a.com/x/y?q#f", "#g", "https://a.com/x/y?q#g"},
		{"https://a.com/x", "//b.org/p", "https://b.org/p"},
		{"https://a.com/x", "http://c.net/", "http://c.net/"},
		{"https://a.com/a/b/c", "../../../..", "https://a.com/"},
		{"https://a.com/a//b/c", "d//e", "https://a.com/a/b/d/e"},
		{"https://a.com", "p", "https://a.com/p"},
		{"https://a.com/x", "mailto:z", "mailto:z"},
		{"https://a.com/x", "https:rel", "https://a.com/rel"},
		{"https://a.com/x", "///abs", "https://a.com/abs"},
		{"https://a.com/x?", ".", "https://a.com/"},
		{"http://h/a/b", "g;x?y#s", "http://h/a/g;x?y#s"},
	}
	for _, c := range cases {
		got, err := Join(c[0], c[1])
		if err != nil || got != c[2] {
			t.Errorf("Join(%q, %q) = %q, %v want %q", c[0], c[1], got, err, c[2])
		}
	}
}

func TestUnparseAndHostnameMatchCPython(t *testing.T) {
	cases := [][4]string{
		{"https://H.com/a;p?q#f", "https://H.com/a;p?q#f", "https://H.com/a;p?q", "h.com"},
		{"https://h.com/a?", "https://h.com/a", "https://h.com/a", "h.com"},
		{"http:/x", "http:///x", "http:///x", ""},
		{"https://u@[::1%eth0]:8/x", "https://u@[::1%eth0]:8/x", "https://u@[::1%eth0]:8/x", "::1%eth0"},
		{"mailto:a@b", "mailto:a@b", "mailto:a@b", ""},
	}
	for _, c := range cases {
		u, err := Parse(c[0])
		if err != nil {
			t.Fatal(err)
		}
		noFrag := u
		noFrag.Fragment = ""
		if Unparse(u) != c[1] || Unparse(noFrag) != c[2] || u.Hostname() != c[3] {
			t.Errorf("%q gave %q %q %q", c[0], Unparse(u), Unparse(noFrag), u.Hostname())
		}
	}
	for _, c := range []struct {
		url, domain string
		want        bool
	}{{"https://raw.githubusercontent.com/a", "raw.githubusercontent.com", true}, {"x.raw.githubusercontent.com/p", "raw.githubusercontent.com", true},
		{"https://evil.com/raw.githubusercontent.com", "raw.githubusercontent.com", false}, {"", "a.com", false}} {
		if got, err := BaseURLHostMatches(c.url, c.domain); err != nil || got != c.want {
			t.Errorf("BaseURLHostMatches(%q) = %v %v", c.url, got, err)
		}
	}
}

func TestQuoteMatchesCPython(t *testing.T) {
	for in, want := range map[string]string{"references/a b.md": "references/a%20b.md", "x!$&'()*+,;=:@~é": "x%21%24%26%27%28%29%2A%2B%2C%3B%3D%3A%40~%C3%A9"} {
		if got := Quote(in, "/"); got != want {
			t.Errorf("Quote(%q) = %q want %q", in, got, want)
		}
	}
}
