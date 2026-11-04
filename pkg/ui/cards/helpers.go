package cards

import (
	"os"
	"strings"
)

// Width a card needs before it spends two columns on a scroll gutter
const minGutterWidth = 40

// First of two strings that is not empty
func or(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

// Path with the home directory shortened to a tilde
func homed(path string) string {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return path
	}
	if path == home {
		return "~"
	}
	if strings.HasPrefix(path, home+string(os.PathSeparator)) {
		return "~" + path[len(home):]
	}
	return path
}
