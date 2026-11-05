package provider

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"sync"
	"testing"
	"time"
)

const bedrockRetryModel = "us.amazon.nova-pro-v1:0"

// Bedrock runtime stub that hangs or refuses every call and counts arrivals
type bedrockRefusingServer struct {
	mu      sync.Mutex
	hits    int
	mode    string
	release chan struct{}
}

func newBedrockRefusingServer(t *testing.T, mode string) *bedrockRefusingServer {
	s := &bedrockRefusingServer{mode: mode, release: make(chan struct{})}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		s.hits++
		s.mu.Unlock()
		if s.mode == "hang" {
			select {
			case <-s.release:
			case <-time.After(30 * time.Second):
			}
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("x-amzn-ErrorType", "ThrottlingException")
		w.WriteHeader(400)
		_, _ = w.Write([]byte(`{"message":"Too many requests, please wait before trying again."}`))
	}))
	t.Cleanup(func() { close(s.release); srv.Close() })
	bedrockIsolatedEnv(t)
	t.Setenv("AWS_ENDPOINT_URL_BEDROCK_RUNTIME", srv.URL)
	t.Setenv("AWS_ACCESS_KEY_ID", "AKIA-FAKE")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "fake-secret")
	return s
}

func (s *bedrockRefusingServer) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.hits
}

func bedrockConverseOnce(t *testing.T) (*awsServiceClient, error) {
	c, err := bedrockRuntimeClient(context.Background(), "us-east-1", bedrockRetryModel)
	if err != nil {
		t.Fatal(err)
	}
	in := map[string]any{"messages": []any{map[string]any{"role": "user", "content": []any{map[string]any{"text": "hi"}}}}}
	var out map[string]any
	return c, c.doJSON(context.Background(), "Converse", http.MethodPost, bedrockModelPath(bedrockRetryModel, "converse"), nil, in, &out)
}

func TestBedrockHungConverseFailsAfterOneReadTimeout(t *testing.T) {
	s := newBedrockRefusingServer(t, "hang")
	bedrockWriteConfig(t, bedrockHomeOf(t), "providers:\n  bedrock:\n    request_timeout_seconds: 0.5\n")
	_, err := bedrockConverseOnce(t)
	var rt *AWSReadTimeoutError
	if !errors.As(err, &rt) {
		t.Fatalf("hung call: %v", err)
	}
	if s.count() != 1 {
		t.Fatalf("the timed out call was re-sent %d times", s.count())
	}
	if !ClassifyError(ClassifyInput{Err: err, Provider: "bedrock", Model: bedrockRetryModel}).Retryable {
		t.Fatal("read timeout must be retryable by the agent loop")
	}
}

func TestBedrockThrottleReachesTheAgentRetryLoopOnFirstRefusal(t *testing.T) {
	s := newBedrockRefusingServer(t, "throttle")
	c, err := bedrockConverseOnce(t)
	var svc *AWSServiceError
	if !errors.As(err, &svc) || svc.Code != "ThrottlingException" {
		t.Fatalf("throttle: %v", err)
	}
	if s.count() != 1 {
		t.Fatalf("the client retried the throttle %d times under the agent's own backoff", s.count())
	}
	if c.readTimeout != 60*time.Second {
		t.Fatalf("unset config read timeout %v", c.readTimeout)
	}
	got := ClassifyError(ClassifyInput{Err: err, Provider: "bedrock", Model: bedrockRetryModel})
	if got.Reason != ReasonRateLimit || !got.Retryable {
		t.Fatalf("classified %s retryable=%v", got.Reason, got.Retryable)
	}
}

// Icebath home the isolated environment points at
func bedrockHomeOf(t *testing.T) string {
	h := os.Getenv("ICEBATH_HOME")
	if h == "" {
		t.Fatal("isolated home not set")
	}
	return h
}
