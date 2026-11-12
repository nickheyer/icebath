package tui

import (
	"context"
	"regexp"
	"strconv"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/nickheyer/icebath/pkg/ui/cards"
)

const (
	undoLimit        = 200
	multiClickWindow = 500 * time.Millisecond
	hintFallback     = "#808080"
	sgrInverse       = "\x1b[7m"
	sgrInverseOff    = "\x1b[27m"
	sgrDefaultFg     = "\x1b[39m"
)

var (
	bracketPasteRe = regexp.MustCompile("\x1b?\\[20[01]~")
	fwdDeleteRe    = regexp.MustCompile("\x1b\\[3(?:[~$^]|;)")
	hexColorRe     = regexp.MustCompile(`^#[0-9a-fA-F]{6}$`)
)

// Clipboard seams of the composer, the host clipboard in production
var (
	clipboardWrite = writeClipboardText
	clipboardRead  = readClipboardText
)

// Value and caret after an edit, the caret a byte offset into the value
type textInsertResult struct {
	Cursor int
	Value  string
}

// Byte range of a selection, start possibly after end while the caret moves left of the anchor
type textRange struct{ Start, End int }

// Paste handed to the composer, hotkey for a paste chord with no text yet
type pasteRequest struct {
	Bracketed bool
	Cursor    int
	Hotkey    bool
	Text      string
	Value     string
}

// Composer answer to a paste, an immediate result, a deferred one, or neither when unhandled
type pasteOutcome struct {
	Result *textInsertResult
	Later  func() *textInsertResult
}

// Deferred paste result arriving back, applied only while no edit happened since the paste began
type pasteResolvedMsg struct {
	input    *textInput
	version  int
	result   *textInsertResult
	fallback string
}

// Clipboard text read for a paste chord without a composer paste handler
type clipboardPasteMsg struct {
	input *textInput
	text  string
}

// Clipboard write of a cut finished, removing the range when the write succeeded and the selection is unchanged
type cutDoneMsg struct {
	input *textInput
	ok    bool
	rng   textRange
}

// Text of only printable characters, space through tilde and everything from U+00A0 on
func isPrintable(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if !(r >= 0x20 && r <= 0x7e) && r < 0xa0 {
			return false
		}
	}
	return true
}

// Byte offsets of every grapheme boundary of s, starting at 0 and ending at its length
func graphemeStops(s string) []int {
	stops := []int{0}
	at := 0
	for _, g := range cards.Graphemes(s) {
		at += len(g)
		stops = append(stops, at)
	}
	if stops[len(stops)-1] != len(s) {
		stops = append(stops, len(s))
	}
	return stops
}

// Offset moved back onto the grapheme boundary at or before it
func snapPos(s string, p int) int {
	pos := max(0, min(p, len(s)))
	last := 0
	for _, stop := range graphemeStops(s) {
		if stop > pos {
			break
		}
		last = stop
	}
	return last
}

// Boundary of the grapheme before the offset
func prevPos(s string, p int) int {
	pos := snapPos(s, p)
	prev := 0
	for _, stop := range graphemeStops(s) {
		if stop >= pos {
			return prev
		}
		prev = stop
	}
	return prev
}

// Boundary of the grapheme after the offset
func nextPos(s string, p int) int {
	pos := snapPos(s, p)
	for _, stop := range graphemeStops(s) {
		if stop > pos {
			return stop
		}
	}
	return len(s)
}

// Rune index of a byte offset and the byte offsets of every rune start plus the end
func runeOffsets(s string) []int {
	offs := make([]int, 0, len(s)+1)
	for i := range s {
		offs = append(offs, i)
	}
	return append(offs, len(s))
}

// Index into rune offsets of the rune starting at or containing a byte offset
func runeIndexOf(offs []int, p int) int {
	for i, o := range offs {
		if o >= p {
			return i
		}
	}
	return len(offs) - 1
}

// Start of the word left of the offset, skipping whitespace first
func wordLeft(s string, p int) int {
	offs := runeOffsets(s)
	rs := []rune(s)
	i := runeIndexOf(offs, snapPos(s, p)) - 1
	for i > 0 && isJSSpace(rs[i]) {
		i--
	}
	for i > 0 && !isJSSpace(rs[i-1]) {
		i--
	}
	return offs[max(0, i)]
}

