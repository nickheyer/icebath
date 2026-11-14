package pyurl

import (
	"net/http"
	"testing"
)

func TestFormatURLHostMatchesHermes(t *testing.T) {
	for in, want := range map[string]string{
		"127.0.0.1": "127.0.0.1", " ::1 ": "[::1]", "[::1]": "[::1]", "fe80::1%eth0": "[fe80::1%25eth0]",
		"[fe80::1%eth0]": "[fe80::1%25eth0]", "[host]": "[host]", "example.com": "example.com",
	} {
		if got := FormatURLHost(in); got != want {
			t.Errorf("FormatURLHost(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestEnvironProxiesMatchesUrllib(t *testing.T) {
	got := EnvironProxies([]string{"HTTP_PROXY=http://upper:1", "http_proxy=http://lower:2", "NO_PROXY=", "no_proxy=local", "FTP_PROXY=ftp://x"})
	if got["http"] != "http://lower:2" || got["no"] != "local" || got["ftp"] != "ftp://x" {
		t.Fatalf("proxies = %v", got)
	}
	cgi := EnvironProxies([]string{"REQUEST_METHOD=GET", "HTTP_PROXY=http://upper:1"})
	if _, ok := cgi["http"]; ok {
		t.Fatalf("CGI request kept the uppercase HTTP_PROXY: %v", cgi)
	}
	if lower := EnvironProxies([]string{"REQUEST_METHOD=GET", "http_proxy=http://lower:2"}); lower["http"] != "http://lower:2" {
		t.Fatalf("CGI request dropped the lowercase http_proxy: %v", lower)
	}
}

func TestProxyBypassEnvironmentMatchesUrllib(t *testing.T) {
	proxies := map[string]string{"no": " .Example.com, localhost ,,127.0.0.1"}
	for host, want := range map[string]bool{
		"example.com": true, "api.example.com:443": true, "LOCALHOST:8080": true, "127.0.0.1:9231": true,
		"127.0.0.2": false, "notexample.com": false, "localhost:": true,
	} {
		if got := ProxyBypassEnvironment(host, proxies); got != want {
			t.Errorf("ProxyBypassEnvironment(%q) = %v, want %v", host, got, want)
		}
	}
	if ProxyBypassEnvironment("example.com", map[string]string{}) || !ProxyBypassEnvironment("x", map[string]string{"no": "*"}) {
		t.Fatal("missing no_proxy must not bypass and * must bypass everything")
	}
}

func TestUrlopenProxyKeepsLoopbackOnTheProxy(t *testing.T) {
	t.Setenv("http_proxy", "proxy.internal:3128")
	t.Setenv("no_proxy", "")
	t.Setenv("NO_PROXY", "")
	req, _ := http.NewRequest("POST", "http://127.0.0.1:9231/api", nil)
	u, err := UrlopenProxy(req)
	if err != nil || u == nil || u.String() != "http://proxy.internal:3128" {
		t.Fatalf("proxy = %v, %v", u, err)
	}
	t.Setenv("no_proxy", "127.0.0.1")
	if u, _ := UrlopenProxy(req); u != nil {
		t.Fatalf("no_proxy host still proxied through %v", u)
	}
}
