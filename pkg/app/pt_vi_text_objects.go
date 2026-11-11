package app

import "strings"

// ptRenderInfo is what the last paint of the input window showed, the WindowRenderInfo vi screen motions read
type ptRenderInfo struct {
	displayedLines []int
	windowHeight   int
	windowWidth    int
	lineHeights    []int
}

// Input row shown on the top screen row
func (r *ptRenderInfo) firstVisibleLine() int { return r.displayedLines[0] }

// Input row shown on the bottom screen row
func (r *ptRenderInfo) lastVisibleLine() int { return r.displayedLines[len(r.displayedLines)-1] }

// Input row halfway between the first and last visible rows
func (r *ptRenderInfo) centerVisibleLine() int {
	first := r.firstVisibleLine()
	return first + (r.lastVisibleLine()-first)/2
}

// Screen rows an input line takes when wrapped, one past the content
func (r *ptRenderInfo) heightForLine(line int) int {
	if line < 0 || line >= len(r.lineHeights) {
		return 1
	}
	return r.lineHeights[line]
}

// Every vi text object of load_vi_bindings in its registration order
func ptAddViTextObjects(kb *ptKeyBindings, app *ptApplication, nav, hasArg func() bool) {
	obj := func(fn func(e *ptEvent) ptTextObject, keys ...string) {
		ptViTextObject(kb, app, fn, nil, false, false, false, keys...)
	}
	noMove := func(fn func(e *ptEvent) ptTextObject, keys ...string) {
		ptViTextObject(kb, app, fn, nil, true, false, false, keys...)
	}
	doc := func(e *ptEvent) ptDocument { return e.currentBuffer().document() }
	orZero := func(pos int, ok bool) int {
		if !ok {
			return 0
		}
		return pos
	}
	obj(func(e *ptEvent) ptTextObject {
		return ptTextObject{start: orZero(doc(e).findStartOfPreviousWord(e.arg(), false))}
	}, "b")
	obj(func(e *ptEvent) ptTextObject {
		return ptTextObject{start: orZero(doc(e).findStartOfPreviousWord(e.arg(), true))}
	}, "B")
	obj(func(e *ptEvent) ptTextObject { return ptTextObject{start: doc(e).endOfLinePosition()} }, "$")
	wordForward := func(bigWord bool) func(e *ptEvent) ptTextObject {
		return func(e *ptEvent) ptTextObject {
			d := doc(e)
			if pos, ok := d.findNextWordBeginningKind(e.arg(), bigWord); ok && pos != 0 {
				return ptTextObject{start: pos}
			}
			return ptTextObject{start: len(d.runes) - d.cursor}
		}
	}
	obj(wordForward(false), "w")
	obj(wordForward(true), "W")
	wordEnd := func(bigWord bool) func(e *ptEvent) ptTextObject {
		return func(e *ptEvent) ptTextObject {
			end, ok := doc(e).findNextWordEndingKind(false, e.arg(), bigWord)
			if ok && end != 0 {
				return ptTextObject{start: end - 1, typ: ptTextInclusive}
			}
			return ptTextObject{typ: ptTextInclusive}
		}
	}
	obj(wordEnd(false), "e")
	obj(wordEnd(true), "E")
	boundaries := func(bigWord, trailing bool) func(e *ptEvent) ptTextObject {
		return func(e *ptEvent) ptTextObject {
			start, end := doc(e).findBoundariesOfCurrentWord(bigWord, false, trailing)
			return ptTextObject{start: start, end: end}
		}
	}
	noMove(boundaries(false, false), "i", "w")
	noMove(boundaries(false, true), "a", "w")
	noMove(boundaries(true, false), "i", "W")
	noMove(boundaries(true, true), "a", "W")
	noMove(func(e *ptEvent) ptTextObject {
		d := doc(e)
		return ptTextObject{start: d.startOfParagraph(1, false), end: d.endOfParagraph(e.arg(), false)}
	}, "a", "p")
	obj(func(e *ptEvent) ptTextObject { return ptTextObject{start: doc(e).startOfLine(true)} }, "^")
	obj(func(e *ptEvent) ptTextObject { return ptTextObject{start: doc(e).startOfLine(false)} }, "0")
	ciCa := func(ciStart, ciEnd rune, inner bool, key string) {
		handler := func(e *ptEvent) ptTextObject {
			d := doc(e)
			var start, end int
			var startOK, endOK bool
			if ciStart == ciEnd {
				start, startOK = d.findBackwards(string(ciStart), false, false, 1)
				end, endOK = d.find(string(ciEnd), false, false, false, 1)
			} else {
				start, startOK = d.findEnclosingBracketLeft(ciStart, ciEnd, nil)
				end, endOK = d.findEnclosingBracketRight(ciStart, ciEnd, nil)
			}
			if startOK && endOK {
				offset := 1
				if inner {
					offset = 0
				}
				return ptTextObject{start: start + 1 - offset, end: end + offset}
			}
			return ptTextObject{}
		}
		prefix := "a"
		if inner {
			prefix = "i"
		}
		if key == "" {
			noMove(handler, prefix, string(ciStart))
			noMove(handler, prefix, string(ciEnd))
			return
		}
		noMove(handler, prefix, key)
	}
	for _, inner := range []bool{false, true} {
		for _, pair := range [][2]rune{{'"', '"'}, {'\'', '\''}, {'`', '`'}, {'[', ']'}, {'<', '>'}, {'{', '}'}, {'(', ')'}} {
			ciCa(pair[0], pair[1], inner, "")
		}
		ciCa('(', ')', inner, "b")
		ciCa('{', '}', inner, "B")
	}
	obj(func(e *ptEvent) ptTextObject { return ptTextObject{start: doc(e).startOfParagraph(e.arg(), true)} }, "{")
	obj(func(e *ptEvent) ptTextObject { return ptTextObject{start: doc(e).endOfParagraph(e.arg(), true)} }, "}")
	obj(func(e *ptEvent) ptTextObject {
		e.app.viState.lastCharacterFind = &ptCharacterFind{character: e.data()}
		if match, ok := doc(e).find(e.data(), true, false, false, e.arg()); ok && match != 0 {
			return ptTextObject{start: match, typ: ptTextInclusive}
		}
		return ptTextObject{}
	}, "f", ptKeyAny)
	obj(func(e *ptEvent) ptTextObject {
		e.app.viState.lastCharacterFind = &ptCharacterFind{character: e.data(), backwards: true}
		return ptTextObject{start: orZero(doc(e).findBackwards(e.data(), true, false, e.arg()))}
	}, "F", ptKeyAny)
	obj(func(e *ptEvent) ptTextObject {
		e.app.viState.lastCharacterFind = &ptCharacterFind{character: e.data()}
		if match, ok := doc(e).find(e.data(), true, false, false, e.arg()); ok && match != 0 {
			return ptTextObject{start: match - 1, typ: ptTextInclusive}
		}
		return ptTextObject{}
	}, "t", ptKeyAny)
	obj(func(e *ptEvent) ptTextObject {
		e.app.viState.lastCharacterFind = &ptCharacterFind{character: e.data(), backwards: true}
		if match, ok := doc(e).findBackwards(e.data(), true, false, e.arg()); ok && match != 0 {
			return ptTextObject{start: match + 1}
		}
		return ptTextObject{}
	}, "T", ptKeyAny)
	repeat := func(reverse bool) {
		key := ";"
		if reverse {
			key = ","
		}
		obj(func(e *ptEvent) ptTextObject {
			pos, ok := 0, true
			typ := ptTextExclusive
			if find := e.app.viState.lastCharacterFind; find != nil {
				backwards := find.backwards != reverse
				if backwards {
					pos, ok = doc(e).findBackwards(find.character, true, false, e.arg())
				} else {
					pos, ok = doc(e).find(find.character, true, false, false, e.arg())
					typ = ptTextInclusive
				}
			}
			if ok && pos != 0 {
				return ptTextObject{start: pos, typ: typ}
			}
			return ptTextObject{}
		}, key)
	}
	repeat(true)
	repeat(false)
	left := func(e *ptEvent) ptTextObject { return ptTextObject{start: doc(e).cursorLeftPosition(e.arg())} }
	obj(left, "left")
	obj(left, "h")
	ptViTextObject(kb, app, func(e *ptEvent) ptTextObject {
		return ptTextObject{start: doc(e).cursorDownPosition(e.arg(), nil), typ: ptTextLinewise}
	}, nil, true, true, false, "j")
	ptViTextObject(kb, app, func(e *ptEvent) ptTextObject {
		return ptTextObject{start: doc(e).cursorUpPosition(e.arg(), nil), typ: ptTextLinewise}
	}, nil, true, true, false, "k")
	right := func(e *ptEvent) ptTextObject { return ptTextObject{start: doc(e).cursorRightPosition(e.arg())} }
	obj(right, "right")
	obj(right, " ")
	obj(right, "l")
	screenLine := func(pick func(r *ptRenderInfo) int, fallback func(d ptDocument) int) func(e *ptEvent) ptTextObject {
		return func(e *ptEvent) ptTextObject {
			d := doc(e)
			if info := e.app.inputRenderInfo; info != nil {
				return ptTextObject{start: d.translateRowColToIndex(pick(info), 0) - d.cursor, typ: ptTextLinewise}
			}
			return ptTextObject{start: fallback(d), typ: ptTextLinewise}
		}
	}
	obj(screenLine((*ptRenderInfo).firstVisibleLine, func(d ptDocument) int { return -d.cursor }), "H")
	obj(screenLine((*ptRenderInfo).centerVisibleLine, func(d ptDocument) int { return -d.cursor }), "M")
	obj(screenLine((*ptRenderInfo).lastVisibleLine, func(d ptDocument) int { return len(d.runes) - d.cursor }), "L")
	noMove(func(e *ptEvent) ptTextObject {
		b := e.currentBuffer()
		return ptTextObject{start: b.getSearchPosition(ptSearchState{}, false, e.arg()) - b.cursor}
	}, "n")
	kb.add(func(e *ptEvent) { e.currentBuffer().applySearch(ptSearchState{}, false, e.arg()) }, ptBind{filter: nav}, "n")
	noMove(func(e *ptEvent) ptTextObject {
		b := e.currentBuffer()
		return ptTextObject{start: b.getSearchPosition(ptSearchState{}.inverted(), false, e.arg()) - b.cursor}
	}, "N")
	kb.add(func(e *ptEvent) { e.currentBuffer().applySearch(ptSearchState{}.inverted(), false, e.arg()) }, ptBind{filter: nav}, "N")
	navOrSel := func() bool { return app.viNavigationMode() || app.viSelectionMode() }
	scrollTop := func(e *ptEvent) { e.app.inputScroll = e.currentBuffer().document().cursorRow() }
	for _, key := range []string{"enter", "t", "+"} {
		kb.add(scrollTop, ptBind{filter: navOrSel}, "z", key)
	}
	for _, key := range []string{"b", "-"} {
		kb.add(func(e *ptEvent) { e.app.inputScroll = 0 }, ptBind{filter: navOrSel}, "z", key)
	}
	kb.add(func(e *ptEvent) {
		info := e.app.inputRenderInfo
		if info == nil {
			return
		}
		scrollHeight := info.windowHeight / 2
		y := max(0, e.currentBuffer().document().cursorRow()-1)
		height := 0
		for y > 0 {
			lineHeight := info.heightForLine(y)
			if height+lineHeight >= scrollHeight {
				break
			}
			height += lineHeight
			y--
		}
		e.app.inputScroll = y
	}, ptBind{filter: navOrSel}, "z", "z")
	obj(func(e *ptEvent) ptTextObject {
		d := doc(e)
		if e.argPresent() {
			if n := e.arg(); n > 0 && n <= 100 {
				index := d.translateRowColToIndex((n*d.lineCount()-1)/100, 0)
				return ptTextObject{start: index - d.cursor, typ: ptTextLinewise}
			}
			return ptTextObject{}
		}
		if match := d.findMatchingBracketPosition(nil, nil); match != 0 {
			return ptTextObject{start: match, typ: ptTextInclusive}
		}
		return ptTextObject{}
	}, "%")
	obj(func(e *ptEvent) ptTextObject { return ptTextObject{start: doc(e).getColumnCursorPosition(e.arg() - 1)} }, "|")
	obj(func(e *ptEvent) ptTextObject {
		d := doc(e)
		if e.argPresent() {
			return ptTextObject{start: d.translateRowColToIndex(e.arg()-1, 0) - d.cursor, typ: ptTextLinewise}
		}
		return ptTextObject{start: -d.cursor, typ: ptTextLinewise}
	}, "g", "g")
	obj(func(e *ptEvent) ptTextObject {
		return ptTextObject{start: doc(e).lastNonBlankOfCurrentLinePosition(), typ: ptTextInclusive}
	}, "g", "_")
	previousEnd := func(bigWord bool) func(e *ptEvent) ptTextObject {
		return func(e *ptEvent) ptTextObject {
			if prev, ok := doc(e).findPreviousWordEndingKind(e.arg(), bigWord); ok {
				return ptTextObject{start: prev - 1, typ: ptTextInclusive}
			}
			return ptTextObject{typ: ptTextInclusive}
		}
	}
	obj(previousEnd(false), "g", "e")
	obj(previousEnd(true), "g", "E")
	obj(func(e *ptEvent) ptTextObject {
		info := e.app.inputRenderInfo
		if info == nil {
			return ptTextObject{}
		}
		d := doc(e)
		start := d.startOfLine(false)
		half := float64(info.windowWidth) / 2
		if lineLength := float64(len([]rune(d.currentLine()))); lineLength < half {
			half = lineLength
		}
		return ptTextObject{start: start + int(half), typ: ptTextInclusive}
	}, "g", "m")
	obj(func(e *ptEvent) ptTextObject {
		d := doc(e)
		return ptTextObject{start: d.translateRowColToIndex(d.lineCount()-1, 0) - d.cursor, typ: ptTextLinewise}
	}, "G")
	kb.add(func(e *ptEvent) { e.currentBuffer().goToHistory(e.arg() - 1) }, ptBind{filter: hasArg}, "G")
}