// End of the word right of the offset including the whitespace after it
func wordRight(s string, p int) int {
	offs := runeOffsets(s)
	rs := []rune(s)
	i := runeIndexOf(offs, snapPos(s, p))
	for i < len(rs) && !isJSSpace(rs[i]) {
		i++
	}
	for i < len(rs) && isJSSpace(rs[i]) {
		i++
	}
	return offs[i]
}

// Printable text inserted at the caret or over the range, nil when the text holds control characters
func applyPrintableInsert(value string, cursor int, text string, rng *textRange) *textInsertResult {
	if !isPrintable(text) {
		return nil
	}
	if rng != nil {
		return &textInsertResult{Cursor: rng.Start + len(text), Value: value[:rng.Start] + text + value[rng.End:]}
	}
	return &textInsertResult{Cursor: cursor + len(text), Value: value[:cursor] + text + value[cursor:]}
}

// Multi character input that must go through the paste path
func shouldRouteMultiCharInputAsPaste(text string) bool { return strings.Contains(text, "\n") }

// Draft to submit on Return, keeping printable text that rode in on the same keypress before the newline
func valueForReturnSubmit(value string, cursor int, input string, rng *textRange) textInsertResult {
	pending := strings.ReplaceAll(bracketPasteRe.ReplaceAllString(input, ""), "\r\n", "\n")
	pending = strings.ReplaceAll(pending, "\r", "\n")
	if pending == "" {
		return textInsertResult{Cursor: cursor, Value: value}
	}
	before, _, _ := strings.Cut(pending, "\n")
	if before == "" {
		return textInsertResult{Cursor: cursor, Value: value}
	}
	if r := applyPrintableInsert(value, cursor, before, rng); r != nil {
		return *r
	}
	return textInsertResult{Cursor: cursor, Value: value}
}

// Terminals that collapse a modified Enter to a bare LF, so Ctrl+J must insert a newline
func shouldPreserveCtrlJNewline(env func(string) string) bool {
	if env("WT_SESSION") != "" {
		return true
	}
	if env("SSH_CONNECTION") != "" || env("SSH_CLIENT") != "" || env("SSH_TTY") != "" {
		return true
	}
	if env("GHOSTTY_RESOURCES_DIR") != "" || env("GHOSTTY_BIN_DIR") != "" {
		return true
	}
	if strings.ToLower(env("TERM")) == "xterm-ghostty" || strings.ToLower(env("TERM_PROGRAM")) == "ghostty" {
		return true
	}
	return strings.Contains(strings.ToLower(env("WSL_DISTRO_NAME")), "microsoft")
}

// Return inserts a newline instead of submitting, for a modified Enter or a bare LF where it means one
func shouldInsertNewlineOnReturn(k inkKey, sequence string) bool {
	mod := k.Meta
	if isMac {
		mod = isActionMod(k)
	}
	if k.Shift || k.Ctrl || mod {
		return true
	}
	return sequence == "\n" && (isMac || shouldPreserveCtrlJNewline(getenv))
}

// Value with the word right of the caret removed, the caret kept
func deleteWordForward(value string, cursor int) textInsertResult {
	return textInsertResult{Cursor: cursor, Value: value[:cursor] + value[wordRight(value, cursor):]}
}

// Last index of ch at or before position, the position clamped as JavaScript lastIndexOf clamps it
func jsLastIndex(s string, ch byte, position int) int {
	for i := min(max(position, 0), len(s)-1); i >= 0; i-- {
		if s[i] == ch {
			return i
		}
	}
	return -1
}

// First index of ch at or after from, -1 when absent
func indexFrom(s string, ch byte, from int) int {
	if from >= len(s) {
		return -1
	}
	if i := strings.IndexByte(s[max(0, from):], ch); i >= 0 {
		return max(0, from) + i
	}
	return -1
}

