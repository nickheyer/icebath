package tty

import (
	"bytes"
	"strings"
	"time"

	"github.com/charmbracelet/x/ansi"
)

// Writes frames as they come, at most once per interval, until done closes
func (r *Renderer) Loop(interval time.Duration, done <-chan struct{}) {
	var last time.Time
	timer := time.NewTimer(time.Hour)
	defer timer.Stop()
	for {
		select {
		case <-done:
			return
		case <-r.wake:
		case <-timer.C:
		}
		now := time.Now()
		if wait := interval - now.Sub(last); wait > 0 {
			timer.Reset(wait)
			continue
		}
		last = now
		next := r.Flush(now)
		if next > 0 {
			timer.Reset(next)
		}
	}
}

// Writes whatever the terminal lacks, returning how long until a probe deadline wants another write, zero for none
func (r *Renderer) Flush(now time.Time) time.Duration {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.held || r.awaitView {
		return 0
	}
	b, next := r.compose(now)
	if len(b) > 0 {
		if _, err := r.out.Write(b); err != nil && r.err == nil {
			r.err = err
		}
	}
	if r.onRestore != nil {
		if rb := r.undo(nil); string(rb) != string(r.restore) {
			r.restore = rb
			r.onRestore(rb)
		}
	}
	return next
}

// First write error, the terminal gone when it is not nil
func (r *Renderer) Err() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.err
}

// Bytes of the next write and the wait before the earliest probe deadline
func (r *Renderer) compose(now time.Time) ([]byte, time.Duration) {
	b := r.flush[:0]
	defer func() { r.flush = b[:0] }()
	v := r.view
	if v == nil || r.w <= 0 || r.h <= 0 {
		if len(r.pass) > 0 && r.entered {
			b = append(b, r.pass...)
			r.foreign(r.pass)
			r.pass = r.pass[:0]
		}
		return b, 0
	}
	m := modesOf(v)
	switch {
	case !r.entered:
		b = r.enter(b, m.alt, now)
		r.shown = nil
	case r.shown != nil && r.shown.alt != m.alt:
		b = append(b, ansi.ResetModifyOtherKeys...)
		b = append(b, ansi.PopKittyKeyboard(1)...)
		if m.alt {
			b = append(b, ansi.SetModeAltScreenSaveCursor...)
		} else {
			b = append(b, ansi.ResetModeAltScreenSaveCursor...)
		}
		r.repaint = true
	}
	b = modeDelta(b, r.shown, &m)
	r.shown = &m
	s := r.scr
	s.out = s.out[:0]
	if r.repaint {
		s.setPen(Style{}, 0)
		s.eraseScreen()
		r.prev = nil
		r.repaint = false
	}
	if len(r.pass) > 0 {
		s.raw(string(r.pass))
		r.foreign(r.pass)
		r.pass = r.pass[:0]
	}
	if !r.parsed {
		r.parse.parse(&r.frame, v.Content, r.w, r.h)
		r.parsed = true
	}
	waiting := r.shaking && now.Sub(r.shakeAt) < handshakeLimit
	if r.shaking && !waiting && !r.answered {
		r.silent = true
	}
	need := r.target(now)
	r.sendProbes(need, now)
	end := cursor{x: 0, y: 0}
	if c := m.cursor; c != nil {
		end = cursor{x: min(max(c.X, 0), r.w-1), y: min(max(c.Y, 0), r.h-1), xok: true, yok: true}
	}
	if !waiting {
		save := end.xok && s.xok && s.yok && s.cx == end.x && s.cy == end.y
		r.pl.paint(s, r.tgt, end, save)
		if end.xok && !save {
			s.moveTo(end.x, end.y)
		}
	}
	body := s.out
	show := m.cursor != nil && !waiting
	if len(body) > 0 || show != r.visibleCur {
		hideFirst := !show && r.visibleCur
		flicker := !r.syncOK && r.visibleCur && show && len(body) > 0
		if r.syncOK && len(body) > 0 {
			b = append(b, ansi.SetModeSynchronizedOutput...)
		}
		if hideFirst || flicker {
			b = append(b, ansi.ResetModeTextCursorEnable...)
		}
		b = append(b, body...)
		if show && (!r.visibleCur || flicker) {
			b = append(b, ansi.SetModeTextCursorEnable...)
		}
		if r.syncOK && len(body) > 0 {
			b = append(b, ansi.ResetModeSynchronizedOutput...)
		}
		r.visibleCur = show
	}
	return b, r.nextDeadline(now, waiting)
}

