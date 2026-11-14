package tool

import (
	"strconv"

	"github.com/nickheyer/icebath/internal/config"
)

const (
	defaultMaxBytes      = 50_000
	defaultMaxLines      = 2000
	defaultMaxLineLength = 2000
	headRatio            = 0.4
)

type OutputLimits struct {
	MaxBytes      int
	MaxLines      int
	MaxLineLength int
}

func Limits(cfg *config.Config) OutputLimits {
	l := OutputLimits{MaxBytes: defaultMaxBytes, MaxLines: defaultMaxLines, MaxLineLength: defaultMaxLineLength}
	if cfg == nil {
		return l
	}
	if v := cfg.ToolOutput.MaxBytes; v >= 1 {
		l.MaxBytes = v
	}
	if v := cfg.ToolOutput.MaxLines; v >= 1 {
		l.MaxLines = v
	}
	if v := cfg.ToolOutput.MaxLineLength; v >= 1 {
		l.MaxLineLength = v
	}
	return l
}

func (l OutputLimits) Resolved() OutputLimits {
	d := Limits(nil)
	if l.MaxBytes < 1 {
		l.MaxBytes = d.MaxBytes
	}
	if l.MaxLines < 1 {
		l.MaxLines = d.MaxLines
	}
	if l.MaxLineLength < 1 {
		l.MaxLineLength = d.MaxLineLength
	}
	return l
}

func TruncationNotice(omitted, total int, label, unit string) string {
	if label == "" {
		label = "OUTPUT"
	}
	if unit == "" {
		unit = "chars"
	}
	return "\n\n... [" + label + " TRUNCATED - " + groupThousands(omitted) + " " + unit + " omitted out of " + groupThousands(total) + " total] ...\n\n"
}

func HeadTailSplit(budget int) (head, tail int) {
	head = int(float64(budget) * headRatio)
	return head, budget - head
}

func TruncateHeadTail(text string, maxChars int, label string) string {
	runes := []rune(text)
	if len(runes) <= maxChars {
		return text
	}
	head, tail := HeadTailSplit(maxChars)
	omitted := len(runes) - head - tail
	return string(runes[:head]) + TruncationNotice(omitted, len(runes), label, "") + string(runes[len(runes)-tail:])
}

func groupThousands(n int) string {
	s := strconv.Itoa(n)
	neg := false
	if n < 0 {
		neg, s = true, s[1:]
	}
	if len(s) <= 3 {
		if neg {
			return "-" + s
		}
		return s
	}
	out := make([]byte, 0, len(s)+len(s)/3)
	lead := len(s) % 3
	if lead > 0 {
		out = append(out, s[:lead]...)
	}
	for i := lead; i < len(s); i += 3 {
		if len(out) > 0 {
			out = append(out, ',')
		}
		out = append(out, s[i:i+3]...)
	}
	if neg {
		return "-" + string(out)
	}
	return string(out)
}
