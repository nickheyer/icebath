package hermetic

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestBlockExternalHTTPRefusesOtherHostsAndServesLoopback(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { io.WriteString(w, "local") }))
	defer srv.Close()
	BlockExternalHTTP()
	BlockExternalHTTP()
	clone := http.DefaultTransport.(*http.Transport).Clone()
	for name, client := range map[string]*http.Client{"default": {}, "clone": {Transport: clone}} {
		resp, err := client.Get(srv.URL)
		if err != nil {
			t.Fatalf("%s client: loopback refused: %v", name, err)
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if string(body) != "local" {
			t.Fatalf("%s client: loopback body %q", name, body)
		}
		for url, addr := range map[string]string{"http://example.invalid/x": "example.invalid:80", "https://192.0.2.1/x": "192.0.2.1:443", "http://10.0.0.1:8080/": "10.0.0.1:8080"} {
			if _, err := client.Get(url); err == nil || !strings.Contains(err.Error(), "network disabled in tests: "+addr) {
				t.Fatalf("%s client: %s answered %v", name, url, err)
			}
		}
	}
	for host, want := range map[string]bool{"localhost": true, "127.0.0.1": true, "::1": true, "example.com": false, "": false, "192.168.1.5": false} {
		if loopbackHost(host) != want {
			t.Fatalf("loopbackHost(%q) = %v", host, !want)
		}
	}
}
