// Resolves the icebath home, its named profiles, the files below it, scratch space and directory permissions
package home

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"unicode"
	"unicode/utf8"

	"github.com/nickheyer/icebath/internal/hostinfo"
)

// Environment variables the home layout reads and the child environments it writes
const (
	// Home directory of the running process
	EnvHome = "ICEBATH_HOME"
	// Real account home exported to children whose HOME is moved
	EnvRealHome = "ICEBATH_REAL_HOME"
	// Scratch directory icebath itself exported into the temp variables
	EnvScratchDir = "ICEBATH_SCRATCH_DIR"
	envSuffix     = "ICEBATH_DATA_DIR_SUFFIX"
)

// Error for a lookup that needs the account home when neither the environment nor the account database names one
var ErrNoUserHome = errors.New("could not determine the user home directory")

// Process wide home state, the one-time warning latch and the caches each home decision keeps
type Resolver struct {
	host    *hostinfo.Host
	warned  atomic.Bool
	pruned  atomic.Bool
	keyMu   sync.Mutex
	keys    map[string]string
	initMu  sync.Mutex
	ensured map[string]bool
	idMu    sync.Mutex
	idRoot  string
	id      string
	acl     *aclState
}

// Resolver reading container facts from host, one per process
func New(host *hostinfo.Host) *Resolver {
	return &Resolver{host: host, keys: map[string]string{}, ensured: map[string]bool{}, acl: newACLState()}
}

// Context key carrying a per-context home override
type overrideKey struct{}

// Context whose home resolution answers path instead of the process home, an empty path clearing it
func WithOverride(ctx context.Context, path string) context.Context {
	return context.WithValue(ctx, overrideKey{}, path)
}

// Home override ctx carries, empty when none is in force
func Override(ctx context.Context) string {
	if ctx == nil {
		return ""
	}
	v, _ := ctx.Value(overrideKey{}).(string)
	return v
}

// Home path with environment references and a leading tilde expanded, then cleaned
func expandHome(path string) string {
	return filepath.Clean(ExpandUser(expandReferences(path)))
}

// Stripped value of an environment variable, empty when unset
func envValue(name string) string { return strings.TrimSpace(os.Getenv(name)) }

// Account home of this process, ErrNoUserHome when it cannot be determined
func UserHome() (string, error) {
	h, ok := userHome()
	if !ok {
		return "", ErrNoUserHome
	}
	return filepath.Clean(h), nil
}

// Platform default home of this process environment, ignoring ICEBATH_HOME
func PlatformDefault() (string, error) { return platformDefaultFrom(os.Getenv, UserHome) }

// Home of the running process from ICEBATH_HOME, else the platform default, never a context override
func ProcessHome() (string, error) {
	if v := envValue(EnvHome); v != "" {
		return expandHome(v), nil
	}
	return PlatformDefault()
}

// Directory in effect for ctx, its override first, then ICEBATH_HOME, then the platform default
func (r *Resolver) Home(ctx context.Context) (string, error) {
	if o := Override(ctx); o != "" {
		return expandHome(o), nil
	}
	if envValue(EnvHome) == "" {
		r.warnProfileFallback()
	}
	return ProcessHome()
}

// Warns once on stderr when ICEBATH_HOME is unset while a non-default profile is sticky active
func (r *Resolver) warnProfileFallback() {
	if r.warned.Swap(true) {
		return
	}
	fallback, err := PlatformDefault()
	if err != nil {
		return
	}
	data, err := os.ReadFile(filepath.Join(fallback, string(ActiveProfile)))
	if err != nil || !utf8.Valid(data) {
		return
	}
	active := strings.TrimSpace(string(data))
	if active == "" || active == "default" {
		return
	}
	q := reprQuote(active)
	fmt.Fprintf(os.Stderr, "[ICEBATH_HOME fallback] ICEBATH_HOME is unset but active profile is %s. "+
		"Falling back to %s, which is the DEFAULT profile — not %s. Any data this process writes will land "+
		"in the wrong profile. The subprocess spawner should pass ICEBATH_HOME explicitly (see issue #18594).\n",
		q, fallback, q)
}

// Single or double quoted literal of s with backslashes, quotes and unprintable runes escaped, invalid bytes as surrogate escapes
func reprQuote(s string) string {
	quote := byte('\'')
	if strings.ContainsRune(s, '\'') && !strings.ContainsRune(s, '"') {
		quote = '"'
	}
	var b strings.Builder
	b.WriteByte(quote)
	for i := 0; i < len(s); {
		r, size := utf8.DecodeRuneInString(s[i:])
		switch {
		case r == utf8.RuneError && size <= 1:
			fmt.Fprintf(&b, `\udc%02x`, s[i])
		case r == '\\' || r == rune(quote):
			b.WriteByte('\\')
			b.WriteRune(r)
		case r == '\t':
			b.WriteString(`\t`)
		case r == '\n':
			b.WriteString(`\n`)
		case r == '\r':
			b.WriteString(`\r`)
		case r < 0x20 || r == 0x7f:
			fmt.Fprintf(&b, `\x%02x`, r)
		case unicode.IsPrint(r):
			b.WriteRune(r)
		case r < 0x100:
			fmt.Fprintf(&b, `\x%02x`, r)
		case r < 0x10000:
			fmt.Fprintf(&b, `\u%04x`, r)
		default:
			fmt.Fprintf(&b, `\U%08x`, r)
		}
		i += max(size, 1)
	}
	b.WriteByte(quote)
	return b.String()
}

// Home this process serves as its own profile, set by an embedding host that mirrors per-turn homes into ICEBATH_HOME
var pinnedHome atomic.Pointer[string]

// Pins path as the home this process serves for routed profile decisions
func PinProcessHome(path string) { pinnedHome.Store(&path) }

// Clears the pinned process home so routing follows ICEBATH_HOME again
func UnpinProcessHome() { pinnedHome.Store(nil) }

// Reports whether a host pinned the process home
func ProcessHomePinned() bool { return pinnedHome.Load() != nil }

// Launch home routed profile decisions compare against, the pinned home else the process home
func RoutingHome() (string, error) {
	if p := pinnedHome.Load(); p != nil && *p != "" {
		return expandHome(*p), nil
	}
	return ProcessHome()
}

// Sets ICEBATH_HOME to the default root when the launch environment leaves it empty
func PinLauncherHome() error {
	if os.Getenv(EnvHome) != "" {
		return nil
	}
	root, err := DefaultRoot()
	if err != nil {
		return err
	}
	return os.Setenv(EnvHome, root)
}

// Integer text in base with an optional sign, an optional 0o prefix in base 8 and single underscores between digits
func parseInt(text string, base int) (int64, bool) {
	t := strings.TrimSpace(text)
	sign := ""
	if t != "" && (t[0] == '+' || t[0] == '-') {
		sign, t = t[:1], t[1:]
	}
	if base == 8 && len(t) > 1 && t[0] == '0' && (t[1] == 'o' || t[1] == 'O') {
		t = strings.TrimPrefix(t[2:], "_")
	}
	if t == "" || t[0] == '_' || t[0] == '+' || t[0] == '-' || t[len(t)-1] == '_' || strings.Contains(t, "__") {
		return 0, false
	}
	n, err := strconv.ParseInt(sign+strings.ReplaceAll(t, "_", ""), base, 64)
	return n, err == nil
}
