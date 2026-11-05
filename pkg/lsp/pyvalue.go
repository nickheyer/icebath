package lsp

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"math"
	"math/big"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"unicode"
	"unicode/utf8"
)

// Python str.isspace for one rune
func pyIsSpace(r rune) bool { return unicode.IsSpace(r) || (r >= 0x1c && r <= 0x1f) }

// Python str.strip with no arguments
func pyStrip(s string) string { return strings.TrimFunc(s, pyIsSpace) }

// Python repr of a str
func pyStrRepr(s string) string {
	quote := byte('\'')
	if strings.ContainsRune(s, '\'') && !strings.ContainsRune(s, '"') {
		quote = '"'
	}
	var b strings.Builder
	b.WriteByte(quote)
	for _, r := range s {
		switch {
		case r == '\\':
			b.WriteString(`\\`)
		case r == rune(quote):
			b.WriteByte('\\')
			b.WriteByte(quote)
		case r == '\t':
			b.WriteString(`\t`)
		case r == '\n':
			b.WriteString(`\n`)
		case r == '\r':
			b.WriteString(`\r`)
		case r < 0x20 || r == 0x7f:
			fmt.Fprintf(&b, `\x%02x`, r)
		case r < 0x7f || unicode.IsPrint(r):
			b.WriteRune(r)
		case r < 0x100:
			fmt.Fprintf(&b, `\x%02x`, r)
		case r < 0x10000:
			fmt.Fprintf(&b, `\u%04x`, r)
		default:
			fmt.Fprintf(&b, `\U%08x`, r)
		}
	}
	b.WriteByte(quote)
	return b.String()
}

// Python repr of a bytes value
func pyBytesRepr(p []byte) string {
	quote := byte('\'')
	if bytes.IndexByte(p, '\'') >= 0 && bytes.IndexByte(p, '"') < 0 {
		quote = '"'
	}
	var b strings.Builder
	b.WriteString("b")
	b.WriteByte(quote)
	for _, c := range p {
		switch {
		case c == '\\':
			b.WriteString(`\\`)
		case c == quote:
			b.WriteByte('\\')
			b.WriteByte(quote)
		case c == '\t':
			b.WriteString(`\t`)
		case c == '\n':
			b.WriteString(`\n`)
		case c == '\r':
			b.WriteString(`\r`)
		case c < 0x20 || c >= 0x7f:
			fmt.Fprintf(&b, `\x%02x`, c)
		default:
			b.WriteByte(c)
		}
	}
	b.WriteByte(quote)
	return b.String()
}

// Python int() of an ASCII string, underscores between digits allowed
func pyParseInt(s string) (*big.Int, bool) {
	s = pyStrip(s)
	neg := false
	if s != "" && (s[0] == '+' || s[0] == '-') {
		neg = s[0] == '-'
		s = s[1:]
	}
	if s == "" || s[0] == '_' || s[len(s)-1] == '_' || strings.Contains(s, "__") {
		return nil, false
	}
	digits := strings.ReplaceAll(s, "_", "")
	for _, c := range digits {
		if c < '0' || c > '9' {
			return nil, false
		}
	}
	n, ok := new(big.Int).SetString(digits, 10)
	if !ok {
		return nil, false
	}
	if neg {
		n.Neg(n)
	}
	return n, true
}

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

// Python repr of a float
func pyFloatRepr(f float64) string {
	switch {
	case math.IsInf(f, 1):
		return "inf"
	case math.IsInf(f, -1):
		return "-inf"
	case math.IsNaN(f):
		return "nan"
	}
	sci := strconv.FormatFloat(f, 'e', -1, 64)
	mant, expPart, _ := strings.Cut(sci, "e")
	exp, _ := strconv.Atoi(expPart)
	if exp < -4 || exp >= 16 {
		sign := "+"
		if exp < 0 {
			sign, exp = "-", -exp
		}
		return fmt.Sprintf("%se%s%02d", mant, sign, exp)
	}
	s := strconv.FormatFloat(f, 'f', -1, 64)
	if !strings.ContainsAny(s, ".") {
		s += ".0"
	}
	return s
}

