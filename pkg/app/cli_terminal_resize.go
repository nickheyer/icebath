package app

import (
	"encoding/base64"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pyshutil"
)

const (
	resizeHoldMax     = 0.7
	resizeSettle      = 0.03
	resizePaintMargin = 0.01
)

// tuiOutput is the terminal output the redraw and resize recovery write through
type tuiOutput interface {
	GetSize() (rows, columns int, err error)
	ResetAttributes()
	EraseScreen()
	WriteRaw(seq string) error
	CursorGoto(row, column int)
	CursorUp(n int)
	EraseDown()
	EraseEndOfLine()
	Flush() error
}

// tuiRenderer is the paint state of the prompt chrome the redraw and resize recovery read and reset
type tuiRenderer interface {
	Output() tuiOutput
	LastScreen() (id uint64, height int, painted bool)
	PaintedRowWidths() []int
	CursorPos() (x, y int)
	SetCursorPos(x, y int)
	MinAvailableHeight() int
	SetMinAvailableHeight(rows int)
	LastSize() (rows, columns int, ok bool)
	Reset()
}

// tuiScreenApp is a prompt application whose renderer the redraw and resize recovery drive
type tuiScreenApp interface {
	tuiApplication
	Renderer() tuiRenderer
	PreferredHeight(columns, rows int) int
}

// noReplay is the fit of a clear that failed, nothing to replay
func noReplay() *replayFit { return &replayFit{} }

// chromePaint records when a chrome row was written, how wide, and whether the width moved under the paint
type chromePaint struct {
	at      float64
	width   int
	suspect bool
}

// The prompt application as one with a renderer, nil when there is none
func (c *CLI) screenApp() tuiScreenApp {
	c.uiMu.Lock()
	app := c.app
	c.uiMu.Unlock()
	screen, _ := app.(tuiScreenApp)
	return screen
}

// Reports whether terminal IO was marked broken
func (c *CLI) terminalBroken() bool {
	c.uiMu.Lock()
	defer c.uiMu.Unlock()
	return c.terminalIOBroken
}

// Forces a clean full repaint of the prompt after terminal drift the application cannot see
func (c *CLI) forceFullRedraw() {
	if c.terminalBroken() {
		return
	}
	app := c.screenApp()
	if app == nil {
		return
	}
	fit := c.clearPromptToolkitScreen(app, redrawRebuildsScrollback(), false)
	if c.terminalBroken() {
		return
	}
	if fit == nil {
		replayOutputHistoryFn(nil, nil)
	} else {
		replayOutputHistoryFn(fit, app)
	}
	c.petQueueKittyFrame()
	if err := c.appInvalidate(app, "force_full_redraw", true); err != nil {
		cliLogger.Debug("force redraw invalidate failed", "error", err)
	}
}

// Replay of the output history, swapped by tests
var replayOutputHistoryFn = replayOutputHistory

// Repaints after a terminal focus in report, at most once per minInterval seconds
func (c *CLI) scheduleFocusRegainRedraw(minInterval float64) {
	now := monotonic()
	if last := c.tui.lastFocusRedraw; last != nil && now-*last < minInterval {
		return
	}
	c.tui.lastFocusRedraw = &now
	c.forceFullRedraw()
}

// Reports whether redraw and resize recovery also clear the scrollback, display.cli_rebuild_scrollback_on_redraw
func redrawRebuildsScrollback() bool {
	raw := cliMap(CLIConfig(nil), "display")["cli_rebuild_scrollback_on_redraw"]
	if s, ok := raw.(string); ok {
		switch strings.ToLower(strings.TrimSpace(s)) {
		case "1", "true", "yes", "on", "always":
			return true
		}
		return false
	}
	return pyTruthy(raw)
}

// Python truth of a config value
func pyTruthy(v any) bool {
	switch x := v.(type) {
	case nil:
		return false
	case bool:
		return x
	case string:
		return x != ""
	case int:
		return x != 0
	case int64:
		return x != 0
	case float64:
		return x != 0
	}
	return true
}

// Drains stray input and repaints after an interrupted turn, skipped on a dead terminal
func (c *CLI) recoverTerminalAfterInterrupt() {
	if c.terminalBroken() {
		return
	}
	console().FlushStdin()
	c.forceFullRedraw()
}

