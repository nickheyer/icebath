package provider

import (
	"errors"
	"fmt"
	"net"
	"strings"
	"testing"
)

func dialTimeout() error {
	return &net.OpError{Op: "dial", Net: "tcp", Err: &timeoutNetErr{}}
}

type timeoutNetErr struct{}

func (e *timeoutNetErr) Error() string   { return "i/o timeout" }
func (e *timeoutNetErr) Timeout() bool   { return true }
func (e *timeoutNetErr) Temporary() bool { return true }

func TestSDKWrappedTimeoutsNameThePhase(t *testing.T) {
	connect := SummarizeAPIError(&TimeoutPhaseError{Phase: TimeoutPhaseRead, Err: dialTimeout()}, "")
	if !strings.HasPrefix(connect, "Connection to the provider timed out before it was established") {
		t.Fatalf("connect summary %q", connect)
	}
	read := SummarizeAPIError(&TimeoutPhaseError{Phase: TimeoutPhaseRead, Err: &net.OpError{Op: "read", Net: "tcp", Err: &timeoutNetErr{}}}, "")
	midstream := SummarizeAPIError(&net.OpError{Op: "read", Net: "tcp", Err: &timeoutNetErr{}}, "")
	for _, text := range []string{read, midstream} {
		if !strings.HasPrefix(text, "Request timed out waiting for the provider") || !strings.Contains(text, "request_timeout_seconds") {
			t.Fatalf("read summary %q", text)
		}
	}
	if phase := TimeoutPhase(&APIError{Kind: ErrTimeout, Err: dialTimeout()}); phase != TimeoutPhaseConnect {
		t.Fatalf("api error wrapping a dial timeout: %q", phase)
	}
}

func TestNonTimeoutErrorsKeepTheirSummary(t *testing.T) {
	if got := SummarizeAPIError(errors.New("upstream exploded"), ""); got != "upstream exploded" {
		t.Fatalf("summary %q", got)
	}
}

func TestPoolAndWriteTimeoutsGetNoProviderResponseHint(t *testing.T) {
	for _, phase := range []string{TimeoutPhasePool, TimeoutPhaseWrite} {
		if got := SummarizeAPIError(&TimeoutPhaseError{Phase: phase, Err: errors.New("Request timed out.")}, ""); got != "Request timed out." {
			t.Fatalf("%s summary %q", phase, got)
		}
	}
	if got := SummarizeAPIError(&net.OpError{Op: "write", Net: "tcp", Err: &timeoutNetErr{}}, ""); strings.Contains(got, "provider") {
		t.Fatalf("write timeout must keep the raw summary: %q", got)
	}
}

func TestReadTimeoutHintNamesTheProviderKey(t *testing.T) {
	err := &TimeoutPhaseError{Phase: TimeoutPhaseRead, Err: errors.New("timed out")}
	if got := SummarizeAPIError(err, "openrouter"); !strings.Contains(got, "providers.openrouter.request_timeout_seconds") {
		t.Fatalf("summary %q", got)
	}
	if got := SummarizeAPIError(err, ""); !strings.Contains(got, "providers.<provider>.request_timeout_seconds") {
		t.Fatalf("summary %q", got)
	}
}

var cloudflareChallengeHTML = "<!DOCTYPE html>\n<html>\n  <head>\n" +
	`    <meta http-equiv="refresh" content="360"></head>` + "\n" +
	"  <body>\n    <div class=\"data\"><noscript>Enable JavaScript and cookies to continue</noscript>" +
	"<script>(function(){window._cf_chl_opt = {cRay: 'a0ca002c4f91769c',cZone: 'chatgpt.com', cType: 'managed', md: '" + strings.Repeat("x", 4000) + "',};})();</script></div>\n  </body>\n</html>\n"

func TestSummarizeCollapsesCloudflareChallengePage(t *testing.T) {
	summary := SummarizeAPIError(&APIError{StatusCode: 403, Message: cloudflareChallengeHTML, Body: cloudflareChallengeHTML}, "openai")
	lower := strings.ToLower(summary)
	if strings.Contains(lower, "<html") || strings.Contains(lower, "<!doctype") || strings.Contains(summary, "_cf_chl_opt") {
		t.Fatalf("html leaked: %q", summary)
	}
	if len(summary) >= 200 || !strings.Contains(summary, "403") {
		t.Fatalf("summary %q", summary)
	}
	titled := SummarizeAPIError(&APIError{StatusCode: 502, Body: `<html><head><title>502 Bad Gateway</title></head><body>Cloudflare Ray ID: <strong class="x">abc123</strong></body></html>`}, "")
	if titled != "HTTP 502 — 502 Bad Gateway — Ray abc123" {
		t.Fatalf("titled summary %q", titled)
	}
}

func TestEmptyBodyFallsBackToResponseJSONErrorMessage(t *testing.T) {
	summary := SummarizeAPIError(&APIError{StatusCode: 400, Body: `{"error": {"message": "model ` + "`foo`" + ` does not exist", "type": "invalid_request_error"}}`}, "")
	if !strings.Contains(summary, "HTTP 400") || !strings.Contains(summary, "model `foo` does not exist") {
		t.Fatalf("summary %q", summary)
	}
	if got := SummarizeAPIError(&APIError{StatusCode: 400, Body: `{"message": "top level"}`}, ""); got != "HTTP 400: top level" {
		t.Fatalf("top level message %q", got)
	}
	if got := SummarizeAPIError(&APIError{StatusCode: 429, Body: "plain text body with sk-abcdefghijklmnopqrstuvwxyz0123"}, ""); strings.Contains(got, "sk-abcdefghijklmnopqrstuvwxyz0123") {
		t.Fatalf("secret leaked %q", got)
	}
	if got := SummarizeAPIError(&APIError{StatusCode: 429, Message: "Gemini HTTP 429: quota exceeded"}, ""); got != "HTTP 429: Gemini HTTP 429: quota exceeded" {
		t.Fatalf("exception message fallback %q", got)
	}
}