// Bytes entering the session, the screen chosen by the view, cleared, autowrap off, then the handshake
func (r *Renderer) enter(b []byte, alt bool, now time.Time) []byte {
	if alt {
		b = append(b, ansi.SetModeAltScreenSaveCursor...)
	}
	b = append(b, "\x1b[?7l\x1b[m\x1b[2J"...)
	s := r.scr
	s.forget()
	s.pen, s.link, s.penok = Style{}, 0, true
	for y := 0; y < r.h; y++ {
		s.blankRange(y, 0, r.w)
	}
	b = r.handshake(b, now)
	r.entered, r.repaint, r.prev, r.visibleCur = true, false, nil, true
	return b
}

// Wait before the handshake or a width probe runs out, zero when nothing waits
func (r *Renderer) nextDeadline(now time.Time, waiting bool) time.Duration {
	var next time.Duration
	pick := func(d time.Duration) {
		if d <= 0 {
			d = time.Millisecond
		}
		if next == 0 || d < next {
			next = d
		}
	}
	if waiting {
		pick(handshakeLimit - now.Sub(r.shakeAt))
	}
	limit := r.probeLimit()
	for _, at := range r.widths.pending {
		if left := limit - now.Sub(at); left > 0 {
			pick(left)
		}
	}
	return next
}

// What bytes change, the cursor pen and charset DECRC restores, lasting state, cells, pixels and cursor visibility
type effect struct {
	cursor, sticky, edits, pixels bool
	shown                         int8
}

// Effect of bytes that may change anything
var anything = effect{cursor: true, sticky: true, edits: true}

// Introducer of a tmux passthrough, whose escapes inside are doubled
var tmuxPass = []byte("\x1bPtmux;")

// Applies foreign bytes to the model, forgetting the cells they change and the cursor, pen or charset they move
func (r *Renderer) foreign(p []byte) {
	f := classify(p)
	s := r.scr
	switch {
	case f.edits:
		s.forget()
	case f.cursor || f.sticky:
		s.disturb()
	}
	s.pixels = s.pixels || f.pixels
	if f.shown != 0 {
		r.visibleCur = f.shown > 0
	}
}

// Effect of bytes as a whole, cursor moves inside a balanced DECSC and DECRC pair undone by it
func classify(p []byte) effect {
	var f effect
	depth := 0
	add := func(e effect) {
		f.cursor = f.cursor || e.cursor && depth == 0
		f.sticky = f.sticky || e.sticky
		f.edits = f.edits || e.edits
		f.pixels = f.pixels || e.pixels
		if e.shown != 0 {
			f.shown = e.shown
		}
	}
	for len(p) > 0 {
		if bytes.HasPrefix(p, tmuxPass) {
			inner, n := tmuxInner(p)
			add(classify(inner))
			p = p[n:]
			continue
		}
		seq, _, n, _ := ansi.DecodeSequence(p, 0, nil)
		if n <= 0 {
			add(anything)
			break
		}
		p = p[n:]
		switch {
		case len(seq) == 2 && seq[0] == ansi.ESC && seq[1] == '7':
			depth++
		case len(seq) == 2 && seq[0] == ansi.ESC && seq[1] == '8' && depth > 0:
			depth--
		default:
			add(effectOf(seq))
		}
	}
	f.cursor = f.cursor || depth > 0
	return f
}