// Caret moved one logical line up or down at the same column, false on the first or last line
func lineNav(s string, p, dir int) (int, bool) {
	pos := snapPos(s, p)
	curStart := jsLastIndex(s, '\n', pos-1) + 1
	col := pos - curStart
	if dir < 0 {
		if curStart == 0 {
			return 0, false
		}
		prevStart := jsLastIndex(s, '\n', curStart-2) + 1
		return snapPos(s, min(prevStart+col, curStart-1)), true
	}
	nextBreak := indexFrom(s, '\n', pos)
	if nextBreak < 0 {
		return 0, false
	}
	lineEnd := indexFrom(s, '\n', nextBreak+1)
	if lineEnd < 0 {
		lineEnd = len(s)
	}
	return snapPos(s, min(nextBreak+1+col, lineEnd)), true
}

// Kill back to the start of the current line, eating the newline when already there
func killToLineStart(value string, cursor int) textInsertResult {
	start := jsLastIndex(value, '\n', max(0, cursor-1)) + 1
	from := start
	if start == cursor && cursor > 0 {
		from = start - 1
	}
	return textInsertResult{Cursor: from, Value: value[:from] + value[cursor:]}
}

// Kill forward to the end of the current line, eating the newline when already there
func killToLineEnd(value string, cursor int) textInsertResult {
	to := len(value)
	if nl := indexFrom(value, '\n', cursor); nl >= 0 {
		to = nl
		if nl == cursor {
			to = nl + 1
		}
	}
	return textInsertResult{Cursor: cursor, Value: value[:cursor] + value[to:]}
}

// Backspace or forward delete kills to the line boundary only under the super bit
func isLineKillModifier(k inkKey) bool { return k.Super }

// Right click copies a non empty selection and pastes otherwise
func decideRightClickAction(value string, rng *textRange) (copyText string, isCopy bool) {
	if rng != nil && rng.End > rng.Start {
		start, end := max(0, min(rng.Start, len(value))), max(0, min(rng.End, len(value)))
		if text := value[start:end]; text != "" {
			return text, true
		}
	}
	return "", false
}

// Keys the composer leaves to the global handler
func shouldPassThroughToGlobalHandler(input string, k inkKey, voice voiceRecordKey) bool {
	return (k.Ctrl && input == "c") || (k.Ctrl && input == "x") || (k.Ctrl && input == "o") ||
		(k.Ctrl && (input == "r" || input == "t")) || k.Tab || k.PageUp || k.PageDown || k.Escape ||
		isVoiceToggleKey(k, input, voice)
}

// Composer line editor of the ui-tui text input, value, caret, selection, undo and mouse state
type textInput struct {
	value           string
	cur             int
	sel             *textRange
	undo, redo      []textInsertResult
	editVersion     int
	mouseAnchor     int
	lastClickAt     time.Time
	lastClickOffset int

	columns              int
	mask                 string
	focus                bool
	ignoreVerticalArrows bool
	voiceKey             voiceRecordKey
	hint                 string
	hintColor            string
	accentColor          string
	color                string

	onChange func(string)
	onSubmit func(string)
	onPaste  func(pasteRequest) pasteOutcome
	ctx      context.Context
	now      func() time.Time
}

// Focused empty editor with the default voice chord
func newTextInput(ctx context.Context) *textInput {
	return &textInput{ctx: ctx, columns: 80, focus: true, voiceKey: defaultVoiceRecordKey, mouseAnchor: -1, lastClickOffset: -1, now: time.Now}
}

// Value replaced from outside, the caret at its end and selection and undo history cleared
func (t *textInput) setValue(v string) {
	if v == t.value {
		return
	}
	t.value, t.cur, t.sel, t.undo, t.redo = v, len(v), nil, nil, nil
}

// Normalized non empty selection range
func (t *textInput) selRange() *textRange {
	if t.sel == nil || t.sel.Start == t.sel.End {
		return nil
	}
	return &textRange{Start: min(t.sel.Start, t.sel.End), End: max(t.sel.Start, t.sel.End)}
}

// Applies an edit, recording undo when tracked and notifying the owner of a changed value
func (t *textInput) commit(next string, nextCur int, track bool) {
	prev := t.value
	c := snapPos(next, nextCur)
	t.editVersion++
	t.sel = nil
	if track && next != prev {
		t.undo = append(t.undo, textInsertResult{Cursor: t.cur, Value: prev})
		if len(t.undo) > undoLimit {
			t.undo = t.undo[1:]
		}
		t.redo = nil
	}
	t.cur, t.value = c, next
	if next != prev && t.onChange != nil {
		t.onChange(next)
	}
}

