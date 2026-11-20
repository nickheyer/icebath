package tui

import (
	"fmt"
	"math"
	"strings"
	"time"
)

// Local hour and minute stamp of a message, empty when its time is missing or invalid
func fmtMsgTimestamp(createdAt *float64) (string, bool) {
	if createdAt == nil || math.IsNaN(*createdAt) || math.IsInf(*createdAt, 0) || *createdAt <= 0 {
		return "", false
	}
	ms := *createdAt * 1000
	if math.Abs(ms) > 8.64e15 {
		return "", false
	}
	t := time.UnixMilli(int64(math.Trunc(ms))).In(clockLocation())
	return fmt.Sprintf("[%02d:%02d]", t.Hour(), t.Minute()), true
}

// Theme tone of a filled gauge run
func gaugeThemeColor(t theme, style string) string {
	switch style {
	case "red":
		return t.Color.Error
	case "yellow":
		return t.Color.Warn
	}
	return t.Color.Ok
}

// Whether a plain one line system message is a usage row with a gauge bar
func isUsageGaugeRow(m msg) bool {
	return m.Role == "system" && m.Kind == "" && !strings.Contains(m.Text, "\n") && hasUsageGauge(m.Text)
}

// Usage row with its gauge runs colored and the rest muted
func usageGaugeRowView(t theme, text string) string {
	var b strings.Builder
	for _, seg := range splitUsageGaugeLine(stripAnsi(text)) {
		tone := t.Color.Muted
		if seg.Style != "" {
			tone = gaugeThemeColor(t, seg.Style)
		}
		b.WriteString(paint(seg.Text, tone, true))
	}
	return b.String()
}
