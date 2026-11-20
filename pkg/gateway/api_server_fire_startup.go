package gateway

// Whether the runner refuses new work because it is draining
func APIRunnerIsDraining(runner any) bool {
	drainer, ok := runner.(interface{ Draining() bool })
	return ok && drainer.Draining()
}