// Python str() of a decoded JSON value
func pyStr(v any) string {
	switch t := v.(type) {
	case nil:
		return "None"
	case bool:
		if t {
			return "True"
		}
		return "False"
	case int:
		return strconv.Itoa(t)
	case int64:
		return strconv.FormatInt(t, 10)
	case json.Number:
		return string(t)
	case float64:
		return pyFloatRepr(t)
	case string:
		return t
	case []any:
		parts := make([]string, len(t))
		for i, e := range t {
			parts[i] = pyRepr(e)
		}
		return "[" + strings.Join(parts, ", ") + "]"
	case map[string]any:
		keys := make([]string, 0, len(t))
		for k := range t {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		parts := make([]string, len(keys))
		for i, k := range keys {
			parts[i] = pyStrRepr(k) + ": " + pyRepr(t[k])
		}
		return "{" + strings.Join(parts, ", ") + "}"
	}
	return fmt.Sprint(v)
}

// Python repr() of a decoded JSON value
func pyRepr(v any) string {
	if s, ok := v.(string); ok {
		return pyStrRepr(s)
	}
	return pyStr(v)
}

// Python truthiness of a decoded JSON value
func pyTruthy(v any) bool {
	switch t := v.(type) {
	case nil:
		return false
	case bool:
		return t
	case int:
		return t != 0
	case int64:
		return t != 0
	case float64:
		return t != 0
	case string:
		return t != ""
	case []any:
		return len(t) > 0
	case map[string]any:
		return len(t) > 0
	}
	return true
}

// Python value of x or fallback
func pyOr(v, fallback any) any {
	if pyTruthy(v) {
		return v
	}
	return fallback
}

// Python type name of a decoded JSON value
func pyTypeName(v any) string {
	switch v.(type) {
	case nil:
		return "NoneType"
	case bool:
		return "bool"
	case int, int64, json.Number:
		return "int"
	case float64:
		return "float"
	case string:
		return "str"
	case []any:
		return "list"
	case map[string]any:
		return "dict"
	}
	return fmt.Sprintf("%T", v)
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
		n, ok := pyParseInt(t)
		if !ok || !n.IsInt64() {
			return 0, errors.New("ValueError: invalid literal for int() with base 10: " + pyStrRepr(t))
		}
		return n.Int64(), nil
	}
	return 0, errors.New("TypeError: int() argument must be a string, a bytes-like object or a real number, not " +
		pyStrRepr(pyTypeName(v)))
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

// Python OSError subclass and text for a spawn or filesystem error
func pyOSError(err error, filename string) *pyExc {
	var errno syscall.Errno
	if !errors.As(err, &errno) {
		if errors.Is(err, exec.ErrNotFound) {
			errno = syscall.ENOENT
		} else {
			return &pyExc{typ: "OSError", text: err.Error(), err: err}
		}
	}
	typ := "OSError"
	switch errno {
	case syscall.ENOENT:
		typ = "FileNotFoundError"
	case syscall.EACCES, syscall.EPERM:
		typ = "PermissionError"
	case syscall.EEXIST:
		typ = "FileExistsError"
	case syscall.ENOTDIR:
		typ = "NotADirectoryError"
	case syscall.EISDIR:
		typ = "IsADirectoryError"
	}
	msg := errno.Error()
	if msg != "" {
		msg = strings.ToUpper(msg[:1]) + msg[1:]
	}
	text := fmt.Sprintf("[Errno %d] %s", int(errno), msg)
	if filename != "" {
		text += ": " + pyStrRepr(filename)
	}
	return &pyExc{typ: typ, text: text, err: err}
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
	if errors.Is(err, context.DeadlineExceeded) {
		var te *TimeoutError
		if !errors.As(err, &te) {
			return ""
		}
	}
	return err.Error()
}

// Python str.splitlines without line ends
func pySplitLines(s string) []string {
	var out []string
	start := 0
	for i := 0; i < len(s); {
		r, size := utf8.DecodeRuneInString(s[i:])
		switch r {
		case '\r':
			out = append(out, s[start:i])
			if i+1 < len(s) && s[i+1] == '\n' {
				size = 2
			}
			start = i + size
		case '\n', '\v', '\f', '\x1c', '\x1d', '\x1e', '\x85', ' ', ' ':
			out = append(out, s[start:i])
			start = i + size
		}
		i += size
	}
	if start < len(s) {
		out = append(out, s[start:])
	}
	return out
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

// Python posixpath.normpath, ntpath.normpath on Windows
func pyNormpath(path string) string {
	if runtime.GOOS == "windows" {
		return filepath.Clean(path)
	}
	if path == "" {
		return "."
	}
	initial := 0
	if strings.HasPrefix(path, "/") {
		initial = 1
		if strings.HasPrefix(path, "//") && !strings.HasPrefix(path, "///") {
			initial = 2
		}
	}
	var comps []string
	for _, comp := range strings.Split(path, "/") {
		if comp == "" || comp == "." {
			continue
		}
		if comp != ".." || (initial == 0 && len(comps) == 0) || (len(comps) > 0 && comps[len(comps)-1] == "..") {
			comps = append(comps, comp)
		} else if len(comps) > 0 {
			comps = comps[:len(comps)-1]
		}
	}
	out := strings.Repeat("/", initial) + strings.Join(comps, "/")
	if out == "" {
		return "."
	}
	return out
}

// Python os.path.abspath, the cleaned path when no cwd can anchor a relative one
func pyAbspath(p string) string {
	if abs, err := filepath.Abs(p); err == nil {
		return abs
	}
	return filepath.Clean(p)
}
