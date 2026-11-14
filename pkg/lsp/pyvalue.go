package lsp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"math"
	"math/big"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"unicode/utf8"

	"github.com/nickheyer/icebath/pkg/internal/pyint"
	"github.com/nickheyer/icebath/pkg/internal/pypath"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
)

// Length of the UTF-8 sequence at i, or of its maximal invalid subpart with the CPython reason
func utf8Step(p []byte, i int) (n int, reason string, valid bool) {
	c := p[i]
	if c < 0x80 {
		return 1, "", true
	}
	need, lo, hi := 0, byte(0x80), byte(0xBF)
	switch {
	case c >= 0xC2 && c <= 0xDF:
		need = 1
	case c == 0xE0:
		need, lo = 2, 0xA0
	case c >= 0xE1 && c <= 0xEC, c == 0xEE, c == 0xEF:
		need = 2
	case c == 0xED:
		need, hi = 2, 0x9F
	case c == 0xF0:
		need, lo = 3, 0x90
	case c >= 0xF1 && c <= 0xF3:
		need = 3
	case c == 0xF4:
		need, hi = 3, 0x8F
	default:
		return 1, "invalid start byte", false
	}
	j := i + 1
	for k := 0; k < need; k++ {
		if j >= len(p) {
			return j - i, "unexpected end of data", false
		}
		b := p[j]
		if (k == 0 && (b < lo || b > hi)) || (k > 0 && (b < 0x80 || b > 0xBF)) {
			return j - i, "invalid continuation byte", false
		}
		j++
	}
	return j - i, "", true
}

// CPython utf-8 codec error text for the first undecodable sequence
func utf8DecodeErrorText(p []byte) string {
	for i := 0; i < len(p); {
		n, reason, valid := utf8Step(p, i)
		if !valid {
			return codecRangeError(p, i, i+n, reason)
		}
		i += n
	}
	return ""
}

// Decodes bytes as UTF-8 with one replacement character per maximal invalid subpart, as errors replace does
func decodeUTF8Replace(p []byte) string {
	if utf8.Valid(p) {
		return string(p)
	}
	var b strings.Builder
	for i := 0; i < len(p); {
		n, _, valid := utf8Step(p, i)
		if valid {
			b.Write(p[i : i+n])
		} else {
			b.WriteRune(utf8.RuneError)
		}
		i += n
	}
	return b.String()
}

func codecRangeError(p []byte, start, end int, reason string) string {
	if end-start == 1 {
		return fmt.Sprintf("'utf-8' codec can't decode byte 0x%02x in position %d: %s", p[start], start, reason)
	}
	return fmt.Sprintf("'utf-8' codec can't decode bytes in position %d-%d: %s", start, end-1, reason)
}

// Python value of x or fallback
func pyOr(v, fallback any) any {
	if pyrepr.Truthy(v) {
		return v
	}
	return fallback
}

// Python int() of a decoded JSON value, the raised exception text as the error
func pyInt(v any) (int64, error) {
	switch t := v.(type) {
	case bool:
		if t {
			return 1, nil
		}
		return 0, nil
	case int:
		return int64(t), nil
	case int64:
		return t, nil
	case json.Number:
		n, ok := new(big.Int).SetString(string(t), 10)
		if ok && n.IsInt64() {
			return n.Int64(), nil
		}
		return 0, errors.New("OverflowError: Python int too large to convert to C int")
	case float64:
		if math.IsInf(t, 0) {
			return 0, errors.New("OverflowError: cannot convert float infinity to integer")
		}
		if math.IsNaN(t) {
			return 0, errors.New("ValueError: cannot convert float NaN to integer")
		}
		return int64(t), nil
	case string:
		n, ok := pyint.Parse(t)
		if !ok || !n.IsInt64() {
			return 0, errors.New("ValueError: invalid literal for int() with base 10: " + pyrepr.StrRepr(t))
		}
		return n.Int64(), nil
	}
	return 0, errors.New("TypeError: int() argument must be a string, a bytes-like object or a real number, not " +
		pyrepr.StrRepr(pyrepr.TypeName(v)))
}

// Python int of a value at any magnitude, floats truncated toward zero
func pyBigInt(v any) (*big.Int, error) {
	switch t := v.(type) {
	case bool:
		if t {
			return big.NewInt(1), nil
		}
		return big.NewInt(0), nil
	case int:
		return big.NewInt(int64(t)), nil
	case int64:
		return big.NewInt(t), nil
	case *big.Int:
		return new(big.Int).Set(t), nil
	case json.Number:
		if n, ok := new(big.Int).SetString(string(t), 10); ok {
			return n, nil
		}
		f, err := t.Float64()
		if err != nil {
			return nil, errors.New("ValueError: invalid literal for int() with base 10: " + pyrepr.StrRepr(string(t)))
		}
		return pyBigInt(f)
	case float64:
		if math.IsInf(t, 0) {
			return nil, errors.New("OverflowError: cannot convert float infinity to integer")
		}
		if math.IsNaN(t) {
			return nil, errors.New("ValueError: cannot convert float NaN to integer")
		}
		n, _ := new(big.Float).SetFloat64(t).Int(nil)
		return n, nil
	case string:
		n, ok := pyint.Parse(t)
		if !ok {
			return nil, errors.New("ValueError: invalid literal for int() with base 10: " + pyrepr.StrRepr(t))
		}
		return n, nil
	}
	return nil, errors.New("TypeError: int() argument must be a string, a bytes-like object or a real number, not " +
		pyrepr.StrRepr(pyrepr.TypeName(v)))
}

