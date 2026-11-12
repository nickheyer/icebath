package app

import (
	"fmt"
	"io"
	"os"
	"strings"
	"sync/atomic"
	"unicode/utf8"
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

// Returns the bytes to hand on, the end mark appended when a paste outlived the timeout
func (g *ptPasteGuard) filter(data []byte) []byte {
	if len(data) == 0 {
		return data
	}
	text := g.tail + string(data)
	continued := g.inPaste
	contentFrom := -1
	pos := 0
	for {
		mark := pasteStartMark
		if g.inPaste {
			mark = pasteEndMark
		}
		idx := strings.Index(text[pos:], mark)
		if idx < 0 {
			break
		}
		continued = false
		g.inPaste = !g.inPaste
		pos += idx + len(mark)
		g.pasted = 0
		if g.inPaste {
			contentFrom = pos
		} else {
			g.start, contentFrom = nil, -1
		}
	}
	g.tail = text[pos:]
	if len(g.tail) > len(pasteStartMark)-1 {
		g.tail = g.tail[len(g.tail)-(len(pasteStartMark)-1):]
	}
	if !g.inPaste {
		return data
	}
	arrived := string(data)
	if contentFrom >= 0 {
		arrived = text[contentFrom:]
	}
	g.pasted += utf8.RuneCountInString(arrived)
	if !continued && arrived == "" {
		return data
	}
	now := monotonic()
	if g.start == nil {
		g.start = &now
		return data
	}
	if now-*g.start <= bracketedPasteTimeout {
		return data
	}
	cliLogger.Warn(fmt.Sprintf("Bracketed-paste timeout (%.1fs) — flushed %d bytes without end mark. Terminal may have dropped ESC[201~ (see #16263).",
		now-*g.start, g.pasted))
	g.inPaste, g.start, g.tail, g.pasted = false, nil, "", 0
	return append(append([]byte{}, data...), pasteEndMark...)
}

// Reads through the guard, room kept for the end mark it may append
func guardedRead(read func([]byte) (int, error), g *ptPasteGuard, p []byte) (int, error) {
	if len(p) <= 2*len(pasteEndMark) {
		return read(p)
	}
	n, err := read(p[:len(p)-len(pasteEndMark)])
	if n > 0 {
		n = copy(p, g.filter(p[:n]))
	}
	return n, err
}

// ptGuardedReader is a typed byte stream behind the paste guard
type ptGuardedReader struct {
	r io.Reader
	g ptPasteGuard
}

func (r *ptGuardedReader) Read(p []byte) (int, error) { return guardedRead(r.r.Read, &r.g, p) }

// ptGuardedFile is a terminal file behind the paste guard, still a file so raw mode applies to it
type ptGuardedFile struct {
	*os.File
	g ptPasteGuard
}

func (f *ptGuardedFile) Read(p []byte) (int, error) { return guardedRead(f.File.Read, &f.g, p) }

// Set when kqueue cannot watch standard input so terminal files are polled with select
var stdinSelectLoop atomic.Bool

// ptSelectFile is a terminal file named as the tty so the input reader polls it with select
type ptSelectFile struct {
	*os.File
	guarded bool
	g       ptPasteGuard
}

func (f *ptSelectFile) Name() string { return "/dev/tty" }

func (f *ptSelectFile) Read(p []byte) (int, error) {
	if !f.guarded {
		return f.File.Read(p)
	}
	return guardedRead(f.File.Read, &f.g, p)
}

// Wraps the input in the paste guard when the timeout patch is installed and in the select poller when kqueue failed
func guardedInput(in io.Reader) io.Reader {
	if in == nil {
		return in
	}
	guarded := bracketedPasteTimeoutPatched()
	f, isFile := in.(*os.File)
	if isFile && stdinSelectLoop.Load() {
		return &ptSelectFile{File: f, guarded: guarded}
	}
	if !guarded {
		return in
	}
	if isFile {
		return &ptGuardedFile{File: f}
	}
	return &ptGuardedReader{r: in}
}
