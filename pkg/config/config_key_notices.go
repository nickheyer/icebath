package config

import (
	"os"
	"strings"
)

// Terminal color codes the config notices use
const (
	ColorReset  = "\033[0m"
	ColorBold   = "\033[1m"
	ColorDim    = "\033[2m"
	ColorYellow = "\033[33m"
	ColorCyan   = "\033[36m"
)

// Reports whether stdout is a color capable terminal without NO_COLOR or a dumb TERM
func shouldUseColor() bool {
	if _, set := os.LookupEnv("NO_COLOR"); set || os.Getenv("TERM") == "dumb" {
		return false
	}
	st, err := os.Stdout.Stat()
	return err == nil && st.Mode()&os.ModeCharDevice != 0
}

// Text wrapped in color codes when stdout takes color, plain otherwise
func colorize(text string, codes ...string) string {
	if !shouldUseColor() {
		return text
	}
	return strings.Join(codes, "") + text + ColorReset
}
