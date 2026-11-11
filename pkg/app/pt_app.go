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
	uv "github.com/charmbracelet/ultraviolet"
	"github.com/charmbracelet/x/ansi"
	xterm "github.com/charmbracelet/x/term"

	"github.com/nickheyer/icebath/pkg/internal/pythread"
	"github.com/nickheyer/icebath/pkg/internal/pyunicode"
	"github.com/nickheyer/icebath/pkg/ui/cards"
	"github.com/nickheyer/icebath/pkg/ui/theme"
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

// Builds an application reading keys from in and drawing to out
func newPTApplication(in io.Reader, out io.Writer) *ptApplication {
	installPromptKeyAliases()
	a := &ptApplication{in: in, out: out, clipboard: &ptClipboard{}, keyBindings: &ptKeyBindings{}, timeoutLen: time.Second,
		outboxSignal: make(chan struct{}, 1), editingMode: ptEditingEmacs, viState: newPTViState()}
	a.cprNotSupportedCallback = func() {
		a.RunInTerminal(func() {
			_, _ = io.WriteString(a.out, "WARNING: your terminal doesn't support cursor position requests (CPR).\r\n")
		})
	}
	a.setBuffer(newPTBuffer(nil))
	a.processor = &ptKeyProcessor{app: a}
	a.defaultBindings = ptLoadDefaultBindings(a)
	user, _ := newPTStyle(nil)
	a.style = newPTMergedStyle(user)
	return a
}

// Installs the input buffer, wiring its repaint and background work to the loop
func (a *ptApplication) setBuffer(b *ptBuffer) {
	a.buffer = b
	b.onChange = a.publishSnapshot
	b.editingVi = func() bool { return a.editingMode == ptEditingVi }
	b.spawn = func(work func() func()) {
		if !a.running.Load() {
			if apply := work(); apply != nil {
				apply()
			}
			return
		}
		go func() {
			if apply := work(); apply != nil {
				a.CallSoonThreadsafe(apply)
			}
		}()
	}
	a.publishSnapshot()
}

