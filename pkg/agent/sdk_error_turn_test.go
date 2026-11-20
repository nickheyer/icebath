package agent

import (
	"context"
	"net/http"
	"reflect"
	"regexp"
	"strconv"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/provider"
	"github.com/nickheyer/icebath/pkg/state"
)

func TestSurfaceStatusReadsSDKAndStreamErrors(t *testing.T) {
	if status, ok := surfaceExceptionStatus(&provider.AuxSDKError{Class: "RateLimitError", StatusCode: 429}); !ok || status != 429 {
		t.Fatalf("SDK status error %d %v", status, ok)
	}
	if _, ok := surfaceExceptionStatus(&provider.AuxSDKError{Class: "APIConnectionError", Message: "Connection error."}); ok {
		t.Fatal("a connection error has no status_code")
	}
	if status, ok := surfaceExceptionStatus(&provider.ProviderStreamError{StatusCode: 502}); !ok || status != 502 {
		t.Fatalf("stream error status %d %v", status, ok)
	}
}

func TestProviderRetryAfterReadsSDKErrorHeaderAndBody(t *testing.T) {
	header := &provider.AuxSDKError{Class: "RateLimitError", StatusCode: 429, Headers: http.Header{"Retry-After": {"3"}}}
	if d, ok := providerRetryAfterSeconds(header); !ok || d != 3*time.Second {
		t.Fatalf("Retry-After header %v %v", d, ok)
	}
	body := &provider.AuxSDKError{Class: "InternalServerError", StatusCode: 503, Body: map[string]any{"retry_after": 2.0}}
	if d, ok := providerRetryAfterSeconds(body); !ok || d != 2*time.Second {
		t.Fatalf("retry_after body field %v %v", d, ok)
	}
	if _, ok := providerRetryAfterSeconds(&provider.AuxSDKError{Class: "RateLimitError", StatusCode: 429}); ok {
		t.Fatal("no declared cooldown")
	}
}

func TestRetryStatusMessagesReadsTheBuffer(t *testing.T) {
	a := &AIAgent{}
	a.BufferDiagnosticStatus("❌ Could not open a stream to 127.0.0.1 after 3 attempts")
	a.bufferVprint("   💀 Final error")
	if got := a.RetryStatusMessages(); !reflect.DeepEqual(got, []string{"❌ Could not open a stream to 127.0.0.1 after 3 attempts", "   💀 Final error"}) {
		t.Fatalf("messages %q", got)
	}
}

func TestEmptyRetryGuardReadsTheRawResponseUsage(t *testing.T) {
	a := newFixtureAgent(t, AIAgentOptions{BaseURL: "http://127.0.0.1:19211/v1", Model: "fake-model"})
	a.interruptRequested.Store(true)
	s := &LoopState{Response: map[string]any{"usage": map[string]any{"prompt_tokens": 1200, "completion_tokens": 80, "total_tokens": 1280}}}
	msg := &provider.Response{Message: &state.Message{Role: "assistant"}, FinishReason: "stop"}
	if _, err := recoverEmptyResponse(context.Background(), a, s, msg, "stop", ""); err != nil {
		t.Fatal(err)
	}
	if h := a.emptyAttemptHistory; len(h) != 1 || !h[0].usagePresent || h[0].zeroOutput {
		t.Fatalf("an empty reply that billed output tokens is no deterministic empty: %+v", h)
	}
}

func TestEmptyAndInvalidResponseWaitsCarryTheDefaultJitter(t *testing.T) {
	a := newFixtureAgent(t, AIAgentOptions{BaseURL: "http://127.0.0.1:19211/v1", Model: "fake-model"})
	emptyRe := regexp.MustCompile(`retrying \(1/3\) in (\d+)s`)
	invalidRe := regexp.MustCompile(`Retrying in ([0-9.]+)s`)
	emptyJittered, invalidJittered := false, false
	for i := 0; i < 40; i++ {
		a.emptyContentRetries, a.emptyAttemptHistory = 0, nil
		a.clearStatusBuffer()
		a.interruptRequested.Store(true)
		s := &LoopState{Response: map[string]any{}, MaxRetries: 3, Retry: &TurnRetryState{}}
		if _, _, _, err := retryEmpty(context.Background(), a, s, s.Response, "stop", true, false); err != nil {
			t.Fatal(err)
		}
		a.interruptRequested.Store(true)
		if _, err := retryInvalidResponse(context.Background(), a, s, []string{"response.choices is empty"}); err != nil {
			t.Fatal(err)
		}
		for _, line := range a.RetryStatusMessages() {
			if m := emptyRe.FindStringSubmatch(line); m != nil {
				n, _ := strconv.Atoi(m[1])
				if n < 5 || n > 8 {
					t.Fatalf("empty retry wait %ds outside 5s plus half jitter", n)
				}
				emptyJittered = emptyJittered || n > 5
			}
			if m := invalidRe.FindStringSubmatch(line); m != nil {
				f, _ := strconv.ParseFloat(m[1], 64)
				if f < 5 || f > 7.5 {
					t.Fatalf("invalid response wait %.1fs outside 5s plus half jitter", f)
				}
				invalidJittered = invalidJittered || f > 5.05
			}
		}
	}
	if !emptyJittered || !invalidJittered {
		t.Fatalf("waits never jittered: empty %v invalid %v", emptyJittered, invalidJittered)
	}
}
