package selfupdate

import (
	"context"

	"github.com/nickheyer/icebath/pkg/observability"
	"github.com/nickheyer/icebath/pkg/selfupdate/updaterecord"
)

// Publishes final update receipts and the install channel answers through the shared metrics hooks
func init() {
	updaterecord.SetUpdateReceiptRecorder(func(ctx context.Context, receipt map[string]any) {
		observability.RecordUpdateReceipt(ctx, receipt, false)
	})
	observability.ResolveUpdateChannelHook = func(cfg map[string]any, projectRoot string) (string, error) {
		return ResolveUpdateChannel(cfg, projectRoot)
	}
}
