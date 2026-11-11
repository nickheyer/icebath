package app

import (
	"strconv"
	"strings"
	"unicode"

	"github.com/nickheyer/icebath/pkg/internal/pystr"
)

// Register names a vi register command accepts
const ptViRegisterNames = "abcdefghijklmnopqrstuvwxyz0123456789"

func (a *ptApplication) viMode() bool    { return a.editingMode == ptEditingVi }
func (a *ptApplication) emacsMode() bool { return a.editingMode == ptEditingEmacs }

// Emacs editing with no selection on a writable buffer
func (a *ptApplication) emacsInsertMode() bool {
	return a.editingMode == ptEditingEmacs && a.buffer.selection == nil && !a.buffer.isReadOnly()
}

// Vi normal mode, temporary normal mode or any read only buffer, with nothing pending
func (a *ptApplication) viNavigationMode() bool {
	s := a.viState
	if a.editingMode != ptEditingVi || s.operatorFunc != nil || s.waitingForDigraph || a.buffer.selection != nil {
		return false
	}
	return s.inputMode == ptViNavigation || s.temporaryNavigationMode || a.buffer.isReadOnly()
}

// Vi input mode equal to mode on a writable buffer with nothing pending
func (a *ptApplication) viInputModeIs(mode ptInputMode) bool {
	s := a.viState
	if a.editingMode != ptEditingVi || s.operatorFunc != nil || s.waitingForDigraph || a.buffer.selection != nil ||
		s.temporaryNavigationMode || a.buffer.isReadOnly() {
		return false
	}
	return s.inputMode == mode
}

func (a *ptApplication) viInsertMode() bool         { return a.viInputModeIs(ptViInsert) }
func (a *ptApplication) viInsertMultipleMode() bool { return a.viInputModeIs(ptViInsertMultiple) }
func (a *ptApplication) viReplaceMode() bool        { return a.viInputModeIs(ptViReplace) }
func (a *ptApplication) viReplaceSingleMode() bool  { return a.viInputModeIs(ptViReplaceSingle) }
func (a *ptApplication) viSelectionMode() bool      { return a.viMode() && a.buffer.selection != nil }
func (a *ptApplication) viDigraphMode() bool        { return a.viMode() && a.viState.waitingForDigraph }
func (a *ptApplication) viRecordingMacro() bool {
	return a.viMode() && a.viState.recordingRegister != nil
}

// Vi with an operator waiting for its text object
func (a *ptApplication) viWaitingForTextObjectMode() bool {
	return a.viMode() && a.viState.operatorFunc != nil
}

// ptTextObjectType is how a motion range treats its ends and lines
type ptTextObjectType int

// Text object kinds
const (
	ptTextExclusive ptTextObjectType = iota
	ptTextInclusive
	ptTextLinewise
	ptTextBlock
)

// ptTextObject is a motion range relative to the cursor
type ptTextObject struct {
	start, end int
	typ        ptTextObjectType
}

// Selection kind the range cuts as
func (t ptTextObject) selectionType() ptSelectionType {
	switch t.typ {
	case ptTextLinewise:
		return ptSelectLines
	case ptTextBlock:
		return ptSelectBlock
	}
	return ptSelectCharacters
}

// Start and end in ascending order
func (t ptTextObject) sorted() (int, int) {
	if t.start < t.end {
		return t.start, t.end
	}
	return t.end, t.start
}

// Half open range an operator acts on, exclusive motions ending at a line start pulled back and lines widened
func (t ptTextObject) operatorRange(doc ptDocument) (int, int) {
	start, end := t.sorted()
	if t.typ == ptTextExclusive {
		if _, col := doc.translateIndexToPosition(end + doc.cursor); col == 0 {
			end--
		}
	}
	if t.typ == ptTextInclusive {
		end++
	}
	if t.typ == ptTextLinewise {
		row, _ := doc.translateIndexToPosition(start + doc.cursor)
		start = doc.translateRowColToIndex(row, 0) - doc.cursor
		row, _ = doc.translateIndexToPosition(end + doc.cursor)
		end = doc.translateRowColToIndex(row, len([]rune(doc.lines()[row]))) - doc.cursor
	}
	return start, end
}

