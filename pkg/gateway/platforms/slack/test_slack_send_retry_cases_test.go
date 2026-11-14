package slack

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"testing"
)

// test_slack_send_retry.py TestSlackSendRetryable.test_429_returns_retryable_with_retry_after
func TestSlackSendRetryable429ReturnsRetryableWithRetryAfter(t *testing.T) {
	a, f, _ := newHarnessAdapter(t, nil)
	f.RespondStatus("chat.postMessage", 429, map[string]string{"Retry-After": "30"}, map[string]any{"ok": false, "error": "ratelimited"})
	result, err := a.Send(context.Background(), "C123", "hello", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	if result.Success || !result.Retryable || result.RetryAfter == nil || *result.RetryAfter != 30.0 {
		t.Fatalf("result = %+v retry_after %v", result, result.RetryAfter)
	}
}

// test_slack_send_retry.py TestSlackSendRetryable.test_500_is_retryable_no_retry_after
func TestSlackSendRetryable500IsRetryableNoRetryAfter(t *testing.T) {
	a, f, _ := newHarnessAdapter(t, nil)
	f.RespondStatus("chat.postMessage", 500, nil, map[string]any{"ok": false, "error": "internal_error"})
	result, err := a.Send(context.Background(), "C123", "hello", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	if result.Success || !result.Retryable || result.RetryAfter != nil {
		t.Fatalf("result = %+v retry_after %v", result, result.RetryAfter)
	}
}

// test_slack_send_retry.py TestSlackSendRetryable.test_edit_api_failure_logs_the_slack_error_code
func TestSlackSendRetryableEditAPIFailureLogsTheSlackErrorCode(t *testing.T) {
	a, f, _ := newHarnessAdapter(t, nil)
	f.Respond("chat.update", map[string]any{"ok": false, "error": "message_not_found"})
	var logs bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelError})))
	result := a.editMessage(context.Background(), "C123", "123.456", "hello", false, nil)
	slog.SetDefault(prev)
	if result.Success {
		t.Fatal("edit succeeded")
	}
	if !strings.Contains(logs.String(), "api_error=message_not_found") {
		t.Fatalf("logs:\n%s", logs.String())
	}
}