// Moves the newest entry of one history stack onto the other and restores it
func (t *textInput) swap(fromRedo bool) {
	from, to := &t.undo, &t.redo
	if fromRedo {
		from, to = &t.redo, &t.undo
	}
	if len(*from) == 0 {
		return
	}
	entry := (*from)[len(*from)-1]
	*from = (*from)[:len(*from)-1]
	*to = append(*to, textInsertResult{Cursor: t.cur, Value: t.value})
	t.commit(entry.Value, entry.Cursor, false)
}

// Hands a paste to the composer, true when it took it, a deferred result returned as a command
func (t *textInput) emitPaste(e pasteRequest) (bool, tea.Cmd) {
	if t.onPaste == nil {
		return false, nil
	}
	out := t.onPaste(e)
	if out.Later != nil {
		version, fallback, later := t.editVersion, e.Text, out.Later
		return true, func() tea.Msg {
			return pasteResolvedMsg{input: t, version: version, result: later(), fallback: fallback}
		}
	}
	if out.Result != nil {
		t.commit(out.Result.Value, out.Result.Cursor, true)
		return true, nil
	}
	return false, nil
}

// Applies a deferred paste, inserting its plain text instead when the draft changed meanwhile
func (t *textInput) resolvePaste(m pasteResolvedMsg) {
	if m.result == nil {
		return
	}
	if t.editVersion == m.version {
		t.commit(m.result.Value, m.result.Cursor, true)
		return
	}
	if m.fallback != "" && isPrintable(m.fallback) {
		t.commit(t.value[:t.cur]+m.fallback+t.value[t.cur:], t.cur+len(m.fallback), true)
	}
}

// Selects the whole value
func (t *textInput) selectAll() {
	end := len(t.value)
	if end == 0 {
		return
	}
	t.sel = &textRange{Start: 0, End: end}
	t.cur = end
}

// Moves the caret, extending the selection from its anchor when asked
func (t *textInput) moveCursor(next int, extend bool) {
	c := snapPos(t.value, next)
	anchor := t.cur
	if t.sel != nil {
		anchor = t.sel.Start
	}
	if !extend || anchor == c {
		t.sel = nil
	} else {
		t.sel = &textRange{Start: anchor, End: c}
	}
	t.cur = c
}

// Inserts plain clipboard text at the caret or over the selection
func (t *textInput) pastePlainText(text string) {
	cleaned := strings.ReplaceAll(strings.ReplaceAll(text, "\r\n", "\n"), "\r", "\n")
	if cleaned == "" {
		return
	}
	if r := t.selRange(); r != nil {
		t.commit(t.value[:r.Start]+cleaned+t.value[r.End:], r.Start+len(cleaned), true)
		return
	}
	t.commit(t.value[:t.cur]+cleaned+t.value[t.cur:], t.cur+len(cleaned), true)
}

// Command writing text to the clipboard
func (t *textInput) copyCmd(text string) tea.Cmd {
	ctx := t.ctx
	return func() tea.Msg {
		clipboardWrite(ctx, text)
		return nil
	}
}

// Command cutting the selection, the text removed only after the clipboard write succeeds
func (t *textInput) cutCmd() tea.Cmd {
	r := t.selRange()
	if r == nil {
		return nil
	}
	ctx, text, rng := t.ctx, t.value[r.Start:r.End], *r
	return func() tea.Msg { return cutDoneMsg{input: t, ok: clipboardWrite(ctx, text), rng: rng} }
}

// Removes the cut range once the clipboard holds it and the selection still covers it
func (t *textInput) finishCut(m cutDoneMsg) {
	cur := t.selRange()
	if !m.ok || cur == nil || *cur != m.rng {
		return
	}
	t.commit(t.value[:cur.Start]+t.value[cur.End:], cur.Start, true)
}

// Handles the asynchronous results the editor started
func (t *textInput) update(msg tea.Msg) bool {
	switch m := msg.(type) {
	case pasteResolvedMsg:
		if m.input == t {
			t.resolvePaste(m)
			return true
		}
	case clipboardPasteMsg:
		if m.input == t {
			if m.text != "" {
				t.pastePlainText(m.text)
			}
			return true
		}
	case cutDoneMsg:
		if m.input == t {
			t.finishCut(m)
			return true
		}
	}
	return false
}

