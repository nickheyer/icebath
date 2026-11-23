package app

// Prints each line through cprint
func cp(lines ...string) {
	for _, line := range lines {
		cprint(line)
	}
}

// Text wrapped in the dim escape
func dimText(text string) string { return dimANSI + text + resetANSI }

// Two space indented dim line, the standard slash command hint shape
func dimLine(text string) string { return "  " + dimText(text) }
