package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"syscall"
	"unicode/utf8"

	"github.com/nickheyer/icebath/pkg/internal/homeguard"
)

// Python equality where bools and numbers compare by value through nested lists and mappings
func pyEq(a, b any) bool {
	if af, ok := pyNumber(a); ok {
		bf, ok := pyNumber(b)
		return ok && af == bf
	}
	switch x := a.(type) {
	case []any:
		y, ok := b.([]any)
		if !ok || len(x) != len(y) {
			return false
		}
		for i := range x {
			if !pyEq(x[i], y[i]) {
				return false
			}
		}
		return true
	case map[string]any:
		y, ok := b.(map[string]any)
		if !ok || len(x) != len(y) {
			return false
		}
		for k, v := range x {
			w, has := y[k]
			if !has || !pyEq(v, w) {
				return false
			}
		}
		return true
	}
	if _, ok := pyNumber(b); ok {
		return false
	}
	return reflect.DeepEqual(a, b)
}

// Reason of a file failure followed by the failing path
func pyOSErrorString(err error) string {
	var pathErr *os.PathError
	var errno syscall.Errno
	if errors.As(err, &pathErr) && errors.As(pathErr.Err, &errno) {
		return errno.Error() + ": " + pathErr.Path
	}
	return err.Error()
}

// Whether an environment variable is set to a truthy spelling
func envVarEnabled(name string) bool {
	return isTruthyString(os.Getenv(name))
}

// Numeric value of a bool or number for Python style equality
func pyNumber(v any) (float64, bool) {
	switch x := v.(type) {
	case bool:
		if x {
			return 1, true
		}
		return 0, true
	case int:
		return float64(x), true
	case int64:
		return float64(x), true
	case uint64:
		return float64(x), true
	case float64:
		return x, true
	}
	return 0, false
}

// UnicodeDecodeError text Python reports for the first invalid UTF-8 sequence, nil when the bytes decode
func pyUTF8DecodeError(data []byte) error {
	for i := 0; i < len(data); {
		r, size := utf8.DecodeRune(data[i:])
		if r != utf8.RuneError || size > 1 {
			i += size
			continue
		}
		b := data[i]
		need := 0
		switch {
		case b >= 0xC2 && b <= 0xDF:
			need = 1
		case b >= 0xE0 && b <= 0xEF:
			need = 2
		case b >= 0xF0 && b <= 0xF4:
			need = 3
		default:
			return fmt.Errorf("not valid UTF-8: byte 0x%02x at position %d cannot start a character", b, i)
		}
		tail := data[i+1:]
		if len(tail) > need {
			tail = tail[:need]
		}
		valid := 0
		for valid < len(tail) && tail[valid]&0xC0 == 0x80 {
			valid++
		}
		if valid == len(tail) && i+1+need > len(data) {
			if len(data)-i == 1 {
				return fmt.Errorf("not valid UTF-8: byte 0x%02x at position %d begins a character the text ends inside", b, i)
			}
			return fmt.Errorf("not valid UTF-8: bytes at positions %d-%d begin a character the text ends inside", i, len(data)-1)
		}
		return fmt.Errorf("not valid UTF-8: byte 0x%02x at position %d begins a character that the next byte breaks", b, i)
	}
	return nil
}

// Absolute path with every existing symlink resolved and the missing tail kept, as Path.resolve does
func pyResolve(p string) (string, error) {
	abs, err := filepath.Abs(p)
	if err != nil {
		return "", err
	}
	tail := ""
	for cur := abs; ; {
		if real, err := filepath.EvalSymlinks(cur); err == nil {
			return filepath.Join(real, tail), nil
		}
		parent := filepath.Dir(cur)
		if parent == cur {
			return abs, nil
		}
		tail = filepath.Join(filepath.Base(cur), tail)
		cur = parent
	}
}

// Path with a leading tilde expanded the way os.path.expanduser does
func pyExpandUser(p string) string {
	if !strings.HasPrefix(p, "~") {
		return p
	}
	name, rest, _ := strings.Cut(p[1:], string(filepath.Separator))
	var dir string
	if name == "" {
		if dir = os.Getenv("HOME"); dir == "" {
			u, err := homeguard.Current()
			if err != nil {
				return p
			}
			dir = u.HomeDir
		}
	} else {
		u, err := homeguard.Lookup(name)
		if err != nil {
			return p
		}
		dir = u.HomeDir
	}
	if rest == "" && !strings.Contains(p, string(filepath.Separator)) {
		return dir
	}
	return strings.TrimRight(dir, string(filepath.Separator)) + string(filepath.Separator) + rest
}

// Profile directory the bot mode roster names
type BotProfile struct {
	Name string
	Dir  string
}