// Applies one keypress to the draft, returning whether it was consumed and any clipboard or paste work
func (t *textInput) handleKey(ev inputEvent) (bool, tea.Cmd) {
	if !t.focus {
		return false, nil
	}
	k, inp := ev.Key, ev.Input
	if ev.Name == "f7" || shouldPassThroughToGlobalHandler(inp, k, t.voiceKey) {
		return false, nil
	}
	if ev.Raw == "\x1bv" || ev.Raw == "\x1bV" || ev.Raw == "\x16" || (isMac && isActionMod(k) && strings.ToLower(inp) == "v") {
		if t.onPaste != nil {
			_, cmd := t.emitPaste(pasteRequest{Cursor: t.cur, Hotkey: true, Value: t.value})
			return true, cmd
		}
		if isMac {
			ctx := t.ctx
			return true, func() tea.Msg {
				text, _ := clipboardRead(ctx)
				return clipboardPasteMsg{input: t, text: text}
			}
		}
		return true, nil
	}
	if isMac && isActionMod(k) && strings.ToLower(inp) == "c" {
		if r := t.selRange(); r != nil {
			return true, t.copyCmd(t.value[r.Start:r.End])
		}
		return true, nil
	}
	if (k.UpArrow || k.DownArrow) && !t.ignoreVerticalArrows {
		dir := 1
		if k.UpArrow {
			dir = -1
		}
		if next, ok := lineNav(t.value, t.cur, dir); ok {
			t.moveCursor(next, k.Shift)
		}
		return true, nil
	}
	if k.Return {
		pending := valueForReturnSubmit(t.value, t.cur, inp, t.selRange())
		if shouldInsertNewlineOnReturn(k, ev.Sequence) {
			t.commit(pending.Value[:pending.Cursor]+"\n"+pending.Value[pending.Cursor:], pending.Cursor+1, true)
		} else if t.onSubmit != nil {
			t.onSubmit(pending.Value)
		}
		return true, nil
	}
	return t.edit(ev)
}

