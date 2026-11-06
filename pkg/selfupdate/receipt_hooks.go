package selfupdate

import (
	"context"

	"github.com/nickheyer/icebath/pkg/observability"
	"github.com/nickheyer/icebath/pkg/pm"
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
	observability.PackageChannelHook = packagedChannelValue
}

// Payload or update mechanism value that makes the install a packaged channel, empty for a source install
func packagedChannelValue(projectRoot string) (string, error) {
	stamp := pm.ReadInstallStamp(projectRoot)
	if !packageChannel(stamp) {
		return "", nil
	}
	if payload, ok := stampGet(stamp, "payload").(string); ok {
		switch payload {
		case "bundled", "light", "runtime":
			return payload, nil
		}
	}
	mechanism, _ := stampGet(stamp, "updateMechanism").(string)
	return mechanism, nil
}