// Unescaped bytes a tmux passthrough carries and the length of the passthrough
func tmuxInner(p []byte) ([]byte, int) {
	var inner []byte
	for i := len(tmuxPass); i < len(p); i++ {
		if p[i] != ansi.ESC {
			inner = append(inner, p[i])
			continue
		}
		if i+1 < len(p) && p[i+1] == '\\' {
			return inner, i + 2
		}
		if i+1 < len(p) && p[i+1] == ansi.ESC {
			i++
		}
		inner = append(inner, ansi.ESC)
	}
	return inner, len(p)
}

// Effect of one sequence or text cluster
func effectOf(seq []byte) effect {
	switch {
	case len(seq) == 0:
		return effect{}
	case ansi.HasCsiPrefix(seq):
		return csiEffect(seq)
	case ansi.HasOscPrefix(seq):
		return oscEffect(stringBody(seq))
	case ansi.HasDcsPrefix(seq):
		return dcsEffect(stringBody(seq))
	case ansi.HasApcPrefix(seq):
		return apcEffect(stringBody(seq))
	case ansi.HasPmPrefix(seq), ansi.HasSosPrefix(seq):
		return effect{}
	case seq[0] == ansi.ESC:
		return escEffect(seq)
	case len(seq) == 1 && (seq[0] < 0x20 || seq[0] == ansi.DEL):
		return controlEffect(seq[0])
	case len(seq) == 1 && seq[0] >= 0x80 && seq[0] < 0xa0:
		return anything
	}
	return effect{cursor: true, edits: true}
}

// Data of a string sequence without its introducer and terminator
func stringBody(seq []byte) []byte {
	if seq[0] == ansi.ESC {
		seq = seq[2:]
	} else {
		seq = seq[1:]
	}
	switch n := len(seq); {
	case n >= 2 && seq[n-2] == ansi.ESC && seq[n-1] == '\\':
		return seq[:n-2]
	case n >= 1 && (seq[n-1] == ansi.BEL || seq[n-1] == ansi.ST):
		return seq[:n-1]
	}
	return seq
}

// Effect of a C0 control, line feeds scrolling at the bottom margin
func controlEffect(c byte) effect {
	switch c {
	case ansi.BS, ansi.HT, ansi.CR, ansi.SO, ansi.SI:
		return effect{cursor: true}
	case ansi.LF, ansi.VT, ansi.FF:
		return effect{cursor: true, edits: true}
	}
	return effect{}
}

// Effect of an escape sequence that introduces no string or control sequence
func escEffect(seq []byte) effect {
	if len(seq) < 2 {
		return anything
	}
	switch c := seq[1]; {
	case len(seq) == 2 && (c == '=' || c == '>'):
		return effect{}
	case len(seq) == 2 && (c == '7' || c == '8'):
		return effect{cursor: true}
	case len(seq) == 2 && (c == 'D' || c == 'E' || c == 'M'):
		return effect{cursor: true, edits: true}
	case len(seq) >= 3 && strings.IndexByte("()*+-./", c) >= 0:
		return effect{cursor: true}
	}
	return anything
}

// Private marker, parameters, intermediates and final byte of a control sequence
func splitCSI(seq []byte) (priv byte, params, inter []byte, final byte) {
	b := seq[1:]
	if seq[0] == ansi.ESC {
		b = seq[2:]
	}
	if len(b) > 0 && b[0] >= '<' && b[0] <= '?' {
		priv, b = b[0], b[1:]
	}
	i := 0
	for i < len(b) && b[i] >= 0x30 && b[i] <= 0x3f {
		i++
	}
	params, b = b[:i], b[i:]
	i = 0
	for i < len(b) && b[i] >= 0x20 && b[i] <= 0x2f {
		i++
	}
	inter = b[:i]
	if i < len(b) {
		final = b[i]
	}
	return priv, params, inter, final
}

