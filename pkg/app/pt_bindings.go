package app

import (
	"regexp"
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

// ptKeyBindings is an ordered registry, later bindings winning for the same keys
type ptKeyBindings struct {
	bindings []*ptBinding
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

// Queues several key presses in order
func (p *ptKeyProcessor) feedMultiple(keys []ptKeyPress, first bool) {
	if first {
		p.inputQueue = append(append([]ptKeyPress{}, keys...), p.inputQueue...)
		return
	}
	p.inputQueue = append(p.inputQueue, keys...)
}

// ptEditReadOnly is the EditReadOnlyBuffer a handler raises editing a read only buffer
type ptEditReadOnly struct{}

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
