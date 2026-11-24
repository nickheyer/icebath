package termutil

import (
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"

	xterm "github.com/charmbracelet/x/term"
)

var (
	errNoDescriptor = errors.New("stream has no descriptor")
	errNoSize       = errors.New("terminal reports no size")
)

// Columns and lines of the terminal: COLUMNS and LINES when they hold positive whole numbers, else the size of the terminal on stdout, else the fallback
func Size(fallbackCols, fallbackLines int) (cols, lines int) {
	cols, lines = envDimension("COLUMNS"), envDimension("LINES")
	if cols > 0 && lines > 0 {
		return cols, lines
	}
	termCols, termLines, err := SizeOf(os.Stdout)
	if err != nil {
		termCols, termLines = 0, 0
	}
	if cols <= 0 {
		cols = positiveOr(termCols, fallbackCols)
	}
	if lines <= 0 {
		lines = positiveOr(termLines, fallbackLines)
	}
	return cols, lines
}

// Columns and lines of the terminal a stream refers to: a uintptr descriptor or handle, or anything Fd finds a descriptor in, read without changing a file's blocking mode; an error when it is not a terminal or reports no size, COLUMNS and LINES never consulted
func SizeOf(stream any) (cols, lines int, err error) {
	fd, ok := stream.(uintptr)
	if !ok {
		if fd, ok = Fd(stream); !ok {
			return 0, 0, errNoDescriptor
		}
	}
	cols, lines, err = xterm.GetSize(fd)
	switch {
	case err != nil:
		return 0, 0, fmt.Errorf("terminal size: %w", err)
	case cols <= 0:
		return 0, 0, errNoSize
	}
	return cols, lines, nil
}

// Whole number an environment variable holds, zero when it is unset or not a number
func envDimension(key string) int {
	n, err := strconv.Atoi(strings.TrimSpace(os.Getenv(key)))
	if err != nil {
		return 0
	}
	return n
}

func positiveOr(n, fallback int) int {
	if n > 0 {
		return n
	}
	return fallback
}