// Effect of a control sequence, queries and keyboard or cursor shape settings changing nothing on screen
func csiEffect(seq []byte) effect {
	priv, params, inter, final := splitCSI(seq)
	ps, is := string(params), string(inter)
	switch {
	case is == "" && (final == 'n' || final == 'c' || final == 't'):
		return effect{}
	case is == "$" && final == 'p', is == " " && final == 'q', is == "\"" && final == 'q':
		return effect{}
	case priv == '>' && is == "" && (final == 'q' || final == 'm' || final == 'u'):
		return effect{}
	case (priv == '<' || priv == '=') && is == "" && final == 'u':
		return effect{}
	case priv == '?' && is == "" && (final == 'u' || final == 'S' || final == 'm'):
		return effect{}
	case priv == '?' && is == "" && (final == 'h' || final == 'l'):
		return modesEffect(ps, final == 'h')
	case priv != 0 || is != "":
		return anything
	case final == 'm', final == 'u' && ps == "", strings.IndexByte("ABCDEFGHIZadefjk`", final) >= 0:
		return effect{cursor: true}
	case final == 's' && ps == "":
		return effect{}
	case strings.IndexByte("@JKLMPSTXb", final) >= 0:
		return effect{cursor: true, edits: true}
	}
	return anything
}

// Effect of setting or resetting DEC private modes, input reports, output pacing and image placement changing no cell
func modesEffect(ps string, set bool) effect {
	var e effect
	for _, m := range strings.Split(ps, ";") {
		switch m {
		case "9", "12", "80", "1000", "1001", "1002", "1003", "1004", "1005", "1006", "1015", "1016", "1036", "1039", "2004", "2026", "2048", "8452":
		case "25":
			e.shown = -1
			if set {
				e.shown = 1
			}
		default:
			e.cursor, e.sticky, e.edits = true, true, true
		}
	}
	return e
}

// Effect of an operating system command, a link outlasting DECRC, sized text printing and inline images drawing
func oscEffect(body []byte) effect {
	switch {
	case bytes.HasPrefix(body, []byte("8;")):
		return effect{sticky: true}
	case bytes.HasPrefix(body, []byte("66;")):
		return effect{cursor: true, edits: true}
	case bytes.HasPrefix(body, []byte("1337;File=")):
		return effect{cursor: true, pixels: true}
	}
	return effect{}
}

// Effect of a device control string, a sixel image drawing and moving the cursor, status and capability queries none
func dcsEffect(body []byte) effect {
	i := 0
	for i < len(body) && (body[i] >= '0' && body[i] <= '9' || body[i] == ';' || body[i] == ':') {
		i++
	}
	rest := body[i:]
	switch {
	case len(rest) > 0 && rest[0] == 'q':
		return effect{cursor: true, pixels: true}
	case bytes.HasPrefix(rest, []byte("$q")), bytes.HasPrefix(rest, []byte("+q")), bytes.HasPrefix(body, []byte("=1s")), bytes.HasPrefix(body, []byte("=2s")):
		return effect{}
	}
	return anything
}

// Effect of an application program command, a kitty image shown at the cursor moving it unless C=1 or U=1
func apcEffect(body []byte) effect {
	if len(body) == 0 || body[0] != 'G' {
		return effect{}
	}
	keys := body[1:]
	if i := bytes.IndexByte(keys, ';'); i >= 0 {
		keys = keys[:i]
	}
	action, still := byte('t'), false
	for _, kv := range bytes.Split(keys, []byte(",")) {
		switch {
		case len(kv) > 2 && kv[0] == 'a' && kv[1] == '=':
			action = kv[2]
		case string(kv) == "C=1", string(kv) == "U=1":
			still = true
		}
	}
	if (action == 'T' || action == 'p') && !still {
		return effect{cursor: true}
	}
	return effect{}
}

