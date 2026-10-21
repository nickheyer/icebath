package provider

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"iter"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/protocol"
)

// Line protocol used only here so provider tests stay independent of any real wire format
type lineProtocol struct{}

func (lineProtocol) ID() protocol.ID { return "line-test" }

func (lineProtocol) Encode(req protocol.Request) (protocol.Call, error) {
	if req.Model == "" {
		return protocol.Call{}, errors.New("model required")
	}
	return protocol.Call{
		Method: http.MethodPost,
		Path:   "/v1/lines",
		Query:  url.Values{"stream": {map[bool]string{true: "1", false: "0"}[req.Stream]}},
		Header: http.Header{"Content-Type": {"text/plain"}, "X-Proto": {"line"}},
		Body:   []byte(req.Model),
	}, nil
}

func (lineProtocol) Decode(r io.Reader) (protocol.Response, error) {
	b, err := io.ReadAll(r)
	if err != nil {
		return protocol.Response{}, err
	}
	return protocol.Response{Content: []protocol.Part{protocol.Text{Text: string(b)}}, StopReason: protocol.StopEnd}, nil
}

func (lineProtocol) Stream(r io.Reader) iter.Seq2[protocol.Event, error] {
	return func(yield func(protocol.Event, error) bool) {
		b, err := io.ReadAll(r)
		if err != nil {
			yield(protocol.Event{}, err)
			return
		}
		var bld protocol.Builder
		bld.Start("id", "m")
		bld.OpenText("t")
		if err := bld.Delta("t", string(b)); err != nil {
			yield(protocol.Event{}, err)
			return
		}
		if err := bld.Finish(protocol.StopEnd, "stop", protocol.Usage{Output: len(b)}); err != nil {
			yield(protocol.Event{}, err)
			return
		}
		for _, ev := range bld.Events() {
			if !yield(ev, nil) {
				return
			}
		}
	}
}

func (lineProtocol) DecodeError(status int, body []byte) *protocol.Error {
	return &protocol.Error{Status: status, Type: "line", Message: strings.TrimSpace(string(body)), Body: body}
}

func (lineProtocol) Capabilities() protocol.Capabilities {
	return protocol.Capabilities{}
}

func TestCompleteAppliesEndpointHeadersAndAuth(t *testing.T) {
	var got *http.Request
	var body string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r
		b, _ := io.ReadAll(r.Body)
		body = string(b)
		io.WriteString(w, "echo "+body)
	}))
	defer srv.Close()
	p := &Provider{
		Name: "t", Protocol: lineProtocol{}, BaseURL: srv.URL + "/",
		Auth:   Header{Name: "x-api-key", Value: "secret"},
		Header: http.Header{"X-Extra": {"yes"}, "X-Proto": {"override"}},
		Query:  url.Values{"api-version": {"2026"}},
	}
	resp, err := p.Complete(context.Background(), protocol.Request{Model: "hello"})
	if err != nil {
		t.Fatal(err)
	}
	if resp.Content[0].(protocol.Text).Text != "echo hello" || body != "hello" {
		t.Fatalf("%+v %q", resp, body)
	}
	if got.URL.Path != "/v1/lines" || got.URL.Query().Get("api-version") != "2026" || got.URL.Query().Get("stream") != "0" {
		t.Fatalf("url %s", got.URL)
	}
	if got.Header.Get("x-api-key") != "secret" || got.Header.Get("X-Extra") != "yes" || got.Header.Get("X-Proto") != "override" || got.Header.Get("Content-Type") != "text/plain" {
		t.Fatalf("headers %v", got.Header)
	}
}

func TestStreamPassesEventsThrough(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("stream") != "1" || r.Header.Get("Authorization") != "Bearer tok" {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		io.WriteString(w, "abc")
	}))
	defer srv.Close()
	p := &Provider{Name: "t", Protocol: lineProtocol{}, BaseURL: srv.URL, Auth: Bearer("tok")}
	resp, err := protocol.Collect(p.Stream(context.Background(), protocol.Request{Model: "m"}))
	if err != nil {
		t.Fatal(err)
	}
	if resp.Content[0].(protocol.Text).Text != "abc" || resp.Usage.Output != 3 {
		t.Fatalf("%+v", resp)
	}
}

