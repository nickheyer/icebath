package pathutil

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/nickheyer/icebath/pkg/internal/homeguard"
)

// Error of a home directory lookup that found nothing
var ErrNoHome = errors.New("home directory cannot be determined")

// Cleaned home directory of the current user, from the environment and else from the account database
func Home() (string, error) {
	h, err := currentHome()
	if err != nil {
		return "", err
	}
	return filepath.Clean(h), nil
}

// Path with a leading ~ or ~name replaced by that user's home directory, unchanged when that home cannot be found
func ExpandUser(p string) string {
	out, err := expandUser(p)
	if err != nil {
		return p
	}
	return out
}

// Cleaned path with a leading ~ or ~name replaced by that user's home directory, an error wrapping ErrNoHome when that home cannot be found
func ExpandUserPath(p string) (string, error) {
	out, err := expandUser(p)
	if err != nil {
		return "", err
	}
	return filepath.Clean(out), nil
}

// CleanKeepDotDot form of p with a leading ~ or ~name replaced by that user's home directory, so a dot-dot after a symlink still names what the system opens; an error wrapping ErrNoHome when that home cannot be found
func ExpandUserKeepDotDot(p string) (string, error) {
	out, err := expandUser(p)
	if err != nil {
		return "", err
	}
	return CleanKeepDotDot(out), nil
}

func expandUser(p string) (string, error) {
	if !strings.HasPrefix(p, "~") {
		return p, nil
	}
	i := 1
	for i < len(p) && !os.IsPathSeparator(p[i]) {
		i++
	}
	home, err := homeOf(p[1:i])
	if err != nil {
		return "", err
	}
	seps := "/"
	if runtime.GOOS == "windows" {
		seps = `/\`
	}
	trimmed := strings.TrimRight(home, seps)
	if rest := p[i:]; rest != "" {
		return trimmed + rest, nil
	}
	if trimmed == "" || strings.HasSuffix(trimmed, ":") {
		return home, nil
	}
	return trimmed, nil
}

// Home directory of the named user, the current user for an empty name
func homeOf(name string) (string, error) {
	if name == "" || runtime.GOOS == "windows" && strings.EqualFold(name, os.Getenv("USERNAME")) {
		return currentHome()
	}
	u, err := homeguard.Lookup(name)
	if err == nil && u.HomeDir != "" {
		return u.HomeDir, nil
	}
	if runtime.GOOS == "windows" {
		if own, herr := currentHome(); herr == nil && strings.EqualFold(filepath.Base(own), os.Getenv("USERNAME")) {
			return filepath.Join(filepath.Dir(own), name), nil
		}
	}
	if err == nil {
		return "", fmt.Errorf("%w for user %q", ErrNoHome, name)
	}
	return "", fmt.Errorf("%w for user %q: %w", ErrNoHome, name, err)
}

// Home directory of the current user from the platform's home variables, else from the account database
func currentHome() (string, error) {
	if h := homeFromEnv(); h != "" {
		return h, nil
	}
	if u, err := homeguard.Current(); err == nil && u.HomeDir != "" {
		return u.HomeDir, nil
	}
	return "", ErrNoHome
}

func homeFromEnv() string {
	switch runtime.GOOS {
	case "windows":
		if h := os.Getenv("USERPROFILE"); h != "" {
			return h
		}
		if hp := os.Getenv("HOMEPATH"); hp != "" {
			return filepath.Join(os.Getenv("HOMEDRIVE"), hp)
		}
		return ""
	case "plan9":
		return os.Getenv("home")
	}
	return os.Getenv("HOME")
}