// Fills the target from the frame in the widths the terminal was measured to give, returning the probes still needed
func (r *Renderer) target(now time.Time) []probeKey {
	w, h := r.w, r.h
	if cap(r.tgt) < w*h {
		r.tgt = make([]want, w*h)
	}
	r.tgt = r.tgt[:w*h]
	var need []probeKey
	seen := map[probeKey]bool{}
	ask := func(k probeKey) {
		if !seen[k] && !r.silent {
			seen[k] = true
			if _, sent := r.widths.pending[k]; !sent {
				need = append(need, k)
			}
		}
	}
	for y := 0; y < h; y++ {
		trow := r.tgt[y*w : (y+1)*w]
		if !r.reconcileRow(r.frame.Row(y), trow, ask, now) {
			if r.prev != nil && len(r.prev) == w*h {
				copy(trow, r.prev[y*w:(y+1)*w])
			} else {
				for x := range trow {
					trow[x] = want{Cell: blankCell}
				}
			}
		}
	}
	if cap(r.prev) < w*h {
		r.prev = make([]want, w*h)
	}
	r.prev = r.prev[:w*h]
	copy(r.prev, r.tgt)
	return need
}

// Width of a form, asking for a probe when it is unknown, with ok false while an answer is awaited
func (r *Renderer) widthOf(k probeKey, ask func(probeKey), now time.Time) (n int, ok, late bool) {
	if n, ok := r.widths.of(k.text, k.enc); ok {
		return n, true, false
	}
	if at, sent := r.widths.pending[k]; sent && now.Sub(at) >= r.probeLimit() {
		return 0, false, true
	}
	ask(k)
	return 0, false, false
}

// Builds one target row from a frame row, false when a width it needs is still being measured
func (r *Renderer) reconcileRow(frow []Cell, trow []want, ask func(probeKey), now time.Time) bool {
	w := len(frow)
	ready := true
	for x := range trow {
		trow[x] = want{Cell: blankCell}
	}
	for x := 0; x < w; {
		c := frow[x]
		m := int(c.Width)
		if m == 0 {
			trow[x] = want{Cell: blankCell}
			x++
			continue
		}
		t, ok, late := r.widthOf(probeKey{c.Text, encText}, ask, now)
		if r.silent && !ok {
			t, ok, late = m, true, false
		}
		switch {
		case late:
			put(trow, x, want{Cell: c, blind: true}, m)
		case !ok:
			ready = false
		case t == m:
			put(trow, x, want{Cell: c}, m)
		case r.widths.sized:
			put(trow, x, want{Cell: c, enc: encSized}, m)
		case t < m:
			r.padded(trow, x, c, encText, t, m)
		default:
			n, ok := r.wider(frow, trow, x, c, t, ask, now)
			if !ok {
				ready = false
			}
			x += n
			continue
		}
		x += m
	}
	return ready
}

// Places a target cluster over m columns, its own width first and covered columns after
func put(trow []want, x int, c want, m int) {
	trow[x] = c
	for i := 1; i < m && x+i < len(trow); i++ {
		trow[x+i] = want{Cell: coveredCell}
	}
}

// Places a cluster the terminal draws in t columns where the frame gave m, the rest blank in its style
func (r *Renderer) padded(trow []want, x int, c Cell, e enc, t, m int) {
	c.Width = uint8(t)
	put(trow, x, want{Cell: c, enc: e}, t)
	pad := Cell{Text: " ", Width: 1, Style: c.Style, Link: c.Link}
	for i := t; i < m && x+i < len(trow); i++ {
		trow[x+i] = want{Cell: pad}
	}
}

