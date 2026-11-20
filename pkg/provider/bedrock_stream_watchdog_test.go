package provider

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/state"
)

// Fake Bedrock runtime answering ConverseStream with the handler, held open when it asks, and Converse with a plain hi
func bedrockStreamServer(t *testing.T, stream func(w http.ResponseWriter) bool) {
	t.Helper()
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/converse-stream") {
			if !stream(w) {
				return
			}
			select {
			case <-release:
			case <-r.Context().Done():
			}
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"output":{"message":{"role":"assistant","content":[{"text":"hi"}]}},"stopReason":"end_turn","usage":{"inputTokens":1,"outputTokens":1,"totalTokens":2}}`))
	}))
	t.Cleanup(func() { close(release); srv.Close() })
	bedrockIsolatedEnv(t)
	t.Setenv("AWS_ENDPOINT_URL_BEDROCK_RUNTIME", srv.URL)
	t.Setenv("AWS_ACCESS_KEY_ID", "AKIA-FAKE")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "fake-secret")
}

// Streams one Bedrock turn under a retry session and collects the status events
func bedrockStreamTurn(t *testing.T, session *RetrySession) (*Response, []StreamEvent, error) {
	t.Helper()
	client, err := NewClient(Route{Provider: "bedrock", Model: bedrockRetryModel, Mode: modeBedrockConverse, Region: "us-east-1", APIKey: "aws-sdk"})
	if err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	var statuses []StreamEvent
	resp, err := client.Stream(withStaleStreak(t.Context(), session), &Request{Messages: []*state.Message{{Role: "user", Content: "hi"}}, MaxTokens: 64}, func(ev StreamEvent) error {
		if ev.Kind == EventStatus {
			mu.Lock()
			statuses = append(statuses, ev)
			mu.Unlock()
		}
		return nil
	})
	return resp, statuses, err
}

func TestBedrockStreamStaleEventGapAbortsEvictsTheClientAndFeedsTheBreaker(t *testing.T) {
	bedrockStreamServer(t, func(w http.ResponseWriter) bool {
		w.Header().Set("Content-Type", "application/vnd.amazon.eventstream")
		w.WriteHeader(200)
		_, _ = w.Write(bc2Frame(t, "messageStart", map[string]any{"role": "assistant"}))
		w.(http.Flusher).Flush()
		return true
	})
	bedrockWriteConfig(t, bedrockHomeOf(t), "providers:\n  bedrock:\n    stale_timeout_seconds: 0.4\n")
	prev := bedrockStreamPoll
	bedrockStreamPoll = 50 * time.Millisecond
	t.Cleanup(func() { bedrockStreamPoll = prev })
	before, err := bedrockRuntimeClient(t.Context(), "us-east-1", bedrockRetryModel)
	if err != nil {
		t.Fatal(err)
	}
	session := &RetrySession{}
	logs := captureLogs(t)
	_, statuses, err := bedrockStreamTurn(t, session)
	var te *TimeoutPhaseError
	if !errors.As(err, &te) || !strings.Contains(err.Error(), "Bedrock stream produced no events for 0s (threshold 0s)") {
		t.Fatalf("err %v", err)
	}
	if session.StaleStreak() != 1 || len(statuses) != 1 || !strings.Contains(statuses[0].Text, "No events from Bedrock for 0s (model: "+bedrockRetryModel+")") {
		t.Fatalf("streak %d statuses %+v", session.StaleStreak(), statuses)
	}
	if logCount(logs, "Bedrock stream stale for") != 1 {
		t.Fatalf("logs %v", logs.lines)
	}
	if after, _ := bedrockRuntimeClient(t.Context(), "us-east-1", bedrockRetryModel); after == before {
		t.Fatal("stale stream kept the cached runtime client")
	}
	t.Setenv("ICEBATH_STREAM_STALE_GIVEUP", "2")
	if _, _, err := bedrockStreamTurn(t, session); err == nil || !strings.Contains(err.Error(), "2 consecutive stale attempts") {
		t.Fatalf("giveup err %v", err)
	}
}

func TestStreamingDenialFallsBackToConverseViaBedrockStream(t *testing.T) {
	bedrockStreamServer(t, func(w http.ResponseWriter) bool {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("x-amzn-ErrorType", "AccessDeniedException")
		w.WriteHeader(403)
		_, _ = w.Write([]byte(`{"message":"User is not authorized to perform: bedrock:InvokeModelWithResponseStream"}`))
		return false
	})
	before, err := bedrockRuntimeClient(t.Context(), "us-east-1", bedrockRetryModel)
	if err != nil {
		t.Fatal(err)
	}
	session := &RetrySession{}
	resp, statuses, err := bedrockStreamTurn(t, session)
	if err != nil || resp.Message.Text() != "hi" || !session.DisableStreaming {
		t.Fatalf("resp %+v err %v disable %v", resp, err, session.DisableStreaming)
	}
	if len(statuses) != 1 || !statuses[0].Diagnostic || !strings.Contains(statuses[0].Text, "InvokeModelWithResponseStream") {
		t.Fatalf("statuses %+v", statuses)
	}
	if after, _ := bedrockRuntimeClient(t.Context(), "us-east-1", bedrockRetryModel); after != before {
		t.Fatal("denial evicted the cached runtime client")
	}
}

func TestExplicitInterruptKeepsPartialResponseContract(t *testing.T) {
	interrupted := false
	events := []map[string]any{
		{"contentBlockDelta": map[string]any{"delta": map[string]any{"text": "partial"}}},
		{"contentBlockStop": map[string]any{"contentBlockIndex": 0}},
	}
	result, stopped, err := streamConverseWithCallbacks(bedrockEventList(events), bedrockStreamCallbacks{
		OnTextDelta:      func(string) error { interrupted = true; return nil },
		OnInterruptCheck: func() bool { return interrupted },
	})
	if err != nil || !stopped || result.Content != "partial" {
		t.Fatalf("result %+v stopped %v err %v", result, stopped, err)
	}
	final, err := finalizeBedrockRelayEvents([]any{events[0], events[1]})
	if err != nil || final != nil {
		t.Fatalf("finalizer recorded %#v %v", final, err)
	}
}
