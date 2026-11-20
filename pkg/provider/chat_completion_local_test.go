package provider

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// Local llama-server stand in answering health with the given status, counting each probe
func healthServer(t *testing.T, status int) (string, *int) {
	t.Helper()
	probes := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/health" {
			probes++
		}
		w.WriteHeader(status)
	}))
	t.Cleanup(srv.Close)
	return srv.URL + "/v1", &probes
}

func TestLocalStaleBreakerResetHealthyLocalServerGetsOneAttempt(t *testing.T) {
	base, _ := healthServer(t, http.StatusOK)
	s := &RetrySession{}
	s.staleStreak.Store(5)
	if !s.RetryRecoveredLocalServer(ModeChatCompletions, "llamacpp", base) {
		t.Fatal("healthy local server refused")
	}
	if err := checkStaleGiveup(s); err != nil {
		t.Fatalf("next call blocked: %v", err)
	}
	s.staleStreak.Add(1)
	if err := checkStaleGiveup(s); err == nil || !strings.Contains(err.Error(), "unresponsive") {
		t.Fatalf("one more stale attempt must shut the breaker, got %v", err)
	}
}

func TestLocalStaleBreakerResetClosedBreakerSkipsTheHealthCheck(t *testing.T) {
	base, probes := healthServer(t, http.StatusOK)
	s := &RetrySession{}
	s.staleStreak.Store(2)
	if s.RetryRecoveredLocalServer(ModeChatCompletions, "llamacpp", base) || *probes != 0 || s.StaleStreak() != 2 {
		t.Fatalf("closed breaker probed %d streak %d", *probes, s.StaleStreak())
	}
}

func TestLocalStaleBreakerResetUnhealthyOrCloudKeepsStreak(t *testing.T) {
	sick, _ := healthServer(t, http.StatusServiceUnavailable)
	s := &RetrySession{}
	s.staleStreak.Store(5)
	if s.RetryRecoveredLocalServer(ModeChatCompletions, "llamacpp", sick) {
		t.Fatal("unhealthy server retried")
	}
	healthy, probes := healthServer(t, http.StatusOK)
	if s.RetryRecoveredLocalServer(ModeChatCompletions, "openrouter", healthy) || *probes != 0 {
		t.Fatal("cloud route probed")
	}
	if s.StaleStreak() != 5 {
		t.Fatalf("streak %d", s.StaleStreak())
	}
}
