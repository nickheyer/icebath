package app

import (
	"io"
	"os"
	"sync/atomic"
)

const (
	pasteStartMark = "\x1b[200~"
	pasteEndMark   = "\x1b[201~"
)

// ptPasteGuard ends a bracketed paste whose end mark never arrives once more input follows the timeout
type ptPasteGuard struct {
	inPaste bool
	start   *float64
	tail    string
	pasted  int
}

// ptGuardedReader is a typed byte stream behind the paste guard
type ptGuardedReader struct {
	r io.Reader
	g ptPasteGuard
}

// ptGuardedFile is a terminal file behind the paste guard, still a file so raw mode applies to it
type ptGuardedFile struct {
	*os.File
	g ptPasteGuard
}

// Set when kqueue cannot watch standard input so terminal files are polled with select
var stdinSelectLoop atomic.Bool

// ptSelectFile is a terminal file named as the tty so the input reader polls it with select
type ptSelectFile struct {
	*os.File
	guarded bool
	g       ptPasteGuard
}