// Repeat counts, replace modes, multiple cursors, line completion, digraphs, temporary normal mode and macros
func ptAddViInsertBindings(kb *ptKeyBindings, app *ptApplication, nav, sel, hasArg func() bool, or func(fs ...func() bool) func() bool) {
	countModes := or(nav, sel, app.viWaitingForTextObjectMode)
	for _, n := range "123456789" {
		kb.add(func(e *ptEvent) { e.appendToArgCount(e.data()) }, ptBind{filter: countModes}, string(n))
	}
	kb.add(func(e *ptEvent) { e.appendToArgCount(e.data()) }, ptBind{filter: func() bool { return countModes() && hasArg() }}, "0")
	kb.add(func(e *ptEvent) {
		b := e.currentBuffer()
		b.requireWritable()
		b.insertText(e.data(), true, true, true)
	}, ptBind{filter: app.viReplaceMode}, ptKeyAny)
	kb.add(func(e *ptEvent) {
		b := e.currentBuffer()
		b.requireWritable()
		b.insertText(e.data(), true, true, true)
		b.setCursorPosition(b.cursor - 1)
		e.app.viState.setInputMode(ptViNavigation)
	}, ptBind{filter: app.viReplaceSingleMode}, ptKeyAny)
	kb.add(func(e *ptEvent) {
		b := e.currentBuffer()
		original := []rune(b.text())
		var text strings.Builder
		p := 0
		for _, p2 := range b.multipleCursors {
			text.WriteString(ptRuneSlice(original, p, p2))
			text.WriteString(e.data())
			p = p2
		}
		text.WriteString(ptRuneSlice(original, p, len(original)))
		positions := make([]int, len(b.multipleCursors))
		for i, pos := range b.multipleCursors {
			positions[i] = pos + i + 1
		}
		b.mustSetText(text.String())
		b.multipleCursors = positions
		b.setCursorPosition(b.cursor + 1)
	}, ptBind{filter: app.viInsertMultipleMode, saveBefore: ptIfNoRepeat}, ptKeyAny)
	kb.add(func(e *ptEvent) {
		b := e.currentBuffer()
		original := []rune(b.text())
		deleted := false
		var parts []string
		p := 0
		for _, p2 := range b.multipleCursors {
			if p2 > 0 && original[p2-1] != '\n' {
				parts = append(parts, ptRuneSlice(original, p, p2-1))
				deleted = true
			} else {
				parts = append(parts, ptRuneSlice(original, p, p2))
			}
			p = p2
		}
		parts = append(parts, ptRuneSlice(original, p, len(original)))
		if !deleted {
			e.app.bell()
			return
		}
		b.mustSetText(strings.Join(parts, ""))
		b.multipleCursors = ptAccumulateLengths(parts[:len(parts)-1])
		b.setCursorPosition(b.cursor - 1)
	}, ptBind{filter: app.viInsertMultipleMode}, "backspace")
	kb.add(func(e *ptEvent) {
		b := e.currentBuffer()
		original := []rune(b.text())
		deleted := false
		var parts []string
		p := 0
		for _, p2 := range b.multipleCursors {
			parts = append(parts, ptRuneSlice(original, p, p2))
			if p2 >= len(original) || original[p2] == '\n' {
				p = p2
			} else {
				p = p2 + 1
				deleted = true
			}
		}
		parts = append(parts, ptRuneSlice(original, p, len(original)))
		if !deleted {
			e.app.bell()
			return
		}
		b.mustSetText(strings.Join(parts, ""))
		b.multipleCursors = ptAccumulateLengths(parts[:len(parts)-1])
	}, ptBind{filter: app.viInsertMultipleMode}, "delete")
	kb.add(func(e *ptEvent) {
		b := e.currentBuffer()
		doc := b.document()
		positions := make([]int, 0, len(b.multipleCursors))
		for _, p := range b.multipleCursors {
			if _, col := doc.translateIndexToPosition(p); col > 0 {
				p--
			}
			positions = append(positions, p)
		}
		b.multipleCursors = positions
		if doc.cursorCol() > 0 {
			b.setCursorPosition(b.cursor - 1)
		}
	}, ptBind{filter: app.viInsertMultipleMode}, "left")
	kb.add(func(e *ptEvent) {
		b := e.currentBuffer()
		doc := b.document()
		lines := doc.lines()
		positions := make([]int, 0, len(b.multipleCursors))
		for _, p := range b.multipleCursors {
			if row, col := doc.translateIndexToPosition(p); col < len([]rune(lines[row])) {
				p++
			}
			positions = append(positions, p)
		}
		b.multipleCursors = positions
		if !doc.isCursorAtTheEndOfLine() {
			b.setCursorPosition(b.cursor + 1)
		}
	}, ptBind{filter: app.viInsertMultipleMode}, "right")
	kb.add(func(*ptEvent) {}, ptBind{filter: app.viInsertMultipleMode}, "down")
	kb.add(func(*ptEvent) {}, ptBind{filter: app.viInsertMultipleMode}, "up")
	kb.add(func(e *ptEvent) { e.currentBuffer().startHistoryLinesCompletion() }, ptBind{filter: app.viInsertMode}, "c-x", "c-l")
	kb.add(func(*ptEvent) {}, ptBind{filter: app.viInsertMode}, "c-x", "c-f")
	kb.add(func(e *ptEvent) { e.app.viState.waitingForDigraph = true }, ptBind{filter: or(app.viInsertMode, app.viReplaceMode)}, "c-k")
	kb.add(func(e *ptEvent) {
		symbol := e.data()
		e.app.viState.digraphSymbol1 = &symbol
	}, ptBind{filter: func() bool { return app.viDigraphMode() && app.viState.digraphSymbol1 == nil }}, ptKeyAny)
	kb.add(func(e *ptEvent) {
		s := e.app.viState
		defer func() {
			s.waitingForDigraph = false
			s.digraphSymbol1 = nil
		}()
		first := ""
		if s.digraphSymbol1 != nil {
			first = *s.digraphSymbol1
		}
		code, ok := ptDigraphs[[2]string{first, e.data()}]
		if !ok {
			code, ok = ptDigraphs[[2]string{e.data(), first}]
		}
		if !ok {
			e.app.bell()
			return
		}
		b := e.currentBuffer()
		b.requireWritable()
		b.insertText(string(code), s.inputMode == ptViReplace, true, true)
		s.waitingForDigraph = false
	}, ptBind{filter: func() bool { return app.viDigraphMode() && app.viState.digraphSymbol1 != nil }}, ptKeyAny)
	kb.add(func(e *ptEvent) { e.app.viState.temporaryNavigationMode = true }, ptBind{filter: or(app.viInsertMode, app.viReplaceMode)}, "c-o")
	kb.add(func(e *ptEvent) {
		c := e.keySequence[1].Data
		if strings.Contains(ptViRegisterNames, c) {
			s := e.app.viState
			s.recordingRegister = &c
			s.currentRecording = ""
		}
	}, ptBind{filter: func() bool { return nav() && !app.viRecordingMacro() }}, "q", ptKeyAny)
	kb.add(func(e *ptEvent) {
		s := e.app.viState
		if s.recordingRegister != nil {
			s.namedRegisters[*s.recordingRegister] = ptClipboardData{text: s.currentRecording}
			s.recordingRegister = nil
			s.currentRecording = ""
		}
	}, ptBind{filter: func() bool { return nav() && app.viRecordingMacro() }}, "q")
	kb.add(func(e *ptEvent) {
		macro, ok := e.app.viState.namedRegisters[e.keySequence[1].Data]
		if !ok {
			return
		}
		var keys []ptKeyPress
		parser := newVT100Parser(func(k ptKeyPress) { keys = append(keys, k) })
		parser.feed(macro.text)
		parser.flush()
		for range max(e.arg(), 0) {
			e.app.processor.feedMultiple(keys, true)
		}
	}, ptBind{filter: nav, skipMacro: true}, "@", ptKeyAny)
}

// Running totals of the rune lengths of parts, the itertools.accumulate of the cursor rebuild
func ptAccumulateLengths(parts []string) []int {
	out := make([]int, len(parts))
	total := 0
	for i, part := range parts {
		total += len([]rune(part))
		out[i] = total
	}
	return out
}
