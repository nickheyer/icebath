package tools

import (
	"context"
	"encoding/json"
	"strconv"
	"strings"

	"github.com/nickheyer/icebath/pkg/settings"
)

const headRatio = 0.4

// OutputLimits holds the resolved tool_output caps from config.yaml
type OutputLimits struct {
	MaxBytes      int
	MaxLines      int
	MaxLineLength int
}

// Integer at least minimum, else the default on any issue
func coerceInt(value any, def, minimum int) int {
	iv, ok := asIntValue(value)
	if !ok || iv < minimum {
		return def
	}
	return iv
}

// Integer from an int, float, or digit string the way int(value) reads it
func asIntValue(value any) (int, bool) {
	switch v := value.(type) {
	case int:
		return v, true
	case int64:
		return int(v), true
	case float64:
		return int(v), true
	case json.Number:
		if n, err := v.Int64(); err == nil {
			return int(n), true
		}
		if f, err := v.Float64(); err == nil {
			return int(f), true
		}
		return 0, false
	case string:
		n, err := strconv.Atoi(strings.TrimSpace(v))
		if err != nil {
			return 0, false
		}
		return n, true
	case bool:
		if v {
			return 1, true
		}
		return 0, true
	}
	return 0, false
}

// Output caps of the env's profile
func ToolOutputLimits(env *Env) OutputLimits {
	ctx := envHomeCtx(context.Background(), env)
	return OutputLimits{
		MaxBytes:      settings.ToolOutputMaxBytes.Get(ctx),
		MaxLines:      settings.ToolOutputMaxLines.Get(ctx),
		MaxLineLength: settings.ToolOutputMaxLineLength.Get(ctx),
	}
}

// Formats an integer with thousands separators the way Python {n:,} does
func commaInt(n int) string {
	s := strconv.Itoa(n)
	neg := strings.HasPrefix(s, "-")
	if neg {
		s = s[1:]
	}
	var b strings.Builder
	for i, r := range s {
		if i > 0 && (len(s)-i)%3 == 0 {
			b.WriteByte(',')
		}
		b.WriteRune(r)
	}
	if neg {
		return "-" + b.String()
	}
	return b.String()
}

// Marker inserted between the kept head and tail of a truncated output
func TruncationNotice(omitted, total int, label, unit string) string {
	if label == "" {
		label = "OUTPUT"
	}
	if unit == "" {
		unit = "chars"
	}
	return "\n\n... [" + label + " TRUNCATED - " + commaInt(omitted) + " " + unit + " omitted out of " + commaInt(total) + " total] ...\n\n"
}

// Head and tail character budgets for a total budget, 40 percent head
func HeadTailSplit(budget int) (int, int) {
	head := int(float64(budget) * headRatio)
	return head, budget - head
}

// Keeps the head and tail of text around a truncation notice when it exceeds maxChars
func TruncateHeadTail(text string, maxChars int, label string) string {
	runes := []rune(text)
	if len(runes) <= maxChars {
		return text
	}
	head, tail := HeadTailSplit(maxChars)
	omitted := len(runes) - head - tail
	return string(runes[:head]) + TruncationNotice(omitted, len(runes), label, "chars") + string(runes[len(runes)-tail:])
}
