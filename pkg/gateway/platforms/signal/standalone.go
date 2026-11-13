package signal

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"time"

	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/i18n"
)

// Batch numbers joined for a warning, each with a leading hash
func batchList(nums []int) string {
	parts := make([]string, len(nums))
	for i, n := range nums {
		parts[i] = fmt.Sprintf("#%d", n)
	}
	return strings.Join(parts, ", ")
}

// Out of process send for cron and icebath send over raw JSON-RPC, attachments batched under the shared scheduler
func standaloneSend(ctx context.Context, kwargs map[string]any) (map[string]any, error) {
	pconfig, _ := kwargs["pconfig"].(*gateway.PlatformConfig)
	chatID, _ := kwargs["chat_id"].(string)
	message, _ := kwargs["message"].(string)
	media, _ := kwargs["media_files"].([]gateway.MediaRef)
	var extra map[string]any
	if pconfig != nil {
		extra = pconfig.Extra
	}
	account := strings.TrimSpace(extraString(extra, "account", ""))
	if account == "" {
		return gateway.SendError("Signal account not configured"), nil
	}
	c := &rpcClient{baseURL: strings.TrimRight(extraString(extra, "http_url", defaultHTTPURL), "/"), account: account,
		http: gateway.LimitedHTTPClient(gateway.DefaultHTTPXLimits(), false)}
	defer c.http.CloseIdleConnections()
	var paths []string
	for _, m := range media {
		if _, err := os.Stat(m.Path); err != nil {
			slog.Warn("Signal media file not found, skipping: " + m.Path)
			continue
		}
		paths = append(paths, m.Path)
	}
	batches := attachmentBatches(paths)
	if len(batches) == 0 {
		batches = [][]string{nil}
	}
	text, styles := markdownToSignal(message)
	base := targetParams(map[string]any{"account": account}, chatID, chatID)
	sched := sharedScheduler()
	slog.Info(fmt.Sprintf("send_message Signal: scheduler state=%s, %d attachment(s) in %d batch(es)", sched, len(paths), len(batches)))
	var rateLimited, failed []int
	for i, batch := range batches {
		n := len(batch)
		if wait := sched.estimateWait(n); n > 0 && wait >= batchPacingNoticeThreshold {
			notice := i18n.T(ctx, "platform.signal.batch_pacing_notice", "", map[string]any{"wait": formatWait(ctx, wait), "batch": i + 1,
				"total": len(batches)})
			if _, err := c.call(ctx, "send", withParams(base, "message", notice), fmt.Sprintf("notice_%d", time.Now().UnixMilli()), 0); err != nil {
				slog.Warn(fmt.Sprintf("Signal: inline notice failed: %v", err))
			}
		}
		params := withParams(base, "message", "")
		if i == 0 {
			params["message"] = text
			if text != "" {
				setStyles(params, styles)
			}
		}
		if n > 0 {
			params["attachments"] = batch
		}
		res := sendBatch(ctx, sched, c, params, n, fmt.Sprintf("%d/%d", i+1, len(batches)))
		switch {
		case ctx.Err() != nil:
			return nil, ctx.Err()
		case res.outcome != nil:
		case res.rateLimited:
			rateLimited = append(rateLimited, i+1)
		case res.permanent:
			return gateway.SendError(fmt.Sprintf("Signal RPC error on batch %d/%d: %v", i+1, len(batches), res.err)), nil
		default:
			failed = append(failed, i+1)
		}
	}
	var warnings []string
	if len(paths) < len(media) {
		warnings = append(warnings, "Some media files were skipped (not found on disk)")
	}
	if len(rateLimited) > 0 {
		warnings = append(warnings, fmt.Sprintf("Signal rate-limited %d batch(es) (%s)", len(rateLimited), batchList(rateLimited)))
	}
	if len(failed) > 0 {
		warnings = append(warnings, fmt.Sprintf("Signal failed to deliver %d batch(es) after retries (%s)", len(failed), batchList(failed)))
	}
	switch {
	case len(rateLimited) == len(batches):
		return gateway.SendError(fmt.Sprintf("Signal: every batch (%d) hit rate limit; nothing delivered", len(batches))), nil
	case len(rateLimited)+len(failed) == len(batches):
		return gateway.SendError(fmt.Sprintf("Signal: every batch (%d) failed; nothing delivered", len(batches))), nil
	}
	shownChat := chatID
	if strings.HasPrefix(chatID, "group:") {
		shownChat = "group:***"
	}
	result := map[string]any{"success": true, "platform": "signal", "chat_id": shownChat}
	if len(warnings) > 0 {
		result["warnings"] = warnings
	}
	return result, nil
}
