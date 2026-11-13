package connections

import (
	"regexp"
	"strings"
)

var launchProfileNameRE = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,63}$`)

// ParseLaunchProfile is the valid lowercased --profile value of argv, empty when absent or invalid
func ParseLaunchProfile(argv []string) string {
	for i, arg := range argv {
		if arg == "--profile" {
			if i+1 < len(argv) {
				return normalizeLaunchProfile(argv[i+1])
			}
			return ""
		}
		if value, ok := strings.CutPrefix(arg, "--profile="); ok {
			return normalizeLaunchProfile(value)
		}
	}
	return ""
}

func normalizeLaunchProfile(raw string) string {
	name := strings.ToLower(strings.TrimSpace(raw))
	if launchProfileNameRE.MatchString(name) {
		return name
	}
	return ""
}