// Places a cluster drawn wider than its frame width, returning columns taken and false while a form is measured
func (r *Renderer) wider(frow []Cell, trow []want, x int, c Cell, t int, ask func(probeKey), now time.Time) (int, bool) {
	m := int(c.Width)
	if _, ok := decGraphics[c.Text]; ok {
		n, ok, late := r.widthOf(probeKey{c.Text, encDEC}, ask, now)
		if !ok && !late {
			return m, false
		}
		if ok && n >= 1 && n <= m {
			r.padded(trow, x, c, encDEC, n, m)
			return m, true
		}
	}
	if x+t <= len(frow) && absorbable(frow[x+m:x+t], c) {
		c.Width = uint8(t)
		put(trow, x, want{Cell: c}, t)
		return t, true
	}
	for _, k := range fallbacks(c.Text) {
		if k.enc == encDEC {
			continue
		}
		n, ok, late := r.widthOf(k, ask, now)
		if late {
			continue
		}
		if !ok {
			return m, false
		}
		if n >= 1 && n <= m {
			r.padded(trow, x, Cell{Text: k.text, Width: uint8(n), Style: c.Style, Link: c.Link}, encText, n, m)
			return m, true
		}
	}
	r.padded(trow, x, Cell{Text: "?", Width: 1, Style: c.Style, Link: c.Link}, encText, 1, m)
	return m, true
}

// Reports whether frame columns are plain blanks a wider cluster may cover, matching its background and link
func absorbable(cells []Cell, c Cell) bool {
	for i := range cells {
		b := &cells[i]
		if b.Width != 1 || b.Text != " " || b.Rider != "" || b.After != "" || b.Link != c.Link || b.shown().Style != (Style{Bg: c.Style.Bg}) {
			return false
		}
	}
	return true
}

// Writes width probes at the scratch row, each followed by a cursor report request
func (r *Renderer) sendProbes(need []probeKey, now time.Time) {
	if len(need) == 0 {
		return
	}
	s := r.scr
	x, y := r.scratchCol(), r.scratchRow()
	reach := 0
	for _, k := range need {
		s.moveTo(x, y)
		if k.enc == encDEC {
			s.charset(true)
			s.out = append(s.out, decGraphics[k.text])
			s.charset(false)
		} else {
			s.charset(false)
			s.out = append(s.out, k.text...)
		}
		s.out = append(s.out, "\x1b[6n"...)
		s.xok = false
		r.probes = append(r.probes, probe{key: k, x: x, y: y, what: probeWidth, sentAt: now})
		r.widths.pending[k] = now
		reach = max(reach, widthBound(k.text))
	}
	known := s.rowKnown(y)
	for i := x; i < min(x+reach, r.w); i++ {
		known[i] = false
	}
	s.unsplit(y, x, min(x+reach, r.w))
}

// Undoes everything the session set and tells the guard nothing is left to undo, the renderer ready to enter again
func (r *Renderer) Release() {
	r.mu.Lock()
	defer r.mu.Unlock()
	b := append(r.flush[:0], r.pass...)
	r.pass = r.pass[:0]
	b = r.undo(b)
	if len(b) > 0 {
		if _, err := r.out.Write(b); err != nil && r.err == nil {
			r.err = err
		}
	}
	r.flush = b[:0]
	if r.onRestore != nil && len(r.restore) > 0 {
		r.restore = r.restore[:0]
		r.onRestore(nil)
	}
	r.entered, r.shown, r.visibleCur = false, nil, false
	r.scr.forget()
	r.widths.clear()
	r.probes, r.da1 = r.probes[:0], r.da1[:0]
}

// Appends the bytes that undo the session as it stands, nothing before it entered
func (r *Renderer) undo(b []byte) []byte {
	if !r.entered {
		return b
	}
	m := r.shown
	if m == nil {
		m = &modes{alt: r.view != nil && r.view.AltScreen}
	}
	b = append(b, ansi.ResetModifyOtherKeys...)
	b = append(b, ansi.PopKittyKeyboard(1)...)
	b = append(b, "\x1b[m\x1b]8;;\a\x1b(B\x1b[?7h"...)
	b = cup(b, 0, max(r.h-1, 0))
	if m.alt {
		b = append(b, ansi.ResetModeAltScreenSaveCursor...)
	} else {
		b = append(b, ansi.EraseScreenBelow...)
	}
	if !r.visibleCur {
		b = append(b, ansi.SetModeTextCursorEnable...)
	}
	b = modeUndo(b, m)
	if r.unsetGraphemes {
		b = append(b, ansi.ResetModeUnicodeCore...)
	}
	return b
}
