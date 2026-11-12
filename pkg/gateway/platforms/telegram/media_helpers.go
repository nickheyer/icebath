package telegram

import (
	"fmt"
	"log/slog"

	"github.com/nickheyer/icebath/pkg/internal/pyint"
)

// Note telling the agent a Telegram file was skipped for exceeding the download cap
func telegramMediaTooLargeNote(label string, fileSize any, maxBytes int) string {
	limitMB := max(1, maxBytes/(1024*1024))
	sizeText := "unknown size"
	if size, ok := intOrZero(fileSize); ok {
		sizeText = fmt.Sprintf("%.1f MB", float64(size)/(1024*1024))
	}
	return fmt.Sprintf("[Telegram %s skipped: file size %s exceeds the %d MB limit. Ask the user to send a smaller file.]", label, sizeText, limitMB)
}

// Integer of a value with falsy reading as zero, false when the value is not integral
func intOrZero(value any) (int64, bool) {
	if !pyTruthy(value) {
		return 0, true
	}
	n, err := pyint.Of(value)
	if err != nil {
		return 0, false
	}
	return n.Int64(), true
}

// Whether a media file fits the getFile cap before downloading, with the skip note when it does not
func telegramMediaSizeAllowed(fileSize any, maxDocBytes int, label string) (bool, *string) {
	if maxDocBytes == 0 {
		maxDocBytes = 20 * 1024 * 1024
	}
	size, _ := intOrZero(fileSize)
	if size <= 0 || size <= int64(maxDocBytes) {
		return true, nil
	}
	note := telegramMediaTooLargeNote(label, size, maxDocBytes)
	return false, &note
}

// Caption cut at the Telegram caption limit, nil for an empty caption
func caption1024(caption string) *string {
	if caption == "" {
		return nil
	}
	clipped := clipRunes(caption, 1024)
	return &clipped
}

// Whether this connection drops updates queued while offline, logged on every cold boot
func coldBootDropPending(name string, dropPendingOnColdBoot, isReconnect bool) bool {
	dropPending := dropPendingOnColdBoot && !isReconnect
	if !isReconnect {
		action, setting := "preserving", "false"
		if dropPending {
			action = "dropping"
		}
		if dropPendingOnColdBoot {
			setting = "true"
		}
		slog.Info(fmt.Sprintf("[%s] Cold boot: %s Telegram updates queued while offline (platforms.telegram.extra.drop_pending_on_cold_boot: %s)",
			name, action, setting))
	}
	return dropPending
}
