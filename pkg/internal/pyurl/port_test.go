package pyurl

import "testing"

func TestPortMatchesCPython(t *testing.T) {
	cases := []struct {
		raw     string
		port    int
		present bool
		err     string
	}{
		{"https://example.com/x", 0, false, ""},
		{"https://example.com:/x", 0, false, ""},
		{"https://example.com:8443/x", 8443, true, ""},
		{"https://u:p@example.com:0080", 80, true, ""},
		{"https://u:p:q@example.com", 0, false, ""},
		{"https://[::1]:22/x", 22, true, ""},
		{"https://[::1]/x", 0, false, ""},
		{"https://example.com:65535", 65535, true, ""},
		{"https://example.com:65536", 0, false, "Port out of range 0-65535"},
		{"https://example.com:000000000000000000000001", 1, true, ""},
		{"https://example.com:99999999999999999999", 0, false, "Port out of range 0-65535"},
		{"https://example.com:x1", 0, false, "Port could not be cast to integer value as 'x1'"},
		{"https://example.com:-1", 0, false, "Port could not be cast to integer value as '-1'"},
		{"https://example.com:\xd9\xa1", 0, false, "Port could not be cast to integer value as '\xd9\xa1'"},
	}
	for _, c := range cases {
		u, err := Split(c.raw)
		if err != nil {
			t.Fatalf("%q: %v", c.raw, err)
		}
		port, present, err := u.Port()
		got := ""
		if err != nil {
			got = err.Error()
		}
		if port != c.port || present != c.present || got != c.err {
			t.Errorf("%q: got (%d, %v, %q) want (%d, %v, %q)", c.raw, port, present, got, c.port, c.present, c.err)
		}
	}
}