// First and last row the range touches
func (t ptTextObject) getLineNumbers(b *ptBuffer) (int, int) {
	doc := b.document()
	from, to := t.operatorRange(doc)
	from += b.cursor
	to += b.cursor
	fromRow, _ := doc.translateIndexToPosition(from)
	toRow, _ := doc.translateIndexToPosition(to)
	return fromRow, toRow
}

// Document without the range and the clipboard entry holding it
func (t ptTextObject) cut(b *ptBuffer) (ptDocument, ptClipboardData) {
	doc := b.document()
	from, to := t.operatorRange(doc)
	from += b.cursor
	to += b.cursor
	if t.typ != ptTextLinewise {
		to--
	}
	selected := ptDocument{runes: doc.runes, cursor: to, selection: &ptSelection{original: from, typ: t.selectionType()}, vi: doc.vi}
	return selected.cutSelection()
}

// Registers a text object as the operator target, a normal mode motion and a visual mode motion
func ptViTextObject(kb *ptKeyBindings, app *ptApplication, fn func(e *ptEvent) ptTextObject, filter func() bool, noMove, noSelection, eager bool, keys ...string) {
	and := func(conds ...func() bool) func() bool {
		return func() bool {
			for _, c := range conds {
				if c != nil && !c() {
					return false
				}
			}
			return true
		}
	}
	notWaiting := func() bool { return !app.viWaitingForTextObjectMode() }
	kb.add(func(e *ptEvent) {
		s := e.app.viState
		operatorArg := 1
		if s.operatorArg != nil && *s.operatorArg != 0 {
			operatorArg = *s.operatorArg
		}
		eventArg := e.arg()
		if eventArg == 0 {
			eventArg = 1
		}
		merged := strconv.Itoa(operatorArg * eventArg)
		e.argText = &merged
		obj := fn(e)
		if op := s.operatorFunc; op != nil {
			op(e, obj)
		}
		e.app.viState.operatorFunc = nil
		e.app.viState.operatorArg = nil
	}, ptBind{filter: and(app.viWaitingForTextObjectMode, filter), eager: eager}, keys...)
	if !noMove {
		kb.add(func(e *ptEvent) {
			obj := fn(e)
			b := e.currentBuffer()
			b.setCursorPosition(b.cursor + obj.start)
		}, ptBind{filter: and(notWaiting, filter, app.viNavigationMode), eager: eager}, keys...)
	}
	if !noSelection {
		kb.add(func(e *ptEvent) {
			obj := fn(e)
			b := e.currentBuffer()
			sel := b.selection
			if sel == nil {
				return
			}
			if obj.end != 0 {
				start, end := obj.operatorRange(b.document())
				start += b.cursor
				end += b.cursor
				sel.original = start
				b.setCursorPosition(end)
				if obj.typ == ptTextLinewise {
					sel.typ = ptSelectLines
				} else {
					sel.typ = ptSelectCharacters
				}
				return
			}
			b.setCursorPosition(b.cursor + obj.start)
		}, ptBind{filter: and(notWaiting, filter, app.viSelectionMode), eager: eager}, keys...)
	}
}

// Registers an operator that waits for a text object in normal mode and acts on the selection in visual mode
func ptViOperator(kb *ptKeyBindings, app *ptApplication, op func(e *ptEvent, obj ptTextObject), filter func() bool, keys ...string) {
	ok := func(mode func() bool) func() bool {
		return func() bool { return !app.viWaitingForTextObjectMode() && (filter == nil || filter()) && mode() }
	}
	kb.add(func(e *ptEvent) {
		e.app.viState.operatorFunc = op
		arg := e.arg()
		e.app.viState.operatorArg = &arg
	}, ptBind{filter: ok(app.viNavigationMode)}, keys...)
	kb.add(func(e *ptEvent) {
		b := e.currentBuffer()
		sel := b.selection
		if sel == nil {
			return
		}
		typ := ptTextInclusive
		switch sel.typ {
		case ptSelectLines:
			typ = ptTextLinewise
		case ptSelectBlock:
			typ = ptTextBlock
		}
		op(e, ptTextObject{start: sel.original - b.cursor, typ: typ})
		b.selection = nil
		b.changed()
	}, ptBind{filter: ok(app.viSelectionMode)}, keys...)
}