func TestRetriesThenSucceeds(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch calls.Add(1) {
		case 1:
			w.Header().Set("Retry-After", "0")
			w.WriteHeader(http.StatusTooManyRequests)
			io.WriteString(w, "slow down")
		case 2:
			w.WriteHeader(http.StatusBadGateway)
		default:
			io.WriteString(w, "ok")
		}
	}))
	defer srv.Close()
	p := &Provider{Name: "t", Protocol: lineProtocol{}, BaseURL: srv.URL, Retry: Retry{Attempts: 3, Base: time.Millisecond, Max: time.Millisecond}}
	resp, err := p.Complete(context.Background(), protocol.Request{Model: "m"})
	if err != nil || resp.Content[0].(protocol.Text).Text != "ok" || calls.Load() != 3 {
		t.Fatalf("%v %+v %d", err, resp, calls.Load())
	}
}

func TestGivesUpWithTheWireError(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusServiceUnavailable)
		io.WriteString(w, "down")
	}))
	defer srv.Close()
	p := &Provider{Name: "t", Protocol: lineProtocol{}, BaseURL: srv.URL, Retry: Retry{Attempts: 2, Base: time.Millisecond}}
	_, err := p.Complete(context.Background(), protocol.Request{Model: "m"})
	var perr *protocol.Error
	if !errors.As(err, &perr) || perr.Status != 503 || perr.Message != "down" || calls.Load() != 2 {
		t.Fatalf("%v %d", err, calls.Load())
	}
}

func TestDoesNotRetryClientErrors(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusBadRequest)
		io.WriteString(w, "bad")
	}))
	defer srv.Close()
	p := &Provider{Name: "t", Protocol: lineProtocol{}, BaseURL: srv.URL}
	_, err := protocol.Collect(p.Stream(context.Background(), protocol.Request{Model: "m"}))
	var perr *protocol.Error
	if !errors.As(err, &perr) || perr.Status != 400 || calls.Load() != 1 {
		t.Fatalf("%v %d", err, calls.Load())
	}
}

func TestEncodeAndBaseURLFailures(t *testing.T) {
	p := &Provider{Name: "t", Protocol: lineProtocol{}, BaseURL: "http://127.0.0.1:1"}
	if _, err := p.Complete(context.Background(), protocol.Request{}); err == nil {
		t.Fatal("encode failure accepted")
	}
	p.BaseURL = "not a url"
	if _, err := p.Complete(context.Background(), protocol.Request{Model: "m"}); err == nil {
		t.Fatal("bad base url accepted")
	}
	if _, err := (&Provider{Name: "t"}).Complete(context.Background(), protocol.Request{Model: "m"}); err == nil {
		t.Fatal("missing protocol accepted")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	p.BaseURL = "http://127.0.0.1:1"
	if _, err := p.Complete(ctx, protocol.Request{Model: "m"}); err == nil {
		t.Fatal("cancelled context accepted")
	}
}

func TestDoIssuesRawCalls(t *testing.T) {
	var calls atomic.Int32
	var got *http.Request
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r
		if calls.Add(1) == 1 {
			w.WriteHeader(http.StatusBadGateway)
			return
		}
		io.WriteString(w, `{"data":[]}`)
	}))
	defer srv.Close()
	p := &Provider{Name: "t", Protocol: lineProtocol{}, BaseURL: srv.URL + "/v1", Auth: Bearer("tok"), Retry: Retry{Attempts: 2, Base: time.Millisecond}}
	resp, err := p.Do(context.Background(), protocol.Call{Method: http.MethodGet, Path: "/models", Query: url.Values{"limit": {"5"}}})
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if string(body) != `{"data":[]}` || got.URL.Path != "/v1/models" || got.URL.Query().Get("limit") != "5" || got.Header.Get("Authorization") != "Bearer tok" || got.Header.Get("User-Agent") != UserAgent || calls.Load() != 2 {
		t.Fatalf("%s %s %v %d", body, got.URL, got.Header, calls.Load())
	}
	if _, err := (&Provider{Name: "t"}).Do(context.Background(), protocol.Call{Method: http.MethodGet, Path: "/"}); err == nil {
		t.Fatal("missing protocol accepted")
	}
}

type refreshing struct {
	err   error
	count int
}

func (r *refreshing) Refresh(context.Context) error { r.count++; return r.err }
func (r *refreshing) Apply(req *http.Request)       { req.Header.Set("X-Fresh", fmt.Sprint(r.count)) }

