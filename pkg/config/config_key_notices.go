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
	ColorRed    = "\033[31m"
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

// Refusal for a known key given under the wrong prefix with an optional suggestion
func UnknownSubkeyRefusal(key, suggestion string) string {
	lines := []string{colorize("✗ '"+key+"' is not a recognized config key — nothing was written.", ColorRed)}
	if suggestion != "" {
		lines = append(lines, colorize("  Did you mean: "+suggestion, ColorYellow))
	}
	lines = append(lines, colorize("  (Custom top-level keys are supported; use --force to write this path anyway.)", ColorDim))
	return strings.Join(lines, "\n")
}