func TestNetworkResolutionFailureExplainsOffline(t *testing.T) {
	for _, msg := range []string{"Temporary failure in name resolution", "Name or service not known", "nodename nor servname provided, or not known", "getaddrinfo failed", "No address associated with hostname", "Network is unreachable"} {
		summary := SummarizeAPIError(fmt.Errorf("[Errno -3] %s", msg), "")
		if !strings.Contains(summary, "You may be offline") || strings.Contains(strings.ToLower(summary), "name resolution") {
			t.Fatalf("%s: %q", msg, summary)
		}
	}
	wrapped := fmt.Errorf("Connection error.: %w", &net.DNSError{Err: "no such host", Name: "api.example.com", IsNotFound: true})
	summary := SummarizeAPIError(wrapped, "")
	if !strings.Contains(summary, "You may be offline") || strings.Contains(summary, "Connection error") {
		t.Fatalf("wrapped dns: %q", summary)
	}
}

func TestStreamParseAndXAIDecoration(t *testing.T) {
	if !IsProviderStreamParseError(errors.New("EOF while parsing a value at line 1 column 10")) {
		t.Fatal("stream parse marker")
	}
	if got := SummarizeAPIError(errors.New("expected value at line 1 column 1"), ""); !strings.HasPrefix(got, "Malformed provider streaming response: ") {
		t.Fatalf("stream parse summary %q", got)
	}
	detail := "You do not have an active Grok subscription"
	decorated := DecorateXAIEntitlementError(detail)
	if !strings.HasSuffix(decorated, XAIEntitlementHint) || DecorateXAIEntitlementError(decorated) != decorated {
		t.Fatalf("decoration %q", decorated)
	}
	if DecorateXAIEntitlementError("plain") != "plain" {
		t.Fatal("plain detail untouched")
	}
	summary := SummarizeAPIError(&APIError{StatusCode: 403, Body: `{"error":{"message":"` + detail + `"}}`}, "xai-oauth")
	if !strings.Contains(summary, "X Premium+ does NOT include") {
		t.Fatalf("summary not decorated: %q", summary)
	}
}

func TestIsEntitlementFailure(t *testing.T) {
	if !IsEntitlementFailure(map[string]any{"message": "You do not have an active Grok subscription"}, 403) {
		t.Fatal("entitlement text")
	}
	if IsEntitlementFailure(map[string]any{"message": "does not have permission to execute grok", "error": "[WKE=unauthenticated:token expired]"}, 403) {
		t.Fatal("stale token signal must defer to the refresh path")
	}
	if IsEntitlementFailure(map[string]any{"message": "oauth2 access token could not be validated grok"}, 401) {
		t.Fatal("validation failure is recoverable")
	}
	if IsEntitlementFailure(map[string]any{"message": "You do not have an active Grok subscription"}, 500) {
		t.Fatal("only 401 403 or unknown status")
	}
	if IsEntitlementFailure(nil, 403) || IsEntitlementFailure(map[string]any{"message": ""}, 0) {
		t.Fatal("empty context")
	}
}

func TestCoerceAPIErrorDetail(t *testing.T) {
	if CoerceAPIErrorDetail("x") != "x" || CoerceAPIErrorDetail(nil) != "" {
		t.Fatal("string and nil")
	}
	if CoerceAPIErrorDetail(map[string]any{"message": "  ", "detail": "d"}) != "d" {
		t.Fatal("first non blank key")
	}
	if CoerceAPIErrorDetail(map[string]any{"error": map[string]any{"code": "c"}}) != "c" {
		t.Fatal("nested detail")
	}
	if CoerceAPIErrorDetail(map[string]any{"b": 1, "a": 2}) != `{"a":2,"b":1}` {
		t.Fatal("sorted json fallback")
	}
	if CoerceAPIErrorDetail([]any{"a", "", map[string]any{"message": "b"}}) != "a; b" {
		t.Fatal("list join")
	}
	if CoerceAPIErrorDetail(42) != "42" {
		t.Fatal("other values stringify")
	}
}

func TestMaskAndCleanHelpers(t *testing.T) {
	if MaskAPIKeyForLogs("") != "" || MaskAPIKeyForLogs("short") != "***" || MaskAPIKeyForLogs("sk-abcdefghijklmnopqrstuvwxyz") != "sk-abcde...wxyz" {
		t.Fatal("mask")
	}
	if MaskAPIKeyForLogs(func() string { return "x" }) != "<entra-id-bearer>" {
		t.Fatal("callable mask")
	}
	if CleanErrorMessage("") != "Unknown error" || CleanErrorMessage("<!DOCTYPE html><html>") != "Service temporarily unavailable (HTML error page returned)" {
		t.Fatal("clean html")
	}
	if CleanErrorMessage("  a \n b  ") != "a b" || len([]rune(CleanErrorMessage(strings.Repeat("x", 200)))) != 153 {
		t.Fatal("clean whitespace and cap")
	}
}
