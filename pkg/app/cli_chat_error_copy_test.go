package app

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/provider"
)

func TestChatErrorResponseLeadsWithPlainCopyAndPointer(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	cases := []struct {
		name    string
		exc     error
		pointer string
		absent  string
	}{
		{"auth", &provider.APIError{Message: "HTTP 401: Invalid API key", StatusCode: 401}, "icebath model", "HTTP 401"},
		{"billing", errors.New("Error code: 402 - insufficient credits"), "/model", "402"},
		{"model_not_found", errors.New("HTTP 404: model not found"), "/model", "HTTP 404"},
		{"rate_limit", &provider.APIError{Message: "rate limit exceeded", StatusCode: 429}, "/model", "429"},
		{"unknown", errors.New("Unknown error"), "icebath doctor", "Unknown error"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			text := chatErrorResponse(context.Background(), tc.exc, "openrouter", "foo/bar", "")
			lead := strings.SplitN(text, "\n", 2)[0]
			if !strings.Contains(lead, tc.pointer) {
				t.Fatalf("lead %q lacks %q", lead, tc.pointer)
			}
			if strings.Contains(lead, tc.absent) {
				t.Fatalf("raw HTTP code or exception text must not be the lead sentence: %q", lead)
			}
			if strings.HasPrefix(lead, "Error:") {
				t.Fatalf("lead %q starts with Error:", lead)
			}
			if !strings.Contains(text, "Details: ") || !strings.Contains(text, tc.exc.Error()) {
				t.Fatalf("text %q lacks the details line", text)
			}
		})
	}
}

func TestChatErrorResponseAcceptsPlainStringSummary(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	text := chatErrorResponse(context.Background(), "HTTP 401: Invalid API key", "nous", "m", "")
	if lead := strings.SplitN(text, "\n", 2)[0]; !strings.Contains(lead, "icebath model") {
		t.Fatalf("lead %q", lead)
	}
}

func TestChatErrorResponseTrustsStampedProviderVerdictOverReclassifyingText(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	text := chatErrorResponse(context.Background(), "upstream said no", "openrouter", "m", "rate_limit")
	if lead := strings.SplitN(text, "\n", 2)[0]; !strings.Contains(lead, "Rate limited") {
		t.Fatalf("lead %q", lead)
	}
	if !strings.Contains(text, "Details: upstream said no") {
		t.Fatalf("text %q", text)
	}
}

func TestChatErrorResponseReturnsSiteCopyVerbatimInsteadOfDoubleWrapping(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	curated := "The model's reply was cut off before it finished. Send `continue`."
	text := chatErrorResponse(context.Background(), curated, "openrouter", "m", "truncated")
	if text != curated || strings.Contains(text, "Details:") {
		t.Fatalf("got %q", text)
	}
}

func TestUnknownCommandSuggestsCloseMatchAndSaysNothingSent(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	lead, pointer := unknownCommandLines(context.Background(), "/modle", []string{"/model", "/help", "/quit"})
	for _, want := range []string{"/modle", "nothing was sent", "Did you mean /model?"} {
		if !strings.Contains(lead, want) {
			t.Fatalf("lead %q lacks %q", lead, want)
		}
	}
	if !strings.Contains(pointer, "/help") {
		t.Fatalf("pointer %q", pointer)
	}
}

func TestUnknownCommandWithoutCloseMatchHasNoSuggestion(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	lead, pointer := unknownCommandLines(context.Background(), "/zzzzzz", []string{"/model", "/help"})
	if strings.Contains(lead, "Did you mean") || !strings.Contains(pointer, "/help") {
		t.Fatalf("lead %q pointer %q", lead, pointer)
	}
}
