package tools

import (
	"fmt"
	"math"
	"math/big"
	"strconv"
	"strings"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
)

// Python repr of a float with the short repr exponent thresholds
func pyFloatRepr(f float64) string {
	switch {
	case math.IsNaN(f):
		return "nan"
	case math.IsInf(f, 1):
		return "inf"
	case math.IsInf(f, -1):
		return "-inf"
	}
	sci := strconv.FormatFloat(f, 'e', -1, 64)
	sign := ""
	if strings.HasPrefix(sci, "-") {
		sign, sci = "-", sci[1:]
	}
	mant, expText, _ := strings.Cut(sci, "e")
	exp, _ := strconv.Atoi(expText)
	digits := strings.Replace(mant, ".", "", 1)
	if exp < -4 || exp >= 16 {
		m := digits[:1]
		if len(digits) > 1 {
			m += "." + digits[1:]
		}
		es := "+"
		if exp < 0 {
			es, exp = "-", -exp
		}
		return fmt.Sprintf("%s%se%s%02d", sign, m, es, exp)
	}
	if exp < 0 {
		return sign + "0." + strings.Repeat("0", -exp-1) + digits
	}
	if len(digits) <= exp+1 {
		return sign + digits + strings.Repeat("0", exp+1-len(digits)) + ".0"
	}
	return sign + digits[:exp+1] + "." + digits[exp+1:]
}

// Python repr of a decoded JSON value
func pyJSONRepr(v any) string {
	switch x := v.(type) {
	case nil:
		return "None"
	case bool:
		if x {
			return "True"
		}
		return "False"
	case int64:
		return strconv.FormatInt(x, 10)
	case *big.Int:
		return x.String()
	case float64:
		return pyFloatRepr(x)
	case string:
		return pyReprStr(x)
	case []any:
		parts := make([]string, len(x))
		for i, e := range x {
			parts[i] = pyJSONRepr(e)
		}
		return "[" + strings.Join(parts, ", ") + "]"
	case *pyjson.Object:
		parts := make([]string, len(x.Keys))
		for i, k := range x.Keys {
			parts[i] = pyReprStr(k) + ": " + pyJSONRepr(x.Vals[k])
		}
		return "{" + strings.Join(parts, ", ") + "}"
	}
	return fmt.Sprint(v)
}

// Python str of a decoded JSON value
func pyJSONStr(v any) string {
	if s, ok := v.(string); ok {
		return s
	}
	return pyJSONRepr(v)
}

// Python type name of a decoded JSON value for TypeError text
func pyJSONTypeName(v any) string {
	switch v.(type) {
	case nil:
		return "NoneType"
	case bool:
		return "bool"
	case int64, *big.Int:
		return "int"
	case float64:
		return "float"
	case string:
		return "str"
	case []any:
		return "list"
	case *pyjson.Object:
		return "dict"
	}
	return fmt.Sprintf("%T", v)
}

// Items enumerate yields for a decoded JSON value, or the TypeError for a non iterable
func pyJSONIter(v any) ([]any, error) {
	switch x := v.(type) {
	case []any:
		return x, nil
	case string:
		out := []any{}
		for i := 0; i < len(x); {
			_, size := pystr.DecodeRune(x[i:])
			out = append(out, x[i:i+size])
			i += size
		}
		return out, nil
	case *pyjson.Object:
		out := make([]any, len(x.Keys))
		for i, k := range x.Keys {
			out[i] = k
		}
		return out, nil
	}
	return nil, fmt.Errorf("'%s' object is not iterable", pyJSONTypeName(v))
}

// TypeError for an unhashable JSON value used as a set or dict key
func pyJSONHashable(v any) error {
	switch v.(type) {
	case []any, *pyjson.Object:
		return fmt.Errorf("unhashable type: '%s'", pyJSONTypeName(v))
	}
	return nil
}
