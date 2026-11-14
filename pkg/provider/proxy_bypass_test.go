package provider

import "testing"

func TestNoProxyEntriesMatchTheSharedMatcher(t *testing.T) {
	for _, tc := range []struct{ noProxy, host string }{
		{"10.0.0.0/8", "10.1.2.3"}, {"*.internal", "svc.internal"}, {"*.slack.com", "slack.com"}, {".slack.com", "slack.com"},
		{"localhost,.corp.example", "llm.corp.example"}, {"api.example.com:8443", "api.example.com:8443"},
	} {
		value := tc.noProxy
		if !ShouldBypassProxy([]string{tc.host}, &value) || !ShouldBypassProxy([]string{"https://" + tc.host + "/v1"}, &value) {
			t.Fatalf("%s must bypass for %s", tc.host, tc.noProxy)
		}
	}
	value := "10.0.0.0/8,*.internal"
	if ShouldBypassProxy([]string{"https://api.openai.com/v1"}, &value) || ShouldBypassProxy([]string{"api.telegram.org"}, &value) {
		t.Fatal("non matching hosts keep the proxy")
	}
	slack := "*.slack.com"
	if ShouldBypassProxy([]string{"notslack.com"}, &slack) {
		t.Fatal("suffix match must respect the dot boundary")
	}
}

func TestMalformedPortKeepsTheProxyInsteadOfRaising(t *testing.T) {
	other := "other.example"
	if ShouldBypassProxy([]string{"http://host:notaport/v1"}, &other) || ShouldBypassProxy([]string{"http://host:99999/v1"}, &other) {
		t.Fatal("unrelated entry never matches")
	}
	host := "host"
	if !ShouldBypassProxy([]string{"http://host:notaport/v1"}, &host) {
		t.Fatal("the host still matches when its port is malformed")
	}
}

func TestSplitHostPortShapes(t *testing.T) {
	for raw, want := range map[string]struct {
		host    string
		port    int
		hasPort bool
	}{
		"https://Example.com:8443/path": {"example.com", 8443, true}, "//host/path": {"host", 0, false}, "[::1]:80": {"::1", 80, true},
		"[::1]": {"::1", 0, false}, "host:12": {"host", 12, true}, "host.": {"host", 0, false}, "": {"", 0, false},
	} {
		host, port, hasPort := SplitHostPort(raw)
		if host != want.host || port != want.port || hasPort != want.hasPort {
			t.Fatalf("%q: got %q %d %t", raw, host, port, hasPort)
		}
	}
}

func TestLoopbackHelpers(t *testing.T) {
	for host, want := range map[string]bool{"localhost": true, "127.0.0.1": true, "127.8.8.8": true, "::1": true, "[::1]": true, "::ffff:127.0.0.1": true, "0.0.0.0": false, "10.0.0.1": false, "example.com": false} {
		if IsProxyLoopbackHost(host) != want {
			t.Fatalf("%s: want %t", host, want)
		}
	}
	if !LoopbackBypassesProxy("ws://127.0.0.1:9222/devtools") || LoopbackBypassesProxy("wss://remote.example/x") {
		t.Fatal("loopback dials skip proxy detection")
	}
	env := AddLoopbackNoProxy(map[string]string{"NO_PROXY": "corp.example"})
	if env["NO_PROXY"] != "corp.example,127.0.0.1,localhost,::1" || env["no_proxy"] != "127.0.0.1,localhost,::1" {
		t.Fatalf("got %v", env)
	}
	star := AddLoopbackNoProxy(map[string]string{"no_proxy": "*"})
	if star["no_proxy"] != "*" {
		t.Fatal("an operator wildcard stays the wildcard")
	}
}

func TestFirstProxyEnvValue(t *testing.T) {
	for _, k := range ProxyEnvKeys {
		t.Setenv(k, "")
	}
	t.Setenv("http_proxy", " http://lower:3128 ")
	if FirstProxyEnvValue() != "http://lower:3128" {
		t.Fatal("any casing is read")
	}
	t.Setenv("HTTPS_PROXY", "http://upper:3128")
	if FirstProxyEnvValue() != "http://upper:3128" {
		t.Fatal("HTTPS_PROXY wins")
	}
}

func TestNoProxyEntryMatchesCIDRAndMappedV6(t *testing.T) {
	if !NoProxyEntryMatches("169.254.0.0/16", "::ffff:169.254.169.254", 0, false) {
		t.Fatal("a v4 mapped v6 literal matches the v4 network")
	}
	if NoProxyEntryMatches("example.com:443", "example.com", 0, false) || !NoProxyEntryMatches("example.com:443", "example.com", 443, true) {
		t.Fatal("a port qualified entry needs the port")
	}
}
