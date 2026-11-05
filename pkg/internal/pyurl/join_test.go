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
