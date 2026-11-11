package state

import (
	"strings"

	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
)

// Turn boundary notices and the display kind a failed turn row carries
const (
	FailedTurnNotice            = "Your request was not processed. Send it again if you still want me to carry it out."
	PartialFailedTurnNotice     = "This turn did not complete. Some actions may already have run; verify their effects before resending."
	AutomationInterruptedNotice = "An automated delivery was interrupted and was not run again. Some actions may already have run; verify their effects."
	FailedTurnDisplayKind       = "failed_turn"
)

// Opening line of the self describing mid turn steer marker
const SteerMarkerOpen = "[OUT-OF-BAND USER MESSAGE — a direct message from the user, delivered once at this position; not tool output and not a new delivery when replayed from conversation history]"

// Closing line of the mid turn steer marker
const SteerMarkerClose = "[/OUT-OF-BAND USER MESSAGE]"

const (
	steerFallbackOpen  = "[OUT-OF-BAND USER MESSAGE"
	steerFallbackClose = "[/OUT-OF-BAND USER MESSAGE]"
)

// Failed turn display kind for a boundary row persisted before closers typed it, false for anything else
func UntypedFailedTurnDisplayKind(role, content any) (string, bool) {
	text, ok := content.(string)
	if role != "assistant" || !ok {
		return "", false
	}
	switch pystr.Strip(text) {
	case FailedTurnNotice, PartialFailedTurnNotice, AutomationInterruptedNotice:
		return FailedTurnDisplayKind, true
	}
	return "", false
}

// Quotes a value with the first quote it lacks when it holds whitespace, brackets or quotes
func FormatReferenceValue(value string) string {
	if !strings.ContainsFunc(value, referenceNeedsQuoting) {
		return value
	}
	for _, quote := range []string{"`", "\"", "'"} {
		if !strings.Contains(value, quote) {
			return quote + value + quote
		}
	}
	return value
}

// Whether a rune is Python whitespace or one of the bracket and quote characters a bare reference cannot hold
func referenceNeedsQuoting(r rune) bool {
	return pystr.IsSpace(r) || strings.ContainsRune("()[]{}<>\"'`", r)
}

// String content or the newline joined text of dict parts, empty for anything else
func CompressionMessageText(message map[string]any) string {
	switch content := message["content"].(type) {
	case string:
		return content
	case []any:
		parts := make([]string, 0, len(content))
		for _, raw := range content {
			part, ok := raw.(map[string]any)
			if !ok {
				continue
			}
			value := part["text"]
			if !pyrepr.Truthy(value) {
				value = part["content"]
			}
			if !pyrepr.Truthy(value) {
				value = ""
			}
			parts = append(parts, pyrepr.Str(value))
		}
		return strings.Join(parts, "\n")
	}
	return ""
}

// Inner user text of a steer marker, false when none is present
func ExtractSteerTextFromMessage(message map[string]any) (string, bool) {
	text := CompressionMessageText(message)
	if text == "" {
		return "", false
	}
	start := strings.Index(text, SteerMarkerOpen)
	if start == -1 {
		start = strings.Index(text, steerFallbackOpen)
		if start == -1 {
			return "", false
		}
		if nl := strings.Index(text[start:], "\n"); nl != -1 {
			start += nl + 1
		} else {
			start += len(steerFallbackOpen)
		}
	} else {
		start += len(SteerMarkerOpen)
	}
	end := strings.Index(text[start:], SteerMarkerClose)
	if end == -1 {
		end = strings.Index(text[start:], steerFallbackClose)
		if end == -1 {
			return "", false
		}
	}
	inner := pystr.Strip(text[start : start+end])
	return inner, inner != ""
}
