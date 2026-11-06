package state

import "strings"

// Index of the first serve or dashboard token in a command line, false when neither appears
func dashboardSubcommandIndex(argv []string) (int, bool) {
	for i, tok := range argv {
		if tok == "serve" || tok == "dashboard" {
			return i, true
		}
	}
	return 0, false
}

// True for Desktop style serve or dashboard backends started with port zero
func isEphemeralPortZeroBackend(argv []string) bool {
	if _, ok := dashboardSubcommandIndex(argv); !ok {
		return false
	}
	for i, tok := range argv {
		if tok == "--port" && i+1 < len(argv) && argv[i+1] == "0" {
			return true
		}
		if value, ok := strings.CutPrefix(tok, "--port="); ok && strings.TrimSpace(value) == "0" {
			return true
		}
	}
	return false
}