func TestRefresherRunsBeforeEveryAttempt(t *testing.T) {
	var seen []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = append(seen, r.Header.Get("X-Fresh"))
		if len(seen) == 1 {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		io.WriteString(w, "ok")
	}))
	defer srv.Close()
	auth := &refreshing{}
	p := &Provider{Name: "t", Protocol: lineProtocol{}, BaseURL: srv.URL, Auth: auth, Retry: Retry{Attempts: 2, Base: time.Millisecond}}
	if _, err := p.Complete(context.Background(), protocol.Request{Model: "m"}); err != nil {
		t.Fatal(err)
	}
	if strings.Join(seen, ",") != "1,2" {
		t.Fatalf("%v", seen)
	}
	auth.err = errors.New("no credentials")
	if _, err := p.Complete(context.Background(), protocol.Request{Model: "m"}); err == nil || !strings.Contains(err.Error(), "no credentials") {
		t.Fatalf("%v", err)
	}
}

func TestRetryable(t *testing.T) {
	if Retryable(nil) || Retryable(context.Canceled) || Retryable(fmt.Errorf("wrapped: %w", context.DeadlineExceeded)) {
		t.Fatal("never retry nothing or cancellation")
	}
	if !Retryable(errors.New("connection reset")) || !Retryable(&protocol.Error{Status: 503}) || !Retryable(fmt.Errorf("provider x: %w", &protocol.Error{Status: 429})) {
		t.Fatal("transport failures and retryable statuses")
	}
	if Retryable(&protocol.Error{Status: 400}) || Retryable(&protocol.Error{Status: 401}) {
		t.Fatal("client errors")
	}
}

func TestRetryAfterAndBackoff(t *testing.T) {
	if retryAfter("3") != 3*time.Second || retryAfter("") != 0 || retryAfter("junk") != 0 {
		t.Fatal("seconds form")
	}
	future := time.Now().Add(2 * time.Second).UTC().Format(http.TimeFormat)
	if d := retryAfter(future); d <= 0 || d > 2*time.Second {
		t.Fatalf("http date %v", d)
	}
	r := Retry{Base: 100 * time.Millisecond, Max: 250 * time.Millisecond}
	for attempt, ceiling := range map[int]time.Duration{1: 100 * time.Millisecond, 2: 200 * time.Millisecond, 3: 250 * time.Millisecond} {
		d := backoff(r, attempt)
		if d < ceiling/2 || d > ceiling {
			t.Fatalf("attempt %d backoff %v", attempt, d)
		}
	}
}

// Protocol writing the merged body so provider extras can be observed
type extraProtocol struct{ lineProtocol }

func (extraProtocol) Encode(req protocol.Request) (protocol.Call, error) {
	body, err := protocol.MarshalBody(map[string]any{"model": req.Model, "knobs": map[string]any{"a": 1}}, req.Extra)
	if err != nil {
		return protocol.Call{}, err
	}
	return protocol.Call{Method: http.MethodPost, Path: "/v1/extra", Body: body}, nil
}

func TestProviderExtrasMergeBeneathTheRequest(t *testing.T) {
	var bodies []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		bodies = append(bodies, string(b))
		io.WriteString(w, "ok")
	}))
	defer srv.Close()
	extra := map[string]json.RawMessage{"knobs": json.RawMessage(`{"b":2,"c":3}`), "temperature": json.RawMessage(`0.1`)}
	p := &Provider{Name: "x", Protocol: extraProtocol{}, BaseURL: srv.URL, Extra: extra}
	if _, err := p.Complete(context.Background(), protocol.Request{Model: "m"}); err != nil {
		t.Fatal(err)
	}
	if _, err := p.Complete(context.Background(), protocol.Request{Model: "m", Extra: map[string]json.RawMessage{"knobs": json.RawMessage(`{"c":9}`), "top_p": json.RawMessage(`1`)}}); err != nil {
		t.Fatal(err)
	}
	if bodies[0] != `{"knobs":{"a":1,"b":2,"c":3},"model":"m","temperature":0.1}` {
		t.Fatalf("%s", bodies[0])
	}
	if bodies[1] != `{"knobs":{"a":1,"b":2,"c":9},"model":"m","temperature":0.1,"top_p":1}` {
		t.Fatalf("request extras must win over provider extras: %s", bodies[1])
	}
	if string(extra["knobs"]) != `{"b":2,"c":3}` {
		t.Fatal("provider extras must not be mutated by a request")
	}
}

