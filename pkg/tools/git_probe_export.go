package tools

import "time"

// Runs a short noninteractive git probe and returns its stripped stdout, empty on any failure
func BoundedGitProbe(argv []string, timeout time.Duration, env map[string]string) string {
	return boundedGitProbe(argv, timeout, env)
}
