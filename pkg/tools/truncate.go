package tools

import (
	"context"
	"encoding/json"
	"strconv"
	"strings"
	"sync"

	"github.com/nickheyer/icebath/pkg/home"
)

const (
	DefaultMaxBytes      = 50000
	DefaultMaxLines      = 2000
	DefaultMaxLineLength = 2000
	headRatio            = 0.4
)

// OutputLimits holds the resolved tool_output caps from config.yaml
type OutputLimits struct {
	MaxBytes      int
	MaxLines      int
	MaxLineLength int
}

var (
	outputLimitsMu    sync.Mutex
	outputLimitsCache = map[string]OutputLimits{}
)

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

// Resolved output caps cached per profile home so every profile keeps its own limits
func ToolOutputLimits(env *Env) OutputLimits {
	key := home.Key(envHome(env))
	outputLimitsMu.Lock()
	defer outputLimitsMu.Unlock()
	if cached, ok := outputLimitsCache[key]; ok {
		return cached
	}
	section := cfgTree(env).Map("tool_output")
	limits := OutputLimits{
		MaxBytes:      coerceInt(section["max_bytes"], DefaultMaxBytes, 1),
		MaxLines:      coerceInt(section["max_lines"], DefaultMaxLines, 1),
		MaxLineLength: coerceInt(section["max_line_length"], DefaultMaxLineLength, 1),
	}
	outputLimitsCache[key] = limits
	return limits
}

// Drops the cached limits for tests or after a config hot reload
func ResetToolOutputLimitsCache() {
	outputLimitsMu.Lock()
	defer outputLimitsMu.Unlock()
	outputLimitsCache = map[string]OutputLimits{}
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

// Truncates a tool result to the tool's own result cap or the configured output byte cap
func TruncateResult(env *Env, name, result string) string {
	limit := Default.MaxResultSize(context.Background(), ScopeKey(envHome(env)), name, 0)
	if limit < 0 {
		return result
	}
	if limit == 0 {
		limit = ToolOutputLimits(env).MaxBytes
	}
	return TruncateHeadTail(result, limit, "OUTPUT")
}
