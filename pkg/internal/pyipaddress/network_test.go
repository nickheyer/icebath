package pyipaddress

import (
	"net/netip"
	"testing"
)

func TestIPNetworkMatchesCPython(t *testing.T) {
	cases := map[string]string{
		"10.0.0.0/8": "10.0.0.0/8", "10.1.2.3/8": "10.0.0.0/8", "10.0.0.0/255.0.0.0": "10.0.0.0/8",
		"10.0.0.0/0.255.255.255": "10.0.0.0/8", "10.0.0.0/+8": "", "10.0.0.0/ 8": "", "::1/128": "::1/128",
		"fe80::1%eth0/64": "fe80::/64", "1.2.3.4": "1.2.3.4/32", "1.2.3.4/33": "", "a/b/c": "", "": "",
		"10.0.0.0/255.0.255.0": "", "10.0.0.0/0.0.0.0": "0.0.0.0/0", "10.0.0.0/255.255.255.255": "10.0.0.0/32",
		"::ffff:1.2.3.4/96": "::ffff:0.0.0.0/96", "01.2.3.4/32": "", "::/0": "::/0", "2001:db8::/ffff::": "",
	}
	for input, want := range cases {
		got, ok := IPNetwork(input, false)
		if want == "" {
			if ok {
				t.Errorf("IPNetwork(%q) = %v, want ValueError", input, got)
			}
			continue
		}
		if !ok || got.String() != want {
			t.Errorf("IPNetwork(%q) = %v %v, want %s", input, got, ok, want)
		}
	}
	if _, ok := IPNetwork("10.1.2.3/8", true); ok {
		t.Error("strict network with host bits must be refused")
	}
	linkLocal, _ := IPNetwork("fe80::/10", false)
	if !NetworkContains(linkLocal, netip.MustParseAddr("fe80::1%eth0")) {
		t.Error("a scoped address belongs to its network")
	}
	private, _ := IPNetwork("10.0.0.0/8", false)
	if NetworkContains(private, netip.MustParseAddr("::ffff:10.0.0.1")) {
		t.Error("a mapped address is not in an IPv4 network")
	}
}
