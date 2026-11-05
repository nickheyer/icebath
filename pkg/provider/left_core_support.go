package provider

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"math"
	"math/big"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync/atomic"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/mattn/go-isatty"
	"github.com/nickheyer/icebath/pkg/home"
)

// PluginManagerHooks are the plugin manager entry points the plugins package installs into this package
type PluginManagerHooks struct {
	CatalogEntry            func(name string) (string, bool)
	Install                 func(ctx context.Context, name string, consent bool) (map[string]any, error)
	SharedDependencyAnswers func(fn func())
	LoadError               func(plugin string) string
	RefreshBoundHome        func()
	DependencyHomes         func() []string
	AuxiliaryTasks          func() []map[string]any
}

var pluginManagerHooks atomic.Pointer[PluginManagerHooks]

// InstallPluginManagerHooks wires the plugin manager into provider resolution and the left core migration
func InstallPluginManagerHooks(h PluginManagerHooks) { pluginManagerHooks.Store(&h) }

func leftCoreHooks() *PluginManagerHooks {
	h := pluginManagerHooks.Load()
	if h == nil {
		panic("provider: plugin manager hooks were never installed by the plugins package")
	}
	return h
}

// Plugin auxiliary task registrations sorted by key after idempotent discovery
func pluginAuxiliaryTasks() []map[string]any { return leftCoreHooks().AuxiliaryTasks() }

const (
	leftCoreRetrySeconds    = 3600.0
	leftCoreRetryMaxSeconds = 7 * 24 * 3600.0
)

func leftCoreFailureStamp(h, plugin string) string {
	return filepath.Join(home.CacheDir(h), "left-core-"+plugin+".failed")
}

// Consecutive failures in a stamp, an empty or non numeric stamp counting once
func leftCoreFailureCount(stamp string) int {
	data, err := os.ReadFile(stamp)
	if err != nil {
		return 0
	}
	text := strings.TrimSpace(strings.TrimPrefix(string(data), "\ufeff"))
	if text == "" || strings.TrimFunc(text, unicode.IsDigit) != "" {
		return 1
	}
	n, err := strconv.Atoi(text)
	if err != nil {
		return math.MaxInt32
	}
	return n
}

// NoteLeftCoreFailure bumps the failed install stamp whose mtime and count gate the next startup attempt
func NoteLeftCoreFailure(h, plugin string) {
	stamp := leftCoreFailureStamp(h, plugin)
	count := leftCoreFailureCount(stamp)
	if err := os.MkdirAll(filepath.Dir(stamp), 0o700); err != nil {
		slog.Default().Debug("left-core retry stamp not written", "home", h, "error", err)
		return
	}
	if err := os.WriteFile(stamp, []byte(strconv.Itoa(count+1)), 0o600); err != nil {
		slog.Default().Debug("left-core retry stamp not written", "home", h, "error", err)
	}
}

// LeftCoreFailedRecently reports a last failure younger than an hour doubled per failure, capped at a week
func LeftCoreFailedRecently(h, plugin string) bool {
	stamp := leftCoreFailureStamp(h, plugin)
	info, err := os.Stat(stamp)
	if err != nil {
		return false
	}
	age := time.Since(info.ModTime()).Seconds()
	failures := min(leftCoreFailureCount(stamp), 32)
	return age < math.Min(leftCoreRetrySeconds*math.Pow(2, float64(failures-1)), leftCoreRetryMaxSeconds)
}

// NoteLeftCoreSuccess clears the failure stamp so a later miss backs off from an hour again
func NoteLeftCoreSuccess(h, plugin string) {
	if err := os.Remove(leftCoreFailureStamp(h, plugin)); err != nil && !os.IsNotExist(err) {
		slog.Default().Debug("left-core retry stamp not removed", "home", h, "error", err)
	}
}

// PluginInstallCommand is the exact command installing a plugin into a home, bare only for the sticky default
func PluginInstallCommand(name, h string) string {
	profile := home.ProfileNameFor(h)
	if profile == "" || (profile == "default" && activeStickyProfile() == "default") {
		return "icebath plugins install " + name
	}
	return fmt.Sprintf("icebath -p %s plugins install %s", profile, name)
}

// PluginHomeLabel names a home by its profile, else by its path
func PluginHomeLabel(h string) string {
	if profile := home.ProfileNameFor(h); profile != "" {
		return fmt.Sprintf("profile '%s'", profile)
	}
	return h
}

