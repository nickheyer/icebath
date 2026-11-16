package app

import (
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
	xterm "github.com/charmbracelet/x/term"

	"github.com/nickheyer/icebath/pkg/internal/pythread"
	"github.com/nickheyer/icebath/pkg/internal/pyunicode"
	"github.com/nickheyer/icebath/pkg/ui/cards"
)

// ptWindowKind tells what a layout row group draws
type ptWindowKind int

const (
	ptWindowText ptWindowKind = iota
	ptWindowBuffer
	ptWindowMenu
)

// ptWindow is one child of the prompt chrome column
type ptWindow struct {
	name       string
	kind       ptWindowKind
	fragments  func() []styledFragment
	height     func() int
	wrapLines  bool
	alignRight bool
	char       string
	style      string
	filter     func() bool
	maxHeight  int
}

// Reports whether the window takes part in this frame
func (w *ptWindow) visible() bool { return w.filter == nil || w.filter() }

// ptDim is the minimum, maximum and preferred row count of a window
type ptDim struct{ min, max, pref int }

// ptSeg is a run of text in one style
type ptSeg struct{ style, text string }

// ptUnbounded is the row count of a window with no maximum
const ptUnbounded = 1 << 30

// Message waking the loop to run queued callbacks and repaint
type ptWakeMsg struct{}

// Message running a function with the terminal handed back to it
type ptRunInTerminalMsg struct{ fn func() }

// Message running the after render hooks
type ptAfterRenderMsg struct{}

// ptApplication is the prompt application, a bubbletea inline program with prompt_toolkit key and layout semantics
type ptApplication struct {
	in              io.Reader
	out             io.Writer
	buffer          *ptBuffer
	keyBindings     *ptKeyBindings
	defaultBindings *ptKeyBindings
	processor       *ptKeyProcessor
	clipboard       *ptClipboard
	layout          []*ptWindow
	style           *ptMergedStyle
	quotedInsert    bool
	timeoutLen      time.Duration

	emacsMacro          []ptKeyPress
	emacsMacroRecording *[]ptKeyPress

	editingMode     string
	viState         *ptViState
	pasteMode       bool
	inputRenderInfo *ptRenderInfo

	prompt      func() []styledFragment
	emptyHint   func() string
	password    func() bool
	inputHeight func() int
	inputStyle  string

	program       *tea.Program
	running       atomic.Bool
	done          atomic.Bool
	hidden        bool
	busy          atomic.Bool
	released      atomic.Bool
	loopGID       atomic.Uint64
	queueMu       sync.Mutex
	queue         []func()
	outbox        []tea.Msg
	outboxSignal  chan struct{}
	wakePending   atomic.Bool
	width         atomic.Int32
	rows          atomic.Int32
	chromeRows    atomic.Int32
	snapshot      atomic.Pointer[modalInputSnapshot]
	invalidations atomic.Int64
	bells         atomic.Int64
	runErr        atomic.Pointer[error]
	initialCols   int
	initialRows   int
	disableKitty  bool

	rawMu                   sync.Mutex
	rawPending              strings.Builder
	cursorX                 int
	cursorY                 int
	aimRows                 int
	frameID                 uint64
	screenValid             bool
	lastRowWidths           []int
	lastPaintCols           int
	lastPaintRows           int
	appliedCols             int
	appliedRows             int
	sizeSeen                bool
	lastView                tea.View
	onResizeSignal          func()
	redrawGate              func(renderAsDone bool) bool
	afterRedraw             func(full bool)
	sizeProbe               func() (rows, columns int, err error)
	callLaterHook           func(d time.Duration, fn func())
	callSoonHook            func(fn func())
	inputWaitingFn          func() bool
	guardedPaint            bool
	refreshInterval         float64
	onStart                 []func()
	enableCPR               bool
	cprAnswered             bool
	minAvailableHeight      int
	cprNotSupportedCallback func()
	fallbackSize            func() (rows, columns int)

	lastHeight        int
	inputScroll       int
	inputScroll2      int
	menuScroll        int
	afterRender       []func()
	afterRenderQueued bool
	onResize          []func(cols, rows int)
}

// Replaces the user style rules
func (a *ptApplication) setStyle(d *styleDict) error {
	user, err := ptStyleFromDict(d)
	if err != nil {
		return err
	}
	a.style = newPTMergedStyle(user)
	return nil
}

// Starts recording an emacs keyboard macro
func (a *ptApplication) startMacro() { a.emacsMacroRecording = &[]ptKeyPress{} }

// Ends the recording and keeps it as the macro
func (a *ptApplication) endMacro() {
	if a.emacsMacroRecording != nil {
		a.emacsMacro = *a.emacsMacroRecording
	}
	a.emacsMacroRecording = nil
}