// Swaps the case of every cased letter with full case mapping the way str.swapcase does
func ptSwapCase(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch {
		case unicode.IsUpper(r) || unicode.Is(unicode.Other_Uppercase, r):
			b.WriteString(pystr.Lower(string(r)))
		case unicode.IsLower(r) || unicode.Is(unicode.Other_Lowercase, r):
			b.WriteString(pystr.Upper(string(r)))
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}

// Elements of items between a and b with Python slice semantics for negative and out of range bounds
func ptPySlice(items []string, a, b int) []string {
	n := len(items)
	resolve := func(i int) int {
		if i < 0 {
			i += n
		}
		return min(max(i, 0), n)
	}
	a, b = resolve(a), resolve(b)
	if a >= b {
		return nil
	}
	return items[a:b]
}

// Rotates ASCII letters by thirteen, the rot_13 codec
func ptRot13(s string) string {
	return strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z':
			return 'a' + (r-'a'+13)%26
		case r >= 'A' && r <= 'Z':
			return 'A' + (r-'A'+13)%26
		}
		return r
	}, s)
}

// Flips vi editing on and adds the vi key bindings to the defaults, the EditingMode.VI of display.vim_mode
func ptLoadViBindings(app *ptApplication) {
	app.editingMode = ptEditingVi
	kb := &ptKeyBindings{}
	ptAddViBindings(kb, app)
	ptConditional(kb.bindings, app.viMode)
	app.defaultBindings.bindings = append(app.defaultBindings.bindings, kb.bindings...)
}