func interactiveTerminal() bool {
	return isatty.IsTerminal(os.Stdin.Fd()) && isatty.IsTerminal(os.Stdout.Fd())
}

// Dependency consent without a terminal comes from the lazy install policy of the home in ctx
func unattendedConsent(ctx context.Context) bool {
	return !interactiveTerminal() && leftCoreLazyInstallsAllowed(ctx)
}

func homeConsent(h string) bool {
	return unattendedConsent(home.WithOverride(context.Background(), h))
}

// Toolset list a saved platform_toolsets value encodes, a list literal string parsed into names
func parsePlatformToolsetsValue(value any) ([]any, bool) {
	switch v := value.(type) {
	case []any:
		return v, true
	case string:
		text := strings.TrimSpace(v)
		if !strings.HasPrefix(text, "[") {
			return nil, false
		}
		items, ok := pyLiteralList(text)
		if !ok {
			return nil, false
		}
		out := make([]any, len(items))
		for i, item := range items {
			out[i] = item.str()
		}
		return out, true
	}
	return nil, false
}

// Config string list from a list, a list literal string or one scalar name
func parseConfigStringList(value any) []string {
	switch v := value.(type) {
	case string:
		if text := strings.TrimSpace(v); strings.HasPrefix(text, "[") {
			if items, ok := pyLiteralList(text); ok {
				out := make([]string, len(items))
				for i, item := range items {
					out[i] = item.str()
				}
				return out
			}
		}
		return []string{v}
	case []any:
		return pyStrList(v)
	case []string:
		return append([]string(nil), v...)
	}
	return nil
}

// Python repr of a decoded config value
func leftCorePyRepr(v any) string {
	switch t := v.(type) {
	case nil:
		return "None"
	case bool:
		if t {
			return "True"
		}
		return "False"
	case string:
		return pyStringRepr(t)
	case int:
		return strconv.Itoa(t)
	case int64:
		return strconv.FormatInt(t, 10)
	case uint64:
		return strconv.FormatUint(t, 10)
	case float64:
		return pyFloatRepr(t)
	case json.Number:
		return t.String()
	case []any:
		parts := make([]string, len(t))
		for i, item := range t {
			parts[i] = leftCorePyRepr(item)
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
			parts[i] = pyStringRepr(k) + ": " + leftCorePyRepr(t[k])
		}
		return "{" + strings.Join(parts, ", ") + "}"
	}
	return fmt.Sprint(v)
}

// Python repr of a float with shortest digits, fixed between 1e-4 and 1e16
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
	exp, _ := strconv.Atoi(sci[strings.IndexByte(sci, 'e')+1:])
	if exp >= -4 && exp < 16 {
		s := strconv.FormatFloat(f, 'f', -1, 64)
		if !strings.Contains(s, ".") {
			s += ".0"
		}
		return s
	}
	mant, expPart, _ := strings.Cut(sci, "e")
	sign := expPart[0]
	digits := strings.TrimLeft(expPart[1:], "0")
	if len(digits) < 2 {
		digits = strings.Repeat("0", 2-len(digits)) + digits
	}
	return mant + "e" + string(sign) + digits
}

// Python repr of a string, single quoted unless only double quotes avoid escaping
func pyStringRepr(s string) string {
	quote := byte('\'')
	if strings.Contains(s, "'") && !strings.Contains(s, `"`) {
		quote = '"'
	}
	var b strings.Builder
	b.WriteByte(quote)
	for _, r := range s {
		switch {
		case r == rune(quote) || r == '\\':
			b.WriteByte('\\')
			b.WriteRune(r)
		case r == '\n':
			b.WriteString(`\n`)
		case r == '\r':
			b.WriteString(`\r`)
		case r == '\t':
			b.WriteString(`\t`)
		case r < 0x20 || r == 0x7f:
			fmt.Fprintf(&b, `\x%02x`, r)
		case r < 0x7f || unicode.IsPrint(r):
			b.WriteRune(r)
		case r <= 0xff:
			fmt.Fprintf(&b, `\x%02x`, r)
		case r <= 0xffff:
			fmt.Fprintf(&b, `\u%04x`, r)
		default:
			fmt.Fprintf(&b, `\U%08x`, r)
		}
	}
	b.WriteByte(quote)
	return b.String()
}

// One Python literal value carrying its str and repr forms
type pyLit struct {
	isStr bool
	text  string
	repr  string
}

func (l pyLit) str() string {
	if l.isStr {
		return l.text
	}
	return l.repr
}

type pyLitParser struct {
	s string
	i int
}