// Rings the terminal bell
func (a *ptApplication) bell() {
	a.bells.Add(1)
	if a.running.Load() {
		_ = a.WriteRaw("\a")
	}
}

// Erases the screen and repaints the chrome from the top
func (a *ptApplication) clearRenderer() {
	a.lastHeight = 0
	if a.running.Load() {
		a.send(tea.ClearScreen())
	}
}

// Queues a message for the program in order
func (a *ptApplication) send(msg tea.Msg) {
	a.queueMu.Lock()
	a.outbox = append(a.outbox, msg)
	a.queueMu.Unlock()
	select {
	case a.outboxSignal <- struct{}{}:
	default:
	}
}

// Asks the loop to run queued callbacks and repaint, coalescing repeated requests
func (a *ptApplication) wake() {
	if a.wakePending.CompareAndSwap(false, true) {
		a.send(ptWakeMsg{})
	}
}

// Requests a repaint, returning the error that stopped the program when there is one
func (a *ptApplication) Invalidate() error {
	a.invalidations.Add(1)
	if errp := a.runErr.Load(); errp != nil {
		return *errp
	}
	if a.running.Load() && !a.OnLoop() {
		a.wake()
	}
	return nil
}

// Reports whether the program is running
func (a *ptApplication) IsRunning() bool { return a.running.Load() }

// Reports whether the caller is the goroutine running the program loop
func (a *ptApplication) OnLoop() bool {
	return a.running.Load() && a.loopGID.Load() == pythread.GoroutineID()
}

// Schedules fn on the loop, false when no loop is running
func (a *ptApplication) CallSoonThreadsafe(fn func()) bool {
	if a.callSoonHook != nil {
		a.callSoonHook(fn)
		return true
	}
	if !a.running.Load() {
		return false
	}
	a.queueMu.Lock()
	a.queue = append(a.queue, fn)
	a.queueMu.Unlock()
	a.wake()
	return true
}

// Writes an escape sequence through the program output, straight to the output when idle
func (a *ptApplication) WriteRaw(seq string) error {
	if !a.running.Load() {
		_, err := io.WriteString(a.out, seq)
		return err
	}
	a.send(tea.RawMsg{Msg: seq})
	if errp := a.runErr.Load(); errp != nil {
		return *errp
	}
	return nil
}

// Terminal rows a printed line takes at the given width, wide characters wrapping early
func ptPrintedRows(line string, width int) int {
	if width <= 0 {
		return 1
	}
	rows, col := 1, 0
	for _, g := range cards.Graphemes(ansi.Strip(line)) {
		w := ansi.StringWidth(g)
		if col+w > width {
			rows++
			col = 0
		}
		col += w
	}
	return rows
}

// Prints text above the live chrome, the scrollback keeping it
func (a *ptApplication) PrintAbove(text string) {
	if !a.running.Load() {
		_, _ = io.WriteString(a.out, text+"\n")
		return
	}
	width := int(a.width.Load())
	lines := strings.Split(text, "\n")
	for i, line := range lines {
		lineWidth := ansi.StringWidth(line)
		if width > 0 && lineWidth > width && ptPrintedRows(line, width) != 1+lineWidth/width {
			lines[i] = ansi.Hardwrap(line, width, true)
		}
	}
	a.lastHeight = 0
	a.send(tea.Println(strings.Join(lines, "\n"))())
}

// Writes text to the terminal as it is
func (a *ptApplication) PrintRaw(text string) {
	if !a.running.Load() {
		_, _ = io.WriteString(a.out, text)
		return
	}
	a.send(tea.RawMsg{Msg: strings.ReplaceAll(text, "\n", "\r\n")})
}

// Reports whether a function currently owns the terminal
func (a *ptApplication) TerminalBusy() bool { return a.busy.Load() }

// Reports whether the terminal is out of the program's hands while a function draws on it
func (a *ptApplication) TerminalReleased() bool { return a.released.Load() }

// Hides the chrome, hands the terminal to fn, then draws the chrome again below its output
func (a *ptApplication) RunInTerminal(fn func()) {
	if !a.running.Load() {
		fn()
		return
	}
	a.busy.Store(true)
	run := func() {
		a.hidden = true
		a.send(ptRunInTerminalMsg{fn: fn})
	}
	if a.OnLoop() {
		run()
		return
	}
	a.CallSoonThreadsafe(run)
}