// Clears the terminal and resets the renderer, returning the fit for the replay that follows, nil for the whole history
func (c *CLI) clearPromptToolkitScreen(app tuiScreenApp, rebuildScrollback, keepAbove bool) (fit *replayFit) {
	if c.terminalBroken() {
		return noReplay()
	}
	defer func() {
		if r := recover(); r != nil {
			if err, ok := r.(error); ok && isEIO(err) {
				c.markTerminalIOBroken("clear_screen")
			}
			fit = noReplay()
		}
	}()
	renderer := app.Renderer()
	out := renderer.Output()
	rows, _, err := out.GetSize()
	if err != nil {
		panic(err)
	}
	out.ResetAttributes()
	if rebuildScrollback {
		out.EraseScreen()
		if err := out.WriteRaw("\x1b[3J"); err != nil {
			panic(err)
		}
		out.CursorGoto(0, 0)
		setChromeFloor(nil)
	} else {
		room, columns, painted := transcriptRoom(app)
		room += takeSuspectRows()
		reflows, known := terminalReflowsFn()
		keepAbove = keepAbove && known && !reflows
		var historyRows *int
		if keepAbove {
			historyRows = outputHistoryRows(room, columns, painted)
		}
		if keepAbove && historyRows != nil {
			if err := out.WriteRaw("\r"); err != nil {
				panic(err)
			}
			_, cursorY := renderer.CursorPos()
			out.CursorUp(cursorY + *historyRows)
			out.EraseDown()
			fit = &replayFit{Rows: room, Columns: columns, Painted: painted}
			if painted && currentChromeFloor() != nil {
				top := room - *historyRows
				fit.Top = &top
			}
		} else {
			for row := 1; row <= rows; row++ {
				out.CursorGoto(row, 0)
				out.EraseEndOfLine()
			}
			out.CursorGoto(0, 0)
			fit = &replayFit{Rows: room, Columns: columns, Painted: painted}
			if painted {
				top := 0
				fit.Top = &top
			}
		}
	}
	if err := out.Flush(); err != nil {
		panic(err)
	}
	renderer.Reset()
	return fit
}

// Terminal reflow probe, swapped by tests
var terminalReflowsFn = terminalReflows

// Viewport rows the transcript fills above the chrome, the columns, and whether rows count at their painted width
func transcriptRoom(app tuiScreenApp) (rows, columns int, painted bool) {
	renderer := app.Renderer()
	sizeRows, sizeColumns, _ := renderer.Output().GetSize()
	_, drawn, hasScreen := renderer.LastScreen()
	if !hasScreen {
		drawn = app.PreferredHeight(sizeColumns, sizeRows)
	}
	floor := 0
	if f := currentChromeFloor(); f != nil {
		floor = *f
	}
	reflows, known := terminalReflowsFn()
	return max(0, sizeRows-max(renderer.MinAvailableHeight(), drawn, floor)), sizeColumns, !(known && reflows)
}

// Cells each row of the renderer's last paint reaches
func paintedRowWidths(renderer tuiRenderer) []int {
	if _, _, painted := renderer.LastScreen(); !painted {
		return nil
	}
	return renderer.PaintedRowWidths()
}

// Remembers when each chrome row was written and how wide, full meaning the whole chrome was written anew
func (c *CLI) noteChromePaint(app tuiScreenApp, full bool) {
	renderer := app.Renderer()
	_, lastColumns, haveSize := renderer.LastSize()
	suspect := true
	if haveSize {
		if _, columns, err := renderer.Output().GetSize(); err == nil {
			suspect = columns != lastColumns
		}
	}
	now := monotonic()
	var previous [][]chromePaint
	if !full {
		previous = c.tui.chromePaints
	}
	var paints [][]chromePaint
	for y, width := range paintedRowWidths(renderer) {
		var history []chromePaint
		if y < len(previous) {
			history = previous[y]
		}
		if len(history) == 0 || history[len(history)-1].width != width {
			if len(history) > 3 {
				history = history[len(history)-3:]
			}
			history = append(append([]chromePaint{}, history...), chromePaint{now, width, suspect})
		}
		paints = append(paints, history)
	}
	c.tui.chromePaints = paints
	c.tui.chromePaintsScreen, _, _ = renderer.LastScreen()
}

// Ceiling of a over b
func ceilDiv(a, b int) int { return (a + b - 1) / b }

