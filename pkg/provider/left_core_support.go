package provider

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/nickheyer/icebath/pkg/internal/pyast"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"log/slog"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync/atomic"
	"time"
	"unicode"

	"github.com/mattn/go-isatty"
	"github.com/nickheyer/icebath/pkg/home"
)

// PluginManagerHooks are the plugin manager entry points the plugins package installs into this package
type PluginManagerHooks struct {
	CatalogEntry    func(name string) (string, bool)
	Install         func(ctx context.Context, name string, consent bool) (map[string]any, error)
	LoadError       func(plugin string) string
	DependencyHomes func() ([]string, error)
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

// Installed plugin manager hooks, panicking when the plugins package never installed them
func PluginHooks() *PluginManagerHooks { return leftCoreHooks() }

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

var interactiveTerminal = func() bool {
	return isatty.IsTerminal(os.Stdin.Fd()) && isatty.IsTerminal(os.Stdout.Fd())
}

// SwapLeftCoreTableForTests replaces the rows and returns the restore of the previous table
func SwapLeftCoreTableForTests(rows []LeftCoreFeature) func() {
	prev := leftCoreTable
	leftCoreTable = rows
	return func() { leftCoreTable = prev }
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
		if !strings.HasPrefix(pystr.Strip(v), "[") {
			return nil, false
		}
		items, ok, err := pyast.LiteralStrList(v)
		if err != nil {
			panic(err)
		}
		if !ok {
			return nil, false
		}
		out := make([]any, len(items))
		for i, item := range items {
			out[i] = item
		}
		return out, true
	}
	return nil, false
}

// Config string list from a list, a list literal string or one scalar name
func parseConfigStringList(value any) []string {
	switch v := value.(type) {
	case string:
		if strings.HasPrefix(pystr.Strip(v), "[") {
			items, ok, err := pyast.LiteralStrList(v)
			if err != nil {
				panic(err)
			}
			if ok {
				return items
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
