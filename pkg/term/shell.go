package term

import (
	"strings"
)

// Variables the terminal sets for every program it starts
var termEnv = map[string]string{"TERM": "xterm-256color", "COLORTERM": "truecolor", "TERM_PROGRAM": "icebath"}

// Program a terminal starts when none is configured, the login shell of the user or the plain shell of the system
func DefaultShell() Command { return Command{Path: defaultShell()} }

// Environment a program on the terminal starts with, the terminal variables set over the given base
func Environ(base []string, version string) []string {
	out := make([]string, 0, len(base)+len(termEnv)+1)
	for _, kv := range base {
		key, _, _ := strings.Cut(kv, "=")
		if _, drop := termEnv[key]; drop || key == "TERM_PROGRAM_VERSION" || key == "LINES" || key == "COLUMNS" {
			continue
		}
		out = append(out, kv)
	}
	for _, key := range []string{"TERM", "COLORTERM", "TERM_PROGRAM"} {
		out = append(out, key+"="+termEnv[key])
	}
	if version != "" {
		out = append(out, "TERM_PROGRAM_VERSION="+version)
	}
	return out
}
