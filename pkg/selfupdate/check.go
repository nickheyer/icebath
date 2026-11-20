//go:build !icebath_core && !icebath_noextras

package selfupdate

import (
	"context"

	"github.com/nickheyer/icebath/pkg/selfupdate/updaterecord"
)

// Copies a channel record so later config writes cannot alias the retirement baseline
func deepCopyRecord(record map[string]any) map[string]any {
	out := make(map[string]any, len(record))
	for key, value := range record {
		if nested, ok := value.(map[string]any); ok {
			value = deepCopyRecord(nested)
		}
		out[key] = value
	}
	return out
}

// Names the exit about to stop this update as one closed token on the open receipt
func recordStop(ctx context.Context, reason string) {
	updaterecord.RecordStopReason(ctx, reason)
}
