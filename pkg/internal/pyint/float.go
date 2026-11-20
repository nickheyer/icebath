package pyint

import (
	"math"
	"math/big"
	"regexp"
	"strconv"
	"strings"
	"unicode"

	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
)

var (
	floatNumberRE  = regexp.MustCompile(`^[+-]?(?:[0-9](?:_?[0-9])*(?:\.(?:[0-9](?:_?[0-9])*)?)?|\.[0-9](?:_?[0-9])*)(?:[eE][+-]?[0-9](?:_?[0-9])*)?$`)
	floatSpecialRE = regexp.MustCompile(`(?i)^([+-]?)(inf|infinity|nan)$`)
)

// Python float of a string, any script's decimal digits, underscores between digits, inf and nan spellings
func ParseFloat(text string) (float64, error) {
	stripped := pystr.Strip(text)
	var ascii strings.Builder
	for _, r := range stripped {
		if r > unicode.MaxASCII && unicode.Is(unicode.Nd, r) {
			start := r
			for unicode.Is(unicode.Nd, start-1) {
				start--
			}
			ascii.WriteByte(byte('0' + (r-start)%10))
			continue
		}
		ascii.WriteRune(r)
	}
	s := ascii.String()
	if m := floatSpecialRE.FindStringSubmatch(s); m != nil {
		if strings.EqualFold(m[2], "nan") {
			return math.NaN(), nil
		}
		if m[1] == "-" {
			return math.Inf(-1), nil
		}
		return math.Inf(1), nil
	}
	if !floatNumberRE.MatchString(s) {
		return 0, &Error{"ValueError", "could not convert string to float: " + pyrepr.StrRepr(text)}
	}
	f, err := strconv.ParseFloat(strings.ReplaceAll(s, "_", ""), 64)
	if err != nil && !math.IsInf(f, 0) && f != 0 {
		return 0, &Error{"ValueError", "could not convert string to float: " + pyrepr.StrRepr(text)}
	}
	return f, nil
}

// Python float of a decoded JSON or config value with the TypeError, ValueError or OverflowError float raises
func Float(v any) (float64, error) {
	switch t := v.(type) {
	case bool:
		if t {
			return 1, nil
		}
		return 0, nil
	case float64:
		return t, nil
	case float32:
		return float64(t), nil
	case string:
		return ParseFloat(t)
	}
	if n, ok := pyrepr.Int(v); ok {
		f, _ := new(big.Float).SetInt(n).Float64()
		if math.IsInf(f, 0) {
			return 0, &Error{"OverflowError", "int too large to convert to float"}
		}
		return f, nil
	}
	return 0, &Error{"TypeError", "float() argument must be a string or a real number, not '" + pyrepr.TypeName(v) + "'"}
}
