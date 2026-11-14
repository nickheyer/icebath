package app

import (
	"strings"
	"testing"
)

func TestPasteGuardTimesOnlyPastesThatReceivedContent(t *testing.T) {
	g := &ptPasteGuard{}
	if got := string(g.filter([]byte("ab" + pasteStartMark))); got != "ab"+pasteStartMark || !g.inPaste || g.start != nil {
		t.Fatalf("a paste start without content began the timer, out %q start %v", got, g.start)
	}
	g.filter([]byte("héllo"))
	if g.start == nil || g.pasted != 5 {
		t.Fatalf("content did not begin the timer, start %v pasted %d", g.start, g.pasted)
	}
	g.filter([]byte("x" + pasteEndMark + "y" + pasteStartMark))
	if !g.inPaste || g.start != nil || g.pasted != 0 {
		t.Fatalf("a restarted paste without content kept the old timer, start %v pasted %d", g.start, g.pasted)
	}
	g.filter([]byte("zz"))
	stale := monotonic() - bracketedPasteTimeout - 1
	g.start = &stale
	out := string(g.filter([]byte("\x1b[20")))
	if !strings.HasSuffix(out, pasteEndMark) || g.inPaste || g.pasted != 0 {
		t.Fatalf("a stale paste was not flushed, out %q", out)
	}
}
