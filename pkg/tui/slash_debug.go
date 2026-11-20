package tui

import (
	"os"
	"runtime"
	"strconv"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/nickheyer/icebath/pkg/ui/render"
	"github.com/nickheyer/icebath/pkg/ui/txt"
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
		a.sys(tr(txt.SlashCmdDebugHeapdumpFailed, dump.err.Error()))
		return
	}
	a.sys(tr(txt.SlashCmdDebugHeapdumpHeapPath, dump.heapPath))
	a.sys(tr(txt.SlashCmdDebugHeapdumpDiagPath, dump.diagPath))
}

// Rows of the theme panel: what the terminal answered, what the detection read and decided, the brand, the painted defaults and every color slot
func (a *app) themeInfoRows() [][2]string {
	t, themes := a.store.ui.Theme, a.store.themes
	unset := tr(txt.SlashCmdDebugThemeInfoUnset)
	orUnset := func(value string) string {
		if value == "" {
			return unset
		}
		return value
	}
	answer := func(hex *string) string {
		if hex == nil {
			return tr(txt.SlashCmdDebugThemeInfoNoReply)
		}
		return *hex
	}
	rows := [][2]string{
		{tr(txt.SlashCmdDebugThemeInfoOsc11Background), answer(themes.reportedBg)},
		{tr(txt.SlashCmdDebugThemeInfoOsc10Foreground), answer(themes.reportedFg)},
	}
	for _, name := range themeDetectionInputs {
		value, set := os.LookupEnv(name)
		if !set {
			value = unset
		}
		rows = append(rows, [2]string{name, value})
	}
	mode := tr(txt.SlashCmdDebugThemeInfoDark)
	if detectLightMode(os.Getenv) {
		mode = tr(txt.SlashCmdDebugThemeInfoLight)
	}
	c := t.Color
	return append(rows, [][2]string{
		{tr(txt.SlashCmdDebugThemeInfoDetectedMode), mode},
		{tr(txt.SlashCmdDebugThemeInfoBrand), t.Brand.Name},
		{tr(txt.SlashCmdDebugThemeInfoPromptSymbol), t.Brand.Prompt},
		{tr(txt.SlashCmdDebugThemeInfoPaintedBackground), orUnset(themes.terminalBg)},
		{tr(txt.SlashCmdDebugThemeInfoPaintedForeground), orUnset(themes.terminalFg)},
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
		{tr(txt.SlashCmdDebugMemHeapUsed), size(stats.HeapAlloc)},
		{tr(txt.SlashCmdDebugMemHeapLive), size(snap.heap)},
		{tr(txt.SlashCmdDebugMemHeapTotal), size(stats.HeapSys - stats.HeapReleased)},
		{tr(txt.SlashCmdDebugMemHeapObjects), strconv.FormatUint(stats.HeapObjects, 10)},
		{tr(txt.SlashCmdDebugMemFromSystem), size(stats.Sys)},
		{tr(txt.SlashCmdDebugMemCollections), strconv.FormatUint(uint64(stats.NumGC), 10)},
		{tr(txt.SlashCmdDebugMemGoroutines), count(runtime.NumGoroutine())},
		{tr(txt.SlashCmdDebugMemTranscriptRows), count(len(a.history))},
		{tr(txt.SlashCmdDebugMemBlocksKept), count(len(a.card.view.cache))},
		{tr(txt.SlashCmdDebugMemPicturesKept), count(len(a.images.renders))},
		{tr(txt.SlashCmdDebugMemRss), size(snap.rss)},
		{tr(txt.SlashCmdDebugMemUptime), tr(txt.SlashCmdDebugMemSeconds, strconv.FormatFloat(time.Since(processStarted).Seconds(), 'f', 0, 64))},
	}
}

// Commands that inspect the terminal UI itself, its theme, its memory and its widgets
func (a *app) debugCommands() []slashCommand {
	return []slashCommand{
		{name: "theme-info", help: "show what the theme was detected from, its brand and its colors", run: func(r *slashRun) tea.Cmd {
			a.panel(tr(txt.SlashCmdDebugThemeInfoPanelTitle), []panelSection{{Rows: a.themeInfoRows()}})
			return nil
		}},
		{name: "mem", help: "show the memory the terminal UI holds", run: func(r *slashRun) tea.Cmd {
			a.panel(tr(txt.SlashCmdDebugMemPanelTitle), []panelSection{{Rows: a.memoryRows()}})
			return nil
		}},
		{name: "heapdump", help: "write a heap profile and the memory facts of the terminal UI (see ICEBATH_HEAPDUMP_DIR)", run: func(r *slashRun) tea.Cmd {
			snap := sampleMemory()
			r.sys(tr(txt.SlashCmdDebugHeapdumpWriting, render.Bytes(int64(snap.heapUsed)), render.Bytes(int64(snap.rss))))
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
				parts := []string{tr(txt.SlashCmdDebugWidgetsReloadNone)}
				if len(loaded) > 0 {
					names := make([]string, len(loaded))
					for i, id := range loaded {
						names[i] = "/" + id
					}
					parts[0] = tr(txt.SlashCmdDebugWidgetsReloadLoaded, strings.Join(names, ", "))
				}
				for _, failure := range failures {
					parts = append(parts, failure.Plugin+": "+failure.Message)
				}
				r.sys(tr(txt.SlashCmdDebugWidgetsReloadSummary, strings.Join(parts, " · ")))
			})
		}},
	}
}
