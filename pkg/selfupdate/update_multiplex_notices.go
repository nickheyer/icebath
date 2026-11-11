package selfupdate

import (
	"context"

	"github.com/nickheyer/icebath/pkg/gateway"
)

// The multiplex opt out rewrite notice once more before its marker is cleared
func consumeRewrittenNotice() []string { return gateway.ConsumeRewrittenNotice("") }

// Standalone gateway warning box rebuilt from the live gateway runtime status
func recordedStandaloneWarningLines(ctx context.Context) []string {
	return gateway.RecordedStandaloneWarningLines(ctx)
}
