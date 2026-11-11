package app

import (
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode"
)

// ptHandler runs for a matched key sequence
type ptHandler func(e *ptEvent)

// ptBinding ties a key sequence to a handler under a filter
type ptBinding struct {
	keys          []string
	handler       ptHandler
	filter        func() bool
	eager         bool
	saveBefore    func(e *ptEvent) bool
	recordInMacro bool
}

// ptBind carries the optional parts of a binding
type ptBind struct {
	filter     func() bool
	eager      bool
	saveBefore func(e *ptEvent) bool
	skipMacro  bool
}

// Reports whether the binding is active right now
func (b *ptBinding) active() bool { return b.filter == nil || b.filter() }

// ptKeyBindings is an ordered registry, later bindings winning for the same keys
type ptKeyBindings struct {
	bindings []*ptBinding
}

// Registers a handler for a key sequence, spellings normalized to canonical key names
func (kb *ptKeyBindings) add(h ptHandler, opt ptBind, keys ...string) *ptBinding {
	parsed := make([]string, len(keys))
	for i, k := range keys {
		parsed[i] = ptParseKey(k)
	}
	b := &ptBinding{keys: parsed, handler: h, filter: opt.filter, eager: opt.eager, saveBefore: opt.saveBefore, recordInMacro: !opt.skipMacro}
	kb.bindings = append(kb.bindings, b)
	return b
}

// Bindings whose keys equal the pressed keys, wildcard heavy ones first
func ptBindingsForKeys(all []*ptBinding, keys []string) []*ptBinding {
	type scored struct {
		any int
		b   *ptBinding
	}
	var result []scored
	for _, b := range all {
		if len(keys) != len(b.keys) {
			continue
		}
		match, anyCount := true, 0
		for i, k := range b.keys {
			if k != keys[i] && k != ptKeyAny {
				match = false
				break
			}
			if k == ptKeyAny {
				anyCount++
			}
		}
		if match {
			result = append(result, scored{anyCount, b})
		}
	}
	sort.SliceStable(result, func(i, j int) bool { return result[i].any > result[j].any })
	out := make([]*ptBinding, len(result))
	for i, s := range result {
		out[i] = s.b
	}
	return out
}

// Bindings longer than the pressed keys that start with them
func ptBindingsStartingWith(all []*ptBinding, keys []string) []*ptBinding {
	var out []*ptBinding
	for _, b := range all {
		if len(keys) >= len(b.keys) {
			continue
		}
		match := true
		for i, k := range keys {
			if b.keys[i] != k && b.keys[i] != ptKeyAny {
				match = false
				break
			}
		}
		if match {
			out = append(out, b)
		}
	}
	return out
}

// ptEvent is what a key handler receives
type ptEvent struct {
	app                 *ptApplication
	keySequence         []ptKeyPress
	previousKeySequence []ptKeyPress
	isRepeat            bool
	argText             *string
}

// Typed text of the last key of the sequence
func (e *ptEvent) data() string { return e.keySequence[len(e.keySequence)-1].Data }

// Buffer the key was typed into
func (e *ptEvent) currentBuffer() *ptBuffer { return e.app.buffer }

// Repeat count typed before the key, one when none
func (e *ptEvent) arg() int {
	if e.argText == nil {
		return 1
	}
	if *e.argText == "-" {
		return -1
	}
	n, err := strconv.Atoi(*e.argText)
	if err != nil || n >= 1000000 {
		return 1
	}
	return n
}

// Reports whether a repeat count was typed
func (e *ptEvent) argPresent() bool { return e.argText != nil }

// Adds a digit or minus sign to the pending repeat count
func (e *ptEvent) appendToArgCount(data string) {
	result := data
	if data != "-" && e.argText != nil {
		result = *e.argText + data
	}
	e.app.processor.arg = &result
}

// ptFlushKey marks a timeout flush in the key queue
const ptFlushKey = "<flush>"

// ptKeyProcessor matches queued key presses against the bindings
type ptKeyProcessor struct {
	app                 *ptApplication
	inputQueue          []ptKeyPress
	keyBuffer           []ptKeyPress
	arg                 *string
	previousKeySequence []ptKeyPress
	previousHandler     *ptBinding
	flushTimer          *time.Timer
	flushGen            int
	beforeKeyPress      []func()
	afterKeyPress       []func()
}

// Every binding in priority order, defaults first and application bindings last
func (p *ptKeyProcessor) allBindings() []*ptBinding {
	app := p.app
	out := make([]*ptBinding, 0, len(app.defaultBindings.bindings)+len(app.keyBindings.bindings))
	out = append(out, app.defaultBindings.bindings...)
	return append(out, app.keyBindings.bindings...)
}

// Names of the keys of a press list
func pressKeys(presses []ptKeyPress) []string {
	keys := make([]string, len(presses))
	for i, k := range presses {
		keys[i] = k.Key
	}
	return keys
}

