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

// Platform and stdin probes of the cooked mode watchdog, swapped by tests
var (
	termiosDriftGOOS  = runtime.GOOS
	termiosDriftStdin = func() (fd int, tty bool) { return int(os.Stdin.Fd()), console().StdinIsTTY() }
	healCookedDriftFn = healCookedModeDrift
)
