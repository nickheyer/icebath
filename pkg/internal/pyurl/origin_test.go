package pyurl

import "testing"

func TestBaseURLOrigin(t *testing.T) {
	cases := []struct {
		in     string
		scheme string
		host   string
		port   int
	}{
		{"https://Api.Example.com/v1", "https", "api.example.com", 443},
		{"http://h", "http", "h", 80},
		{"https://h:443", "https", "h", 443},
		{"https://h:8443/x", "https", "h", 8443},
		{"h.example.com:9000/v1", "", "h.example.com", 9000},
		{"bare.host.", "", "bare.host", 0},
		{"ws://h", "ws", "h", 0},
		{"https://h:99999", "", "", 0},
		{"https://h:abc", "", "", 0},
		{"", "", "", 0},
		{"   ", "", "", 0},
		{"https:///path", "", "", 0},
	}
	for _, tc := range cases {
		s, h, p, err := BaseURLOrigin(tc.in)
		if err != nil || s != tc.scheme || h != tc.host || p != tc.port {
			t.Errorf("%q: got (%q, %q, %d, %v) want (%q, %q, %d)", tc.in, s, h, p, err, tc.scheme, tc.host, tc.port)
		}
	}
	if _, _, _, err := BaseURLOrigin("http://[::1"); err == nil {
		t.Errorf("unbalanced IPv6 netloc must raise the urlparse ValueError")
	}
}