// Applies an editing or typing keypress, the part of the handler after navigation, Return and clipboard chords
func (t *textInput) edit(ev inputEvent) (bool, tea.Cmd) {
	k, inp := ev.Key, ev.Input
	c, v := t.cur, t.value
	mod := isActionMod(k)
	wordMod := mod || k.Meta
	actionHome := k.Home || (!isMac && mod && inp == "a") || isMacActionFallback(k, inp, "a")
	actionEnd := k.End || (mod && inp == "e") || isMacActionFallback(k, inp, "e")
	actionDeleteToStart := (mod && inp == "u") || isMacActionFallback(k, inp, "u")
	actionKillToEnd := (mod && inp == "k") || isMacActionFallback(k, inp, "k")
	actionDeleteWord := (mod && inp == "w") || isMacActionFallback(k, inp, "w")
	rng := t.selRange()
	delFwd := k.Delete || fwdDeleteRe.MatchString(ev.Raw)
	switch {
	case mod && inp == "z":
		t.swap(false)
		return true, nil
	case (mod && inp == "y") || (mod && k.Shift && strings.ToLower(inp) == "z"):
		t.swap(true)
		return true, nil
	case isMac && mod && inp == "a":
		t.selectAll()
		return true, nil
	case actionHome:
		t.moveCursor(0, k.Shift)
		return true, nil
	case actionEnd:
		t.moveCursor(len(v), k.Shift)
		return true, nil
	case k.LeftArrow:
		if rng != nil && !wordMod && !k.Shift {
			t.sel = nil
			c = rng.Start
		} else if wordMod {
			c = wordLeft(v, c)
		} else {
			c = prevPos(v, c)
		}
		t.moveCursor(c, k.Shift)
		return true, nil
	case k.RightArrow:
		if rng != nil && !wordMod && !k.Shift {
			t.sel = nil
			c = rng.End
		} else if wordMod {
			c = wordRight(v, c)
		} else {
			c = nextPos(v, c)
		}
		t.moveCursor(c, k.Shift)
		return true, nil
	case wordMod && inp == "b":
		t.sel = nil
		c = wordLeft(v, c)
	case wordMod && inp == "f":
		t.sel = nil
		c = wordRight(v, c)
	case wordMod && inp == "d":
		if rng != nil {
			v, c = v[:rng.Start]+v[rng.End:], rng.Start
		} else if c < len(v) {
			t.sel = nil
			r := deleteWordForward(v, c)
			v, c = r.Value, r.Cursor
		} else {
			return true, nil
		}
	case rng != nil && (k.Backspace || delFwd):
		v, c = v[:rng.Start]+v[rng.End:], rng.Start
	case k.Backspace && c > 0:
		switch {
		case isLineKillModifier(k):
			r := killToLineStart(v, c)
			v, c = r.Value, r.Cursor
		case wordMod:
			to := wordLeft(v, c)
			v, c = v[:to]+v[c:], to
		default:
			to := prevPos(v, c)
			v, c = v[:to]+v[c:], to
		}
	case delFwd && c < len(v):
		switch {
		case isLineKillModifier(k):
			r := killToLineEnd(v, c)
			v, c = r.Value, r.Cursor
		case wordMod:
			v = deleteWordForward(v, c).Value
		default:
			v = v[:c] + v[nextPos(v, c):]
		}
	case actionDeleteWord:
		if rng != nil {
			v, c = v[:rng.Start]+v[rng.End:], rng.Start
		} else if c > 0 {
			t.sel = nil
			to := wordLeft(v, c)
			v, c = v[:to]+v[c:], to
		} else {
			return true, nil
		}
	case actionDeleteToStart:
		if rng != nil {
			v, c = v[:rng.Start]+v[rng.End:], rng.Start
		} else {
			r := killToLineStart(v, c)
			v, c = r.Value, r.Cursor
		}
	case actionKillToEnd:
		if rng != nil {
			v, c = v[:rng.Start]+v[rng.End:], rng.Start
		} else {
			r := killToLineEnd(v, c)
			v, c = r.Value, r.Cursor
		}
	case ev.IsPasted || (inp != "" && !ev.IsControlChord):
		return t.typeText(ev, v, c, rng)
	default:
		return false, nil
	}
	t.commit(v, c, true)
	return true, nil
}

// Inserts typed or pasted text, routing bracketed and multi line text through the composer paste handler
func (t *textInput) typeText(ev inputEvent, v string, c int, rng *textRange) (bool, tea.Cmd) {
	bracketed := ev.IsPasted || strings.Contains(ev.Input, "[200~")
	text := strings.ReplaceAll(bracketPasteRe.ReplaceAllString(ev.Input, ""), "\r\n", "\n")
	text = strings.ReplaceAll(text, "\r", "\n")
	if bracketed {
		if took, cmd := t.emitPaste(pasteRequest{Bracketed: true, Cursor: c, Text: text, Value: v}); took {
			return true, cmd
		}
	}
	if text == "" {
		return true, nil
	}
	if text == "\n" {
		t.commit(v[:c]+"\n"+v[c:], c+1, true)
		return true, nil
	}
	if shouldRouteMultiCharInputAsPaste(text) {
		took, cmd := t.emitPaste(pasteRequest{Cursor: c, Text: text, Value: v})
		if !took {
			t.commit(v[:c]+text+v[c:], c+len(text), true)
		}
		return true, cmd
	}
	inserted := applyPrintableInsert(v, c, text, rng)
	if inserted == nil {
		return true, nil
	}
	t.commit(inserted.Value, inserted.Cursor, true)
	return true, nil
}

// Text with every character but newlines replaced by the mask glyph, two glyphs for a character outside the BMP
func maskText(s, mask string) string {
	if mask == "" {
		return s
	}
	glyph := []rune(mask)[0]
	var b strings.Builder
	for _, r := range s {
		switch {
		case r == '\n':
			b.WriteRune('\n')
		case r > 0xffff:
			b.WriteRune(glyph)
			b.WriteRune(glyph)
		default:
			b.WriteRune(glyph)
		}
	}
	return b.String()
}

// Displayed text, masked for secrets
func (t *textInput) display() string { return maskText(t.value, t.mask) }

