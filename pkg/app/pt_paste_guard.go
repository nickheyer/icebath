package app

import (
	"fmt"
	"io"
	"os"
	"strings"
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
}

// Returns the bytes to hand on, the end mark appended when a paste outlived the timeout
func (g *ptPasteGuard) filter(data []byte) []byte {
	if len(data) == 0 {
		return data
	}
	wasInPaste := g.inPaste
	text := g.tail + string(data)
	followed := false
	for text != "" {
		mark := pasteStartMark
		if g.inPaste {
			mark = pasteEndMark
		}
		idx := strings.Index(text, mark)
		if idx < 0 {
			followed = followed || (g.inPaste && len(text) > 0)
			break
		}
		g.inPaste = !g.inPaste
		if !g.inPaste {
			g.start = nil
		}
		text = text[idx+len(mark):]
	}
	g.tail = text
	if len(g.tail) > len(pasteStartMark)-1 {
		g.tail = g.tail[len(g.tail)-(len(pasteStartMark)-1):]
	}
	if !g.inPaste || !(wasInPaste || followed) {
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
	cliLogger.Warn(fmt.Sprintf("Bracketed-paste timeout (%.1fs) — flushed input without end mark. Terminal may have dropped ESC[201~ (see #16263).", now-*g.start))
	g.inPaste, g.start, g.tail = false, nil, ""
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

// Wraps the input in the paste guard when the timeout patch is installed
func guardedInput(in io.Reader) io.Reader {
	if in == nil || !bracketedPasteTimeoutPatched() {
		return in
	}
	if f, ok := in.(*os.File); ok {
		return &ptGuardedFile{File: f}
	}
	return &ptGuardedReader{r: in}
}