// Items of a Python list literal as ast.literal_eval reads it, false when malformed or not a list
func pyLiteralList(text string) ([]pyLit, bool) {
	p := &pyLitParser{s: text}
	p.ws()
	if p.i >= len(p.s) || p.s[p.i] != '[' {
		return nil, false
	}
	p.i++
	items, ok := p.seq(']')
	if !ok {
		return nil, false
	}
	p.ws()
	if p.i != len(p.s) {
		return nil, false
	}
	return items, true
}

func (p *pyLitParser) ws() {
	for p.i < len(p.s) && strings.IndexByte(" \t\n\r\f\v", p.s[p.i]) >= 0 {
		p.i++
	}
}

// Comma separated values up to the closing byte, a trailing comma allowed
func (p *pyLitParser) seq(closer byte) ([]pyLit, bool) {
	var items []pyLit
	for {
		p.ws()
		if p.i < len(p.s) && p.s[p.i] == closer {
			p.i++
			return items, true
		}
		item, ok := p.value()
		if !ok {
			return nil, false
		}
		items = append(items, item)
		p.ws()
		if p.i >= len(p.s) {
			return nil, false
		}
		switch p.s[p.i] {
		case ',':
			p.i++
		case closer:
			p.i++
			return items, true
		default:
			return nil, false
		}
	}
}

func joinRepr(items []pyLit) string {
	parts := make([]string, len(items))
	for i, item := range items {
		parts[i] = item.repr
	}
	return strings.Join(parts, ", ")
}

// One literal value, containers rendered through their Python repr
func (p *pyLitParser) value() (pyLit, bool) {
	p.ws()
	if p.i >= len(p.s) {
		return pyLit{}, false
	}
	switch c := p.s[p.i]; {
	case c == '[':
		p.i++
		items, ok := p.seq(']')
		return pyLit{repr: "[" + joinRepr(items) + "]"}, ok
	case c == '(':
		p.i++
		items, ok := p.seq(')')
		if !ok {
			return pyLit{}, false
		}
		if len(items) == 1 {
			return pyLit{repr: "(" + items[0].repr + ",)"}, true
		}
		return pyLit{repr: "(" + joinRepr(items) + ")"}, true
	case c == '{':
		return p.braces()
	case c == '\'' || c == '"':
		return p.stringLit(false)
	}
	start := p.i
	for p.i < len(p.s) && (isPyIdentByte(p.s[p.i]) || strings.IndexByte("+-.", p.s[p.i]) >= 0) {
		if (p.s[p.i] == '+' || p.s[p.i] == '-') && p.i > start && p.s[p.i-1] != 'e' && p.s[p.i-1] != 'E' {
			break
		}
		p.i++
	}
	token := p.s[start:p.i]
	if p.i < len(p.s) && (p.s[p.i] == '\'' || p.s[p.i] == '"') {
		switch strings.ToLower(token) {
		case "u":
			return p.stringLit(false)
		case "r":
			return p.stringLit(true)
		}
		return pyLit{}, false
	}
	switch token {
	case "True", "False", "None":
		return pyLit{repr: token}, true
	}
	return pyNumberLit(token)
}

func isPyIdentByte(c byte) bool {
	return c == '_' || (c >= '0' && c <= '9') || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z')
}

// Numeric literal rendered as Python repr renders the value
func pyNumberLit(token string) (pyLit, bool) {
	if token == "" {
		return pyLit{}, false
	}
	body := strings.TrimLeft(token, "+-")
	negative := strings.Count(token[:len(token)-len(body)], "-")%2 == 1
	clean := strings.ReplaceAll(body, "_", "")
	if strings.HasPrefix(body, "_") || strings.HasSuffix(body, "_") || strings.Contains(body, "__") {
		return pyLit{}, false
	}
	if n, ok := new(big.Int).SetString(clean, 0); ok && !(len(clean) > 1 && clean[0] == '0' && clean[1] >= '0' && clean[1] <= '9') {
		if negative {
			n.Neg(n)
		}
		return pyLit{repr: n.String()}, true
	}
	if clean == "" || strings.ContainsAny(clean, "xXoObB") || strings.ContainsAny(clean, "abcdfghijklmnpqrstuvwyzABCDFGHIJKLMNPQRSTUVWYZ") {
		return pyLit{}, false
	}
	f, err := strconv.ParseFloat(clean, 64)
	if err != nil && !strings.Contains(err.Error(), "range") {
		return pyLit{}, false
	}
	if negative {
		f = -f
	}
	return pyLit{repr: pyFloatRepr(f)}, true
}