// Active bindings matching the pressed keys exactly
func (p *ptKeyProcessor) matches(presses []ptKeyPress) []*ptBinding {
	var out []*ptBinding
	for _, b := range ptBindingsForKeys(p.allBindings(), pressKeys(presses)) {
		if b.active() {
			out = append(out, b)
		}
	}
	return out
}

// Reports whether an active longer binding starts with the pressed keys
func (p *ptKeyProcessor) isPrefixOfLongerMatch(presses []ptKeyPress) bool {
	for _, b := range ptBindingsStartingWith(p.allBindings(), pressKeys(presses)) {
		if b.active() {
			return true
		}
	}
	return false
}

// Queues a key press, at the front when first is set
func (p *ptKeyProcessor) feed(k ptKeyPress, first bool) {
	if first {
		p.inputQueue = append([]ptKeyPress{k}, p.inputQueue...)
		return
	}
	p.inputQueue = append(p.inputQueue, k)
}

// Queues several key presses in order
func (p *ptKeyProcessor) feedMultiple(keys []ptKeyPress, first bool) {
	if first {
		p.inputQueue = append(append([]ptKeyPress{}, keys...), p.inputQueue...)
		return
	}
	p.inputQueue = append(p.inputQueue, keys...)
}

// Drains the key queue through the bindings, then arms the pending sequence timeout
func (p *ptKeyProcessor) processKeys() {
	isFlush := false
	for len(p.inputQueue) > 0 && !p.app.isDone() {
		k := p.inputQueue[0]
		p.inputQueue = p.inputQueue[1:]
		isFlush = k.Key == ptFlushKey
		if !isFlush {
			for _, fn := range p.beforeKeyPress {
				fn()
			}
		}
		p.step(k)
		if !isFlush {
			for _, fn := range p.afterKeyPress {
				fn()
			}
		}
	}
	if !isFlush {
		p.startTimeout()
	}
}

// Feeds one key into the matching state machine
func (p *ptKeyProcessor) step(k ptKeyPress) {
	flush := k.Key == ptFlushKey
	if !flush {
		p.keyBuffer = append(p.keyBuffer, k)
	}
	for len(p.keyBuffer) > 0 {
		matches := p.matches(p.keyBuffer)
		longer := !flush && p.isPrefixOfLongerMatch(p.keyBuffer)
		var eager []*ptBinding
		for _, m := range matches {
			if m.eager {
				eager = append(eager, m)
			}
		}
		if len(eager) > 0 {
			matches, longer = eager, false
		}
		if longer {
			return
		}
		if len(matches) > 0 {
			seq := append([]ptKeyPress{}, p.keyBuffer...)
			p.keyBuffer = p.keyBuffer[:0]
			p.callHandler(matches[len(matches)-1], seq)
			return
		}
		found := false
		for i := len(p.keyBuffer); i > 0; i-- {
			if m := p.matches(p.keyBuffer[:i]); len(m) > 0 {
				seq := append([]ptKeyPress{}, p.keyBuffer[:i]...)
				p.keyBuffer = append(p.keyBuffer[:0], p.keyBuffer[i:]...)
				p.callHandler(m[len(m)-1], seq)
				found = true
				break
			}
		}
		if !found {
			p.keyBuffer = append(p.keyBuffer[:0], p.keyBuffer[1:]...)
		}
		flush = false
	}
}

// ptEditReadOnly is the EditReadOnlyBuffer a handler raises editing a read only buffer
type ptEditReadOnly struct{}

// Runs a handler with the pending repeat count, saving undo state first when the binding asks
func (p *ptKeyProcessor) callHandler(b *ptBinding, seq []ptKeyPress) {
	app := p.app
	wasRecording := app.emacsMacroRecording != nil
	wasRecordingVi := app.viState.recordingRegister != nil
	wasTemporaryNavigationMode := app.viState.temporaryNavigationMode
	arg := p.arg
	p.arg = nil
	e := &ptEvent{app: app, keySequence: seq, previousKeySequence: p.previousKeySequence, isRepeat: b == p.previousHandler, argText: arg}
	if b.saveBefore == nil || b.saveBefore(e) {
		app.buffer.saveToUndoStack(true)
	}
	p.runHandler(b, e)
	if wasTemporaryNavigationMode {
		p.leaveViTempNavigationMode()
	}
	p.previousKeySequence = seq
	p.previousHandler = b
	if b.recordInMacro {
		if app.emacsMacroRecording != nil && wasRecording {
			*app.emacsMacroRecording = append(*app.emacsMacroRecording, seq...)
		}
		if app.viState.recordingRegister != nil && wasRecordingVi {
			for _, k := range seq {
				app.viState.currentRecording += k.Data
			}
		}
	}
}

