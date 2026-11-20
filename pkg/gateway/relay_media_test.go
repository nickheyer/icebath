package gateway

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// Loopback server answering every GET with the body and headers, recording each request header set
func relayMediaServer(t *testing.T, body []byte, headers map[string]string) (*httptest.Server, func() []http.Header) {
	t.Helper()
	var mu sync.Mutex
	var seen []http.Header
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		seen = append(seen, r.Header.Clone())
		mu.Unlock()
		for k, v := range headers {
			w.Header().Set(k, v)
		}
		w.Write(body)
	}))
	t.Cleanup(srv.Close)
	return srv, func() []http.Header {
		mu.Lock()
		defer mu.Unlock()
		return append([]http.Header(nil), seen...)
	}
}

func TestClientUploadRejectsOversizeAndMissing(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	tmp := t.TempDir()
	c := NewRelayMediaClient("https://c.example", "gw1", "sec")
	if ref, err := c.Upload(context.Background(), filepath.Join(tmp, "nope.bin"), nil, nil); ref != "" || err != nil {
		t.Fatalf("missing file upload = %q, %v", ref, err)
	}
	empty := filepath.Join(tmp, "empty.bin")
	if err := os.WriteFile(empty, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if ref, err := c.Upload(context.Background(), empty, nil, nil); ref != "" || err != nil {
		t.Fatalf("empty file upload = %q, %v", ref, err)
	}
}

func TestDownloadSendsAUserAgentOnEveryRequest(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	t.Setenv("TMPDIR", t.TempDir())
	srv, seen := relayMediaServer(t, []byte("OggS"), map[string]string{"Content-Type": "audio/ogg", "Content-Length": "4"})
	c := NewRelayMediaClient(srv.URL, "gw1", "sec")
	for _, u := range []string{srv.URL + "/attachments/1/2/v.ogg", srv.URL + "/relay/media/deadbeef"} {
		if path, err := c.Download(context.Background(), u, nil); path == "" || err != nil {
			t.Fatalf("download %s = %q, %v", u, path, err)
		}
	}
	got := seen()
	if len(got) != 2 {
		t.Fatalf("requests = %d", len(got))
	}
	for _, h := range got {
		ua := h.Get("User-Agent")
		if ua == "" || strings.Contains(strings.ToLower(ua), "python-urllib") {
			t.Fatalf("user agent %q", ua)
		}
	}
	if !strings.HasPrefix(got[1].Get("Authorization"), "Bearer ") {
		t.Fatalf("rehost request lacks its bearer: %v", got[1])
	}
}

func TestIsRelayMediaURLDistinguishesRehostFromPublic(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	c := NewRelayMediaClient("https://conn.example", "gw1", "sec")
	for u, want := range map[string]bool{
		"https://conn.example/relay/media/aa11":            true,
		"http://other.host:8080/relay/media/x":             true,
		"https://cdn.discordapp.com/attachments/1/2/v.ogg": false,
		"https://conn.example/relay/mediafile":             false,
		"":                                                 false,
	} {
		if got := c.IsRelayMediaURL(u); got != want {
			t.Errorf("IsRelayMediaURL(%q) = %v", u, got)
		}
	}
}

func TestDownloadRoutesAuthDecisionThroughIsRelayMediaURL(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	t.Setenv("TMPDIR", t.TempDir())
	srv, seen := relayMediaServer(t, []byte("\x89PNG"), map[string]string{"Content-Type": "image/png", "Content-Length": "4"})
	c := NewRelayMediaClient(srv.URL, "", "")
	if c.Enabled() {
		t.Fatal("client without credentials is enabled")
	}
	if path, err := c.Download(context.Background(), srv.URL+"/relay/media/deadbeef", nil); path != "" || err != nil {
		t.Fatalf("disabled rehost download = %q, %v", path, err)
	}
	if n := len(seen()); n != 0 {
		t.Fatalf("disabled rehost download reached the network %d times", n)
	}
	if path, err := c.Download(context.Background(), srv.URL+"/attachments/1/2/i.png", nil); path == "" || err != nil {
		t.Fatalf("public download = %q, %v", path, err)
	}
	got := seen()
	if len(got) != 1 || got[0].Get("Authorization") != "" {
		t.Fatalf("public download headers %v", got)
	}
}