// Points the renderer's erase at the top of its last paint as a reflowing terminal re-wrapped it to columns
func (c *CLI) aimEraseAtReflowedChrome(app tuiScreenApp, columns int) {
	renderer := app.Renderer()
	screen, _, painted := renderer.LastScreen()
	if !painted || columns <= 0 {
		return
	}
	var paints [][]chromePaint
	if c.tui.chromePaintsScreen == screen {
		paints = c.tui.chromePaints
	}
	seen := monotonic()
	if c.tui.resizeSeenAt != nil {
		seen = *c.tui.resizeSeenAt
	}
	cutoff := seen - resizePaintMargin
	cursorX, cursorY := renderer.CursorPos()
	rows := cursorX / columns
	for y := 0; y < cursorY; y++ {
		var history []chromePaint
		if y < len(paints) {
			history = paints[y]
		}
		settled := -1
		for i, p := range history {
			if p.at < cutoff && !p.suspect {
				settled = i
			}
		}
		if settled < 0 {
			rows++
			continue
		}
		narrowest := history[settled].width
		for _, p := range history[settled:] {
			narrowest = min(narrowest, p.width)
		}
		rows += ceilDiv(narrowest, columns)
	}
	renderer.SetCursorPos(cursorX, rows)
}

// Recovers a resized prompt, aiming the erase on a reflowing terminal and refilling the viewport on one that truncates
func (c *CLI) recoverAfterResize(app tuiScreenApp, originalOnResize func()) {
	defer func() {
		if r := recover(); r != nil {
			cliLogger.Debug(fmt.Sprintf("resize recovery failed: %v", r))
		}
		c.uiMu.Lock()
		pending := c.resizeRecoveryPending
		c.uiMu.Unlock()
		if !pending {
			c.tui.resizeHoldSince = nil
			releasePaints()
		}
	}()
	c.uiMu.Lock()
	monitor := c.subagentMonitor
	c.uiMu.Unlock()
	if monitor != nil && monitor.isOpening() {
		return
	}
	c.statusBarSuppressedAfterResize.Store(true)
	newWidth, haveNew := c.resizeWidth()
	prev := c.lastResizeWidth
	narrowest, haveNarrowest := 0, false
	for _, w := range []*int{newWidthPtr(newWidth, haveNew), c.tui.resizeNarrowest} {
		if w != nil && *w != 0 && (!haveNarrowest || *w < narrowest) {
			narrowest, haveNarrowest = *w, true
		}
	}
	c.tui.resizeNarrowest = nil
	widthChanged := haveNew && prev != nil && newWidth != *prev
	narrowed := haveNarrowest && prev != nil && narrowest < *prev
	reflows, known := terminalReflowsFn()
	reflows = reflows && known
	switch {
	case widthChanged && redrawRebuildsScrollback():
		c.clearPromptToolkitScreen(app, true, false)
		replayOutputHistoryFn(nil, nil)
	case reflows && widthChanged && newWidth < *prev:
		c.aimEraseAtReflowedChrome(app, newWidth)
	case !reflows && narrowed:
		replayOutputHistoryFn(c.clearPromptToolkitScreen(app, false, true), app)
	}
	if haveNew {
		c.lastResizeWidth = &newWidth
	}
	if widthChanged {
		c.petQueueKittyFrame()
	}
	originalOnResize()
	c.scheduleStatusBarUnsuppressFn(app, 0.35)
}

// Pointer to a width when one was read
func newWidthPtr(width int, ok bool) *int {
	if !ok {
		return nil
	}
	return &width
}

// Terminal width the resize recovery settles at, false when it cannot be read
func (c *CLI) resizeWidth() (width int, ok bool) {
	defer func() {
		if recover() != nil {
			width, ok = 0, false
		}
	}()
	if c.tui.resizeWidthProbe != nil {
		return c.tui.resizeWidthProbe()
	}
	return c.getTUITerminalWidth(), true
}

// Cancels the timer in the slot and starts a new one whose callback learns its own timer
func (c *CLI) restartDebounceTimer(slot **time.Timer, delay float64, fn func(timer *time.Timer)) {
	if old := *slot; old != nil {
		old.Stop()
	}
	var timer *time.Timer
	ready := make(chan struct{})
	timer = time.AfterFunc(time.Duration(delay*float64(time.Second)), func() {
		<-ready
		fn(timer)
	})
	*slot = timer
	close(ready)
}

