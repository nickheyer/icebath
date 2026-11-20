package tui

import (
	"os"
	"runtime"
	"strconv"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/nickheyer/icebath/pkg/ui/render"
)

// Gap between two looks for a session to stand and how many looks a notice of the start waits through
const (
	sessionWaitEvery = 500 * time.Millisecond
	sessionWaitTries = 120
)

// Runs tell once a session stands, at once when one does, since the start of a session replaces what the transcript held before it, and after
// the last look all the same
func (a *app) onceSessionStands(tries int, tell func()) tea.Cmd {
	if a.store.ui.Sid == "" && tries > 0 {
		return tea.Tick(sessionWaitEvery, func(time.Time) tea.Msg {
			return asyncDoneMsg{run: func() tea.Cmd { return a.onceSessionStands(tries-1, tell) }}
		})
	}
	tell()
	return nil
}

// Environment variables the light or dark detection reads, in the order it weighs them
var themeDetectionInputs = []string{"ICEBATH_TUI_BACKGROUND", "ICEBATH_TUI_THEME", "ICEBATH_TUI_LIGHT", "COLORFGBG", "TERM_PROGRAM"}

// Prints where a memory dump went, its heap profile and its facts file, or why it failed
func (a *app) showHeapDump(dump heapDumpResult) {
	if dump.err != nil {
		a.sys(tr("slashCmd.debug.heapdump.failed", dump.err.Error()))
		return
	}
	a.sys(tr("slashCmd.debug.heapdump.heapPath", dump.heapPath))
	a.sys(tr("slashCmd.debug.heapdump.diagPath", dump.diagPath))
}

// Rows of the theme panel: what the terminal answered, what the detection read and decided, the brand, the painted defaults and every color slot
func (a *app) themeInfoRows() [][2]string {
	t, themes := a.store.ui.Theme, a.store.themes
	unset := tr("slashCmd.debug.themeInfo.unset")
	orUnset := func(value string) string {
		if value == "" {
			return unset
		}
		return value
	}
	answer := func(hex *string) string {
		if hex == nil {
			return tr("slashCmd.debug.themeInfo.noReply")
		}
		return *hex
	}
	rows := [][2]string{
		{tr("slashCmd.debug.themeInfo.osc11Background"), answer(themes.reportedBg)},
		{tr("slashCmd.debug.themeInfo.osc10Foreground"), answer(themes.reportedFg)},
	}
	for _, name := range themeDetectionInputs {
		value, set := os.LookupEnv(name)
		if !set {
			value = unset
		}
		rows = append(rows, [2]string{name, value})
	}
	mode := tr("slashCmd.debug.themeInfo.dark")
	if detectLightMode(os.Getenv) {
		mode = tr("slashCmd.debug.themeInfo.light")
	}
	c := t.Color
	return append(rows, [][2]string{
		{tr("slashCmd.debug.themeInfo.detectedMode"), mode},
		{tr("slashCmd.debug.themeInfo.brand"), t.Brand.Name},
		{tr("slashCmd.debug.themeInfo.promptSymbol"), t.Brand.Prompt},
		{tr("slashCmd.debug.themeInfo.paintedBackground"), orUnset(themes.terminalBg)},
		{tr("slashCmd.debug.themeInfo.paintedForeground"), orUnset(themes.terminalFg)},
		{"primary", c.Primary}, {"accent", c.Accent}, {"border", c.Border}, {"text", c.Text}, {"muted", c.Muted},
		{"completionBg", c.CompletionBg}, {"completionCurrentBg", c.CompletionCurrentBg}, {"completionMetaBg", c.CompletionMetaBg},
		{"completionMetaCurrentBg", c.CompletionMetaCurrentBg}, {"label", c.Label}, {"ok", c.Ok}, {"error", c.Error}, {"warn", c.Warn},
		{"tool", c.Tool}, {"thinking", c.Thinking}, {"syntaxString", c.SyntaxString}, {"syntaxNumber", c.SyntaxNumber},
		{"syntaxKeyword", c.SyntaxKeyword}, {"syntaxComment", c.SyntaxComment}, {"prompt", c.Prompt}, {"sessionLabel", c.SessionLabel},
		{"sessionBorder", c.SessionBorder}, {"statusBg", c.StatusBg}, {"statusFg", c.StatusFg}, {"statusGood", c.StatusGood},
		{"statusWarn", c.StatusWarn}, {"statusBad", c.StatusBad}, {"statusCritical", c.StatusCritical}, {"selectionBg", c.SelectionBg},
		{"diffAdded", c.DiffAdded}, {"diffRemoved", c.DiffRemoved}, {"diffAddedWord", c.DiffAddedWord}, {"diffRemovedWord", c.DiffRemovedWord},
		{"shellDollar", c.ShellDollar},
	}...)
}

