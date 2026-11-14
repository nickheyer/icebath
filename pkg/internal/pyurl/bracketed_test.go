package pyurl

import "testing"

func TestSplitBracketedNetlocMatchesCPython(t *testing.T) {
	cases := []struct{ url, err string }{
		{"http://[::1]:80/", ""},
		{"http://[2606:4700:4700::1111]/", ""},
		{"http://[v1.x]/", ""},
		{"http://[V1f.host]/", ""},
		{"http://[fe80::1%eth0]/", ""},
		{"http://[fe80::1%25 x]/", ""},
		{"http://u[x@[::1]/", ""},
		{"http://[::ffff:1.2.3.4]/", ""},
		{"http://a[::1]/", "Invalid IPv6 URL"},
		{"http://[::1]x/", "Invalid IPv6 URL"},
		{"http://[::1/", "Invalid IPv6 URL"},
		{"http://::1]/", "Invalid IPv6 URL"},
		{"http://[v1.]/", "IPvFuture address is invalid"},
		{"http://[vg.x]/", "IPvFuture address is invalid"},
		{"http://[1.2.3.4]/", "An IPv4 address cannot be in brackets"},
		{"http://[::g]/", "'::g' does not appear to be an IPv4 or IPv6 address"},
		{"http://[fe80::1%]/", "'fe80::1%' does not appear to be an IPv4 or IPv6 address"},
		{"http://[fe80::1%a%b]/", "'fe80::1%a%b' does not appear to be an IPv4 or IPv6 address"},
		{"http://[x@host]/", "'host]' does not appear to be an IPv4 or IPv6 address"},
		{"http://[]/", "'' does not appear to be an IPv4 or IPv6 address"},
	}
	for _, c := range cases {
		_, err := Split(c.url)
		got := ""
		if err != nil {
			got = err.Error()
		}
		if got != c.err {
			t.Errorf("Split(%q) error %q, want %q", c.url, got, c.err)
		}
	}
}

func TestHostnameLowersLikePythonStr(t *testing.T) {
	u, err := Split("http://İX.example/")
	if err != nil {
		t.Fatal(err)
	}
	if got := u.Hostname(); got != "i̇x.example" {
		t.Fatalf("hostname %q", got)
	}
}