// Every vi binding of prompt_toolkit load_vi_bindings
func ptAddViBindings(kb *ptKeyBindings, app *ptApplication) {
	buf := func() *ptBuffer { return app.buffer }
	not := func(f func() bool) func() bool { return func() bool { return !f() } }
	and := func(fs ...func() bool) func() bool {
		return func() bool {
			for _, f := range fs {
				if !f() {
					return false
				}
			}
			return true
		}
	}
	or := func(fs ...func() bool) func() bool {
		return func() bool {
			for _, f := range fs {
				if f() {
					return true
				}
			}
			return false
		}
	}
	isReadOnly := func() bool { return buf().isReadOnly() }
	writable := not(isReadOnly)
	nav := app.viNavigationMode
	navW := and(nav, writable)
	sel := app.viSelectionMode
	hasArg := func() bool { return app.processor.arg != nil }
	isReturnable := func() bool { return buf().acceptHandler != nil }
	isMultiline := func() bool { return buf().multiline }
	inBlockSelection := func() bool { return buf().selection != nil && buf().selection.typ == ptSelectBlock }
	setMode := func(e *ptEvent, mode ptInputMode) { e.app.viState.setInputMode(mode) }
	add := func(h ptHandler, filter func() bool, keys ...string) { kb.add(h, ptBind{filter: filter}, keys...) }

	add(ptNamedCommands["quoted-insert"], app.viInsertMode, "c-v")
	add(func(e *ptEvent) {
		b := e.currentBuffer()
		s := e.app.viState
		if s.inputMode == ptViInsert || s.inputMode == ptViReplace {
			b.setCursorPosition(b.cursor + b.document().cursorLeftPosition(1))
		}
		s.setInputMode(ptViNavigation)
		if b.selection != nil {
			b.exitSelection()
		}
	}, nil, "escape")
	add(func(e *ptEvent) { e.currentBuffer().cursorUp(e.arg()) }, sel, "k")
	add(func(e *ptEvent) { e.currentBuffer().cursorDown(e.arg()) }, sel, "j")
	upNav := func(e *ptEvent) { e.currentBuffer().autoUp(e.arg(), false) }
	add(upNav, nav, "c-p")
	add(upNav, nav, "up")
	add(func(e *ptEvent) { e.currentBuffer().autoUp(e.arg(), true) }, nav, "k")
	downNav := func(e *ptEvent) { e.currentBuffer().autoDown(e.arg(), false) }
	add(downNav, nav, "c-n")
	add(downNav, nav, "down")
	add(func(e *ptEvent) { e.currentBuffer().autoDown(e.arg(), true) }, nav, "j")
	add(func(e *ptEvent) {
		b := e.currentBuffer()
		b.setCursorPosition(b.cursor + b.document().cursorLeftPosition(e.arg()))
	}, nav, "backspace")
	add(func(e *ptEvent) {
		b := e.currentBuffer()
		if b.completeState != nil {
			b.completeNext(1)
		} else {
			b.startCompletion(true, false, false)
		}
	}, app.viInsertMode, "c-n")
	add(func(e *ptEvent) {
		b := e.currentBuffer()
		if b.completeState != nil {
			b.completePrevious(1)
		} else {
			b.startCompletion(false, true, false)
		}
	}, app.viInsertMode, "c-p")
	acceptCompletion := func(e *ptEvent) {
		e.currentBuffer().completeState = nil
		e.currentBuffer().changed()
	}
	add(acceptCompletion, app.viInsertMode, "c-y")
	add(acceptCompletion, app.viInsertMode, "c-g")
	add(func(e *ptEvent) { e.currentBuffer().cancelCompletion() }, app.viInsertMode, "c-e")
	add(ptNamedCommands["accept-line"], and(nav, isReturnable), "enter")
	add(ptNamedCommands["accept-line"], and(isReturnable, not(isMultiline)), "enter")
	add(func(e *ptEvent) {
		b := e.currentBuffer()
		b.cursorDown(e.arg())
		b.setCursorPosition(b.cursor + b.document().startOfLine(true))
	}, and(not(isReturnable), nav), "enter")
	add(func(e *ptEvent) { setMode(e, ptViInsert) }, nav, "insert")
	add(func(e *ptEvent) { setMode(e, ptViNavigation) }, app.viInsertMode, "insert")
	add(func(e *ptEvent) {
		b := e.currentBuffer()
		b.setCursorPosition(b.cursor + b.document().cursorRightPosition(1))
		setMode(e, ptViInsert)
	}, navW, "a")
	add(func(e *ptEvent) {
		b := e.currentBuffer()
		b.setCursorPosition(b.cursor + b.document().endOfLinePosition())
		setMode(e, ptViInsert)
	}, navW, "A")
	add(func(e *ptEvent) {
		b := e.currentBuffer()
		deleted := b.deleteRaising(b.document().endOfLinePosition())
		e.app.clipboard.setText(deleted)
		setMode(e, ptViInsert)
	}, navW, "C")
	changeLine := func(e *ptEvent) {
		b := e.currentBuffer()
		e.app.clipboard.setData(ptClipboardData{text: b.document().currentLine(), typ: ptSelectLines})
		b.setCursorPosition(b.cursor + b.document().startOfLine(true))
		b.deleteRaising(b.document().endOfLinePosition())
		setMode(e, ptViInsert)
	}
	add(changeLine, navW, "S")
	add(changeLine, navW, "c", "c")
	add(func(e *ptEvent) {
		b := e.currentBuffer()
		deleted := b.deleteRaising(b.document().endOfLinePosition())
		e.app.clipboard.setText(deleted)
	}, nav, "D")
	add(func(e *ptEvent) {
		b := e.currentBuffer()
		doc := b.document()
		lines := doc.lines()
		row := doc.cursorRow()
		before := strings.Join(ptPySlice(lines, 0, row), "\n")
		deleted := strings.Join(ptPySlice(lines, row, row+e.arg()), "\n")
		after := strings.Join(ptPySlice(lines, row+e.arg(), len(lines)), "\n")
		if before != "" && after != "" {
			before += "\n"
		}
		cursor := len([]rune(before)) + len([]rune(after)) - len([]rune(strings.TrimLeft(after, " ")))
		b.mustSetDocument(before+after, cursor)
		e.app.clipboard.setData(ptClipboardData{text: deleted, typ: ptSelectLines})
	}, nav, "d", "d")
	add(func(e *ptEvent) { e.app.clipboard.setData(e.currentBuffer().cutSelection()) }, sel, "x")
	add(func(e *ptEvent) { setMode(e, ptViInsert) }, navW, "i")
	add(func(e *ptEvent) {
		setMode(e, ptViInsert)
		b := e.currentBuffer()
		b.setCursorPosition(b.cursor + b.document().startOfLine(true))
	}, navW, "I")
	insertInBlock := func(e *ptEvent, after bool) {
		b := e.currentBuffer()
		var positions []int
		for i, r := range b.document().selectionRanges() {
			pos := r[0]
			if after {
				pos = r[1]
			}
			positions = append(positions, pos)
			if i == 0 {
				b.setCursorPosition(pos)
			}
		}
		b.multipleCursors = positions
		setMode(e, ptViInsertMultiple)
		b.exitSelection()
	}
	add(func(e *ptEvent) { insertInBlock(e, false) }, and(inBlockSelection, writable), "I")
	add(func(e *ptEvent) { insertInBlock(e, true) }, and(inBlockSelection, writable), "A")
	add(func(e *ptEvent) {
		for range max(e.arg(), 0) {
			e.currentBuffer().joinNextLine(" ")
		}
	}, navW, "J")
	add(func(e *ptEvent) {
		for range max(e.arg(), 0) {
			e.currentBuffer().joinNextLine("")
		}
	}, navW, "g", "J")
	add(func(e *ptEvent) { e.currentBuffer().joinSelectedLines(" ") }, and(sel, writable), "J")
	add(func(e *ptEvent) { e.currentBuffer().joinSelectedLines("") }, and(sel, writable), "g", "J")
	add(func(e *ptEvent) {
		e.currentBuffer().pasteClipboardData(e.app.clipboard.getData(), ptPasteViAfter, e.arg())
	}, nav, "p")
	add(func(e *ptEvent) {
		e.currentBuffer().pasteClipboardData(e.app.clipboard.getData(), ptPasteViBefore, e.arg())
	}, nav, "P")
	pasteRegister := func(mode ptPasteMode) ptHandler {
		return func(e *ptEvent) {
			c := e.keySequence[1].Data
			if !strings.Contains(ptViRegisterNames, c) {
				return
			}
			if data, ok := e.app.viState.namedRegisters[c]; ok {
				e.currentBuffer().pasteClipboardData(data, mode, e.arg())
			}
		}
	}
	add(pasteRegister(ptPasteViAfter), nav, "\"", ptKeyAny, "p")
	add(pasteRegister(ptPasteViBefore), nav, "\"", ptKeyAny, "P")
	add(func(e *ptEvent) { setMode(e, ptViReplaceSingle) }, nav, "r")
	add(func(e *ptEvent) { setMode(e, ptViReplace) }, nav, "R")
	add(func(e *ptEvent) {
		b := e.currentBuffer()
		e.app.clipboard.setText(b.deleteRaising(e.arg()))
		setMode(e, ptViInsert)
	}, navW, "s")
	kb.add(func(e *ptEvent) {
		for range max(e.arg(), 0) {
			e.currentBuffer().undoRaising()
		}
	}, ptBind{filter: nav, saveBefore: ptNoSave}, "u")
	add(func(e *ptEvent) { e.currentBuffer().startSelection(ptSelectLines) }, nav, "V")
	add(func(e *ptEvent) { e.currentBuffer().startSelection(ptSelectBlock) }, nav, "c-v")
	toggleSelection := func(typ ptSelectionType) ptHandler {
		return func(e *ptEvent) {
			b := e.currentBuffer()
			if b.selection == nil {
				return
			}
			if b.selection.typ != typ {
				b.selection.typ = typ
				b.changed()
				return
			}
			b.exitSelection()
		}
	}
	add(toggleSelection(ptSelectLines), sel, "V")
	add(func(e *ptEvent) { e.currentBuffer().startSelection(ptSelectCharacters) }, nav, "v")
	add(toggleSelection(ptSelectCharacters), sel, "v")
	add(toggleSelection(ptSelectBlock), sel, "c-v")
	autoWord := func(e *ptEvent) {
		b := e.currentBuffer()
		if b.selection != nil && b.selection.typ == ptSelectLines {
			b.selection.typ = ptSelectCharacters
			b.changed()
		}
	}
	add(autoWord, sel, "a", "W")
	add(autoWord, sel, "a", "w")
	add(func(e *ptEvent) {
		b := e.currentBuffer()
		count := min(e.arg(), len(b.document().currentLineAfterCursor()))
		if count != 0 {
			e.app.clipboard.setText(b.deleteRaising(count))
		}
	}, nav, "x")
	add(func(e *ptEvent) {
		b := e.currentBuffer()
		count := min(e.arg(), len(b.document().currentLineBeforeCursor()))
		if count != 0 {
			e.app.clipboard.setText(b.deleteBeforeCursorRaising(count))
		}
	}, nav, "X")
	yankLine := func(e *ptEvent) {
		lines := e.currentBuffer().document().linesFromCurrent()
		n := e.arg()
		if n < 0 {
			n = max(0, len(lines)+n)
		}
		text := strings.Join(lines[:min(n, len(lines))], "\n")
		e.app.clipboard.setData(ptClipboardData{text: text, typ: ptSelectLines})
	}
	add(yankLine, nav, "Y")
	add(yankLine, nav, "y", "y")
	add(func(e *ptEvent) {
		b := e.currentBuffer()
		b.setCursorPosition(b.cursor + b.document().cursorDownPosition(e.arg(), nil))
		b.setCursorPosition(b.cursor + b.document().startOfLine(true))
	}, nav, "+")
	add(func(e *ptEvent) {
		b := e.currentBuffer()
		b.setCursorPosition(b.cursor + b.document().cursorUpPosition(e.arg(), nil))
		b.setCursorPosition(b.cursor + b.document().startOfLine(true))
	}, nav, "-")
	indentRows := func(e *ptEvent) {
		b := e.currentBuffer()
		row := b.document().cursorRow()
		ptIndentRows(b, row, row+e.arg(), 1)
	}
	add(indentRows, app.viInsertMode, "c-t")
	add(indentRows, nav, ">", ">")
	unindentRows := func(e *ptEvent) {
		b := e.currentBuffer()
		row := b.document().cursorRow()
		ptUnindentRows(b, row, row+e.arg(), 1)
	}
	add(unindentRows, app.viInsertMode, "c-d")
	add(unindentRows, nav, "<", "<")
	add(func(e *ptEvent) {
		e.currentBuffer().insertLineAbove(!e.app.pasteMode)
		setMode(e, ptViInsert)
	}, navW, "O")
	add(func(e *ptEvent) {
		e.currentBuffer().insertLineBelow(!e.app.pasteMode)
		setMode(e, ptViInsert)
	}, navW, "o")
	add(func(e *ptEvent) {
		b := e.currentBuffer()
		doc := b.document()
		c := ""
		if doc.cursor < len(doc.runes) {
			c = string(doc.currentChar())
		}
		if c != "\n" {
			b.requireWritable()
			b.insertText(ptSwapCase(c), true, true, true)
		}
	}, nav, "~")
	add(func(e *ptEvent) { e.currentBuffer().transformCurrentLine(pystr.Lower) }, navW, "g", "u", "u")
	add(func(e *ptEvent) { e.currentBuffer().transformCurrentLine(pystr.Upper) }, navW, "g", "U", "U")
	add(func(e *ptEvent) { e.currentBuffer().transformCurrentLine(ptSwapCase) }, navW, "g", "~", "~")
	occurrence := func(direction ptSearchDirection) ptHandler {
		return func(e *ptEvent) {
			b := e.currentBuffer()
			state := ptSearchState{text: b.document().getWordUnderCursor(false), direction: direction}
			b.applySearch(state, false, e.arg())
		}
	}
	add(occurrence(ptSearchBackward), nav, "#")
	add(occurrence(ptSearchForward), nav, "*")
	add(func(*ptEvent) {}, nav, "(")
	add(func(*ptEvent) {}, nav, ")")
	add(func(e *ptEvent) { e.app.bell() }, app.viWaitingForTextObjectMode, ptKeyAny)

	ptAddViOperators(kb, app, writable)
	ptAddViTextObjects(kb, app, nav, hasArg)
	ptAddViInsertBindings(kb, app, nav, sel, hasArg, or)
}

