package provider

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
)

func TestRetryAfterDropReportsTheAttemptThatDropped(t *testing.T) {
	agent := &diagAgent{}
	l := &doLoop{ctx: context.Background(), opts: DoOptions{DiagAgent: agent}, session: &RetrySession{},
		sleep: func(context.Context, time.Duration) error { return nil }}
	drop := &causedError{name: "ConnectionError", msg: "drop"}
	if err := l.retryAfterDrop(drop, 0, 2, false, nil); err != nil {
		t.Fatal(err)
	}
	if len(agent.statuses) != 1 || !strings.Contains(agent.statuses[0], "attempt 1/3 dropped") {
		t.Fatalf("statuses %q", agent.statuses)
	}
	if len(agent.activity) != 1 || agent.activity[0] != "stream retry 1/3 after "+pyrepr.TypeName(drop) {
		t.Fatalf("activity %q", agent.activity)
	}
}

func TestChatStreamFillsTheAttemptDiagnostics(t *testing.T) {
	ws := newWireServer(t, func(w http.ResponseWriter, r *http.Request, n int, body map[string]any) {
		sseFrames(w, chunk(map[string]any{"content": "héllo"}, nil, map[string]any{"provider": "  Fireworks  "}),
			chunk(map[string]any{"content": "!"}, nil, map[string]any{"provider": "Other"}),
			chunk(map[string]any{}, "stop", nil), "data: [DONE]\n\n")
	})
	c := newTestClient(t, ws.route("openrouter"))
	call := &streamCall{}
	call.start()
	var activity []string
	_, err := c.Stream(withStreamCall(context.Background(), call), userRequest("hi"), func(ev StreamEvent) error {
		if ev.Kind == EventActivity {
			activity = append(activity, ev.Text)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	d := call.currentDiag()
	if d == nil || d.Chunks != 3 || d.FirstChunkAt == nil || d.Bytes != 3*40+len([]rune("héllo"))+1 {
		t.Fatalf("diag %+v", d)
	}
	if d.ServingProvider != "Fireworks" || !d.FinishReasonSeen || d.HTTPStatus == nil || *d.HTTPStatus != 200 {
		t.Fatalf("diag identity %+v", d)
	}
	received := 0
	for _, a := range activity {
		if a == "receiving stream response" {
			received++
		}
	}
	if received != 3 {
		t.Fatalf("activity %q", activity)
	}
}

func TestStreamOpenFailureCarriesTheWireRequestToTheNotice(t *testing.T) {
	ws := newWireServer(t, func(http.ResponseWriter, *http.Request, int, map[string]any) {})
	route := ws.route("custom")
	ws.srv.Close()
	c := newTestClient(t, route)
	_, err := c.Stream(context.Background(), userRequest("hi"), func(StreamEvent) error { return nil })
	if err == nil {
		t.Fatal("closed endpoint answered")
	}
	req := failedRequest(err)
	if req == nil || !strings.HasPrefix(req.URL, route.BaseURL) || len(req.Content) == 0 {
		t.Fatalf("failed request %+v from %v", req, err)
	}
	var apiErr *APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("classification lost: %T", err)
	}
	if got := ConnectExhaustedNotice(err, 3, ""); !strings.Contains(got, "after 3 attempts (request 1 KB)") {
		t.Fatalf("notice %q", got)
	}
}

func TestEstimateChunkBytesCountsDeltaStrings(t *testing.T) {
	chat := map[string]any{"choices": []any{map[string]any{"delta": map[string]any{"content": "ab", "reasoning": "c",
		"tool_calls": []any{map[string]any{"function": map[string]any{"name": "fn", "arguments": "{}"}}}}}}}
	if got := estimateChunkBytes(chat); got != 40+2+1+2+2 {
		t.Fatalf("chat %d", got)
	}
	event := map[string]any{"delta": map[string]any{"partial_json": "{\"a\"", "text": 5}}
	if got := estimateChunkBytes(event); got != 44 {
		t.Fatalf("event %d", got)
	}
	if got := estimateChunkBytes(map[string]any{"choices": []any{}}); got != 40 {
		t.Fatalf("empty %d", got)
	}
}
