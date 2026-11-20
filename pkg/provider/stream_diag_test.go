package provider

import (
	"net/http"
	"strings"
	"testing"
)

type raisingFenceAgent struct{}

func (raisingFenceAgent) ClaimStreamWriter() int               { panic("boom") }
func (raisingFenceAgent) StreamWriterIsCurrent(token int) bool { panic("boom") }

func TestClaimSwallowsFenceExceptions(t *testing.T) {
	if got := ClaimStreamWriter(raisingFenceAgent{}); got != 0 {
		t.Fatalf("claim = %d, want 0", got)
	}
	if !StreamWriterIsCurrent(raisingFenceAgent{}, 7) {
		t.Fatal("a raising is_current must read as current")
	}
	if ClaimStreamWriter(struct{}{}) != 0 || !StreamWriterIsCurrent(struct{}{}, 3) {
		t.Fatal("a fenceless agent is never fenced")
	}
}

func TestStreamDiagCaptureResponseCollectsKnownHeaders(t *testing.T) {
	diag := StreamDiagInit()
	resp := &http.Response{StatusCode: 200, Header: http.Header{}}
	for k, v := range map[string]string{
		"CF-Ray": "8f1a2b3c4d5e6f7g-LAX", "x-openrouter-provider": "Anthropic", "x-openrouter-id": "gen-abc123",
		"x-request-id": "req-xyz", "server": "cloudflare", "irrelevant-header": "ignored",
	} {
		resp.Header.Set(k, v)
	}
	StreamDiagCaptureResponse(diag, resp)
	if diag.HTTPStatus == nil || *diag.HTTPStatus != 200 {
		t.Fatalf("http status %v", diag.HTTPStatus)
	}
	got := map[string]string{}
	for _, kv := range diag.Headers {
		got[kv[0]] = kv[1]
	}
	want := map[string]string{"cf-ray": "8f1a2b3c4d5e6f7g-LAX", "x-openrouter-provider": "Anthropic", "x-openrouter-id": "gen-abc123", "x-request-id": "req-xyz", "server": "cloudflare"}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("header %s = %q, want %q", k, got[k], v)
		}
	}
	if _, ok := got["irrelevant-header"]; ok {
		t.Fatal("unlisted header captured")
	}
	if diag.Headers[0][0] != "cf-ray" || diag.Headers[len(diag.Headers)-1][0] != "server" {
		t.Fatalf("headers not in listed order: %v", diag.Headers)
	}
}

type causedError struct {
	name, msg string
	cause     error
}

func (e *causedError) Error() string      { return e.msg }
func (e *causedError) Unwrap() error      { return e.cause }
func (e *causedError) PyTypeName() string { return e.name }

func TestFlattenExceptionChainWalksCause(t *testing.T) {
	inner := &causedError{name: "ConnectionError", msg: "upstream closed"}
	middle := &causedError{name: "TimeoutError", msg: "timed out", cause: inner}
	outer := &causedError{name: "RuntimeError", msg: "wrapper", cause: middle}
	chain := FlattenExceptionChain(outer)
	if chain != "RuntimeError(wrapper) <- TimeoutError(timed out) <- ConnectionError(upstream closed)" {
		t.Fatalf("chain %q", chain)
	}
}

func TestFlattenExceptionChainCapsDepth(t *testing.T) {
	var prev error = &causedError{name: "ValueError", msg: "0"}
	for i := 1; i < 8; i++ {
		prev = &causedError{name: "ValueError", msg: string(rune('0' + i)), cause: prev}
	}
	if chain := FlattenExceptionChain(prev); strings.Count(chain, "<-") > 3 {
		t.Fatalf("chain too deep: %q", chain)
	}
	long := &causedError{name: "OSError", msg: strings.Repeat("é", 150) + "\nx"}
	if got := FlattenExceptionChain(long); got != "OSError("+strings.Repeat("é", 140)+"…)" {
		t.Fatalf("long message %q", got)
	}
}

type diagAgent struct {
	statuses []string
	activity []string
}

func (a *diagAgent) StreamProvider() string             { return "" }
func (a *diagAgent) StreamBaseURL() string              { return "" }
func (a *diagAgent) BufferDiagnosticStatus(text string) { a.statuses = append(a.statuses, text) }
func (a *diagAgent) TouchActivity(desc string)          { a.activity = append(a.activity, desc) }
func (a *diagAgent) RetryStatusMessages() []string      { return a.statuses }

func TestConnectExhaustedNoticeOncePerTurn(t *testing.T) {
	agent := &diagAgent{}
	big := &StreamRequestError{Err: &causedError{name: "ConnectError", msg: "reset"}, Request: &FailedStreamRequest{URL: "https://api.example.com/v1/chat", Content: make([]byte, 829*1024)}}
	BufferConnectExhaustedNotice(agent, big, 2, "https://other.example")
	BufferConnectExhaustedNotice(agent, big, 2, "https://other.example")
	if len(agent.statuses) != 1 || agent.statuses[0] != "❌ Could not open a stream to api.example.com after 2 attempts (request 829 KB); the endpoint or a proxy in front of it may reject requests this large." {
		t.Fatalf("statuses %q", agent.statuses)
	}
	if got := ConnectExhaustedNotice(&causedError{name: "ConnectError", msg: "x"}, 1, ""); got != "❌ Could not open a stream to the endpoint after 1 attempt; the endpoint looks unreachable — try again in a moment." {
		t.Fatalf("bare notice %q", got)
	}
	small := &StreamRequestError{Err: &causedError{name: "ConnectError", msg: "reset"}, Request: &FailedStreamRequest{Content: []byte{}}}
	if got := ConnectExhaustedNotice(small, 3, "http://LocalHost:8080/v1"); got != "❌ Could not open a stream to localhost after 3 attempts (request 1 KB); the endpoint looks unreachable — try again in a moment." {
		t.Fatalf("small notice %q", got)
	}
	lifetime := &StreamLifetimeExceededError{Elapsed: 30, Cap: 20}
	if got := ConnectExhaustedNotice(lifetime, 2, ""); got != "❌ "+lifetime.Error()+" (2 attempts)" {
		t.Fatalf("lifetime notice %q", got)
	}
}

func TestEmitStreamDropStatusLine(t *testing.T) {
	agent := &diagAgent{}
	diag := StreamDiagInit()
	EmitStreamDrop(agent, &causedError{name: "RemoteProtocolError", msg: "peer closed"}, 2, 3, true, diag)
	if len(agent.statuses) != 1 || !strings.HasPrefix(agent.statuses[0], "⚠️ provider stream drop mid tool-call (RemoteProtocolError) after 0.") || !strings.HasSuffix(agent.statuses[0], " — attempt 2/3 dropped, reconnecting") {
		t.Fatalf("status %q", agent.statuses)
	}
	if agent.activity[0] != "stream retry 2/3 after RemoteProtocolError" {
		t.Fatalf("activity %q", agent.activity)
	}
}
