package provider

import (
	"bytes"
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"syscall"
	"testing"
	"time"
)

const rateLimitBody = `{"error": {"message": "Rate limit reached for fake-model on requests per min (RPM): Limit 3, Used 3, Requested 1.", "type": "requests", "param": null, "code": "rate_limit_exceeded"}}`

func sdkStatusError(status int, body string, header http.Header) *AuxSDKError {
	if header == nil {
		header = http.Header{}
	}
	return auxSDKStatusError(&http.Response{StatusCode: status, Header: header}, []byte(body))
}

type declaredClassError struct{ class string }

func (e *declaredClassError) Error() string      { return "declared " + e.class }
func (e *declaredClassError) PyTypeName() string { return e.class }

func TestSDKAndStreamErrorsAreNotLocalValidationErrors(t *testing.T) {
	refused := &AuxSDKError{Class: "APIConnectionError", Message: "Connection error.", Err: &net.OpError{Op: "dial", Net: "tcp", Err: os.NewSyscallError("connect", syscall.ECONNREFUSED)}}
	nonJSON := providerStreamErrorFromNonJSON(`{"id": "x", "choices": [{"delta": {"content": "oops`, nil)
	for name, err := range map[string]error{"rate limit": sdkStatusError(429, rateLimitBody, nil), "refused": refused, "stream": nonJSON, "key error": &declaredClassError{"KeyError"}} {
		if IsLocalValidationError(err) {
			t.Fatalf("%s is a provider failure, not a local ValueError", name)
		}
	}
	for _, class := range []string{"ValueError", "TypeError", "UnicodeDecodeError"} {
		if !IsLocalValidationError(&declaredClassError{class}) {
			t.Fatalf("%s is a local validation error", class)
		}
	}
}

func TestClassifyReadsTheSDKErrorStatusAndBody(t *testing.T) {
	cl := ClassifyError(ClassifyInput{Err: sdkStatusError(429, rateLimitBody, nil), Provider: "custom", Model: "fake-model"})
	if cl.StatusCode != 429 || cl.Reason != ReasonRateLimit || !cl.Retryable {
		t.Fatalf("rate limit verdict %+v", cl)
	}
	if !strings.HasPrefix(cl.Message, "Rate limit reached for fake-model") {
		t.Fatalf("message must come from the body, got %q", cl.Message)
	}
	auth := ClassifyError(ClassifyInput{Err: sdkStatusError(401, `{"error": {"message": "nope", "type": "x"}}`, nil), Provider: "custom"})
	if auth.StatusCode != 401 || auth.Reason != ReasonAuth || auth.Retryable {
		t.Fatalf("a bare 401 is auth by its status, got %+v", auth)
	}
	stream := ClassifyError(ClassifyInput{Err: &ProviderStreamError{StatusCode: 503, Body: map[string]any{"error": map[string]any{"message": "busy", "code": "x"}}}})
	if stream.StatusCode != 503 || stream.Reason != ReasonOverloaded {
		t.Fatalf("stream error status %+v", stream)
	}
}

func TestSummarizeSDKErrorsLikeTheSDKBody(t *testing.T) {
	cases := map[string]error{
		"HTTP 429: Rate limit reached for fake-model on requests per min (RPM): Limit 3, Used 3, Requested 1.": sdkStatusError(429, rateLimitBody, nil),
		`HTTP 422: {"detail": [{"loc": ["body", "messages"], "msg": "field required"}]}`:                       sdkStatusError(422, `{"detail": [{"loc": ["body", "messages"], "msg": "field required"}]}`, nil),
		"HTTP 502 — 502 Bad Gateway":                          sdkStatusError(502, "<html><head><title>502 Bad Gateway</title></head></html>\n", nil),
		`{"id": "x", "choices": [{"delta": {"content": "oops`: providerStreamErrorFromNonJSON(`{"id": "x", "choices": [{"delta": {"content": "oops`, nil),
	}
	for want, err := range cases {
		if got := SummarizeAPIError(err, "custom"); got != want {
			t.Fatalf("summary\n got %q\nwant %q", got, want)
		}
	}
}

func TestSDKErrorRetryAfterAndZAIOverload(t *testing.T) {
	limited := sdkStatusError(429, rateLimitBody, http.Header{"Retry-After": {"3"}})
	if h := limited.ResponseHeaders(); h.Get("Retry-After") != "3" {
		t.Fatalf("headers %v", h)
	}
	if d, ok := BodyRetryAfter(map[string]any{"error": map[string]any{"retry_after": 2.0}}); !ok || d != 2*time.Second {
		t.Fatalf("nested retry_after %v %v", d, ok)
	}
	if _, ok := BodyRetryAfter(map[string]any{"retry_after": 0.0}); ok {
		t.Fatal("a zero retry_after declares no wait")
	}
	overload := sdkStatusError(429, `{"error": {"code": "1305", "message": "The service may be temporarily overloaded"}}`, nil)
	if !IsZAICodingOverload("https://api.z.ai/api/coding/paas/v4", "glm-5.2", overload) {
		t.Fatal("SDK raised Z.AI overload")
	}
	if IsZAICodingOverload("https://api.z.ai/api/coding/paas/v4", "glm-5.2", sdkStatusError(429, rateLimitBody, nil)) {
		t.Fatal("plain rate limit is no overload")
	}
}