// Numeric value for Python equality and hashing, false for non numbers
func pyNumber(v any) (float64, bool) {
	switch t := v.(type) {
	case bool:
		if t {
			return 1, true
		}
		return 0, true
	case int:
		return float64(t), true
	case int64:
		return float64(t), true
	case float64:
		return t, true
	case json.Number:
		f, err := strconv.ParseFloat(string(t), 64)
		return f, err == nil
	}
	return 0, false
}

// Python int of a decoded JSON number that is an exact integer, as isinstance(x, int) sees it
func pyExactInt(v any) (int64, bool) {
	switch t := v.(type) {
	case int:
		return int64(t), true
	case int64:
		return t, true
	case bool:
		if t {
			return 1, true
		}
		return 0, true
	}
	return 0, false
}

// Value at key of a dict, nil when the value is not a dict or lacks it
func dget(v any, key string) any {
	if m, ok := v.(map[string]any); ok {
		return m[key]
	}
	return nil
}

// Value at key of a dict with a default when absent
func dgetOr(v any, key string, fallback any) any {
	if m, ok := v.(map[string]any); ok {
		if e, ok := m[key]; ok {
			return e
		}
	}
	return fallback
}

// pyExc is a Python exception with its class name and text
type pyExc struct {
	typ  string
	text string
	err  error
}

func (e *pyExc) Error() string { return e.text }
func (e *pyExc) Unwrap() error { return e.err }

// Python OSError subclass and text for a spawn or filesystem error, a missing executable as ENOENT
func pyOSError(err error, filename string) *pyExc {
	cause := err
	var errno syscall.Errno
	if !errors.As(err, &errno) {
		if !errors.Is(err, exec.ErrNotFound) {
			return &pyExc{typ: "OSError", text: err.Error(), err: err}
		}
		cause = syscall.ENOENT
	}
	text, _ := pypath.ErrnoText(cause)
	if filename != "" {
		text += ": " + pyrepr.StrRepr(filename)
	}
	return &pyExc{typ: pypath.OSErrorClass(cause), text: text, err: err}
}

// Python exception class name for an error the LSP layer logs
func errTypeName(err error) string {
	var pe *ProtocolError
	var re *RequestError
	var te *TimeoutError
	var oe *pyExc
	var pathErr *fs.PathError
	switch {
	case errors.As(err, &re):
		return "LSPRequestError"
	case errors.As(err, &pe):
		return "LSPProtocolError"
	case errors.As(err, &te):
		return "TimeoutError"
	case errors.As(err, &oe):
		return oe.typ
	case errors.Is(err, context.Canceled):
		return "CancelledError"
	case errors.Is(err, context.DeadlineExceeded):
		return "TimeoutError"
	case errors.As(err, &pathErr):
		return pyOSError(pathErr.Err, pathErr.Path).typ
	case errors.As(err, new(syscall.Errno)):
		return pypath.OSErrorClass(err)
	}
	return "RuntimeError"
}

// Python str() of a logged exception
func errText(err error) string {
	var pathErr *fs.PathError
	if errors.As(err, &pathErr) {
		var oe *pyExc
		if !errors.As(err, &oe) {
			return pyOSError(pathErr.Err, pathErr.Path).text
		}
	}
	if errno, ok := err.(syscall.Errno); ok {
		return pypath.OSErrorText(errno)
	}
	if errors.Is(err, context.DeadlineExceeded) {
		var te *TimeoutError
		if !errors.As(err, &te) {
			return ""
		}
	}
	return err.Error()
}

// Python urllib.parse.quote of a str with extra safe characters
func pyQuote(s, safe string) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		if (c >= 'A' && c <= 'Z') || (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') || strings.IndexByte("_.-~", c) >= 0 ||
			(c < 0x80 && strings.IndexByte(safe, c) >= 0) {
			b.WriteByte(c)
			continue
		}
		fmt.Fprintf(&b, "%%%02X", c)
	}
	return b.String()
}

// Value of one hex digit, false for any other byte
func hexVal(c byte) (byte, bool) {
	switch {
	case c >= '0' && c <= '9':
		return c - '0', true
	case c >= 'a' && c <= 'f':
		return c - 'a' + 10, true
	case c >= 'A' && c <= 'F':
		return c - 'A' + 10, true
	}
	return 0, false
}

// Python bytes.fromhex of at most two characters
func fromHex2(s string) ([]byte, bool) {
	var digits []byte
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c == ' ' || c == '\t' || c == '\n' || c == '\r' || c == '\v' || c == '\f' {
			continue
		}
		digits = append(digits, c)
	}
	switch len(digits) {
	case 0:
		return nil, true
	case 2:
		hi, ok1 := hexVal(digits[0])
		lo, ok2 := hexVal(digits[1])
		if ok1 && ok2 && len(s) == 2 {
			return []byte{hi<<4 | lo}, true
		}
	}
	return nil, false
}

// Python urllib.parse.unquote_to_bytes
func unquoteToBytes(s string) []byte {
	parts := strings.Split(s, "%")
	out := []byte(parts[0])
	for _, item := range parts[1:] {
		head := item
		if len(head) > 2 {
			head = head[:2]
		}
		if b, ok := fromHex2(head); ok {
			out = append(out, b...)
			out = append(out, item[len(head):]...)
		} else {
			out = append(out, '%')
			out = append(out, item...)
		}
	}
	return out
}

// Python urllib.parse.unquote with utf-8 and errors replace, decoding only ASCII runs
func pyUnquote(s string) string {
	if !strings.Contains(s, "%") {
		return s
	}
	var b strings.Builder
	for i := 0; i < len(s); {
		j := i
		ascii := s[i] < 0x80
		for j < len(s) && (s[j] < 0x80) == ascii {
			j++
		}
		if ascii {
			b.WriteString(decodeUTF8Replace(unquoteToBytes(s[i:j])))
		} else {
			b.WriteString(s[i:j])
		}
		i = j
	}
	return b.String()
}