// Display offset of a value offset, the masked text being shorter or longer than the value
func (t *textInput) displayOffset(p int) int { return len(maskText(t.value[:p], t.mask)) }

// Value offset of a display offset, the inverse of displayOffset on grapheme boundaries
func (t *textInput) valueOffset(d int) int {
	if t.mask == "" {
		return d
	}
	for _, stop := range graphemeStops(t.value) {
		if t.displayOffset(stop) >= d {
			return stop
		}
	}
	return len(t.value)
}

// Value offset under a row and column of the rendered input
func (t *textInput) offsetAt(row, col int) int {
	return t.valueOffset(offsetFromPosition(t.display(), row, col, t.columns))
}

// Caret row and column of the rendered input
func (t *textInput) layout() caretLayout {
	return cursorLayout(t.display(), t.displayOffset(t.cur), t.columns)
}

// Click places the caret and drops the selection
func (t *textInput) click(row, col int) {
	if !t.focus {
		return
	}
	t.sel = nil
	t.cur = t.offsetAt(row, col)
}

// Mouse press, right button copying the selection or pasting, left starting a selection or selecting all on a double click
func (t *textInput) mouseDown(button, row, col int) tea.Cmd {
	if !t.focus {
		return nil
	}
	if button == 2 {
		if text, isCopy := decideRightClickAction(t.value, t.selRange()); isCopy {
			return t.copyCmd(text)
		}
		_, cmd := t.emitPaste(pasteRequest{Cursor: t.cur, Hotkey: true, Value: t.value})
		return cmd
	}
	if button != 0 {
		return nil
	}
	offset := t.offsetAt(row, col)
	now := t.now()
	multi := now.Sub(t.lastClickAt) < multiClickWindow && offset == t.lastClickOffset
	t.lastClickAt, t.lastClickOffset = now, offset
	if multi {
		t.mouseAnchor = -1
		t.selectAll()
		return nil
	}
	t.startMouseSelection(offset)
	return nil
}

// Anchors a mouse selection at an offset
func (t *textInput) startMouseSelection(next int) {
	c := snapPos(t.value, next)
	t.mouseAnchor = c
	t.sel = &textRange{Start: c, End: c}
	t.cur = c
}

// Extends the mouse selection to the cell under a drag
func (t *textInput) mouseDrag(row, col int) {
	if !t.focus || t.mouseAnchor < 0 {
		return
	}
	c := snapPos(t.value, t.offsetAt(row, col))
	t.sel = &textRange{Start: t.mouseAnchor, End: c}
	t.cur = c
}

// Ends a mouse selection, copying it on macOS and dropping an empty one
func (t *textInput) mouseUp() tea.Cmd {
	t.mouseAnchor = -1
	if t.sel != nil && t.sel.Start == t.sel.End {
		t.sel = nil
		return nil
	}
	if r := t.selRange(); isMac && r != nil {
		return t.copyCmd(t.value[r.Start:r.End])
	}
	return nil
}

// SGR foreground open sequence of a hex color, empty for anything else
func fgSeq(hex string) string {
	hex = strings.TrimSpace(hex)
	if !hexColorRe.MatchString(hex) {
		return ""
	}
	n, _ := strconv.ParseUint(hex[1:], 16, 32)
	return "\x1b[38;2;" + strconv.Itoa(int(n>>16&0xff)) + ";" + strconv.Itoa(int(n>>8&0xff)) + ";" + strconv.Itoa(int(n&0xff)) + "m"
}

// SGR background open sequence of a hex color
func bgSeq(hex string) string { return strings.Replace(fgSeq(hex), "[38;", "[48;", 1) }

// Hint color of the empty prompt, the neutral gray for a missing or malformed color
func hintHex(hex string) string {
	if hexColorRe.MatchString(hex) {
		return hex
	}
	return hintFallback
}

// Empty prompt hint text in the hint color
func colorizeHint(s, hex string) string {
	if s == "" {
		return ""
	}
	return fgSeq(hintHex(hex)) + s + sgrDefaultFg
}

