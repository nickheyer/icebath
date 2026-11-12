package pyint

import (
	"fmt"
	"math"
	"math/big"
	"strings"
	"unicode"

	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
)

// Python exception class and message of a failed int conversion
type Error struct {
	Kind string
	Msg  string
}

func (e *Error) Error() string { return e.Msg }

// Class names of the builtin exception through BaseException as type(exc).__mro__ lists them
func (e *Error) PyExceptionMRO() []string { return pyrepr.ExceptionMROFrom(e.Kind, nil) }

var _ interface{ PyExceptionMRO() []string } = (*Error)(nil)

// Python int of a string in base 10, digits of any script with single underscores between digits
func Parse(text string) (*big.Int, bool) {
	text = pystr.Strip(text)
	sign := ""
	if strings.HasPrefix(text, "+") || strings.HasPrefix(text, "-") {
		sign, text = text[:1], text[1:]
	}
	var digits strings.Builder
	previousUnderscore := true
	for _, r := range text {
		if r == '_' {
			if previousUnderscore {
				return nil, false
			}
			previousUnderscore = true
			continue
		}
		if !unicode.Is(unicode.Nd, r) {
			return nil, false
		}
		start := r
		for unicode.Is(unicode.Nd, start-1) {
			start--
		}
		digits.WriteByte(byte('0' + (r-start)%10))
		previousUnderscore = false
	}
	if digits.Len() == 0 || previousUnderscore {
		return nil, false
	}
	return new(big.Int).SetString(sign+digits.String(), 10)
}

// Python int of a decoded JSON or config value with the TypeError, ValueError or OverflowError int raises
func Of(v any) (*big.Int, error) {
	switch t := v.(type) {
	case bool:
		if t {
			return big.NewInt(1), nil
		}
		return big.NewInt(0), nil
	case string:
		if n, ok := Parse(t); ok {
			return n, nil
		}
		return nil, &Error{"ValueError", "invalid literal for int() with base 10: " + pyrepr.StrRepr(t)}
	case float64:
		if math.IsNaN(t) {
			return nil, &Error{"ValueError", "cannot convert float NaN to integer"}
		}
		if math.IsInf(t, 0) {
			return nil, &Error{"OverflowError", "cannot convert float infinity to integer"}
		}
		n, _ := big.NewFloat(math.Trunc(t)).Int(nil)
		return n, nil
	case float32:
		return Of(float64(t))
	}
	if n, ok := pyrepr.Int(v); ok {
		return n, nil
	}
	return nil, &Error{"TypeError", fmt.Sprintf("int() argument must be a string, a bytes-like object or a real number, not '%s'", pyrepr.TypeName(v))}
}

// Smallest Go value holding n, int64 when it fits
func Value(n *big.Int) any {
	if n.IsInt64() {
		return n.Int64()
	}
	return n
}

// Go int holding n when it fits, otherwise n itself
func Native(n *big.Int) any {
	if n.IsInt64() && n.Int64() >= math.MinInt && n.Int64() <= math.MaxInt {
		return int(n.Int64())
	}
	return n
}

// Integer value of v when isinstance(v, int) holds in Python, bools counting as 0 and 1
func Instance(v any) (*big.Int, bool) {
	if b, ok := v.(bool); ok {
		if b {
			return big.NewInt(1), true
		}
		return big.NewInt(0), true
	}
	return pyrepr.Int(v)
}
