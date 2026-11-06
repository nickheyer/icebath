package media

import (
	"context"
	"encoding/json"
	"errors"
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

func strPtr(s string) *string     { return &s }
func floatPtr(f float64) *float64 { return &f }

func TestFalSDKSubmitPreservesWireContract(t *testing.T) {
	for _, path := range []string{"sub/path", "/sub/path"} {
		f := newFalFixture(t)
		f.replies = append(f.replies, f.accepted())
		client, err := newManagedFalSyncClient(f.mod, "test-token", "  "+f.srv.URL+"///  ")
		if err != nil {
			t.Fatal(err)
		}
		client.syncClient.DefaultTimeout = floatPtr(37)
		headers := map[string]string{"X-Custom": "kept", "X-Idempotency-Key": "one-operation"}
		handle, err := client.Submit(context.Background(), "app", map[string]any{"prompt": "local fixture"}, falSubmitOptions{
			Path: path, Hint: strPtr("runner"), WebhookURL: strPtr("https://hook.test/cb?a=1"), Priority: strPtr("low"),
			StartTimeout: floatPtr(60), Headers: headers})
		if err != nil {
			t.Fatal(err)
		}
		if len(f.snapshot()) != 1 {
			t.Fatalf("requests %d", len(f.snapshot()))
		}
		r := f.snapshot()[0]
		if r.Method != "POST" || r.URL.String() != "/app/sub/path?fal_webhook=https%3A%2F%2Fhook.test%2Fcb%3Fa%3D1" {
			t.Fatalf("got %s %s", r.Method, r.URL)
		}
		if strings.TrimSpace(f.body(0)) != `{"prompt":"local fixture"}` {
			t.Fatalf("body %s", f.body(0))
		}
		for k, v := range map[string]string{"authorization": "Key test-token", "x-custom": "kept", "x-idempotency-key": "one-operation",
			"x-fal-runner-hint": "runner", "x-fal-queue-priority": "low", "x-fal-request-timeout": "60.0"} {
			if r.Header.Get(k) != v {
				t.Fatalf("header %s = %q", k, r.Header.Get(k))
			}
		}
		if f.deadline != 37*time.Second {
			t.Fatalf("timeout %v", f.deadline)
		}
		if len(headers) != 2 {
			t.Fatalf("caller headers mutated %v", headers)
		}
		if handle.RequestID != "r1" || handle.ResponseURL != f.srv.URL+"/result" || handle.StatusURL != f.srv.URL+"/status" ||
			handle.CancelURL != f.srv.URL+"/cancel" || handle.Client != client.httpClient {
			t.Fatalf("handle %+v", handle)
		}
	}
}

func TestFalKeyedErrorsSurviveWithoutDuplicateSubmission(t *testing.T) {
	body := map[string]any{"error": map[string]any{"code": "BILLING_ERROR", "message": "meter disabled",
		"details": map[string]any{"upstreamPayload": map[string]any{"code": "meter_missing", "error": "not configured"}}}}
	for _, status := range []int{402, 409, 429, 503, 0} {
		for _, keyed := range []bool{true, false} {
			f := newFalFixture(t)
			if status == 0 {
				f.replies = append(f.replies, "transport", f.accepted())
			} else {
				f.replies = append(f.replies, falReply{status: status, body: body}, f.accepted())
			}
			client, err := newManagedFalSyncClient(f.mod, "token", f.srv.URL)
			if err != nil {
				t.Fatal(err)
			}
			var headers map[string]string
			if keyed {
				headers = map[string]string{"x-IDEMPOTENCY-key": "operation"}
			}
			handle, err := client.Submit(context.Background(), "app", map[string]any{}, falSubmitOptions{Headers: headers})
			if keyed || status == 402 || status == 503 {
				if err == nil || len(f.snapshot()) != 1 {
					t.Fatalf("status %d keyed %v: err %v requests %d", status, keyed, err, len(f.snapshot()))
				}
				if status != 0 {
					var fe *falClientHTTPError
					if !errors.As(err, &fe) {
						t.Fatalf("want client HTTP error, got %T", err)
					}
					if s, _ := extractHTTPStatus(err); s != status {
						t.Fatalf("status %d", s)
					}
					if !strings.Contains(managedFalBillingError(err, "model"), "meter_missing") {
						t.Fatalf("billing %q", managedFalBillingError(err, "model"))
					}
				}
			} else if err != nil || handle.RequestID != "r1" || len(f.snapshot()) != 2 {
				t.Fatalf("status %d unkeyed retry: %v %d", status, err, len(f.snapshot()))
			}
			if f.snapshot()[0].URL.Path != "/app" || f.snapshot()[0].Header.Get("x-custom") != "" {
				t.Fatalf("first request %s", f.snapshot()[0].URL)
			}
		}
	}
}

func TestFalQueueOriginNormalization(t *testing.T) {
	for in, want := range map[string]string{"https://q.test": "https://q.test/", "https://q.test///": "https://q.test/", "  https://q.test  ": "https://q.test/"} {
		if got, err := normalizeFalQueueURLFormat(in); err != nil || got != want {
			t.Fatalf("%q -> %q %v", in, got, err)
		}
	}
	for _, in := range []string{"", "   "} {
		if _, err := normalizeFalQueueURLFormat(in); err == nil || !strings.Contains(err.Error(), "origin is required") {
			t.Fatalf("%q accepted", in)
		}
	}
}

// Error shape with independently present response and exception statuses
type statusShape struct {
	resp, exc     int
	hasResp, hasE bool
}

func (s statusShape) Error() string               { return "shape" }
func (s statusShape) ResponseStatus() (int, bool) { return s.resp, s.hasResp }
func (s statusShape) ExcStatus() (int, bool)      { return s.exc, s.hasE }

func TestFalExceptionStatusShapes(t *testing.T) {
	cases := []struct {
		shape statusShape
		want  int
		ok    bool
	}{
		{statusShape{resp: 404, hasResp: true}, 404, true},
		{statusShape{exc: 500, hasE: true}, 500, true},
		{statusShape{}, 0, false},
		{statusShape{resp: 200, hasResp: true, exc: 500, hasE: true}, 200, true},
		{statusShape{exc: 503, hasE: true}, 503, true},
	}
	for _, c := range cases {
		if got, ok := extractHTTPStatus(c.shape); got != c.want || ok != c.ok {
			t.Fatalf("%+v -> %d %v", c.shape, got, ok)
		}
	}
	if _, ok := extractHTTPStatus(errors.New("plain")); ok {
		t.Fatal("plain error has a status")
	}
}

func TestFalMissingSDKSurfaceIsActionable(t *testing.T) {
	cases := map[string]func(m *falClientModule){
		"fal_client.SyncClient": func(m *falClientModule) { m.NewSyncClient = nil },
		"fal_client.client":     func(m *falClientModule) { m.Client = nil },
		"SyncClient._client": func(m *falClientModule) {
			base := m.NewSyncClient
			m.NewSyncClient = func(k string) *falSyncClient { c := base(k); c.HTTP = nil; return c }
		},
		"request helpers":   func(m *falClientModule) { m.Client.MaybeRetryRequest = nil },
		"request helpers ":  func(m *falClientModule) { m.Client.RaiseForStatus = nil },
		"SyncRequestHandle": func(m *falClientModule) { m.Client.SyncRequestHandle = nil },
	}
	for msg, mutate := range cases {
		m := newFalClientModule()
		mutate(m)
		if _, err := newManagedFalSyncClient(m, "token", "https://queue.test"); err == nil || !strings.Contains(err.Error(), strings.TrimSpace(msg)) {
			t.Fatalf("%s: %v", msg, err)
		}
	}
}

func TestFalOptionalSDKSurfaceRefusedBeforeHTTP(t *testing.T) {
	for helper, opts := range map[string]falSubmitOptions{
		"add_priority_header": {Priority: strPtr("low")},
		"add_timeout_header":  {StartTimeout: floatPtr(60)},
	} {
		f := newFalFixture(t)
		if helper == "add_priority_header" {
			f.mod.Client.AddPriorityHeader = nil
		} else {
			f.mod.Client.AddTimeoutHeader = nil
		}
		client, _ := newManagedFalSyncClient(f.mod, "token", f.srv.URL)
		if _, err := client.Submit(context.Background(), "app", map[string]any{}, opts); err == nil || !strings.Contains(err.Error(), helper) {
			t.Fatalf("%s: %v", helper, err)
		}
		if len(f.snapshot()) != 0 {
			t.Fatal("request sent")
		}
	}
}

func TestFalOlderSDKTimeoutDefaultAndOptionalHint(t *testing.T) {
	for _, c := range []struct {
		timeout *float64
		want    time.Duration
	}{{nil, 120 * time.Second}, {floatPtr(300), 300 * time.Second}} {
		f := newFalFixture(t)
		f.mod.Client.AddHintHeader = nil
		client, _ := newManagedFalSyncClient(f.mod, "token", f.srv.URL)
		client.syncClient.DefaultTimeout = c.timeout
		f.replies = append(f.replies, f.accepted())
		h, err := client.Submit(context.Background(), "app", map[string]any{}, falSubmitOptions{Hint: strPtr("ignored-on-old-sdk")})
		if err != nil || h.RequestID != "r1" {
			t.Fatal(err)
		}
		if f.deadline != c.want || f.snapshot()[0].Header.Get("x-fal-runner-hint") != "" {
			t.Fatalf("timeout %v hint %q", f.deadline, f.snapshot()[0].Header.Get("x-fal-runner-hint"))
		}
	}
}

func TestFalSubmitWithIdempotencyKeyIsNotRetried(t *testing.T) {
	f := newFalFixture(t)
	retried := 0
	inner := f.mod.Client.MaybeRetryRequest
	f.mod.Client.MaybeRetryRequest = func(ctx context.Context, c *http.Client, m, u string, r falHTTPRequest) (*falResponse, error) {
		retried++
		return inner(ctx, c, m, u, r)
	}
	f.replies = append(f.replies, f.accepted(), f.accepted())
	client, _ := newManagedFalSyncClient(f.mod, "token", f.srv.URL)
	if _, err := client.Submit(context.Background(), "app", map[string]any{}, falSubmitOptions{Headers: map[string]string{"X-Idempotency-Key": "k"}}); err != nil {
		t.Fatal(err)
	}
	if retried != 0 {
		t.Fatal("keyed submit went through the retry helper")
	}
	if _, err := client.Submit(context.Background(), "app", map[string]any{}, falSubmitOptions{}); err != nil || retried != 1 {
		t.Fatalf("unkeyed submit skipped the retry helper %v", err)
	}
}

func TestFalWaitPollsQueueStatusAtGatewayCadence(t *testing.T) {
	f := newFalFixture(t)
	f.replies = append(f.replies, f.accepted(), falReply{status: 200, body: map[string]any{"status": "IN_QUEUE", "queue_position": 0}},
		falReply{status: 200, body: map[string]any{"status": "IN_PROGRESS", "logs": nil}},
		falReply{status: 200, body: map[string]any{"status": "COMPLETED", "logs": nil}},
		falReply{status: 200, body: map[string]any{"status": "COMPLETED", "logs": nil}},
		falReply{status: 200, body: map[string]any{"images": []any{"done"}}})
	client, _ := newManagedFalSyncClient(f.mod, "token", f.srv.URL)
	h, err := client.Submit(context.Background(), "app", map[string]any{}, falSubmitOptions{})
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

// Recorded request body under the fixture lock
func (f *falFixture) body(i int) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.bodies[i]
}