// Terminal size, queried from the output when no resize arrived yet
func (a *ptApplication) Size() (columns, rows int, ok bool) {
	columns, rows = int(a.width.Load()), int(a.rows.Load())
	if columns > 0 && rows > 0 {
		return columns, rows, true
	}
	if f, isFile := a.out.(*os.File); isFile {
		if w, h, err := xterm.GetSize(f.Fd()); err == nil && w > 0 && h > 0 {
			return w, h, true
		}
	}
	if a.fallbackSize != nil {
		rows, columns = a.fallbackSize()
		return columns, rows, true
	}
	return 0, 0, false
}

// Rows the chrome took in the last frame
func (a *ptApplication) ChromeRows() int { return int(a.chromeRows.Load()) }

// Text of the input buffer
func (a *ptApplication) BufferText() string { return a.snapshot.Load().Text }

// Cursor of the input buffer
func (a *ptApplication) BufferCursor() int { return a.snapshot.Load().Cursor }

// Runs a buffer change on the loop, at once when idle or already on it
func (a *ptApplication) mutate(fn func()) {
	if !a.running.Load() || a.OnLoop() {
		fn()
		return
	}
	a.CallSoonThreadsafe(fn)
}

// Replaces the buffer text and cursor
func (a *ptApplication) SetBuffer(text string, cursor int) {
	a.snapshot.Store(&modalInputSnapshot{Text: text, Cursor: cursor})
	a.mutate(func() { _ = a.buffer.setDocument(text, cursor, true) })
}

// Clears the buffer
func (a *ptApplication) ResetBuffer() {
	a.snapshot.Store(&modalInputSnapshot{})
	a.mutate(func() { a.buffer.reset(false) })
}

// Ends the program, the chrome erased
func (a *ptApplication) Exit() {
	a.done.Store(true)
	if a.running.Load() {
		a.send(tea.Quit())
	}
}

// ptModel adapts the application to the bubbletea model contract
type ptModel struct{ app *ptApplication }

// Message telling the cursor position query went unanswered
type ptCPRTimeoutMsg struct{}

// Seconds a cursor position query may stay unanswered
const ptCPRTimeout = 2 * time.Second

// Message applying a terminal size the resize recovery held back
type ptApplyResizeMsg struct{ cols, rows int }

// The renderer of this application
func (a *ptApplication) Renderer() tuiRenderer { return a }

// The terminal output of this application
func (a *ptApplication) Output() tuiOutput { return a }

// Live terminal size as rows then columns
func (a *ptApplication) GetSize() (rows, columns int, err error) {
	if a.sizeProbe != nil {
		return a.sizeProbe()
	}
	columns, rows, ok := a.Size()
	if !ok {
		return 0, 0, errors.New("terminal size unknown")
	}
	return rows, columns, nil
}

// Queues a raw sequence for the next output flush
func (a *ptApplication) queueRaw(seq string) {
	a.rawMu.Lock()
	a.rawPending.WriteString(seq)
	a.rawMu.Unlock()
}

func (a *ptApplication) ResetAttributes() { a.queueRaw("\x1b[0m") }
func (a *ptApplication) EraseScreen()     { a.queueRaw("\x1b[2J") }
func (a *ptApplication) EraseDown()       { a.queueRaw("\x1b[J") }
func (a *ptApplication) EraseEndOfLine()  { a.queueRaw("\x1b[K") }

// Queues an absolute cursor move
func (a *ptApplication) CursorGoto(row, column int) {
	a.queueRaw(fmt.Sprintf("\x1b[%d;%dH", row, column))
}

// Queues a cursor move up
func (a *ptApplication) CursorUp(n int) {
	if n > 0 {
		a.queueRaw(ansi.CursorUp(n))
	}
}

// Writes every queued sequence to the terminal
func (a *ptApplication) Flush() error {
	a.rawMu.Lock()
	seq := a.rawPending.String()
	a.rawPending.Reset()
	a.rawMu.Unlock()
	if seq == "" {
		return nil
	}
	if !a.running.Load() {
		_, err := io.WriteString(a.out, seq)
		return err
	}
	a.send(tea.RawMsg{Msg: seq})
	if errp := a.runErr.Load(); errp != nil {
		return *errp
	}
	return nil
}

// Identity and height of the last painted chrome, painted false after a reset
func (a *ptApplication) LastScreen() (id uint64, height int, painted bool) {
	return a.frameID, len(a.lastRowWidths), a.screenValid
}

// Width each row of the last painted chrome reaches
func (a *ptApplication) PaintedRowWidths() []int { return append([]int{}, a.lastRowWidths...) }

// Cursor position inside the painted chrome, with any pending erase aim applied
func (a *ptApplication) CursorPos() (x, y int) {
	a.rawMu.Lock()
	defer a.rawMu.Unlock()
	return a.cursorX, a.cursorY + a.aimRows
}