// Calls the handler and keeps a navigation mode cursor on the line, a read only edit ringing the bell instead
func (p *ptKeyProcessor) runHandler(b *ptBinding, e *ptEvent) {
	defer func() {
		if r := recover(); r != nil {
			if _, ok := r.(ptEditReadOnly); !ok {
				panic(r)
			}
			p.app.bell()
		}
	}()
	b.handler(e)
	p.fixViCursorPosition()
}

// Pulls the cursor off the end of a non empty line while in vi navigation mode
func (p *ptKeyProcessor) fixViCursorPosition() {
	buff := p.app.buffer
	preferred := buff.preferredColumn
	doc := buff.document()
	if p.app.viNavigationMode() && doc.isCursorAtTheEndOfLine() && len([]rune(doc.currentLine())) > 0 {
		buff.setCursorPosition(buff.cursor - 1)
		buff.preferredColumn = preferred
	}
}

// Returns from temporary navigation mode once no operator or repeat count is pending
func (p *ptKeyProcessor) leaveViTempNavigationMode() {
	if p.app.editingMode == ptEditingVi && p.app.viState.operatorFunc == nil && p.arg == nil {
		p.app.viState.temporaryNavigationMode = false
	}
}

// Arms the timer that flushes a half typed key sequence
func (p *ptKeyProcessor) startTimeout() {
	if p.flushTimer != nil {
		p.flushTimer.Stop()
		p.flushTimer = nil
	}
	p.flushGen++
	if len(p.keyBuffer) == 0 || p.app.timeoutLen <= 0 {
		return
	}
	gen := p.flushGen
	p.flushTimer = p.app.callLater(p.app.timeoutLen, func() {
		if gen == p.flushGen && len(p.keyBuffer) > 0 {
			p.flushKeys()
		}
	})
}

// Decides the pending key sequence as it stands
func (p *ptKeyProcessor) flushKeys() {
	p.feed(ptKeyPress{Key: ptFlushKey}, false)
	p.processKeys()
}

// Forgets pending keys and repeat count
func (p *ptKeyProcessor) reset() {
	p.inputQueue, p.keyBuffer, p.arg = nil, nil, nil
	p.previousKeySequence, p.previousHandler = nil, nil
}