// Clears the status bar suppression once the reflow settles, a fresh resize restarting the wait
func (c *CLI) scheduleStatusBarUnsuppress(app tuiScreenApp, delay float64) {
	clear := func() {
		c.statusBarSuppressedAfterResize.Store(false)
		_ = app.Invalidate()
	}
	c.resizeRecoveryLock.Lock()
	defer c.resizeRecoveryLock.Unlock()
	c.restartDebounceTimer(&c.statusBarUnsuppressTimer, delay, func(*time.Timer) { runOnAppLoop(app, clear) })
}

// Status bar unsuppress scheduler of this CLI, the real one unless a test replaced it
func (c *CLI) scheduleStatusBarUnsuppressFn(app tuiScreenApp, delay float64) {
	if c.tui.unsuppressOverride != nil {
		c.tui.unsuppressOverride(app, delay)
		return
	}
	c.scheduleStatusBarUnsuppress(app, delay)
}

// Debounces resize recovery, a width change holding output paints until its recovery runs
func (c *CLI) scheduleResizeRecovery(app tuiScreenApp, originalOnResize func(), delay float64) {
	width, haveWidth := 0, false
	if _, columns, err := app.Renderer().Output().GetSize(); err == nil {
		width, haveWidth = columns, true
	}
	timerFired := func(timer *time.Timer) {
		runOnAppLoop(app, func() {
			c.resizeRecoveryLock.Lock()
			if c.resizeRecoveryTimer != timer {
				c.resizeRecoveryLock.Unlock()
				return
			}
			c.resizeRecoveryTimer = nil
			c.uiMu.Lock()
			c.resizeRecoveryPending = false
			c.uiMu.Unlock()
			c.resizeRecoveryLock.Unlock()
			c.recoverAfterResize(app, originalOnResize)
		})
	}
	c.resizeRecoveryLock.Lock()
	defer c.resizeRecoveryLock.Unlock()
	now := monotonic()
	c.uiMu.Lock()
	pending := c.resizeRecoveryPending
	c.uiMu.Unlock()
	if pending || !haveWidth || c.lastResizeWidth == nil || width != *c.lastResizeWidth {
		if !pending {
			c.uiMu.Lock()
			c.resizeRecoveryPending = true
			c.uiMu.Unlock()
			c.tui.resizeSeenAt = &now
			if c.tui.resizeHoldSince == nil {
				c.tui.resizeHoldSince = &now
			}
			c.tui.resizeNarrowest = nil
			holdPaints()
		}
		if haveWidth && width > 0 {
			narrowest := width
			if c.tui.resizeNarrowest != nil && *c.tui.resizeNarrowest != 0 {
				narrowest = min(width, *c.tui.resizeNarrowest)
			}
			c.tui.resizeNarrowest = &narrowest
		}
		if c.tui.resizeHoldSince != nil && now-*c.tui.resizeHoldSince >= resizeHoldMax {
			delay = resizeSettle
		}
	}
	c.restartDebounceTimer(&c.resizeRecoveryTimer, delay, timerFired)
}

// Routes application resizes through the debounced recovery and seeds the width baseline
func (c *CLI) installResizeRecovery(app *ptApplication) {
	c.lastResizeWidth = nil
	if _, columns, err := app.GetSize(); err == nil && columns > 0 {
		c.lastResizeWidth = &columns
	} else if columns, _ := pyshutil.GetTerminalSize(80, 24); columns > 0 {
		c.lastResizeWidth = &columns
	}
	original := app.applyResize
	c.tui.resizeOriginalOnResize = original
	app.onResizeSignal = func() { c.scheduleResizeRecovery(app, original, 0.3) }
	app.redrawGate = func(renderAsDone bool) bool {
		if !renderAsDone && c.outputWaitsForResize(app) {
			return false
		}
		if floor := currentChromeFloor(); floor != nil && *floor != 0 {
			app.SetMinAvailableHeight(max(app.MinAvailableHeight(), *floor))
		}
		return true
	}
	app.afterRedraw = func(full bool) { c.noteChromePaint(app, full) }
	setPaintGate(app, func() bool { return c.outputWaitsForResize(app) })
}

