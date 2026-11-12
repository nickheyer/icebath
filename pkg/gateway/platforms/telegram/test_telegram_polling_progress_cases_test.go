package telegram

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// Instrumented polling request against a loopback server running handler
func controlledRequest(t *testing.T, handler http.HandlerFunc) (*ptbRequest, *pollingLifecycle, string) {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	p, _ := newTestLifecycle(t, nil)
	request, err := newPTBRequest(context.Background(), requestSettings{connectTimeout: 5, readTimeout: 5, writeTimeout: 5, poolTimeout: 5})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = request.Shutdown(context.Background()) })
	return instrumentPollingRequest(request, p), p, srv.URL + "/getUpdates"
}

// Asserts the generation stayed unproven with the seeded network error count
func assertUnproven(t *testing.T, p *pollingLifecycle, progress *pollEvent) {
	t.Helper()
	if progress.isSet() || p.networkErrorCount != 3 || !p.isSendPathDegraded() {
		t.Fatalf("progress %v network %d degraded %v", progress.isSet(), p.networkErrorCount, p.isSendPathDegraded())
	}
}

func TestCurrentPollingGenerationSuccessRecordsProgress(t *testing.T) {
	request, p, url := controlledRequest(t, func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte(`{"ok":true,"result":[]}`)) })
	generation, progress := p.beginPollingGeneration()
	p.networkErrorCount = 3
	body, err := postUnderGeneration(t, request, url, generation)
	if err != nil || body != `{"ok":true,"result":[]}` {
		t.Fatalf("got %q %v", body, err)
	}
	if !progress.isSet() || p.networkErrorCount != 0 || p.isSendPathDegraded() || generation <= 0 {
		t.Fatal("a successful getUpdates of the current generation must record progress")
	}
}

func TestUnsuccessfulPollingRequestDoesNotRecordProgress(t *testing.T) {
	t.Run("RuntimeError", func(t *testing.T) {
		request, p, url := controlledRequest(t, func(w http.ResponseWriter, _ *http.Request) {
			conn, _, err := w.(http.Hijacker).Hijack()
			if err == nil {
				conn.Close()
			}
		})
		generation, progress := p.beginPollingGeneration()
		p.networkErrorCount = 3
		if _, err := postUnderGeneration(t, request, url, generation); err == nil || !isPTBError(err, "NetworkError") {
			t.Fatalf("want a NetworkError, got %v", err)
		}
		assertUnproven(t, p, progress)
	})
	t.Run("CancelledError", func(t *testing.T) {
		release := make(chan struct{})
		request, p, url := controlledRequest(t, func(w http.ResponseWriter, r *http.Request) {
			select {
			case <-release:
			case <-r.Context().Done():
			}
		})
		defer close(release)
		generation, progress := p.beginPollingGeneration()
		p.networkErrorCount = 3
		ctx, cancel := context.WithCancel(withPollingGeneration(context.Background(), &generation))
		req, _ := http.NewRequestWithContext(ctx, http.MethodPost, url, strings.NewReader(""))
		cancel()
		if _, err := request.Do(req); err != context.Canceled {
			t.Fatalf("want cancellation, got %v", err)
		}
		assertUnproven(t, p, progress)
	})
}

func TestHttpErrorResponseDoesNotRecordPollingProgress(t *testing.T) {
	request, p, url := controlledRequest(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(500)
		_, _ = w.Write([]byte("bad"))
	})
	generation, progress := p.beginPollingGeneration()
	p.networkErrorCount = 3
	if _, err := postUnderGeneration(t, request, url, generation); err == nil {
		t.Fatal("a 500 must surface as an error")
	}
	assertUnproven(t, p, progress)
}
