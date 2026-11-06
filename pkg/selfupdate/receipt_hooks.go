package selfupdate

import (
	"context"

	"github.com/nickheyer/icebath/pkg/observability"
	"github.com/nickheyer/icebath/pkg/selfupdate/updaterecord"
)

// Publishes every final update receipt through the shared metrics update recorder
func init() {
	updaterecord.SetUpdateReceiptRecorder(func(ctx context.Context, receipt map[string]any) {
		observability.RecordUpdateReceipt(ctx, receipt, false)
	})
}