// Readline commands by name, the functions the default bindings share
var ptNamedCommands = map[string]ptHandler{
	"beginning-of-buffer": func(e *ptEvent) { e.currentBuffer().setCursorPosition(0) },
	"end-of-buffer": func(e *ptEvent) {
		b := e.currentBuffer()
		b.setCursorPosition(len([]rune(b.text())))
	},
	"beginning-of-line": func(e *ptEvent) {
		b := e.currentBuffer()
		b.setCursorPosition(b.cursor + b.document().startOfLinePosition())
	},
	"end-of-line": func(e *ptEvent) {
		b := e.currentBuffer()
		b.setCursorPosition(b.cursor + b.document().endOfLinePosition())
	},
	"forward-char": func(e *ptEvent) {
		b := e.currentBuffer()
		b.setCursorPosition(b.cursor + b.document().cursorRightPosition(e.arg()))
	},
	"backward-char": func(e *ptEvent) {
		b := e.currentBuffer()
		b.setCursorPosition(b.cursor + b.document().cursorLeftPosition(e.arg()))
	},
	"forward-word": func(e *ptEvent) {
		b := e.currentBuffer()
		if pos, ok := b.document().findNextWordEnding(e.arg()); ok && pos != 0 {
			b.setCursorPosition(b.cursor + pos)
		}
	},
	"backward-word": func(e *ptEvent) {
		b := e.currentBuffer()
		if pos, ok := b.document().findPreviousWordBeginning(e.arg()); ok && pos != 0 {
			b.setCursorPosition(b.cursor + pos)
		}
	},
	"clear-screen":      func(e *ptEvent) { e.app.clearRenderer() },
	"previous-history":  func(e *ptEvent) { e.currentBuffer().historyBackward(e.arg()) },
	"next-history":      func(e *ptEvent) { e.currentBuffer().historyForward(e.arg()) },
	"accept-line":       func(e *ptEvent) { ptValidateAndHandle(e.currentBuffer()) },
	"quoted-insert":     func(e *ptEvent) { e.app.quotedInsert = true },
	"undo":              func(e *ptEvent) { e.currentBuffer().undo() },
	"start-kbd-macro":   func(e *ptEvent) { e.app.startMacro() },
	"end-kbd-macro":     func(e *ptEvent) { e.app.endMacro() },
	"yank-pop":          ptYankPop,
	"kill-line":         ptKillLine,
	"kill-word":         ptKillWord,
	"unix-line-discard": ptUnixLineDiscard,
	"beginning-of-history": func(e *ptEvent) {
		e.currentBuffer().goToHistory(0)
	},
	"end-of-history": func(e *ptEvent) {
		b := e.currentBuffer()
		b.historyForward(1 << 30)
		b.goToHistory(len(b.workingLines) - 1)
	},
	"delete-char": func(e *ptEvent) {
		if e.currentBuffer().delete(e.arg()) == "" {
			e.app.bell()
		}
	},
	"backward-delete-char": func(e *ptEvent) {
		b := e.currentBuffer()
		var deleted string
		if e.arg() < 0 {
			deleted = b.delete(-e.arg())
		} else {
			deleted = b.deleteBeforeCursor(e.arg())
		}
		if deleted == "" {
			e.app.bell()
		}
	},
	"self-insert": func(e *ptEvent) {
		if data := e.data(); !strings.HasPrefix(data, "\x1b") {
			e.currentBuffer().insertText(strings.Repeat(data, max(e.arg(), 0)), false, true, true)
		}
	},
	"transpose-chars": func(e *ptEvent) {
		b := e.currentBuffer()
		r := []rune(b.text())
		p := b.cursor
		switch {
		case p == 0:
			return
		case p == len(r) || r[p] == '\n':
			b.swapCharactersBeforeCursor()
		default:
			b.setCursorPosition(b.cursor + b.document().cursorRightPosition(1))
			b.swapCharactersBeforeCursor()
		}
	},
	"uppercase-word":  func(e *ptEvent) { ptTransformWord(e, strings.ToUpper) },
	"downcase-word":   func(e *ptEvent) { ptTransformWord(e, strings.ToLower) },
	"capitalize-word": func(e *ptEvent) { ptTransformWord(e, ptTitle) },
	"unix-word-rubout": func(e *ptEvent) {
		ptWordRubout(e, true)
	},
	"backward-kill-word": func(e *ptEvent) {
		ptWordRubout(e, false)
	},
	"delete-horizontal-space": func(e *ptEvent) {
		b := e.currentBuffer()
		doc := b.document()
		before, after := doc.textBeforeCursor(), doc.textAfterCursor()
		delBefore := len([]rune(before)) - len([]rune(strings.TrimRight(before, "\t ")))
		delAfter := len([]rune(after)) - len([]rune(strings.TrimLeft(after, "\t ")))
		b.deleteBeforeCursor(delBefore)
		b.delete(delAfter)
	},
	"yank": func(e *ptEvent) {
		e.currentBuffer().pasteClipboardData(e.app.clipboard.getData(), ptPasteEmacs, e.arg())
	},
	"yank-nth-arg": func(e *ptEvent) {
		e.currentBuffer().yankNthArg(ptArgOrNil(e), false)
	},
	"yank-last-arg": func(e *ptEvent) {
		e.currentBuffer().yankNthArg(ptArgOrNil(e), true)
	},
	"menu-complete": func(e *ptEvent) {
		b := e.currentBuffer()
		if b.completeState != nil {
			b.completeNext(1)
		} else {
			b.startCompletion(false, false, true)
		}
	},
	"menu-complete-backward": func(e *ptEvent) { e.currentBuffer().completePrevious(1) },
	"call-last-kbd-macro": func(e *ptEvent) {
		if len(e.app.emacsMacro) > 0 {
			e.app.processor.feedMultiple(e.app.emacsMacro, true)
		}
	},
	"insert-comment": func(e *ptEvent) {
		b := e.currentBuffer()
		lines := strings.Split(b.text(), "\n")
		for i, line := range lines {
			if e.arg() != 1 {
				lines[i] = strings.TrimPrefix(line, "#")
			} else {
				lines[i] = "#" + line
			}
		}
		_ = b.setDocument(strings.Join(lines, "\n"), 0, false)
		ptValidateAndHandle(b)
	},
	"operate-and-get-next": func(e *ptEvent) { ptValidateAndHandle(e.currentBuffer()) },
}

// Explicit repeat count of the event, nil when none was typed
func ptArgOrNil(e *ptEvent) *int {
	if !e.argPresent() {
		return nil
	}
	n := e.arg()
	return &n
}

// Runs the accept handler, records the text in history and resets unless the handler keeps the text
func ptValidateAndHandle(b *ptBuffer) {
	keepText := false
	if b.acceptHandler != nil {
		keepText = b.acceptHandler(b)
	}
	b.appendToHistory()
	if !keepText {
		b.reset(false)
	}
}

var ptTitleWordRe = regexp.MustCompile(`[A-Za-z]+`)

// Capitalizes each run of letters the way str.title does
func ptTitle(s string) string {
	return ptTitleWordRe.ReplaceAllStringFunc(s, func(w string) string {
		r := []rune(strings.ToLower(w))
		r[0] = unicode.ToUpper(r[0])
		return string(r)
	})
}

// Rewrites the next word through a case function, once per repeat count
func ptTransformWord(e *ptEvent, transform func(string) string) {
	b := e.currentBuffer()
	for i := 0; i < e.arg(); i++ {
		doc := b.document()
		pos, _ := doc.findNextWordEnding(1)
		after := []rune(doc.textAfterCursor())
		b.insertText(transform(string(after[:min(pos, len(after))])), true, true, true)
	}
}

