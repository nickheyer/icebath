package provider

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"iter"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/protocol"
)

// Protocol that posts the body as given and reads errors as a plain message
type echo struct{}

func (echo) ID() protocol.ID { return "echo" }
func (echo) Encode(req protocol.Request) (protocol.Call, error) {
	body, _ := json.Marshal(req)
	return protocol.Call{Method: http.MethodPost, Path: "/v1/echo", Body: body}, nil
}
func (echo) Decode(r io.Reader) (protocol.Response, error) { return protocol.Response{}, nil }
func (echo) Stream(r io.Reader) iter.Seq2[protocol.Event, error] {
	return func(yield func(protocol.Event, error) bool) {}
}
func (echo) DecodeError(status int, body []byte) *protocol.Error {
	return protocol.Classify(&protocol.Error{Status: status, Message: string(body), Transient: status == 429 || status >= 500})
}

type recorder struct {
	mu       sync.Mutex
	attempts []Attempt
}

func (r *recorder) Observe(a Attempt) {
	r.mu.Lock()
	r.attempts = append(r.attempts, a)
	r.mu.Unlock()
}

func (r *recorder) all() []Attempt {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]Attempt(nil), r.attempts...)
}

func newProvider(url string, rec *recorder) *Provider {
	return &Provider{Name: "test", Protocol: echo{}, BaseURL: url, Auth: None{}, Observer: rec, HeaderTimeout: 100 * time.Millisecond, StallTimeout: 100 * time.Millisecond}
}

func TestHeaderStall(t *testing.T) {
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { <-release }))
	defer srv.Close()
	defer close(release)
	rec := &recorder{}
	p := newProvider(srv.URL, rec)
	start := time.Now()
	_, err := p.Do(context.Background(), protocol.Call{Method: http.MethodPost, Path: "/v1/echo"})
	var stall *StallError
	if !errors.As(err, &stall) || !stall.Headers {
		t.Fatalf("got %v", err)
	}
	if time.Since(start) > 2*time.Second {
		t.Fatalf("waited %s for a stalled call", time.Since(start))
	}
	if got := rec.all(); len(got) != 1 || got[0].Err == nil {
		t.Fatalf("attempts %+v", got)
	}
}

func TestBodyStall(t *testing.T) {
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.(http.Flusher).Flush()
		io.WriteString(w, "data: hello\n\n")
		w.(http.Flusher).Flush()
		<-release
	}))
	defer srv.Close()
	defer close(release)
	rec := &recorder{}
	p := newProvider(srv.URL, rec)
	resp, err := p.Do(WithLabel(context.Background(), "run-7"), protocol.Call{Method: http.MethodPost, Path: "/v1/echo"})
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	_, err = io.ReadAll(resp.Body)
	var stall *StallError
	if !errors.As(err, &stall) || stall.Headers {
		t.Fatalf("got %v", err)
	}
	got := rec.all()
	if len(got) != 1 || got[0].Label != "run-7" || got[0].Status != 200 || !errors.As(got[0].Err, &stall) {
		t.Fatalf("attempts %+v", got)
	}
}

func TestStuckNotRetried(t *testing.T) {
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.WriteHeader(http.StatusTooManyRequests)
		io.WriteString(w, "You exceeded your current quota, please check your plan and billing details.")
	}))
	defer srv.Close()
	p := newProvider(srv.URL, &recorder{})
	_, err := p.Do(context.Background(), protocol.Call{Method: http.MethodPost, Path: "/v1/echo"})
	if e := protocol.StuckError(err); e == nil || !e.Billing {
		t.Fatalf("got %v", err)
	}
	if calls != 1 {
		t.Fatalf("%d calls for a billing refusal", calls)
	}
}

func TestTransientRetried(t *testing.T) {
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if calls < 2 {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()
	rec := &recorder{}
	p := newProvider(srv.URL, rec)
	resp, err := p.Do(context.Background(), protocol.Call{Method: http.MethodPost, Path: "/v1/echo"})
	if err != nil {
		t.Fatal(err)
	}
	io.ReadAll(resp.Body)
	resp.Body.Close()
	if calls != 2 {
		t.Fatalf("%d calls", calls)
	}
	got := rec.all()
	if len(got) != 2 || got[0].Wait <= 0 || got[1].Err != nil {
		t.Fatalf("attempts %+v", got)
	}
}