// Dict or set literal in source order
func (p *pyLitParser) braces() (pyLit, bool) {
	p.i++
	p.ws()
	if p.i < len(p.s) && p.s[p.i] == '}' {
		p.i++
		return pyLit{repr: "{}"}, true
	}
	var parts []string
	isDict := false
	for idx := 0; ; idx++ {
		p.ws()
		if p.i < len(p.s) && p.s[p.i] == '}' && idx > 0 {
			p.i++
			break
		}
		key, ok := p.value()
		if !ok {
			return pyLit{}, false
		}
		p.ws()
		if idx == 0 {
			isDict = p.i < len(p.s) && p.s[p.i] == ':'
		}
		if isDict {
			if p.i >= len(p.s) || p.s[p.i] != ':' {
				return pyLit{}, false
			}
			p.i++
			val, ok := p.value()
			if !ok {
				return pyLit{}, false
			}
			parts = append(parts, key.repr+": "+val.repr)
		} else {
			parts = append(parts, key.repr)
		}
		p.ws()
		if p.i >= len(p.s) {
			return pyLit{}, false
		}
		if p.s[p.i] == ',' {
			p.i++
			continue
		}
		if p.s[p.i] != '}' {
			return pyLit{}, false
		}
		p.i++
		break
	}
	return pyLit{repr: "{" + strings.Join(parts, ", ") + "}"}, true
}

// Adjacent quoted string literals concatenated, escapes decoded unless raw
func (p *pyLitParser) stringLit(raw bool) (pyLit, bool) {
	var b strings.Builder
	for {
		s, ok := p.quoted(raw)
		if !ok {
			return pyLit{}, false
		}
		b.WriteString(s)
		save := p.i
		p.ws()
		if p.i < len(p.s) && (p.s[p.i] == '\'' || p.s[p.i] == '"') {
			raw = false
			continue
		}
		p.i = save
		break
	}
	text := b.String()
	return pyLit{isStr: true, text: text, repr: pyStringRepr(text)}, true
}

// One quoted string, single or triple quoted
func (p *pyLitParser) quoted(raw bool) (string, bool) {
	q := p.s[p.i]
	delim := string(q)
	if strings.HasPrefix(p.s[p.i:], strings.Repeat(delim, 3)) {
		delim = strings.Repeat(delim, 3)
	}
	p.i += len(delim)
	var b strings.Builder
	for p.i < len(p.s) {
		if strings.HasPrefix(p.s[p.i:], delim) {
			p.i += len(delim)
			return b.String(), true
		}
		c := p.s[p.i]
		if c == '\n' && len(delim) == 1 {
			return "", false
		}
		if c != '\\' {
			r, size := utf8.DecodeRuneInString(p.s[p.i:])
			b.WriteRune(r)
			p.i += size
			continue
		}
		if p.i+1 >= len(p.s) {
			return "", false
		}
		if raw {
			b.WriteByte('\\')
			b.WriteByte(p.s[p.i+1])
			p.i += 2
			continue
		}
		if !p.escape(&b) {
			return "", false
		}
	}
	return "", false
}

// Decodes one backslash escape at the cursor
func (p *pyLitParser) escape(b *strings.Builder) bool {
	e := p.s[p.i+1]
	p.i += 2
	simple := map[byte]string{'\\': "\\", '\'': "'", '"': "\"", 'n': "\n", 't': "\t", 'r': "\r", 'a': "\a", 'b': "\b", 'f': "\f", 'v': "\v", '\n': ""}
	if out, ok := simple[e]; ok {
		b.WriteString(out)
		return true
	}
	hexLen := map[byte]int{'x': 2, 'u': 4, 'U': 8}[e]
	if hexLen > 0 {
		if p.i+hexLen > len(p.s) {
			return false
		}
		n, err := strconv.ParseUint(p.s[p.i:p.i+hexLen], 16, 32)
		if err != nil || n > unicode.MaxRune {
			return false
		}
		b.WriteRune(rune(n))
		p.i += hexLen
		return true
	}
	if e >= '0' && e <= '7' {
		end := p.i
		for end < len(p.s) && end < p.i+2 && p.s[end] >= '0' && p.s[end] <= '7' {
			end++
		}
		n, _ := strconv.ParseUint(p.s[p.i-1:end], 8, 32)
		b.WriteRune(rune(n))
		p.i = end
		return true
	}
	b.WriteByte('\\')
	b.WriteByte(e)
	return true
}
