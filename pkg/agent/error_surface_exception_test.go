package agent

import (
	"net/http"
	"os"
	"syscall"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/provider"
)

type surfaceKeyError struct{ key string }

func (e surfaceKeyError) Error() string      { return "'" + e.key + "'" }
func (e surfaceKeyError) PyTypeName() string { return "KeyError" }

type surfaceStatusError struct {
	msg    string
	status int
}

func (e surfaceStatusError) Error() string   { return e.msg }
func (e surfaceStatusError) StatusCode() int { return e.status }

type surfaceHostileError struct{}

func (surfaceHostileError) Error() string   { return "x" }
func (surfaceHostileError) StatusCode() int { panic("hostile attribute") }

type surfaceRateLimitError struct {
	*provider.APIError
	headers http.Header
}

func (e surfaceRateLimitError) Unwrap() error                { return e.APIError }
func (e surfaceRateLimitError) ResponseHeaders() http.Header { return e.headers }

func TestExceptionNonAPIIsGatewayLayer(t *testing.T) {
	s := buildErrorSurfaceFromException(surfaceKeyError{"history"}, "", "", nil)
	if s["layer"] != LayerGateway || s["code"] != "KeyError" || s["retryable"] != true {
		t.Fatalf("surface %v", s)
	}
}

func TestExceptionDiskFullIsDiskLayer(t *testing.T) {
	s := buildErrorSurfaceFromException(&os.PathError{Op: "write", Path: "/state.db", Err: syscall.ENOSPC}, "", "", nil)
	if s["layer"] != LayerDisk {
		t.Fatalf("surface %v", s)
	}
}

func TestExceptionWithStatusCodeRoutesThroughClassifier(t *testing.T) {
	s := buildErrorSurfaceFromException(surfaceStatusError{"rate limited", 429}, "openrouter", "", nil)
	if s["layer"] != LayerProvider || (s["code"] != "rate_limit" && s["code"] != "upstream_rate_limit") {
		t.Fatalf("surface %v", s)
	}
}

func TestAnthropicUsageLimitRoutesToBillingRecovery(t *testing.T) {
	s := buildErrorSurfaceFromException(surfaceStatusError{"usage limit reached", 429}, "anthropic", "claude-opus-5", nil)
	want := map[string]any{"layer": LayerBilling, "code": "billing", "retryable": false, "provider": "anthropic", "provider_label": "Anthropic", "model": "claude-opus-5"}
	if len(s) != len(want) {
		t.Fatalf("surface %v", s)
	}
	for k, v := range want {
		if s[k] != v {
			t.Fatalf("surface %v, %s want %v", s, k, v)
		}
	}
}

func TestExceptionAuthStatusRoutesToAuthLayer(t *testing.T) {
	if s := buildErrorSurfaceFromException(surfaceStatusError{"invalid api key", 401}, "", "", nil); s["layer"] != LayerAuth {
		t.Fatalf("surface %v", s)
	}
}

func TestExceptionNeverRaisesOnWeirdInput(t *testing.T) {
	buildErrorSurfaceFromException(surfaceHostileError{}, "", "", nil)
}

func TestRateLimitResetRidesTheSurface(t *testing.T) {
	rateLimit := func(headers http.Header, body string) error {
		return surfaceRateLimitError{&provider.APIError{StatusCode: 429, Message: "HTTP 429: The usage limit has been reached", Body: body}, headers}
	}
	before := float64(time.Now().UnixNano()) / 1e9
	s := buildErrorSurfaceFromException(rateLimit(http.Header{"Retry-After": {"3600"}},
		`{"error": {"message": "The usage limit has been reached", "type": "usage_limit_reached"}}`), "openai", "gpt-5", nil)
	resets, _ := s["resets_at"].(float64)
	if s["code"] != "rate_limit" || s["retryable"] != true || resets < before+3500 || resets > float64(time.Now().UnixNano())/1e9+3600 {
		t.Fatalf("surface %v", s)
	}
	result := map[string]any{"error": "HTTP 429: The usage limit has been reached", "failure_reason": "rate_limit",
		"failure_retryable": true, "failure_resets_at": int64(1_800_000_000)}
	if got := buildErrorSurfaceFromResult(result, "openai", "")["resets_at"]; got != 1_800_000_000.0 {
		t.Fatalf("result resets_at %v", got)
	}
	bare := buildErrorSurfaceFromException(rateLimit(http.Header{}, `{"error": {"message": "Rate limit exceeded"}}`), "openai", "gpt-5", nil)
	if _, has := bare["resets_at"]; has {
		t.Fatalf("bare 429 carried resets_at %v", bare)
	}
}