// Moves the row the next resize erase starts from
func (a *ptApplication) SetCursorPos(x, y int) {
	a.rawMu.Lock()
	a.aimRows = y - a.cursorY
	a.rawMu.Unlock()
}

func (a *ptApplication) MinAvailableHeight() int        { return a.minAvailableHeight }
func (a *ptApplication) SetMinAvailableHeight(rows int) { a.minAvailableHeight = rows }

// Terminal size of the last painted chrome
func (a *ptApplication) LastSize() (rows, columns int, ok bool) {
	return a.lastPaintRows, a.lastPaintCols, a.screenValid
}

// Forgets the painted chrome so the next frame draws every row from the cursor
func (a *ptApplication) Reset() {
	a.rawMu.Lock()
	x, y := a.cursorX, a.cursorY
	a.aimRows = 0
	a.rawMu.Unlock()
	a.lastHeight, a.screenValid = 0, false
	if !a.running.Load() {
		return
	}
	seq := ansi.CursorHorizontalAbsolute(x + 1)
	if y > 0 {
		seq = ansi.CursorDown(y) + seq
	}
	a.send(tea.RawMsg{Msg: seq})
	a.send(tea.ClearScreen())
}

// Rows the chrome wants at a terminal size
func (a *ptApplication) PreferredHeight(columns, rows int) int {
	pref := 0
	for _, w := range a.layout {
		pref += a.windowDim(w, columns, rows).pref
	}
	return pref
}

// ptFrame is one rendered chrome frame
type ptFrame struct {
	lines  []string
	cx, cy int
}

// Characters of text as cells, control characters drawn the way prompt_toolkit shows them
func ptCells(style, text string) []ptSeg {
	out := make([]ptSeg, 0, len(text))
	for _, r := range text {
		switch {
		case r == 0x7f:
			out = append(out, ptSeg{style + " class:control-character", "^?"})
		case r < 0x20:
			out = append(out, ptSeg{style + " class:control-character", "^" + string(r+64)})
		case r == 0xa0:
			out = append(out, ptSeg{style + " class:nbsp", " "})
		case pyunicode.WcWidth(r) <= 0 && len(out) > 0:
			out[len(out)-1].text += string(r)
		default:
			out = append(out, ptSeg{style, string(r)})
		}
	}
	return out
}

// Display width of one cell
func ptCellWidth(c ptSeg) int {
	w := 0
	for _, r := range c.text {
		w += max(0, pyunicode.WcWidth(r))
	}
	return w
}

// Fragments split into lines of cells at newlines
func ptFragmentLines(fragments []styledFragment) [][]ptSeg {
	lines := [][]ptSeg{nil}
	for _, f := range fragments {
		for i, part := range strings.Split(f.Text, "\n") {
			if i > 0 {
				lines = append(lines, nil)
			}
			lines[len(lines)-1] = append(lines[len(lines)-1], ptCells(f.Style, part)...)
		}
	}
	return lines
}

// Wraps one line of cells into rows of the given width
func ptWrapCells(line []ptSeg, width int) [][]ptSeg {
	rows := [][]ptSeg{nil}
	x := 0
	for _, c := range line {
		w := ptCellWidth(c)
		if x+w > width && x > 0 {
			rows = append(rows, nil)
			x = 0
		}
		rows[len(rows)-1] = append(rows[len(rows)-1], c)
		x += w
	}
	return rows
}

// Visual rows of a text window at the given width
func (w *ptWindow) textRows(width int) [][]ptSeg {
	if w.fragments == nil {
		return nil
	}
	lines := ptFragmentLines(w.fragments())
	if !w.wrapLines {
		return lines
	}
	var rows [][]ptSeg
	for _, line := range lines {
		rows = append(rows, ptWrapCells(line, width)...)
	}
	return rows
}

// ptInputContent is the input buffer laid out as lines of cells with the cursor cell located
type ptInputContent struct {
	lines     [][]ptSeg
	cursorRow int
	cursorCol int
}

// Row counts the window wants at the given width
func (a *ptApplication) windowDim(w *ptWindow, width, available int) ptDim {
	if !w.visible() {
		return ptDim{}
	}
	switch w.kind {
	case ptWindowBuffer:
		h := 1
		if a.inputHeight != nil {
			h = a.inputHeight()
		}
		return ptDim{h, h, h}
	case ptWindowMenu:
		state := a.buffer.completeState
		if state == nil || len(state.completions) == 0 {
			return ptDim{}
		}
		return ptDim{1, w.maxHeight, max(1, min(w.maxHeight, len(state.completions)))}
	}
	if w.height != nil {
		h := max(0, w.height())
		return ptDim{h, h, h}
	}
	return ptDim{0, ptUnbounded, len(w.textRows(width))}
}