// Kills to the end of the line, or the newline when the cursor sits on one
func ptKillLine(e *ptEvent) {
	b := e.currentBuffer()
	doc := b.document()
	var deleted string
	switch {
	case e.arg() < 0:
		deleted = b.deleteBeforeCursor(-doc.startOfLinePosition())
	case doc.currentChar() == '\n':
		deleted = b.delete(1)
	default:
		deleted = b.delete(doc.endOfLinePosition())
	}
	e.app.clipboard.setText(deleted)
}

// Kills the word after the cursor, repeated kills growing one clipboard entry
func ptKillWord(e *ptEvent) {
	b := e.currentBuffer()
	if pos, ok := b.document().findNextWordEnding(e.arg()); ok && pos != 0 {
		deleted := b.delete(pos)
		if e.isRepeat {
			deleted = e.app.clipboard.text() + deleted
		}
		e.app.clipboard.setText(deleted)
	}
}

// Kills the word before the cursor, repeated kills growing one clipboard entry
func ptWordRubout(e *ptEvent, bigWord bool) {
	b := e.currentBuffer()
	pos, ok := b.document().findStartOfPreviousWord(e.arg(), bigWord)
	if !ok {
		pos = -b.cursor
	}
	if pos == 0 {
		e.app.bell()
		return
	}
	deleted := b.deleteBeforeCursor(-pos)
	if e.isRepeat {
		deleted += e.app.clipboard.text()
	}
	e.app.clipboard.setText(deleted)
}

// Kills to the start of the line, or joins with the previous line at column zero
func ptUnixLineDiscard(e *ptEvent) {
	b := e.currentBuffer()
	doc := b.document()
	if doc.cursorCol() == 0 && doc.cursor > 0 {
		b.deleteBeforeCursor(1)
		return
	}
	e.app.clipboard.setText(b.deleteBeforeCursor(-doc.startOfLinePosition()))
}

// Replaces the text just yanked with the next older kill
func ptYankPop(e *ptEvent) {
	b := e.currentBuffer()
	if before := b.documentBeforePaste; before != nil {
		_ = b.setDocument(before.text(), before.cursor, false)
		e.app.clipboard.rotate()
		b.pasteClipboardData(e.app.clipboard.getData(), ptPasteEmacs, 1)
	}
}

// Skips the undo save for a key held down
func ptIfNoRepeat(e *ptEvent) bool { return !e.isRepeat }

// Never saves undo state before the handler
func ptNoSave(*ptEvent) bool { return false }