// Reports whether output must wait for a resize recovery, scheduling one when the width moved unseen
func (c *CLI) outputWaitsForResize(app tuiScreenApp) bool {
	c.uiMu.Lock()
	pending := c.resizeRecoveryPending
	c.uiMu.Unlock()
	if pending {
		return true
	}
	_, width, err := app.Renderer().Output().GetSize()
	if err != nil {
		return false
	}
	if c.lastResizeWidth != nil && width == *c.lastResizeWidth {
		return false
	}
	c.scheduleResizeRecovery(app, c.tui.resizeOriginalOnResize, 0.3)
	c.uiMu.Lock()
	defer c.uiMu.Unlock()
	return c.resizeRecoveryPending
}

// Saves a clipboard image under the images directory and attaches it, true when one was attached
func (c *CLI) tryAttachClipboardImage() bool {
	c.imageCounter++
	path := filepath.Join(home.Dir(c.ctx), "images", fmt.Sprintf("clip_%s_%d.png", time.Now().Format("20060102_150405"), c.imageCounter))
	if clipSaveImage(path) {
		c.attachedImages = append(c.attachedImages, path)
		return true
	}
	c.imageCounter--
	return false
}

// Copies text to the terminal clipboard through OSC 52, wrapped for tmux and screen passthrough
func (c *CLI) writeOSC52Clipboard(text string) {
	seq := "\x1b]52;c;" + base64.StdEncoding.EncodeToString([]byte(text)) + "\x07"
	switch {
	case os.Getenv("TMUX") != "":
		seq = "\x1bPtmux;" + strings.ReplaceAll(seq, "\x1b", "\x1b\x1b") + "\x1b\\"
	case os.Getenv("STY") != "":
		seq = "\x1bP" + seq + "\x1b\\"
	}
	c.uiMu.Lock()
	app := c.app
	c.uiMu.Unlock()
	_ = writeTerminalSequence(app, seq)
}

// Resets terminal input modes after leaked mouse reports, at most twice a second, then asks for extended keys again
func (c *CLI) recoverTerminalInputModes(reason string) {
	now := monotonic()
	if last := c.lastInputModeRecovery; last != nil && now-*last < 0.5 {
		return
	}
	c.lastInputModeRecovery = &now
	c.uiMu.Lock()
	app := c.app
	c.uiMu.Unlock()
	if err := writeTerminalSequence(app, terminalInputModeResetSeq); err != nil {
		return
	}
	cfg := c.config
	if len(cfg) == 0 {
		cfg = CLIConfig(c.ctx)
	}
	if cliMultilineShortcutsEnabled(cfg) {
		enableExtendedEnterKeys(app, nil)
	}
	cliLogger.Warn("Recovered terminal input modes after leak: " + reason)
	if !c.inputModeRecoveryNoticeShown {
		c.inputModeRecoveryNoticeShown = true
		cprint("  " + dimANSI + c.t("cli.terminal.recovered_mouse_modes", nil) + resetANSI)
	}
}

// Platform and stdin probes of the cooked mode watchdog, swapped by tests
var (
	termiosDriftGOOS  = runtime.GOOS
	termiosDriftStdin = func() (fd int, tty bool) { return int(os.Stdin.Fd()), console().StdinIsTTY() }
	healCookedDriftFn = healCookedModeDrift
)

// Heals a terminal that drifted to cooked mode while the prompt owns raw mode, checked once a second while idle
func (c *CLI) checkTermiosDrift() {
	if termiosDriftGOOS == "windows" {
		return
	}
	c.uiMu.Lock()
	app := c.app
	c.uiMu.Unlock()
	if app == nil || !app.IsRunning() || app.TerminalBusy() {
		return
	}
	now := monotonic()
	if last := c.lastTermiosDriftCheck; last != nil && now-*last < 1.0 {
		return
	}
	c.lastTermiosDriftCheck = &now
	fd, tty := termiosDriftStdin()
	if !tty {
		return
	}
	if healCookedDriftFn(fd) {
		cliLogger.Warn("Healed cooked-mode termios drift on stdin — a run_in_terminal cooked→raw restore was lost.")
		_ = c.invalidate()
		if !c.termiosDriftNoticeShown {
			c.termiosDriftNoticeShown = true
			cprint("  " + dimANSI + c.t("cli.terminal.recovered_cooked_mode", nil) + resetANSI)
		}
	}
}
