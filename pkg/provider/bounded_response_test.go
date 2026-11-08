package provider

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// In process server whose routes stream an oversize body, stall after a prefix, or answer normally
func boundedResponseServer(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		flusher, _ := w.(http.Flusher)
		switch r.URL.Path {
		case "/oversize":
			w.Header().Set("Content-Type", "text/plain")
			w.WriteHeader(500)
			block := []byte(strings.Repeat("x", 65536))
			for range 2000 {
				if _, err := w.Write(block); err != nil {
					return
				}
				if flusher != nil {
					flusher.Flush()
				}
			}
		case "/stall":
			w.Header().Set("Content-Type", "text/plain")
			w.WriteHeader(500)
			_, _ = w.Write([]byte("partial failure detail"))
			if flusher != nil {
				flusher.Flush()
			}
			select {
			case <-r.Context().Done():
			case <-time.After(60 * time.Second):
			}
		case "/normal":
			body, _ := json.Marshal(map[string]any{"error": map[string]any{"code": 429, "message": "quota exceeded", "status": "RESOURCE_EXHAUSTED"}})
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(429)
			_, _ = w.Write(body)
		case "/empty":
			w.Header().Set("Content-Length", "0")
			w.WriteHeader(500)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

// Client whose read timeout is generous so the bounding is provably done by the helper
func boundedResponseClient() *http.Client {
	return &http.Client{Transport: &http.Transport{ResponseHeaderTimeout: 5 * time.Second}, Timeout: 45 * time.Second}
}

func TestOversizeBodyIsCapped(t *testing.T) {
	srv := boundedResponseServer(t)
	client := boundedResponseClient()
	start := time.Now()
	resp, err := client.Post(srv.URL+"/oversize", "text/plain", nil)
	if err != nil {
		t.Fatal(err)
	}
	text := ReadStreamingErrorBody(resp, 64*1024, 10*time.Second)
	elapsed := time.Since(start)
	if len(text) == 0 || len(text) > 64*1024 {
		t.Fatalf("capped text length %d", len(text))
	}
	if elapsed >= 9*time.Second {
		t.Fatalf("capping took %s, must return promptly instead of draining the whole body", elapsed)
	}
}

func TestStalledBodyReturnsThePartialPrefixAtTheDeadline(t *testing.T) {
	srv := boundedResponseServer(t)
	client := boundedResponseClient()
	resp, err := client.Post(srv.URL+"/stall", "text/plain", nil)
	if err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	text := ReadStreamingErrorBody(resp, 64*1024, 300*time.Millisecond)
	if text != "partial failure detail" {
		t.Fatalf("partial text %q", text)
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Fatalf("deadline took %s", elapsed)
	}
}

func TestNormalAndEmptyBodiesReadWhole(t *testing.T) {
	srv := boundedResponseServer(t)
	client := boundedResponseClient()
	resp, err := client.Post(srv.URL+"/normal", "text/plain", nil)
	if err != nil {
		t.Fatal(err)
	}
	if text := ReadStreamingErrorBody(resp, 64*1024, 10*time.Second); !strings.Contains(text, "RESOURCE_EXHAUSTED") {
		t.Fatalf("normal body %q", text)
	}
	resp, err = client.Post(srv.URL+"/empty", "text/plain", nil)
	if err != nil {
		t.Fatal(err)
	}
	if text := ReadStreamingErrorBody(resp, 64*1024, 10*time.Second); text != "" {
		t.Fatalf("empty body %q", text)
	}
}
