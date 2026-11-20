package pyhttpx

import (
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestTimeoutTransportLimitsEachReadNotTheWholeStream(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fl := w.(http.Flusher)
		for i := 0; i < 4; i++ {
			_, _ = w.Write([]byte("x"))
			fl.Flush()
			time.Sleep(120 * time.Millisecond)
		}
		if r.URL.Path == "/stall" {
			time.Sleep(time.Second)
		}
	}))
	defer srv.Close()
	client := &http.Client{Transport: TimeoutTransport(nil, 300*time.Millisecond)}
	resp, err := client.Get(srv.URL + "/steady")
	if err != nil {
		t.Fatal(err)
	}
	body, err := io.ReadAll(resp.Body)
	resp.Body.Close()
	if err != nil || string(body) != "xxxx" {
		t.Fatalf("steady stream past the timeout in total got %q %v", body, err)
	}
	resp, err = client.Get(srv.URL + "/stall")
	if err != nil {
		t.Fatal(err)
	}
	_, err = io.ReadAll(resp.Body)
	resp.Body.Close()
	var te *TimeoutError
	if !errors.As(err, &te) || te.Phase != "read" {
		t.Fatalf("stalled read got %v", err)
	}
}
