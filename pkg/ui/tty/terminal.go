package tty

import (
	"errors"
	"os"
	"strconv"

	"github.com/charmbracelet/x/term"
)

// Terminal modes saved before the program changed them
type ttyState struct {
	in, out           *os.File
	inState, outState *term.State
	orig              []byte
}

// Restores the saved modes of both ends
func (t *ttyState) restore() error {
	var err error
	if t.inState != nil {
		err = term.Restore(t.in.Fd(), t.inState)
	}
	if t.outState != nil {
		err = errors.Join(err, term.Restore(t.out.Fd(), t.outState))
	}
	return err
}

// Size from an environment variable, the default when it is missing or not a positive number
func envSize(name string, def int) int {
	if n, err := strconv.Atoi(os.Getenv(name)); err == nil && n > 0 {
		return n
	}
	return def
}