func TestStreamTransportFollowsTheRaisedClass(t *testing.T) {
	refused := &AuxSDKError{Class: "APIConnectionError", Message: "Connection error.", Err: &net.OpError{Op: "dial", Net: "tcp", Err: os.NewSyscallError("connect", syscall.ECONNREFUSED)}}
	if timeout, conn := streamErrorTransport(refused); timeout || conn {
		t.Fatal("an SDK connection error is not an httpx transport error")
	}
	req, _ := http.NewRequest(http.MethodPost, "http://127.0.0.1:19211/v1/chat/completions", bytes.NewReader(bytes.Repeat([]byte("x"), 45_000)))
	reset := sdkStreamReadError(&net.OpError{Op: "read", Net: "tcp", Err: os.NewSyscallError("read", syscall.ECONNRESET)}, &http.Response{Request: req})
	te, ok := reset.(*AuxTransportError)
	want := "read: connection reset by peer"
	if !ok || te.Class != "ReadError" || te.Message != want {
		t.Fatalf("reset read error %#v", reset)
	}
	if timeout, conn := streamErrorTransport(reset); timeout || !conn {
		t.Fatal("ReadError is a transient connection drop")
	}
	notice := ConnectExhaustedNotice(reset, 3, "http://127.0.0.1:19211/v1")
	if notice != "❌ Could not open a stream to 127.0.0.1 after 3 attempts (request 44 KB); the endpoint looks unreachable — try again in a moment." {
		t.Fatalf("notice %q", notice)
	}
}

func TestSDKTimeoutClassesNameTheirPhase(t *testing.T) {
	read := "Request timed out waiting for the provider to respond (if the model needs longer, raise providers.custom.request_timeout_seconds in config.yaml)"
	cases := map[string]error{
		read: auxSDKTimeoutError(&keepaliveTimeoutError{msg: "timed out"}),
		"Connection to the provider timed out before it was established (check the base_url and network path)": auxSDKTimeoutError(&net.OpError{Op: "dial", Net: "tcp", Err: &timeoutNetErr{}}),
	}
	for want, err := range cases {
		if got := SummarizeAPIError(err, "custom"); got != want {
			t.Fatalf("summary\n got %q\nwant %q", got, want)
		}
	}
	midStream := &AuxTransportError{Class: "ReadTimeout", Message: "timed out", Err: &keepaliveTimeoutError{msg: "timed out"}}
	if got := SummarizeAPIError(midStream, "custom"); got != read {
		t.Fatalf("mid stream read timeout %q", got)
	}
	if TimeoutPhase(&AuxTransportError{Class: "WriteTimeout", Message: "timed out"}) != "" || TimeoutPhase(&AuxSDKError{Class: "RateLimitError", StatusCode: 429}) != "" {
		t.Fatal("write timeouts and status errors name no phase")
	}
}

func TestNonStreamingMalformedJSONRaisesTheSDKDecodeError(t *testing.T) {
	broken := `{"id": "chatcmpl-1", "object": "chat.completion", "choices": [{"index": 0, "message": {"role": "assist`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/text/chat/completions" {
			w.Header().Set("Content-Type", "text/plain")
		} else {
			w.Header().Set("Content-Type", "application/json; charset=utf-8")
		}
		_, _ = w.Write([]byte(broken))
	}))
	defer srv.Close()
	zero := 0
	client := newAuxOpenAIClient("k", srv.URL, auxOpenAIOptions{MaxRetries: &zero})
	_, err := client.CreateChatCompletion(context.Background(), map[string]any{"model": "fake-model", "messages": []any{}})
	te, ok := err.(*AuxTransportError)
	if !ok || te.Class != "JSONDecodeError" || te.Message != "Unterminated string starting at: line 1 column 96 (char 95)" {
		t.Fatalf("JSON body that does not parse %#v", err)
	}
	if IsLocalValidationError(err) {
		t.Fatal("a JSONDecodeError is retried, never a local bug")
	}
	textClient := newAuxOpenAIClient("k", srv.URL+"/text", auxOpenAIOptions{MaxRetries: &zero})
	got, err := textClient.CreateChatCompletion(context.Background(), map[string]any{"model": "fake-model", "messages": []any{}})
	if err != nil || got != broken {
		t.Fatalf("a non JSON content type returns the text: %#v %v", got, err)
	}
}
