package home

// Replaces the WSL probe of the cwd translation and returns the probe it replaced
func SetWSLProbe(probe func() bool) (previous func() bool) {
	previous, isWSL = isWSL, probe
	return previous
}