// Publishes the buffer text and cursor for readers off the loop
func (a *ptApplication) publishSnapshot() {
	a.snapshot.Store(&modalInputSnapshot{Text: a.buffer.text(), Cursor: a.buffer.cursor})
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

// Reports whether the application finished or was asked to exit
func (a *ptApplication) isDone() bool { return a.done.Load() }

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

// Delivers queued messages to the program one at a time until it stops
func (a *ptApplication) pump(p *tea.Program, stop <-chan struct{}) {
	for {
		select {
		case <-stop:
			return
		case <-a.outboxSignal:
		}
		for {
			a.queueMu.Lock()
			if len(a.outbox) == 0 {
				a.queueMu.Unlock()
				break
			}
			msg := a.outbox[0]
			a.outbox = a.outbox[1:]
			a.queueMu.Unlock()
			p.Send(msg)
		}
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

// Reports whether typed bytes wait unread on the input
func (a *ptApplication) inputWaiting() bool {
	if a.inputWaitingFn != nil {
		return a.inputWaitingFn()
	}
	if f, ok := a.in.(*os.File); ok {
		return fileReadable(f)
	}
	return false
}

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

// Runs fn on the loop after a delay, nil when no loop is running
func (a *ptApplication) callLater(d time.Duration, fn func()) *time.Timer {
	if a.callLaterHook != nil {
		a.callLaterHook(d, fn)
		return nil
	}
	if !a.running.Load() {
		return nil
	}
	return time.AfterFunc(d, func() { a.CallSoonThreadsafe(fn) })
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

// Feeds key presses through the bindings as if typed
func (a *ptApplication) feedKeys(presses ...ptKeyPress) {
	a.processor.feedMultiple(presses, false)
	a.processor.processKeys()
}

// Runs the program until it exits, the terminal restored on return
func (a *ptApplication) run() error {
	a.loopGID.Store(pythread.GoroutineID())
	opts := []tea.ProgramOption{tea.WithInput(guardedInput(a.in)), tea.WithOutput(a.out), tea.WithoutSignalHandler()}
	if a.initialCols > 0 && a.initialRows > 0 {
		opts = append(opts, tea.WithWindowSize(a.initialCols, a.initialRows))
		a.width.Store(int32(a.initialCols))
		a.rows.Store(int32(a.initialRows))
	}
	opts = append(opts, tea.WithFilter(a.filterMessage))
	p := tea.NewProgram(&ptModel{app: a}, opts...)
	a.program = p
	a.viState.reset()
	stop := make(chan struct{})
	pumped := make(chan struct{})
	a.done.Store(false)
	a.running.Store(true)
	go func() {
		a.pump(p, stop)
		close(pumped)
	}()
	if a.refreshInterval > 0 {
		go func() {
			ticker := time.NewTicker(time.Duration(a.refreshInterval * float64(time.Second)))
			defer ticker.Stop()
			for {
				select {
				case <-stop:
					return
				case <-ticker.C:
					_ = a.Invalidate()
				}
			}
		}()
	}
	_, err := p.Run()
	a.running.Store(false)
	close(stop)
	<-pumped
	a.queueMu.Lock()
	a.queue, a.outbox = nil, nil
	a.queueMu.Unlock()
	a.busy.Store(false)
	a.wakePending.Store(false)
	if err != nil && !errors.Is(err, tea.ErrProgramKilled) {
		a.runErr.Store(&err)
		return err
	}
	return nil
}

// ptModel adapts the application to the bubbletea model contract
type ptModel struct{ app *ptApplication }

// Message telling the cursor position query went unanswered
type ptCPRTimeoutMsg struct{}

// Seconds a cursor position query may stay unanswered
const ptCPRTimeout = 2 * time.Second

// Drops kitty key reporting where the terminal mishandles it and asks for the cursor row when queries are enabled
func (m *ptModel) Init() tea.Cmd {
	var cmds []tea.Cmd
	for _, fn := range m.app.onStart {
		fn()
	}
	if m.app.disableKitty {
		cmds = append(cmds, tea.Raw(ansi.PopKittyKeyboard(1)))
	}
	if m.app.enableCPR {
		cmds = append(cmds, tea.RequestCursorPosition, tea.Tick(ptCPRTimeout, func(time.Time) tea.Msg { return ptCPRTimeoutMsg{} }))
	}
	return tea.Batch(cmds...)
}

// Routes one program message into the key processor, the callback queue or the resize hooks
func (m *ptModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	a := m.app
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		a.width.Store(int32(msg.Width))
		a.rows.Store(int32(msg.Height))
		a.appliedCols, a.appliedRows = msg.Width, msg.Height
		a.lastHeight = 0
		a.screenValid = false
		for _, fn := range a.onResize {
			fn(msg.Width, msg.Height)
		}
	case tea.KeyPressMsg:
		a.feedKeys(ptKeyPressesForEvent(msg.Key())...)
	case tea.PasteMsg:
		a.feedKeys(ptKeyPress{Key: ptKeyBracketedPaste, Data: msg.Content})
	case tea.FocusMsg:
		a.feedKeys(ptKeyPress{Key: ptKeyIgnore, Data: "\x1b[I"})
	case tea.BlurMsg:
		a.feedKeys(ptKeyPress{Key: ptKeyIgnore, Data: "\x1b[O"})
	case tea.CursorPositionMsg:
		a.cprAnswered = true
		a.minAvailableHeight = max(0, int(a.rows.Load())-msg.Y)
	case ptCPRTimeoutMsg:
		if !a.cprAnswered && a.cprNotSupportedCallback != nil {
			a.cprNotSupportedCallback()
		}
	case ptWakeMsg:
		a.wakePending.Store(false)
		a.drainQueue()
	case ptRunInTerminalMsg:
		_ = a.program.ReleaseTerminal()
		msg.fn()
		_ = a.program.RestoreTerminal()
		a.hidden = false
		a.lastHeight = 0
		a.busy.Store(false)
		if a.disableKitty {
			a.send(tea.RawMsg{Msg: ansi.PopKittyKeyboard(1)})
		}
	case ptAfterRenderMsg:
		a.afterRenderQueued = false
		for _, fn := range a.afterRender {
			fn()
		}
		return m, nil
	}
	a.publishSnapshot()
	if len(a.afterRender) > 0 && !a.afterRenderQueued {
		a.afterRenderQueued = true
		return m, tea.Tick(time.Second/60, func(time.Time) tea.Msg { return ptAfterRenderMsg{} })
	}
	return m, nil
}

// Runs every callback queued for the loop
func (a *ptApplication) drainQueue() {
	for {
		a.queueMu.Lock()
		queue := a.queue
		a.queue = nil
		a.queueMu.Unlock()
		if len(queue) == 0 {
			return
		}
		for _, fn := range queue {
			fn()
		}
	}
}

// Message applying a terminal size the resize recovery held back
type ptApplyResizeMsg struct{ cols, rows int }

// Records each terminal size signal, the resize recovery deciding when the renderer learns of it
func (a *ptApplication) filterMessage(_ tea.Model, msg tea.Msg) tea.Msg {
	switch m := msg.(type) {
	case tea.WindowSizeMsg:
		a.width.Store(int32(m.Width))
		a.rows.Store(int32(m.Height))
		if !a.sizeSeen || a.onResizeSignal == nil {
			a.sizeSeen = true
			return msg
		}
		a.onResizeSignal()
		return nil
	case ptApplyResizeMsg:
		return tea.WindowSizeMsg{Width: m.cols, Height: m.rows}
	}
	return msg
}

// Erases the chrome from the renderer's cursor and repaints it at the signalled terminal size
func (a *ptApplication) applyResize() {
	a.rawMu.Lock()
	aim := a.aimRows
	a.aimRows = 0
	a.rawMu.Unlock()
	if aim > 0 {
		a.send(tea.RawMsg{Msg: ansi.CursorUp(aim)})
	}
	a.send(ptApplyResizeMsg{int(a.width.Load()), int(a.rows.Load())})
}

// Draws the chrome, nothing once the application is done or a function owns the terminal
func (m *ptModel) View() tea.View {
	a := m.app
	var v tea.View
	v.ReportFocus = true
	done := a.done.Load()
	if done || a.hidden {
		a.chromeRows.Store(0)
		a.cursorX, a.cursorY = 0, 0
		a.lastView = v
		return v
	}
	if a.redrawGate != nil && !a.redrawGate(false) {
		return a.lastView
	}
	width, rows := a.appliedCols, a.appliedRows
	if width <= 0 {
		width = int(a.width.Load())
	}
	if rows <= 0 {
		rows = int(a.rows.Load())
	}
	if width <= 0 {
		width = 80
	}
	if rows <= 0 {
		rows = 24
	}
	full := !a.screenValid || a.lastPaintCols != width || a.lastPaintRows != rows
	lines, cx, cy := a.paintChrome(width, rows)
	a.chromeRows.Store(int32(len(lines)))
	v.SetContent(strings.Join(lines, "\n"))
	a.cursorX, a.cursorY = 0, 0
	if cy >= 0 && cy < len(lines) {
		cur := tea.NewCursor(cx, cy)
		cur.Shape = tea.CursorBlock
		cur.Blink = false
		v.Cursor = cur
		a.cursorX, a.cursorY = cx, cy
	}
	a.frameID++
	a.screenValid = true
	a.lastPaintCols, a.lastPaintRows = width, rows
	a.lastView = v
	if a.afterRedraw != nil {
		a.afterRedraw(full)
	}
	return v
}

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

// Renders the chrome, a guarded paint retrying a failed render once from a clean previous frame
func (a *ptApplication) paintChrome(width, rows int) ([]string, int, int) {
	if !a.guardedPaint {
		return a.renderChrome(width, rows)
	}
	previous := &ptScreen{height: a.lastHeight}
	result, err := hermesCallOutputScreenDiff(func(prev *ptScreen, _ string, _ int) (frame any, err error) {
		defer func() {
			if r := recover(); r != nil {
				err = fmt.Errorf("chrome render failed: %v", r)
			}
		}()
		if prev == nil {
			a.lastHeight, a.inputScroll, a.inputScroll2, a.menuScroll = 0, 0, 0, 0
		}
		lines, cx, cy := a.renderChrome(width, rows)
		return ptFrame{lines, cx, cy}, nil
	}, &ptScreen{height: a.lastHeight}, previous, "", width)
	if err != nil {
		panic(err)
	}
	frame := result.(ptFrame)
	return frame.lines, frame.cx, frame.cy
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

// Rows a line of the given display width takes when wrapped, at least one
func ptLineHeight(textWidth, width int) int {
	if width <= 0 {
		return 100000000
	}
	return max(1, (textWidth+width-1)/width)
}

// Display width of a row of cells
func ptRowWidth(row []ptSeg) int {
	w := 0
	for _, c := range row {
		w += ptCellWidth(c)
	}
	return w
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

// Builds the input lines with prompt, masking, empty input hint, ghost suggestion and selection applied
func (a *ptApplication) inputContent() ptInputContent {
	b := a.buffer
	doc := b.document()
	base := "class:text-area " + a.inputStyle
	masked := a.password != nil && a.password()
	content := ptInputContent{cursorRow: doc.cursorRow()}
	index := 0
	docLines := doc.lines()
	multiple := a.viInsertMultipleMode()
	for i, line := range docLines {
		lineRunes := []rune(line)
		styles := make([]string, len(lineRunes))
		texts := make([]string, len(lineRunes))
		for j, r := range lineRunes {
			styles[j], texts[j] = base, string(r)
		}
		if from, to, ok := doc.selectionRangeAtLine(i); ok {
			if from == 0 && to == 0 && len(lineRunes) == 0 {
				styles, texts = []string{base + " class:selected "}, []string{" "}
			} else {
				for k := from; k < to; k++ {
					if k < len(styles) {
						styles[k] += " class:selected "
					} else if k == len(styles) {
						styles, texts = append(styles, base+" class:selected "), append(texts, " ")
					}
				}
			}
		}
		if multiple {
			start := doc.translateRowColToIndex(i, 0)
			for _, p := range b.multipleCursors {
				if start <= p && p <= start+len(lineRunes) {
					if column := p - start; column < len(styles) {
						styles[column] += " class:multiple-cursors"
					} else {
						styles, texts = append(styles, base+" class:multiple-cursors"), append(texts, " ")
					}
				}
			}
		}
		var cells []ptSeg
		if i == 0 && a.prompt != nil {
			for _, f := range a.prompt() {
				cells = append(cells, ptCells(base+" class:text-area.prompt "+f.Style, f.Text)...)
			}
		}
		extra := len(styles) - len(lineRunes)
		for j := range lineRunes {
			if index == doc.cursor {
				content.cursorCol = len(cells)
			}
			text := texts[j]
			if masked {
				text = "*"
			}
			cells = append(cells, ptCells(styles[j], text)...)
			index++
		}
		if index == doc.cursor {
			content.cursorCol = len(cells)
		}
		for j := len(lineRunes); j < len(lineRunes)+extra; j++ {
			cells = append(cells, ptCells(styles[j], texts[j])...)
		}
		index++
		if i == 0 && len(doc.runes) == 0 && a.emptyHint != nil {
			if text := a.emptyHint(); text != "" {
				cells = append(cells, ptCells(base+" class:placeholder", text)...)
			}
		}
		if i == len(docLines)-1 && b.suggestion != nil && doc.atEnd() {
			cells = append(cells, ptCells(base+" class:auto-suggestion", *b.suggestion)...)
		}
		if extra > 0 {
			content.lines = append(content.lines, cells)
			continue
		}
		content.lines = append(content.lines, append(cells, ptSeg{base, " "}))
	}
	return content
}

// Scrolls the wrapped input so the cursor line stays visible, the prompt_toolkit window algorithm
func (a *ptApplication) scrollInput(content ptInputContent, width, height int) {
	lineHeight := func(i int) int { return ptLineHeight(ptRowWidth(content.lines[i]), width) }
	cy := content.cursorRow
	if lh := lineHeight(cy); lh > height {
		before := ptLineHeight(ptRowWidth(content.lines[cy][:min(content.cursorCol, len(content.lines[cy]))]), width)
		a.inputScroll = cy
		a.inputScroll2 = min(before-1, lh-height, a.inputScroll2)
		a.inputScroll2 = max(0, before-height, a.inputScroll2)
		return
	}
	a.inputScroll2 = 0
	minScroll := func() int {
		used, prev := 0, cy
		for i := cy; i >= 0; i-- {
			used += lineHeight(i)
			if used > height {
				return prev
			}
			prev = i
		}
		return 0
	}
	maxScroll := func() int {
		used, prev := 0, cy
		for i := cy - 1; i >= 0; i-- {
			used += lineHeight(i)
			if used > 0 {
				return prev
			}
			prev = i
		}
		return prev
	}
	topmost := func() int {
		used, prev := 0, len(content.lines)-1
		for i := len(content.lines) - 1; i >= 0; i-- {
			used += lineHeight(i)
			if used > height {
				return prev
			}
			prev = i
		}
		return prev
	}
	top := topmost()
	a.inputScroll = max(a.inputScroll, min(top, minScroll()))
	a.inputScroll = min(a.inputScroll, maxScroll())
	a.inputScroll = min(a.inputScroll, top)
}

// Visible input rows and the cursor position among them
func (a *ptApplication) inputRows(width, height int) (rows [][]ptSeg, cx, cy int) {
	content := a.inputContent()
	a.scrollInput(content, width, height)
	cx, cy = -1, -1
	y := -a.inputScroll2
	info := &ptRenderInfo{windowHeight: height, windowWidth: width}
	for i := range content.lines {
		info.lineHeights = append(info.lineHeights, len(ptWrapCells(content.lines[i], width)))
	}
	defer func() {
		if len(info.displayedLines) > 0 {
			a.inputRenderInfo = info
		}
	}()
	for i := a.inputScroll; i < len(content.lines); i++ {
		wrapped := ptWrapCells(content.lines[i], width)
		if i == content.cursorRow {
			col, seen := content.cursorCol, 0
			for r, row := range wrapped {
				if col < seen+len(row) {
					cy, cx = y+r, ptRowWidth(row[:col-seen])
					break
				}
				seen += len(row)
			}
		}
		for _, row := range wrapped {
			if y >= 0 && y < height {
				rows = append(rows, row)
				info.displayedLines = append(info.displayedLines, i)
			}
			y++
		}
		if y >= height {
			break
		}
	}
	if cy < 0 || cy >= height {
		cx, cy = -1, -1
	}
	return rows, cx, cy
}

// Completion menu rows with the selected entry scrolled into view
func (a *ptApplication) menuRows(width, height int) [][]ptSeg {
	state := a.buffer.completeState
	if state == nil || len(state.completions) == 0 || height <= 0 {
		return nil
	}
	display := func(c ptCompletion) string {
		if c.Display != "" {
			return c.Display
		}
		return c.Text
	}
	menuWidth, metaWidth := 7, 0
	for _, c := range state.completions {
		menuWidth = max(menuWidth, pyunicode.WcsWidth(display(c))+2)
	}
	menuWidth = min(width-1, menuWidth)
	showMeta := false
	for i, c := range state.completions {
		if c.Meta != "" {
			showMeta = true
			if i < 200 {
				metaWidth = max(metaWidth, pyunicode.WcsWidth(c.Meta)+2)
			}
		}
	}
	if showMeta {
		metaWidth = min(max(0, width-1-menuWidth), metaWidth)
	}
	count := len(state.completions)
	cursor := 0
	if state.index != nil {
		cursor = *state.index
	}
	start := min(1, height/2, cursor)
	end := min(1, height/2, count-1-cursor)
	scroll := max(0, a.menuScroll)
	if scroll > count-height {
		scroll = max(0, count-height)
	}
	if scroll > cursor-start {
		scroll = max(0, cursor-start)
	}
	if scroll < cursor+1-height+end {
		scroll = cursor + 1 - height + end
	}
	a.menuScroll = scroll
	cell := func(style, text string, w int) []ptSeg {
		if w <= 0 {
			return nil
		}
		tw := pyunicode.WcsWidth(text)
		if tw > w-2 {
			r := []rune(text)
			for len(r) > 0 && pyunicode.WcsWidth(string(r))+3 > w-2 {
				r = r[:len(r)-1]
			}
			text = string(r) + "..."
			tw = pyunicode.WcsWidth(text)
		}
		return ptCells(style, " "+text+strings.Repeat(" ", max(0, w-1-tw)))
	}
	thumbStart, thumbEnd := 0, height
	if count > height {
		thumb := max(1, height*height/count)
		thumbStart = scroll * height / count
		thumbEnd = thumbStart + thumb
	}
	var rows [][]ptSeg
	for i := scroll; i < min(count, scroll+height); i++ {
		c := state.completions[i]
		suffix := ""
		if state.index != nil && *state.index == i {
			suffix = ".current"
		}
		row := cell("class:completion-menu class:completion-menu.completion"+suffix, display(c), menuWidth)
		if showMeta {
			row = append(row, cell("class:completion-menu class:completion-menu.meta.completion"+suffix, c.Meta, metaWidth)...)
		}
		bar := "class:completion-menu class:scrollbar.background"
		if y := i - scroll; count > height && y >= thumbStart && y < thumbEnd {
			bar = "class:completion-menu class:scrollbar.button"
		}
		rows = append(rows, append(row, ptSeg{bar, " "}))
	}
	return rows
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

// Splits the available rows among the windows by minimum, preferred then maximum size, false when the minimums overflow
func ptDivideHeights(dims []ptDim, height int, done bool) ([]int, bool) {
	sizes := make([]int, len(dims))
	sumMin, sumPref, sumMax := 0, 0, 0
	for i, d := range dims {
		sizes[i] = d.min
		sumMin += d.min
		sumPref += d.pref
		sumMax = min(ptUnbounded, sumMax+d.max)
	}
	if sumMin > height {
		return nil, false
	}
	if len(dims) == 0 {
		return sizes, true
	}
	total := sumMin
	i := 0
	for stop := min(height, sumPref); total < stop; i = (i + 1) % len(dims) {
		if sizes[i] < dims[i].pref {
			sizes[i]++
			total++
		}
	}
	if !done {
		for stop := min(height, sumMax); total < stop; i = (i + 1) % len(dims) {
			if sizes[i] < dims[i].max {
				sizes[i]++
				total++
			}
		}
	}
	return sizes, true
}

// Splits rows like the stock container, an overflow keeping bottom rows, clipping late top rows and queuing a repaint
func footerSplitDivideHeights(a *ptApplication, dims []ptDim, height int) []int {
	if sizes, ok := ptDivideHeights(dims, height, a.isDone()); ok {
		return sizes
	}
	budget := height
	clipped := make([]int, len(dims))
	for i := len(dims) - 1; i >= 0; i-- {
		clipped[i] = min(dims[i].min, budget)
		budget -= clipped[i]
	}
	_ = a.Invalidate()
	if a.running.Load() {
		a.wake()
	}
	return clipped
}

// Renders the chrome rows as styled lines with the cursor position, minus one when hidden
func (a *ptApplication) renderChrome(width, termRows int) (lines []string, cx, cy int) {
	dims := make([]ptDim, len(a.layout))
	pref := 0
	for i, w := range a.layout {
		dims[i] = a.windowDim(w, width, termRows)
		pref += dims[i].pref
	}
	height := min(termRows, max(a.minAvailableHeight, a.lastHeight, pref))
	sizes := footerSplitDivideHeights(a, dims, height)
	cx, cy = -1, -1
	type placed struct {
		rows  [][]ptSeg
		style string
		right bool
	}
	var out []placed
	for i, w := range a.layout {
		h := sizes[i]
		if h <= 0 {
			continue
		}
		var rows [][]ptSeg
		style := w.style
		switch w.kind {
		case ptWindowBuffer:
			style = "class:text-area " + a.inputStyle
			var x, y int
			rows, x, y = a.inputRows(width, h)
			if y >= 0 {
				cx, cy = x, len(out)+y
			}
		case ptWindowMenu:
			rows = a.menuRows(width, h)
		default:
			if w.char != "" {
				row := ptCells("", strings.Repeat(w.char, width))
				for len(rows) < h {
					rows = append(rows, row)
				}
			} else {
				rows = w.textRows(width)
			}
		}
		for r := 0; r < h; r++ {
			var row []ptSeg
			if r < len(rows) {
				row = rows[r]
			}
			out = append(out, placed{[][]ptSeg{row}, style, w.alignRight})
		}
	}
	for len(out) > 0 && len(out[len(out)-1].rows[0]) == 0 && cy < len(out)-1 {
		out = out[:len(out)-1]
	}
	a.lastHeight = height
	if len(out) == 0 {
		a.lastRowWidths = a.lastRowWidths[:0]
		return nil, -1, -1
	}
	buf := uv.NewScreenBuffer(width, len(out))
	painter := cards.NewPainter(buf.Buffer, theme.Plain())
	a.lastRowWidths = a.lastRowWidths[:0]
	for y, p := range out {
		painter.Styled(0, y, width, 1, a.renderRow(p.rows[0], p.style, p.right, width))
		a.lastRowWidths = append(a.lastRowWidths, a.rowReach(p.rows[0], p.style, p.right, width))
	}
	return strings.Split(buf.Render(), "\n"), cx, cy
}

// Cells a painted row reaches, one past the last cell that shows text or a style
func (a *ptApplication) rowReach(row []ptSeg, windowStyle string, right bool, width int) int {
	if (right || a.style.hasStyle(windowStyle)) && len(row) > 0 {
		return max(1, width)
	}
	x, reach := 0, 0
	for _, c := range row {
		w := ptCellWidth(c)
		if c.text != " " || a.style.hasStyle(windowStyle+" "+c.style) {
			reach = x + w - 1
		}
		x += w
	}
	return 1 + min(reach, max(0, width-1))
}

// Styles one row of cells, right aligned rows padded on the left and styled windows filled to the edge
func (a *ptApplication) renderRow(row []ptSeg, windowStyle string, right bool, width int) string {
	var b strings.Builder
	used := ptRowWidth(row)
	if right && used < width {
		b.WriteString(a.style.render(windowStyle).Render(strings.Repeat(" ", width-used)))
		used = width
	}
	for i := 0; i < len(row); {
		j := i
		var text strings.Builder
		for j < len(row) && row[j].style == row[i].style {
			text.WriteString(row[j].text)
			j++
		}
		b.WriteString(a.style.render(windowStyle + " " + row[i].style).Render(text.String()))
		i = j
	}
	if windowStyle != "" && used < width {
		b.WriteString(a.style.render(windowStyle).Render(strings.Repeat(" ", width-used)))
	}
	return b.String()
}