// Builds the prompt_toolkit basic, emacs and shift selection bindings active for an emacs mode buffer
func ptLoadDefaultBindings(app *ptApplication) *ptKeyBindings {
	kb := &ptKeyBindings{}
	buf := func() *ptBuffer { return app.buffer }
	hasSelection := func() bool { return buf().selection != nil }
	noSelection := func() bool { return buf().selection == nil }
	emacsInsertMode := app.emacsInsertMode
	insertMode := func() bool { return app.viInsertMode() || app.emacsInsertMode() }
	shiftSelection := func() bool { return buf().selection != nil && buf().selection.shiftMode }
	hasArg := func() bool { return app.processor.arg != nil }
	named := func(name string) ptHandler { return ptNamedCommands[name] }
	ignore := func(*ptEvent) {}

	for _, key := range strings.Fields("c-a c-b c-c c-d c-e c-f c-g c-h c-i c-j c-k c-l c-m c-n c-o c-p c-q c-r c-s c-t c-u c-v c-w c-x c-y c-z " +
		"f1 f2 f3 f4 f5 f6 f7 f8 f9 f10 f11 f12 f13 f14 f15 f16 f17 f18 f19 f20 f21 f22 f23 f24 c-@ c-\\ c-] c-^ c-_ backspace up down right left " +
		"s-up s-down s-right s-left home end s-home s-end delete s-delete c-delete pageup pagedown s-tab tab c-s-left c-s-right c-s-home c-s-end " +
		"c-left c-right c-up c-down c-home c-end insert s-insert c-insert <sigint> <ignore>") {
		kb.add(ignore, ptBind{}, key)
	}
	kb.add(named("beginning-of-line"), ptBind{}, "home")
	kb.add(named("end-of-line"), ptBind{}, "end")
	kb.add(named("backward-char"), ptBind{}, "left")
	kb.add(named("forward-char"), ptBind{}, "right")
	kb.add(named("previous-history"), ptBind{}, "c-up")
	kb.add(named("next-history"), ptBind{}, "c-down")
	kb.add(named("clear-screen"), ptBind{}, "c-l")
	kb.add(named("kill-line"), ptBind{filter: insertMode}, "c-k")
	kb.add(named("unix-line-discard"), ptBind{filter: insertMode}, "c-u")
	kb.add(named("backward-delete-char"), ptBind{filter: insertMode, saveBefore: ptIfNoRepeat}, "backspace")
	kb.add(named("delete-char"), ptBind{filter: insertMode, saveBefore: ptIfNoRepeat}, "delete")
	kb.add(named("delete-char"), ptBind{filter: insertMode, saveBefore: ptIfNoRepeat}, "c-delete")
	kb.add(named("self-insert"), ptBind{filter: insertMode, saveBefore: ptIfNoRepeat}, ptKeyAny)
	kb.add(named("transpose-chars"), ptBind{filter: insertMode}, "c-t")
	kb.add(named("menu-complete"), ptBind{filter: insertMode}, "c-i")
	kb.add(named("menu-complete-backward"), ptBind{filter: insertMode}, "s-tab")
	kb.add(named("unix-word-rubout"), ptBind{filter: insertMode}, "c-w")
	kb.add(named("previous-history"), ptBind{filter: noSelection}, "pageup")
	kb.add(named("next-history"), ptBind{filter: noSelection}, "pagedown")
	kb.add(named("delete-char"), ptBind{filter: func() bool { return buf().text() != "" && insertMode() }}, "c-d")
	kb.add(func(e *ptEvent) { e.currentBuffer().newline(true) }, ptBind{filter: insertMode}, "enter")
	kb.add(func(e *ptEvent) { e.app.processor.feed(ptKeyPress{Key: ptKeyEnter, Data: "\r"}, true) }, ptBind{}, "c-j")
	kb.add(func(e *ptEvent) { e.currentBuffer().autoMoveUp(e.arg()) }, ptBind{}, "up")
	kb.add(func(e *ptEvent) { e.currentBuffer().autoMoveDown(e.arg()) }, ptBind{}, "down")
	kb.add(func(e *ptEvent) { e.app.clipboard.setData(e.currentBuffer().cutSelection()) }, ptBind{filter: hasSelection}, "delete")
	kb.add(func(e *ptEvent) { e.currentBuffer().insertText(e.data(), false, true, true) }, ptBind{}, "c-z")
	kb.add(func(e *ptEvent) {
		data := strings.ReplaceAll(strings.ReplaceAll(e.data(), "\r\n", "\n"), "\r", "\n")
		e.currentBuffer().insertText(data, false, true, true)
	}, ptBind{}, ptKeyBracketedPaste)
	kb.add(func(e *ptEvent) {
		e.currentBuffer().insertText(e.data(), false, true, true)
		e.app.quotedInsert = false
	}, ptBind{filter: func() bool { return app.quotedInsert }, eager: true}, ptKeyAny)

	emacsStart := len(kb.bindings)
	kb.add(ignore, ptBind{}, "escape")
	kb.add(named("beginning-of-line"), ptBind{}, "c-a")
	kb.add(named("backward-char"), ptBind{}, "c-b")
	kb.add(named("kill-word"), ptBind{filter: emacsInsertMode}, "c-delete")
	kb.add(named("end-of-line"), ptBind{}, "c-e")
	kb.add(named("forward-char"), ptBind{}, "c-f")
	kb.add(named("backward-word"), ptBind{}, "c-left")
	kb.add(named("forward-word"), ptBind{}, "c-right")
	kb.add(named("yank"), ptBind{filter: emacsInsertMode}, "c-x", "r", "y")
	kb.add(named("yank"), ptBind{filter: emacsInsertMode}, "c-y")
	kb.add(named("backward-word"), ptBind{}, "escape", "b")
	kb.add(named("capitalize-word"), ptBind{filter: emacsInsertMode}, "escape", "c")
	kb.add(named("kill-word"), ptBind{filter: emacsInsertMode}, "escape", "d")
	kb.add(named("forward-word"), ptBind{}, "escape", "f")
	kb.add(named("downcase-word"), ptBind{filter: emacsInsertMode}, "escape", "l")
	kb.add(named("uppercase-word"), ptBind{filter: emacsInsertMode}, "escape", "u")
	kb.add(named("yank-pop"), ptBind{filter: emacsInsertMode}, "escape", "y")
	kb.add(named("backward-kill-word"), ptBind{filter: emacsInsertMode}, "escape", "backspace")
	kb.add(named("delete-horizontal-space"), ptBind{filter: emacsInsertMode}, "escape", "\\")
	kb.add(named("beginning-of-buffer"), ptBind{}, "c-home")
	kb.add(named("end-of-buffer"), ptBind{}, "c-end")
	kb.add(named("undo"), ptBind{filter: emacsInsertMode, saveBefore: ptNoSave}, "c-_")
	kb.add(named("undo"), ptBind{filter: emacsInsertMode, saveBefore: ptNoSave}, "c-x", "c-u")
	kb.add(named("beginning-of-history"), ptBind{filter: noSelection}, "escape", "<")
	kb.add(named("end-of-history"), ptBind{filter: noSelection}, "escape", ">")
	kb.add(named("yank-last-arg"), ptBind{filter: emacsInsertMode}, "escape", ".")
	kb.add(named("yank-last-arg"), ptBind{filter: emacsInsertMode}, "escape", "_")
	kb.add(named("yank-nth-arg"), ptBind{filter: emacsInsertMode}, "escape", "c-y")
	kb.add(named("insert-comment"), ptBind{filter: emacsInsertMode}, "escape", "#")
	kb.add(named("operate-and-get-next"), ptBind{}, "c-o")
	kb.add(named("quoted-insert"), ptBind{filter: noSelection}, "c-q")
	kb.add(named("start-kbd-macro"), ptBind{}, "c-x", "(")
	kb.add(named("end-kbd-macro"), ptBind{}, "c-x", ")")
	kb.add(named("call-last-kbd-macro"), ptBind{skipMacro: true}, "c-x", "e")
	kb.add(func(e *ptEvent) { e.currentBuffer().autoMoveDown(1) }, ptBind{}, "c-n")
	kb.add(func(e *ptEvent) { e.currentBuffer().autoMoveUp(e.arg()) }, ptBind{}, "c-p")
	for _, c := range "0123456789" {
		digit := string(c)
		kb.add(func(e *ptEvent) { e.appendToArgCount(digit) }, ptBind{}, "escape", digit)
		kb.add(func(e *ptEvent) { e.appendToArgCount(digit) }, ptBind{filter: hasArg}, digit)
	}
	kb.add(func(e *ptEvent) {
		if e.argText == nil {
			e.appendToArgCount("-")
		}
	}, ptBind{filter: func() bool { return !hasArg() }}, "escape", "-")
	kb.add(func(e *ptEvent) {
		minus := "-"
		e.app.processor.arg = &minus
	}, ptBind{filter: func() bool { return app.processor.arg != nil && *app.processor.arg == "-" }}, "-")
	search := func(e *ptEvent, count int) {
		b := e.currentBuffer()
		pos, ok := b.document().findChar(e.data(), max(count, -count), count < 0)
		if ok {
			b.setCursorPosition(b.cursor + pos)
		}
	}
	kb.add(func(e *ptEvent) { search(e, e.arg()) }, ptBind{}, "c-]", ptKeyAny)
	kb.add(func(e *ptEvent) { search(e, -e.arg()) }, ptBind{}, "escape", "c-]", ptKeyAny)
	kb.add(ignore, ptBind{}, "escape", "a")
	kb.add(ignore, ptBind{}, "escape", "e")
	kb.add(ignore, ptBind{filter: emacsInsertMode}, "escape", "t")
	kb.add(func(e *ptEvent) {
		b := e.currentBuffer()
		if b.completer == nil {
			return
		}
		var texts []string
		for _, c := range b.completer(b.document().textBeforeCursor()) {
			texts = append(texts, c.Text)
		}
		b.insertText(strings.Join(texts, " "), false, true, true)
	}, ptBind{filter: emacsInsertMode}, "escape", "*")
	kb.add(func(e *ptEvent) {
		b := e.currentBuffer()
		doc := b.document()
		if doc.endOfLinePosition() == 0 {
			b.setCursorPosition(b.cursor + doc.startOfLinePosition())
		} else {
			b.setCursorPosition(b.cursor + doc.endOfLinePosition())
		}
	}, ptBind{}, "c-x", "c-x")
	kb.add(func(e *ptEvent) {
		if b := e.currentBuffer(); b.text() != "" {
			b.startSelection(ptSelectCharacters)
		}
	}, ptBind{}, "c-@")
	kb.add(func(e *ptEvent) {
		e.currentBuffer().completeState = nil
		e.currentBuffer().changed()
	}, ptBind{filter: noSelection}, "c-g")
	kb.add(func(e *ptEvent) { e.currentBuffer().exitSelection() }, ptBind{filter: hasSelection}, "c-g")
	cut := func(e *ptEvent) { e.app.clipboard.setData(e.currentBuffer().cutSelection()) }
	kb.add(cut, ptBind{filter: hasSelection}, "c-x", "r", "k")
	kb.add(cut, ptBind{filter: hasSelection}, "c-w")
	kb.add(func(e *ptEvent) { e.app.clipboard.setData(e.currentBuffer().copySelection(false)) }, ptBind{filter: hasSelection}, "escape", "w")
	kb.add(func(e *ptEvent) {
		b := e.currentBuffer()
		pos, _ := b.document().findPreviousWordBeginning(e.arg())
		b.setCursorPosition(b.cursor + pos)
	}, ptBind{}, "escape", "left")
	kb.add(func(e *ptEvent) {
		b := e.currentBuffer()
		pos, ok := b.document().findNextWordBeginning(e.arg())
		if !ok || pos == 0 {
			pos = len([]rune(b.text())) - b.cursor
		}
		b.setCursorPosition(b.cursor + pos)
	}, ptBind{}, "escape", "right")
	kb.add(func(e *ptEvent) {
		b := e.currentBuffer()
		if b.completeState != nil {
			b.completeNext(1)
		} else {
			b.startCompletion(true, false, false)
		}
	}, ptBind{filter: emacsInsertMode}, "escape", "/")
	kb.add(func(e *ptEvent) { ptIndentSelection(e, true) }, ptBind{filter: hasSelection}, "c-c", ">")
	kb.add(func(e *ptEvent) { ptIndentSelection(e, false) }, ptBind{filter: hasSelection}, "c-c", "<")

	shiftKeys := []string{"s-left", "s-right", "s-up", "s-down", "s-home", "s-end", "c-s-left", "c-s-right", "c-s-home", "c-s-end"}
	unshiftMove := func(e *ptEvent) {
		b := e.currentBuffer()
		switch key := e.keySequence[0].Key; key {
		case "s-up":
			b.autoMoveUp(e.arg())
		case "s-down":
			b.autoMoveDown(e.arg())
		default:
			command := map[string]string{"s-left": "backward-char", "s-right": "forward-char", "s-home": "beginning-of-line", "s-end": "end-of-line",
				"c-s-left": "backward-word", "c-s-right": "forward-word", "c-s-home": "beginning-of-buffer", "c-s-end": "end-of-buffer"}[key]
			if h := ptNamedCommands[command]; h != nil {
				h(e)
			}
		}
	}
	for _, key := range shiftKeys {
		kb.add(func(e *ptEvent) {
			b := e.currentBuffer()
			if b.text() == "" {
				return
			}
			b.startSelection(ptSelectCharacters)
			b.selection.shiftMode = true
			original := b.cursor
			unshiftMove(e)
			if b.cursor == original {
				b.exitSelection()
			}
		}, ptBind{filter: noSelection}, key)
	}
	for _, key := range shiftKeys {
		kb.add(func(e *ptEvent) {
			b := e.currentBuffer()
			unshiftMove(e)
			if sel := b.selection; sel != nil && b.cursor == sel.original {
				b.exitSelection()
			}
		}, ptBind{filter: shiftSelection}, key)
	}
	kb.add(func(e *ptEvent) {
		e.currentBuffer().cutSelection()
		ptNamedCommands["self-insert"](e)
	}, ptBind{filter: shiftSelection}, ptKeyAny)
	kb.add(func(e *ptEvent) {
		e.currentBuffer().cutSelection()
		e.currentBuffer().newline(true)
	}, ptBind{filter: shiftSelection}, "enter")
	kb.add(func(e *ptEvent) { e.currentBuffer().cutSelection() }, ptBind{filter: shiftSelection}, "backspace")
	kb.add(func(e *ptEvent) {
		if b := e.currentBuffer(); b.selection != nil {
			b.cutSelection()
		}
		ptNamedCommands["yank"](e)
	}, ptBind{filter: shiftSelection}, "c-y")
	for _, key := range []string{"left", "right", "up", "down", "home", "end", "c-left", "c-right", "c-home", "c-end"} {
		kb.add(func(e *ptEvent) {
			e.currentBuffer().exitSelection()
			e.app.processor.feed(e.keySequence[0], true)
		}, ptBind{filter: shiftSelection}, key)
	}
	ptConditional(kb.bindings[emacsStart:], app.emacsMode)
	return kb
}