func TestObserverSeesEveryAttemptAndRateLimits(t *testing.T) {
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.Header().Set("request-id", fmt.Sprintf("req-%d", calls))
		w.Header().Set("anthropic-ratelimit-tokens-limit", "1000")
		w.Header().Set("anthropic-ratelimit-tokens-remaining", "900")
		w.Header().Set("anthropic-ratelimit-tokens-reset", "2026-01-01T00:00:00Z")
		if calls == 1 {
			w.Header().Set("Retry-After", "0")
			w.WriteHeader(http.StatusTooManyRequests)
			w.Write([]byte(`{"error":"slow down"}`))
			return
		}
		w.Write([]byte("ok\n"))
	}))
	defer srv.Close()
	var seen []Attempt
	p := &Provider{Name: "t", Protocol: lineProtocol{}, BaseURL: srv.URL, Retry: Retry{Attempts: 3, Base: time.Millisecond, Max: time.Millisecond}, Observer: ObserverFunc(func(a Attempt) { seen = append(seen, a) })}
	resp, err := p.Complete(context.Background(), protocol.Request{Model: "m"})
	if err != nil {
		t.Fatal(err)
	}
	if resp.RequestID != "req-2" || resp.Header.Get("request-id") != "req-2" {
		t.Fatalf("response must carry the request id and headers, got %q %v", resp.RequestID, resp.Header)
	}
	if len(seen) != 2 || seen[0].Status != 429 || seen[0].Err == nil || seen[0].Wait == 0 || seen[0].RequestID != "req-1" || seen[0].OK() {
		t.Fatalf("first attempt %+v", seen[0])
	}
	if !seen[1].OK() || seen[1].Attempt != 2 || seen[1].Wait != 0 || seen[1].RateLimit.Tokens != 1000 || seen[1].RateLimit.TokensRemaining != 900 {
		t.Fatalf("second attempt %+v", seen[1])
	}
	if seen[1].Duration <= 0 || seen[1].Method != http.MethodPost || seen[1].URL == "" {
		t.Fatalf("attempt timing and target %+v", seen[1])
	}
}

func TestStreamStampsHeadersOnDone(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("x-request-id", "abc")
		w.Write([]byte("a\nb\n"))
	}))
	defer srv.Close()
	p := &Provider{Name: "t", Protocol: lineProtocol{}, BaseURL: srv.URL}
	resp, err := protocol.Collect(p.Stream(context.Background(), protocol.Request{Model: "m"}))
	if err != nil {
		t.Fatal(err)
	}
	if resp.RequestID != "abc" || resp.Header.Get("x-request-id") != "abc" {
		t.Fatalf("%+v", resp)
	}
}

func TestZeroBaseDoesNotSpinAndRetryAfterIsCapped(t *testing.T) {
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.Header().Set("Retry-After", "3600")
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer srv.Close()
	var waits []time.Duration
	p := &Provider{Name: "t", Protocol: lineProtocol{}, BaseURL: srv.URL, Retry: Retry{Attempts: 2}, Observer: ObserverFunc(func(a Attempt) { waits = append(waits, a.Wait) })}
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(20 * time.Millisecond)
		cancel()
	}()
	_, err := p.Complete(ctx, protocol.Request{Model: "m"})
	if err == nil || !errors.Is(err, context.Canceled) || !strings.Contains(err.Error(), "503") {
		t.Fatalf("cancellation during backoff must report both the cancel and the last failure: %v", err)
	}
	if len(waits) != 1 || waits[0] != time.Minute || waits[0] > maxWait(Retry{}) {
		t.Fatalf("retry after must be capped, waits %v", waits)
	}
	if backoff(Retry{Base: time.Millisecond, Attempts: 3}, 2) <= 0 {
		t.Fatal("backoff with a base must be positive")
	}
	if calls != 1 {
		t.Fatalf("%d calls", calls)
	}
}

func TestRateLimitParsing(t *testing.T) {
	h := http.Header{}
	h.Set("x-ratelimit-limit-requests", "50")
	h.Set("x-ratelimit-remaining-requests", "49")
	h.Set("x-ratelimit-reset-requests", "6m0s")
	r := ParseRateLimit(h)
	if r.Requests != 50 || r.RequestsRemaining != 49 || r.Reset != 6*time.Minute || !r.Known() {
		t.Fatalf("%+v", r)
	}
	h = http.Header{"X-Ratelimit-Reset": {"1.5"}}
	if ParseRateLimit(h).Reset != 1500*time.Millisecond {
		t.Fatalf("%+v", ParseRateLimit(h))
	}
	if ParseRateLimit(http.Header{}).Known() || parseReset("junk") != 0 || RequestID(http.Header{}) != "" {
		t.Fatal("empty headers must stay unknown")
	}
	if RequestID(http.Header{"Cf-Ray": {"ray"}}) != "ray" {
		t.Fatal("cf-ray")
	}
}
