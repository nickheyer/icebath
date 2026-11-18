package pm

import (
	"io"
	"os"
)

// argTheme is argparse's color theme, every field empty without color
type argTheme struct {
	usage, prog, heading, summaryLong, summaryShort, summaryLabel, longOption, shortOption, label, reset string
}

// Theme argparse picks, colored only when stdout can show color
func argparseTheme(stdout io.Writer) argTheme {
	if !canColorize(stdout) {
		return argTheme{}
	}
	return argTheme{usage: "\x1b[1;34m", prog: "\x1b[1;35m", heading: "\x1b[1;34m", summaryLong: "\x1b[36m",
		summaryShort: "\x1b[32m", summaryLabel: "\x1b[33m", longOption: "\x1b[1;36m", shortOption: "\x1b[1;32m",
		label: "\x1b[1;33m", reset: "\x1b[0m"}
}

// Color codes of the argparse theme for one stream, all empty when the stream shows no color
type ArgparseColors struct {
	Usage, Prog, Heading, SummaryLong, SummaryShort, SummaryLabel, LongOption, ShortOption, Label, Reset string
}

// ArgparseThemeFor exports argparseTheme
func ArgparseThemeFor(stdout io.Writer) ArgparseColors {
	t := argparseTheme(stdout)
	return ArgparseColors{t.usage, t.prog, t.heading, t.summaryLong, t.summaryShort, t.summaryLabel, t.longOption, t.shortOption, t.label, t.reset}
}

// Reports whether Python would color output on this stream, its environment overrides first
func canColorize(stdout io.Writer) bool {
	switch os.Getenv("PYTHON_COLORS") {
	case "0":
		return false
	case "1":
		return true
	}
	if os.Getenv("NO_COLOR") != "" {
		return false
	}
	if os.Getenv("FORCE_COLOR") != "" {
		return true
	}
	if os.Getenv("TERM") == "dumb" {
		return false
	}
	return isTerminalStream(stdout)
}

// Formatter width argparse uses, the terminal columns less two
func argparseWidth() int { return terminalColumns() - 2 }

// ArgparseWidth exports argparseWidth
func ArgparseWidth() int { return argparseWidth() }
