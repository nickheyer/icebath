package tuiserver

import (
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"

	"github.com/nickheyer/icebath/pkg/errchain"
	anyutil "github.com/nickheyer/icebath/pkg/utils/any"
)

// Python float of a number, bool or numeric string, a ValueError or TypeError otherwise
func pyFloat(v any) (float64, error) {
	switch n := v.(type) {
	case float64:
		return n, nil
	case float32:
		return float64(n), nil
	case bool:
		if n {
			return 1, nil
		}
		return 0, nil
	case int:
		return float64(n), nil
	case int64:
		return float64(n), nil
	case uint64:
		return float64(n), nil
	case string:
		text := strings.TrimSpace(n)
		switch strings.ToLower(strings.TrimLeft(text, "+-")) {
		case "inf", "infinity":
			if strings.HasPrefix(text, "-") {
				return math.Inf(-1), nil
			}
			return math.Inf(1), nil
		case "nan":
			return math.NaN(), nil
		}
		valid := text != "" && !strings.ContainsAny(strings.ToLower(text), "xp")
		for i := 0; valid && i < len(text); i++ {
			if text[i] == '_' {
				valid = i > 0 && i+1 < len(text) && isASCIIDigit(text[i-1]) && isASCIIDigit(text[i+1])
			}
		}
		text = strings.ReplaceAll(text, "_", "")
		if valid {
			if f, err := strconv.ParseFloat(text, 64); err == nil || errors.Is(err, strconv.ErrRange) {
				return f, nil
			}
		}
		return 0, fmt.Errorf("%s is not a number", n)
	}
	if whole, ok := anyutil.Int(v); ok {
		return float64(whole), nil
	}
	return 0, fmt.Errorf("expected a number, got %s", errchain.Kind(v))
}

// Whether a byte is an ASCII decimal digit
func isASCIIDigit(b byte) bool { return b >= '0' && b <= '9' }
