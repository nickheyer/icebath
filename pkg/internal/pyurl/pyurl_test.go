package pyurl

import (
	"errors"
	"testing"
)

func TestParse(t *testing.T) {
	cases := []struct {
		raw  string
		want URL
	}{
		{"HTTP://Host:80/a/b;p?q=1#frag", URL{Scheme: "http", Netloc: "Host:80", Path: "/a/b", Params: "p", Query: "q=1", Fragment: "frag"}},
		{" \x01\tdata:image/png;base64,AA", URL{Scheme: "data", Path: "image/png;base64,AA"}},
		{"/tmp/x;y.png", URL{Path: "/tmp/x", Params: "y.png"}},
		{"C:\\dir\\x.png", URL{Scheme: "c", Path: "\\dir\\x.png"}},
		{"1abc:x", URL{Path: "1abc:x"}},
		{"//[::1]:8/p", URL{Netloc: "[::1]:8", Path: "/p"}},
	}
	for _, c := range cases {
		got, err := Parse(c.raw)
		if err != nil || got != c.want {
			t.Errorf("Parse(%q) = %+v %v want %+v", c.raw, got, err, c.want)
		}
	}
	for _, bad := range []string{"//[::1/p", "http://a]/", "//[zz]/"} {
		if _, err := Split(bad); err == nil {
			t.Errorf("Split(%q) accepted", bad)
		}
	}
	if _, err := Split("//[::1/p"); !errors.Is(err, ErrInvalidIPv6) {
		t.Errorf("unbalanced bracket error %v", err)
	}
}