// Makes each binding active only while cond also holds, a ConditionalKeyBindings over them
func ptConditional(bindings []*ptBinding, cond func() bool) {
	for _, b := range bindings {
		filter := b.filter
		if filter == nil {
			b.filter = cond
			continue
		}
		b.filter = func() bool { return cond() && filter() }
	}
}

// Indents or unindents the selected lines by four spaces per repeat count, the cursor keeping its place in the text
func ptIndentSelection(e *ptEvent, indent bool) {
	b := e.currentBuffer()
	if indent {
		b.setCursorPosition(b.cursor + b.document().startOfLineAfterWhitespace())
	}
	doc := b.document()
	from, to := doc.selectionRange()
	fromRow, _ := doc.lineStart(from)
	toRow, _ := doc.lineStart(to)
	row, col := doc.cursorRow(), doc.cursorCol()
	pad := strings.Repeat("    ", max(e.arg(), 0))
	lines := strings.Split(b.text(), "\n")
	for i := fromRow; i <= toRow && i < len(lines); i++ {
		switch {
		case indent:
			lines[i] = pad + lines[i]
		case strings.HasPrefix(lines[i], pad):
			lines[i] = lines[i][len(pad):]
		default:
			lines[i] = strings.TrimLeftFunc(lines[i], unicode.IsSpace)
		}
	}
	text := strings.Join(lines, "\n")
	delta := len(pad)
	if !indent {
		delta = -delta
	}
	_ = b.setDocument(text, newPTDocument(text, 0).rowColToIndex(row, 0), false)
	b.setCursorPosition(b.cursor + col + delta)
}