// Caret chip over the empty prompt hint, the hint color as background with black or white ink by luminance
func hintCursorCell(ch, hex string) string {
	n, _ := strconv.ParseUint(hintHex(hex)[1:], 16, 32)
	r, g, b := float64(n>>16&0xff), float64(n>>8&0xff), float64(n&0xff)
	ink := "#ffffff"
	if 0.2126*r+0.7152*g+0.0722*b > 140 {
		ink = "#000000"
	}
	return bgSeq(hintHex(hex)) + fgSeq(ink) + ch + sgrDefaultFg + "\x1b[49m"
}

// Display slice with the accent opened around each highlighted run, the mask indexed against the whole text
func paintHighlights(value, accentOpen string, mask []bool, offset int) string {
	if accentOpen == "" || mask == nil {
		return value
	}
	var b strings.Builder
	on, at := false, 0
	for _, g := range cards.Graphemes(value) {
		want := offset+at < len(mask) && mask[offset+at]
		if want != on {
			if want {
				b.WriteString(accentOpen)
			} else {
				b.WriteString(sgrDefaultFg)
			}
			on = want
		}
		b.WriteString(g)
		at += len(g)
	}
	if on {
		b.WriteString(sgrDefaultFg)
	}
	return b.String()
}

// Rendered rows of the input and the caret for the hardware cursor, nil when the caret is drawn inline or hidden
func (t *textInput) render(termFocus bool) ([]string, *caretLayout) {
	display := t.display()
	accentOpen := ""
	if t.mask == "" {
		accentOpen = fgSeq(t.accentColor)
	}
	var mask []bool
	if accentOpen != "" {
		mask = highlightMask(display)
	}
	colorOpen := fgSeq(t.color)
	wrapRows := func(rows []string) []string {
		if colorOpen == "" {
			return rows
		}
		for i, r := range rows {
			rows[i] = colorOpen + strings.ReplaceAll(r, sgrDefaultFg, sgrDefaultFg+colorOpen) + sgrDefaultFg
		}
		return rows
	}
	if !t.focus || (display == "" && t.hint != "") {
		if display != "" {
			return wrapRows(t.paintRows(display, accentOpen, mask, -1, nil)), nil
		}
		rows := strings.Split(wrapHard(t.hint, max(1, t.columns)), "\n")
		for i, r := range rows {
			if i == 0 && t.focus && r != "" {
				first := cards.Graphemes(r)[0]
				rows[i] = hintCursorCell(first, t.hintColor) + colorizeHint(r[len(first):], t.hintColor)
				continue
			}
			rows[i] = colorizeHint(r, t.hintColor)
		}
		return rows, nil
	}
	if r := t.selRange(); r != nil {
		sel := &textRange{Start: t.displayOffset(r.Start), End: t.displayOffset(r.End)}
		return wrapRows(t.paintRows(display, accentOpen, mask, -1, sel)), nil
	}
	if termFocus {
		lay := cursorLayout(display, t.displayOffset(t.cur), t.columns)
		return wrapRows(t.paintRows(display, accentOpen, mask, -1, nil)), &lay
	}
	return wrapRows(t.paintRows(display, accentOpen, mask, t.displayOffset(t.cur), nil)), nil
}

// Display split into its visual rows, painted with highlights, an inverted caret cell or an inverted selection
func (t *textInput) paintRows(display, accentOpen string, mask []bool, caret int, sel *textRange) []string {
	var rows []string
	lines := visualLines(display, max(1, t.columns))
	for li, l := range lines {
		var b strings.Builder
		at := l.start
		for _, g := range cards.Graphemes(display[l.start:l.end]) {
			gEnd := at + len(g)
			switch {
			case at == caret:
				b.WriteString(sgrInverse + g + sgrInverseOff)
			case sel != nil && at >= sel.Start && gEnd <= sel.End:
				b.WriteString(sgrInverse + paintHighlights(g, accentOpen, mask, at) + sgrInverseOff)
			default:
				b.WriteString(paintHighlights(g, accentOpen, mask, at))
			}
			at = gEnd
		}
		row := b.String()
		if caret >= 0 && caret == l.end && (li == len(lines)-1 || lines[li+1].start > l.end) {
			if li == len(lines)-1 && ansi.StringWidth(display[l.start:l.end]) >= max(1, t.columns) {
				rows = append(rows, row)
				row = ""
			}
			row += sgrInverse + " " + sgrInverseOff
		}
		rows = append(rows, row)
	}
	return rows
}
