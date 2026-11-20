package provider

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/home"
)

const auxBedrockRetryModel = "us.amazon.nova-pro-v1:0"

// Bedrock runtime double refusing the first requests or a time window, then answering Converse
type auxBedrockRecoveringServer struct {
	mu        sync.Mutex
	hits      []string
	times     []time.Time
	mode      string
	failFirst int
	failFor   time.Duration
}

func (s *auxBedrockRecoveringServer) serve(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	s.hits = append(s.hits, r.URL.Path)
	s.times = append(s.times, time.Now())
	recovered := s.failFor == 0 && len(s.hits) > s.failFirst || s.failFor > 0 && s.times[len(s.times)-1].Sub(s.times[0]) >= s.failFor
	mode := s.mode
	s.mu.Unlock()
	status, errorType, payload := 400, "ThrottlingException", map[string]any{"message": "Too many requests, please wait before trying again."}
	if mode == "unavailable" {
		status, errorType, payload = 503, "ServiceUnavailableException", map[string]any{"message": "Service unavailable, try later."}
	}
	if recovered {
		status, errorType = 200, ""
		payload = map[string]any{"output": map[string]any{"message": map[string]any{"role": "assistant", "content": []any{map[string]any{"text": "pong"}}}},
			"stopReason": "end_turn", "usage": map[string]any{"inputTokens": 1, "outputTokens": 1, "totalTokens": 2}}
	}
	body, _ := json.Marshal(payload)
	w.Header().Set("Content-Type", "application/json")
	if errorType != "" {
		w.Header().Set("x-amzn-ErrorType", errorType)
	}
	w.WriteHeader(status)
	_, _ = w.Write(body)
}

func (s *auxBedrockRecoveringServer) hitCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.hits)
}

func (s *auxBedrockRecoveringServer) arrivalGap() time.Duration {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.times) < 2 {
		return 0
	}
	return s.times[1].Sub(s.times[0])
}

// Fake bedrock endpoint bound to an isolated home whose session_search task routes to bedrock
func auxFakeBedrockRecovery(t *testing.T, mode string, failFirst int, failFor time.Duration) (context.Context, *auxBedrockRecoveringServer) {
	t.Helper()
	s := &auxBedrockRecoveringServer{mode: mode, failFirst: failFirst, failFor: failFor}
	srv := httptest.NewServer(http.HandlerFunc(s.serve))
	t.Cleanup(srv.Close)
	h := bedrockIsolatedEnv(t)
	t.Setenv("AWS_ENDPOINT_URL_BEDROCK_RUNTIME", srv.URL)
	t.Setenv("AWS_ACCESS_KEY_ID", "AKIA-FAKE")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "fake-secret")
	t.Setenv("AWS_EC2_METADATA_DISABLED", "true")
	bedrockWriteConfig(t, h, "auxiliary:\n  session_search:\n    provider: bedrock\n    model: "+auxBedrockRetryModel+"\n    max_output_tokens: 0\n")
	t.Cleanup(ResetBedrockClientCache)
	return home.WithOverride(context.Background(), h), s
}

// Caches the sync bedrock client, its async twin for async requests
func auxSwapBedrockCachedClient(t *testing.T, client *BedrockAuxiliaryClient) {
	t.Helper()
	prev := auxGetCachedClientFn
	t.Cleanup(func() { auxGetCachedClientFn = prev })
	auxGetCachedClientFn = func(_ context.Context, req auxCachedClientRequest) (AuxClient, string, error) {
		if req.AsyncMode {
			return NewAsyncBedrockAuxiliaryClient(client), auxBedrockRetryModel, nil
		}
		return client, auxBedrockRetryModel, nil
	}
}

func TestAuxiliaryCallRecoversFromOneTransientBedrockRefusal(t *testing.T) {
	for _, mode := range []string{"unavailable", "throttle"} {
		ctx, s := auxFakeBedrockRecovery(t, mode, 1, 0)
		prevBackoff := auxTransientRetryBackoffBase
		auxTransientRetryBackoffBase = 0
		t.Cleanup(func() { auxTransientRetryBackoffBase = prevBackoff })
		auxSwapBedrockCachedClient(t, NewBedrockAuxiliaryClient("us-east-1", auxBedrockRetryModel))

		response, err := auxCallLLM(ctx, auxCallRequest{Task: "session_search", Messages: []any{map[string]any{"role": "user", "content": "ping"}}})
		if err != nil || auxActualContent(t, response) != "pong" {
			t.Fatalf("%s: got (%v, %v)", mode, response, err)
		}
		if s.hitCount() != 2 {
			t.Fatalf("%s: hits %d", mode, s.hitCount())
		}
	}
}

func TestAuxiliaryCallWaitsOutABedrockThrottleWindow(t *testing.T) {
	for _, caller := range []string{"sync", "async"} {
		ctx, s := auxFakeBedrockRecovery(t, "throttle", 0, 300*time.Millisecond)
		auxSwapBedrockCachedClient(t, NewBedrockAuxiliaryClient("us-east-1", auxBedrockRetryModel))
		messages := []any{map[string]any{"role": "user", "content": "ping"}}
		var response any
		var err error
		if caller == "sync" {
			response, err = auxCallLLM(ctx, auxCallRequest{Task: "session_search", Messages: messages})
		} else {
			response, err = AsyncCallLLM(ctx, map[string]any{"task": "session_search", "messages": messages})
		}
		if err != nil || auxActualContent(t, response) != "pong" {
			t.Fatalf("%s: got (%v, %v)", caller, response, err)
		}
		if gap := s.arrivalGap(); gap < 250*time.Millisecond {
			t.Fatalf("%s: the throttled call was re-sent without a backoff, gap %v", caller, gap)
		}
	}
}
