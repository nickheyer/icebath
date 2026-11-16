package app

import (
	"context"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/provider"
)

// Pins the display context resolver to one answer for the case
func pinDisplayContextLength(t *testing.T, n int) {
	t.Helper()
	prev := provider.GetModelContextLength
	provider.GetModelContextLength = func(context.Context, provider.ContextLengthRequest) int { return n }
	t.Cleanup(func() { provider.GetModelContextLength = prev })
}

// First printed line naming the context window
func contextLine(lines []string) string {
	for _, l := range lines {
		if strings.Contains(l, "Context:") {
			return l
		}
	}
	return ""
}