// Rows of the memory panel: the heap, what the runtime holds, the collector, what the UI keeps, the resident size and the uptime
func (a *app) memoryRows() [][2]string {
	var stats runtime.MemStats
	runtime.ReadMemStats(&stats)
	snap := sampleMemory()
	size := func(n uint64) string { return render.Bytes(int64(n)) }
	count := func(n int) string { return strconv.Itoa(n) }
	return [][2]string{
		{tr("slashCmd.debug.mem.heapUsed"), size(stats.HeapAlloc)},
		{tr("slashCmd.debug.mem.heapLive"), size(snap.heap)},
		{tr("slashCmd.debug.mem.heapTotal"), size(stats.HeapSys - stats.HeapReleased)},
		{tr("slashCmd.debug.mem.heapObjects"), strconv.FormatUint(stats.HeapObjects, 10)},
		{tr("slashCmd.debug.mem.fromSystem"), size(stats.Sys)},
		{tr("slashCmd.debug.mem.collections"), strconv.FormatUint(uint64(stats.NumGC), 10)},
		{tr("slashCmd.debug.mem.goroutines"), count(runtime.NumGoroutine())},
		{tr("slashCmd.debug.mem.transcriptRows"), count(len(a.history))},
		{tr("slashCmd.debug.mem.blocksKept"), count(len(a.card.view.cache))},
		{tr("slashCmd.debug.mem.picturesKept"), count(len(a.images.renders))},
		{tr("slashCmd.debug.mem.rss"), size(snap.rss)},
		{tr("slashCmd.debug.mem.uptime"), tr("slashCmd.debug.mem.seconds", strconv.FormatFloat(time.Since(processStarted).Seconds(), 'f', 0, 64))},
	}
}

// Commands that inspect the terminal UI itself, its theme, its memory and its widgets
func (a *app) debugCommands() []slashCommand {
	return []slashCommand{
		{name: "theme-info", help: "show what the theme was detected from, its brand and its colors", run: func(r *slashRun) tea.Cmd {
			a.panel(tr("slashCmd.debug.themeInfo.panelTitle"), []panelSection{{Rows: a.themeInfoRows()}})
			return nil
		}},
		{name: "mem", help: "show the memory the terminal UI holds", run: func(r *slashRun) tea.Cmd {
			a.panel(tr("slashCmd.debug.mem.panelTitle"), []panelSection{{Rows: a.memoryRows()}})
			return nil
		}},
		{name: "heapdump", help: "write a heap profile and the memory facts of the terminal UI (see ICEBATH_HEAPDUMP_DIR)", run: func(r *slashRun) tea.Cmd {
			snap := sampleMemory()
			r.sys(tr("slashCmd.debug.heapdump.writing", render.Bytes(int64(snap.heapUsed)), render.Bytes(int64(snap.rss))))
			return func() tea.Msg {
				dump := performHeapDump(dumpManual)
				return asyncDoneMsg{run: func() tea.Cmd {
					if !r.stale() {
						a.showHeapDump(dump)
					}
					return nil
				}}
			}
		}},
		{name: "widgets-reload", help: "scan the plugins again and register the widgets they provide", run: func(r *slashRun) tea.Cmd {
			return a.widgets.rescan(func(loaded []string, failures []WidgetLoadFailure) {
				parts := []string{tr("slashCmd.debug.widgetsReload.none")}
				if len(loaded) > 0 {
					names := make([]string, len(loaded))
					for i, id := range loaded {
						names[i] = "/" + id
					}
					parts[0] = tr("slashCmd.debug.widgetsReload.loaded", strings.Join(names, ", "))
				}
				for _, failure := range failures {
					parts = append(parts, failure.Plugin+": "+failure.Message)
				}
				r.sys(tr("slashCmd.debug.widgetsReload.summary", strings.Join(parts, " · ")))
			})
		}},
	}
}