// Delete, change, yank, transform, indent and reshape operators
func ptAddViOperators(kb *ptKeyBindings, app *ptApplication, writable func() bool) {
	deleteOrChange := func(deleteOnly, withRegister bool) {
		key := "c"
		if deleteOnly {
			key = "d"
		}
		keys := []string{key}
		if withRegister {
			keys = []string{"\"", ptKeyAny, key}
		}
		ptViOperator(kb, app, func(e *ptEvent, obj ptTextObject) {
			b := e.currentBuffer()
			newDocument, data := obj.cut(b)
			b.mustSetDocument(newDocument.text(), newDocument.cursor)
			if data.text != "" {
				if withRegister {
					if reg := e.keySequence[1].Data; strings.Contains(ptViRegisterNames, reg) {
						e.app.viState.namedRegisters[reg] = data
					}
				} else {
					e.app.clipboard.setData(data)
				}
			}
			if !deleteOnly {
				e.app.viState.setInputMode(ptViInsert)
			}
		}, writable, keys...)
	}
	deleteOrChange(false, false)
	deleteOrChange(false, true)
	deleteOrChange(true, false)
	deleteOrChange(true, true)
	transforms := []struct {
		keys   []string
		filter func() bool
		fn     func(string) string
	}{
		{[]string{"g", "?"}, func() bool { return true }, ptRot13},
		{[]string{"g", "u"}, func() bool { return true }, pystr.Lower},
		{[]string{"g", "U"}, func() bool { return true }, pystr.Upper},
		{[]string{"g", "~"}, func() bool { return true }, ptSwapCase},
		{[]string{"~"}, func() bool { return app.viState.tildeOperator }, ptSwapCase},
	}
	for _, t := range transforms {
		filter := t.filter
		fn := t.fn
		ptViOperator(kb, app, func(e *ptEvent, obj ptTextObject) {
			b := e.currentBuffer()
			start, end := obj.operatorRange(b.document())
			if start < end {
				b.transformRegion(b.cursor+start, b.cursor+end, fn)
				move := obj.end
				if move == 0 {
					move = obj.start
				}
				b.setCursorPosition(b.cursor + move)
			}
		}, func() bool { return filter() && writable() }, t.keys...)
	}
	ptViOperator(kb, app, func(e *ptEvent, obj ptTextObject) {
		_, data := obj.cut(e.currentBuffer())
		if data.text != "" {
			e.app.clipboard.setData(data)
		}
	}, nil, "y")
	ptViOperator(kb, app, func(e *ptEvent, obj ptTextObject) {
		c := e.keySequence[1].Data
		if strings.Contains(ptViRegisterNames, c) {
			_, data := obj.cut(e.currentBuffer())
			e.app.viState.namedRegisters[c] = data
		}
	}, nil, "\"", ptKeyAny, "y")
	ptViOperator(kb, app, func(e *ptEvent, obj ptTextObject) {
		b := e.currentBuffer()
		from, to := obj.getLineNumbers(b)
		ptIndentRows(b, from, to+1, e.arg())
	}, nil, ">")
	ptViOperator(kb, app, func(e *ptEvent, obj ptTextObject) {
		b := e.currentBuffer()
		from, to := obj.getLineNumbers(b)
		ptUnindentRows(b, from, to+1, e.arg())
	}, nil, "<")
	ptViOperator(kb, app, func(e *ptEvent, obj ptTextObject) {
		b := e.currentBuffer()
		from, to := obj.getLineNumbers(b)
		ptReshapeText(b, from, to)
	}, nil, "g", "q")
}
