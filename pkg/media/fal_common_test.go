package media

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// Queue fixture replaying canned replies and recording each request
type falFixture struct {
	mu       sync.Mutex
	srv      *httptest.Server
	requests []*http.Request
	bodies   []string
	replies  []any
	sleeps   []float64
	deadline time.Duration
	mod      *falClientModule
}

type falReply struct {
	status int
	body   any
	plain  bool
}

func newFalFixture(t *testing.T) *falFixture {
	f := &falFixture{}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		f.mu.Lock()
		f.requests = append(f.requests, r)
		f.bodies = append(f.bodies, string(raw))
		if len(f.replies) == 0 {
			f.mu.Unlock()
			t.Errorf("unexpected request %s", r.URL)
			panic(http.ErrAbortHandler)
		}
		reply := f.replies[0]
		f.replies = f.replies[1:]
		f.mu.Unlock()
		rep, ok := reply.(falReply)
		if !ok {
			panic(http.ErrAbortHandler)
		}
		if rep.plain {
			w.Header().Set("Content-Type", "text/plain")
		} else {
			w.Header().Set("Content-Type", "application/json")
		}
		w.WriteHeader(rep.status)
		_ = json.NewEncoder(w).Encode(rep.body)
	}))
	t.Cleanup(f.srv.Close)
	f.mod = newFalClientModule()
	f.mod.Client.Sleep = func(s float64) { f.sleeps = append(f.sleeps, s) }
	base := f.mod.NewSyncClient
	f.mod.NewSyncClient = func(key string) *falSyncClient {
		c := base(key)
		inner := c.HTTP.Transport
		c.HTTP.Transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
			if d, ok := r.Context().Deadline(); ok {
				f.deadline = time.Until(d).Round(time.Second)
			}
			return inner.RoundTrip(r)
		})
		return c
	}
	return f
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func (f *falFixture) accepted() falReply {
	return falReply{status: 200, body: map[string]any{
		"request_id": "r1", "response_url": f.srv.URL + "/result",
		"status_url": f.srv.URL + "/status", "cancel_url": f.srv.URL + "/cancel",
	}}
}

func TestFalWaitPollsQueueStatusAtQueueCadence(t *testing.T) {
	f := newFalFixture(t)
	f.replies = append(f.replies, f.accepted(), falReply{status: 200, body: map[string]any{"status": "IN_QUEUE", "queue_position": 0}},
		falReply{status: 200, body: map[string]any{"status": "IN_PROGRESS", "logs": nil}},
		falReply{status: 200, body: map[string]any{"status": "COMPLETED", "logs": nil}},
		falReply{status: 200, body: map[string]any{"status": "COMPLETED", "logs": nil}},
		falReply{status: 200, body: map[string]any{"images": []any{"done"}}})
	h, err := falSubmitRequest(context.Background(), f.mod.Client, f.mod.NewSyncClient("token"), f.srv.URL+"/app", map[string]any{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	got, err := waitFalQueueResult(context.Background(), h, falQueuePollSeconds)
	if err != nil {
		t.Fatal(err)
	}
	if imgs := got.(map[string]any)["images"].([]any); imgs[0] != "done" {
		t.Fatalf("result %v", got)
	}
	if len(f.sleeps) != 2 || f.sleeps[0] != 2.0 || f.sleeps[1] != 2.0 {
		t.Fatalf("sleeps %v", f.sleeps)
	}
	var paths []string
	for _, r := range f.snapshot() {
		paths = append(paths, r.URL.Path)
	}
	if strings.Join(paths, ",") != "/app,/status,/status,/status,/status,/result" {
		t.Fatalf("paths %v", paths)
	}
}

func TestFalImportReturnsCompiledModule(t *testing.T) {
	if importFalClient() != importFalClient() || importFalClient().Submit == nil {
		t.Fatal("module not stable")
	}
}

func TestFalDirectSubmitNeedsCredentials(t *testing.T) {
	t.Setenv("FAL_KEY", "")
	t.Setenv("FAL_KEY_ID", "")
	t.Setenv("FAL_KEY_SECRET", "")
	_, err := newFalClientModule().Submit(context.Background(), nil, "app", map[string]any{}, nil)
	if err == nil || err.Error() != "Please set the FAL_KEY environment variable to your API key." {
		t.Fatalf("got %v", err)
	}
}

// Recorded requests under the fixture lock
func (f *falFixture) snapshot() []*http.Request {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]*http.Request(nil), f.requests...)
}
