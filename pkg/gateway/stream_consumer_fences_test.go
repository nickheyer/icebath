package gateway

import (
	"strings"
	"testing"
)

func TestEscapeCodeFencesForDisplaySingleFenceEscaped(t *testing.T) {
	result := EscapeCodeFencesForDisplay("model used ```python\nx = 1\n``` in its thinking")
	if strings.Contains(result, "```") || !strings.Contains(result, "\\`\\`\\`") {
		t.Fatalf("got %q", result)
	}
}

func TestEscapeCodeFencesForDisplayMultipleFencesAllEscaped(t *testing.T) {
	result := EscapeCodeFencesForDisplay("```\nblock1\n``` and ```python\nblock2\n```")
	if strings.Count(result, "```") != 0 || strings.Count(result, "\\`\\`\\`") != 4 {
		t.Fatalf("got %q", result)
	}
}
