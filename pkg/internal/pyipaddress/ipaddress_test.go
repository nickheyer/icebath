package pyipaddress

import (
	"net/netip"
	"testing"
)

func TestClassification(t *testing.T) {
	cases := []struct {
		ip                           string
		private, loopback, linkLocal bool
	}{
		{"10.1.2.3", true, false, false},
		{"127.0.0.1", true, true, false},
		{"169.254.1.1", true, false, true},
		{"192.0.0.9", false, false, false},
		{"192.0.0.8", true, false, false},
		{"100.64.0.1", false, false, false},
		{"8.8.8.8", false, false, false},
		{"::1", true, true, false},
		{"::ffff:127.0.0.1", true, true, false},
		{"::ffff:8.8.8.8", false, false, false},
		{"fe80::1%eth0", true, false, true},
		{"2001:3::1", false, false, false},
		{"2606:4700::1", false, false, false},
	}
	for _, c := range cases {
		ip, ok := IPAddress(c.ip)
		if !ok {
			t.Fatalf("parse %s", c.ip)
		}
		if IsPrivate(ip) != c.private || IsLoopback(ip) != c.loopback || IsLinkLocal(ip) != c.linkLocal {
			t.Errorf("%s got %v %v %v", c.ip, IsPrivate(ip), IsLoopback(ip), IsLinkLocal(ip))
		}
	}
	for _, bad := range []string{"localhost", "01.2.3.4", "1.2.3.4%x", "[::1]", ""} {
		if _, ok := IPAddress(bad); ok {
			t.Errorf("%q parsed", bad)
		}
	}
}

func TestInetAton(t *testing.T) {
	good := map[string]string{
		"169.254.169.254": "169.254.169.254", "0xa9fea9fe": "169.254.169.254", "2852039166": "169.254.169.254",
		"0251.0376.0251.0376": "169.254.169.254", "169.16689662": "169.254.169.254", "169.254.43518": "169.254.169.254",
		"0": "0.0.0.0", "0x7f.1": "127.0.0.1", "1.2.3.4 trailing": "1.2.3.4",
	}
	for in, want := range good {
		got, ok := InetAton(in)
		if !ok || got != netip.MustParseAddr(want) {
			t.Errorf("%q got %v %v", in, got, ok)
		}
	}
	for _, bad := range []string{"", "1.", "1.2.3.4.", "08", "0x", "256.1.1.1", "1.2.3.256", "4294967296", "a.b", "1..2", "abc"} {
		if _, ok := InetAton(bad); ok {
			t.Errorf("%q accepted", bad)
		}
	}
}
