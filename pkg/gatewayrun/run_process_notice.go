package gatewayrun

import (
	"context"
	"fmt"
	"strings"

	"github.com/nickheyer/icebath/pkg/i18n"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
)

// Shell command collapsed onto one line and capped at limit runes for display
func shortenCommandForDisplay(command string, limit int) string {
	oneLine := []rune(strings.Join(pystr.SplitWhitespace(command, -1), " "))
	if len(oneLine) > limit {
		return string(oneLine[:limit-1]) + "…"
	}
	return string(oneLine)
}

// One line completion message of concise mode, a failure appending a short output tail and the rerun hint
func formatConciseProcessNotification(ctx context.Context, sessionID, command string, exitCode *int, output string, durationSeconds *float64) string {
	ok := exitCode == nil || *exitCode == 0
	key := "gateway.background.task_finished"
	if !ok {
		key = "gateway.background.task_failed"
	}
	parts := []string{i18n.T(ctx, key, "", nil)}
	if shortCmd := shortenCommandForDisplay(command, 80); shortCmd != "" {
		parts = append(parts, "— `"+shortCmd+"`")
	}
	var details []string
	if durationSeconds != nil && *durationSeconds >= 0 {
		secs := int(*durationSeconds)
		switch {
		case secs >= 3600:
			details = append(details, fmt.Sprintf("%dh %dm", secs/3600, (secs%3600)/60))
		case secs >= 60:
			details = append(details, fmt.Sprintf("%dm %ds", secs/60, secs%60))
		default:
			details = append(details, fmt.Sprintf("%ds", secs))
		}
	}
	if !ok {
		details = append(details, i18n.T(ctx, "gateway.background.exit_code", "", map[string]any{"code": *exitCode}))
	}
	if len(details) > 0 {
		parts = append(parts, "("+strings.Join(details, ", ")+")")
	}
	text := strings.Join(parts, " ")
	if !ok && output != "" {
		var tailLines []string
		for _, line := range pystr.Splitlines(pystr.Strip(output), false) {
			if pystr.Strip(line) != "" {
				tailLines = append(tailLines, line)
			}
		}
		tailLines = tailLines[max(0, len(tailLines)-5):]
		tail := []rune(strings.Join(tailLines, "\n"))
		if len(tail) > 500 {
			tail = tail[len(tail)-500:]
		}
		if len(tail) > 0 {
			text += i18n.T(ctx, "gateway.background.last_output", "", map[string]any{"tail": string(tail)})
		}
	}
	if !ok {
		text += i18n.T(ctx, "gateway.background.rerun_hint", "", nil)
	}
	return text
}
