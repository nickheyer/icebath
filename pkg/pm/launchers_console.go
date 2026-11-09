package pm

import (
	"strings"
	"unicode/utf16"

	"github.com/nickheyer/icebath/pkg/home"
)

// Environment Windows gives a new console of this user, swapped by tests, false when the host cannot say
var newConsoleEnvironment = consoleEnvironment

// Parses an environment block of NUL terminated UTF-16 NAME=value entries ending in an empty entry, names upper cased
func readEnvironmentBlock(block []uint16) map[string]string {
	environ := map[string]string{}
	for cursor := 0; cursor < len(block); {
		end := cursor
		for end < len(block) && block[end] != 0 {
			end++
		}
		if end == cursor {
			break
		}
		name, value, _ := strings.Cut(string(utf16.Decode(block[cursor:end])), "=")
		if name != "" {
			environ[strings.ToUpper(name)] = value
		}
		cursor = end + 1
	}
	return environ
}

// Default root a new console resolves from the user's own environment block, false when Windows cannot say
func newConsoleRoot() (string, bool) {
	environ, ok := newConsoleEnvironment()
	if !ok {
		return "", false
	}
	return home.RootForEnvironment(environ), true
}
