package tools

import "time"

// Stripped stdout of a bounded git probe, empty on failure, for callers outside this package
func BoundedGitProbe(argv []string, timeout time.Duration, env map[string]string) string {
	return boundedGitProbe(argv, timeout, env)
}

// Runs a probe child with captured pipes, nil on spawn failure or timeout
func BoundedProbeRun(argv []string, timeout time.Duration, env map[string]string, cwd string, input *string) *ProbeResult {
	return boundedProbeRun(argv, timeout, env, cwd, input)
}
