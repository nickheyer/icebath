package pyshutil

import (
	"os"
	"strconv"
	"strings"

	xterm "github.com/charmbracelet/x/term"
)

// Columns and lines like shutil.get_terminal_size, COLUMNS and LINES first, then the stdout terminal, then the fallback
func GetTerminalSize(fallbackColumns, fallbackLines int) (int, int) {
	columns := envDimension("COLUMNS")
	lines := envDimension("LINES")
	if columns <= 0 || lines <= 0 {
		sizeCols, sizeLines := fallbackColumns, fallbackLines
		if w, h, err := xterm.GetSize(os.Stdout.Fd()); err == nil {
			sizeCols, sizeLines = w, h
		}
		if columns <= 0 {
			columns = sizeCols
			if columns == 0 {
				columns = fallbackColumns
			}
		}
		if lines <= 0 {
			lines = sizeLines
			if lines == 0 {
				lines = fallbackLines
			}
		}
	}
	return columns, lines
}

// int() of an environment variable, zero when unset or unparsable
func envDimension(key string) int {
	raw, ok := os.LookupEnv(key)
	if !ok {
		return 0
	}
	s := strings.TrimSpace(raw)
	if strings.Contains(s, "__") || strings.HasPrefix(s, "_") || strings.HasSuffix(s, "_") {
		return 0
	}
	n, err := strconv.Atoi(strings.ReplaceAll(s, "_", ""))
	if err != nil {
		return 0
	}
	return n
}
