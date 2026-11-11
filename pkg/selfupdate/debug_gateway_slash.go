package selfupdate

import (
	"context"
	"fmt"
	"strings"

	"github.com/nickheyer/icebath/pkg/i18n"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
)

// Redacted share bundle report for a local session's debug local, collected with no network call
func GatewayDebugLocalReport(ctx context.Context) (string, error) {
	bundle, err := debugCollectShareBundle(ctx, 200, true)
	if err != nil {
		return "", err
	}
	return bundle.vals["report"], nil
}

// Uploads only the summary report and returns the chat reply with its link, retention and hints
func GatewayDebugUploadReply(ctx context.Context) (string, error) {
	debugBestEffortSweep(ctx)
	dumpText, err := debugCaptureDump(ctx, true)
	if err != nil {
		return "", err
	}
	report, err := debugCollectDebugReport(ctx, 200, dumpText, nil)
	if err != nil {
		return "", err
	}
	label := i18n.T(ctx, "gateway.debug.report_label", "", nil)
	url, err := debugUploadToPastebin(ctx, report, 1)
	if err != nil {
		return i18n.T(ctx, "gateway.debug.upload_failed", "", map[string]any{"error": RedactDebugSupportText(err)}), nil
	}
	if err := debugScheduleAutoDelete(ctx, []string{url}, debugAutoDeleteSeconds); err != nil {
		return "", err
	}
	labelWidth := pystr.Len(label)
	lines := []string{DebugGatewayPrivacyNotice, "", i18n.T(ctx, "gateway.debug.header", "", nil), "",
		fmt.Sprintf("`%s`  %s", pystr.Ljust(label, labelWidth), url), ""}
	if !debugIsDpasteURL(url) {
		lines = append(lines, i18n.T(ctx, "gateway.debug.auto_delete", "", nil))
	}
	lines = append(lines, i18n.T(ctx, "gateway.debug.full_logs_hint", "", nil), i18n.T(ctx, "gateway.debug.share_hint", "", nil))
	return strings.Join(lines, "\n"), nil
}
