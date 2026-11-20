package tui

import (
	"context"
	"math"
	"os"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/nickheyer/icebath/pkg/tuigateway"
)

const (
	configPollEvery = 5000 * time.Millisecond
	mcpRetryBaseMs  = 5000
	mcpRetryMaxMs   = 5 * 60_000
)

var (
	falsyToggle       = regexp.MustCompile(`(?i)^(?:0|false|no|off)$`)
	defaultIndicator  = "unicode"
	defaultBusyMode   = "queue"
	defaultPasteLines = 5
	defaultPasteChars = 2000
)

// Toggle of an environment value, nil when empty or neither truthy nor falsy
func parseToggle(v string) *bool {
	raw := strings.TrimFunc(v, isJSSpace)
	on := truthyToggle.MatchString(raw)
	if raw == "" || (!on && !falsyToggle.MatchString(raw)) {
		return nil
	}
	return &on
}

// Mouse tracking of the first frame from the native mode, the override, Termux and the legacy disable flag
func bootMouseTracking() string {
	env := func(name string) string { return strings.TrimFunc(os.Getenv(name), isJSSpace) }
	enabled := !truthyToggle.MatchString(env("ICEBATH_TUI_DISABLE_MOUSE"))
	if termuxTUIMode {
		enabled = false
	}
	if override := parseToggle(os.Getenv("ICEBATH_TUI_MOUSE_TRACKING")); override != nil {
		enabled = *override
	}
	if truthyToggle.MatchString(env("ICEBATH_TUI_NATIVE")) {
		enabled = false
	}
	if enabled {
		return "all"
	}
	return "off"
}

// Status bar fields as a set of trimmed lower case names, nil for every field
func statusBarFieldSet(fields []string) map[string]bool {
	set := make(map[string]bool, len(fields))
	for _, name := range fields {
		if name = strings.ToLower(strings.TrimFunc(name, isJSSpace)); name != "" {
			set[name] = true
		}
	}
	if len(set) == 0 {
		return nil
	}
	return set
}

// Whether the status bar shows a fact, under any of the names a field list may call it
func (ui uiState) statusField(names ...string) bool {
	if ui.StatusBar == "off" {
		return false
	}
	if len(ui.StatusBarFields) == 0 {
		return true
	}
	for _, name := range names {
		if ui.StatusBarFields[name] {
			return true
		}
	}
	return false
}

// Per section detail modes of the wire as the UI state keeps them
func sectionModes(sections map[string]tuigateway.DetailsMode) map[string]string {
	out := make(map[string]string, len(sections))
	for name, mode := range sections {
		out[name] = string(mode)
	}
	return out
}

// Config surface the sync sets outside the store, the main model in production
type configApp interface {
	setBellOnComplete(on bool)
	setBellOnPrompt(on bool)
	setVoiceEnabled(on bool)
	setVoiceTTS(on bool)
	setVoiceRecordKey(k voiceRecordKey)
	inlineImageRetry()
}

// Refused MCP revision and when it may be retried
type mcpRefusal struct {
	failures int
	rev      string
	retryAt  float64
}

// Accepted MCP revision, a reload in flight and the last refusal
type mcpRevState struct {
	accepted string
	inFlight bool
	refused  *mcpRefusal
}

// Poll tick of the config mtime for one session generation
type configPollMsg struct{ gen int }

// Config of the session pushed into the UI state, polled for changes and MCP reloads
type configSync struct {
	store *tuiStore
	turn  *turnController
	gw    turnGateway
	app   configApp
	mtime float64
	mcp   mcpRevState
	gen   int
	sid   string
}

// Sync of one app
func newConfigSync(store *tuiStore, turn *turnController, gw turnGateway, app configApp) *configSync {
	return &configSync{store: store, turn: turn, gw: gw, app: app}
}

// Whether the session generation is still the one in view
func (c *configSync) current(gen int, sid string) bool { return gen == c.gen && c.store.ui.Sid == sid }

// Applies the settings of a config to the UI state, the defaults when the read failed, the busy mode only when applyBusy
func (c *configSync) applyDisplay(cfg *tuigateway.ClientConfig, applyBusy bool) tea.Cmd {
	settings := tuigateway.ClientConfigOf(nil)
	if cfg != nil {
		settings = *cfg
	}
	d := settings.Display
	c.app.setBellOnComplete(d.BellOnComplete)
	c.app.setBellOnPrompt(d.BellOnPrompt)
	cmd := c.store.applyConfiguredTuiTheme(d.Theme)
	ui := &c.store.ui
	if cfg != nil {
		key := defaultVoiceRecordKey
		if settings.Voice.RecordKey != "" {
			key = parseVoiceRecordKey(settings.Voice.RecordKey)
		}
		c.app.setVoiceRecordKey(key)
		ui.DestructiveSlashConfirm = settings.Approvals.DestructiveSlashConfirm
	}
	ui.Battery = d.Battery
	if applyBusy {
		ui.BusyInputMode = string(d.BusyInputMode)
	}
	ui.Compact = d.Compact
	ui.DetailsMode, ui.DetailsModeCommandOverride, ui.FocusView = string(d.DetailsMode), false, d.FocusView
	ui.IndicatorStyle, ui.InlineDiffs, ui.MouseTracking = string(d.Indicator), d.InlineDiffs, string(d.MouseTracking)
	ui.InlineImages = d.InlineImages
	c.app.inlineImageRetry()
	ui.PasteCollapseLines, ui.PasteCollapseChars = int(settings.PasteCollapseThreshold), int(settings.PasteCollapseCharThreshold)
	ui.Sections, ui.ShowReasoning = sectionModes(d.Sections), d.ShowReasoning
	ui.StatusBar, ui.StatusBarFields = string(d.StatusBarPlace), statusBarFieldSet(d.StatusBar.Fields)
	ui.Streaming, ui.Timestamps = d.Streaming, d.Timestamps
	ui.Arena, ui.PetRenderMode = d.Arena, d.Pet.RenderMode
	ui.TurnSummary, ui.SpinnerTokenFlow = d.TurnSummary, d.SpinnerTokenFlow
	return cmd
}

// Fetches the full config and the owner's busy mode for the session and applies them while it stays current
func (c *configSync) hydrateFullConfig(gen int, sid string) tea.Cmd {
	gw, canonical := c.gw, c.gw.canonical()
	return func() tea.Msg {
		var full, busy *tuigateway.ConfigGetResult
		var wg sync.WaitGroup
		wg.Add(1)
		go func() {
			defer wg.Done()
			full = quietAsk(gw, tuigateway.ConfigGet, tuigateway.ConfigGetParams{Key: "full", SessionID: sid})
		}()
		if canonical {
			busy = quietAsk(gw, tuigateway.ConfigGet, tuigateway.ConfigGetParams{Key: "busy", SessionID: sid})
		}
		wg.Wait()
		return asyncDoneMsg{run: func() tea.Cmd {
			if !c.current(gen, sid) {
				return nil
			}
			var cfg *tuigateway.ClientConfig
			if full != nil {
				cfg = full.Config
			}
			cmd := c.applyDisplay(cfg, !canonical)
			if mode := configWord(busy); canonical && slices.Contains(tuigateway.BusyInputMode("").Values(), mode) {
				c.store.ui.BusyInputMode = mode
			}
			if cfg == nil {
				return cmd
			}
			language := cfg.Display.Language
			return tea.Batch(cmd, func() tea.Msg {
				syncTUILocale(func(method string, params map[string]any) (any, error) {
					return gw.request(context.Background(), method, params, 0)
				}, language)
				return nil
			})
		}}
	}
}

// Word a config answer carries, empty for none or a flag
func configWord(r *tuigateway.ConfigGetResult) string {
	if r == nil || r.Value == nil {
		return ""
	}
	return r.Value.Text
}

// Voice mode of a canonical session, enabled for voice_only and all and speaking its replies for all
func (c *configSync) hydrateVoiceMode(gen int, sid string) tea.Cmd {
	if !c.gw.canonical() {
		return nil
	}
	gw := c.gw
	return func() tea.Msg {
		r := quietAsk(gw, tuigateway.ConfigGet, tuigateway.ConfigGetParams{Key: "voice", SessionID: sid})
		return asyncDoneMsg{run: func() tea.Cmd {
			if r != nil && c.current(gen, sid) {
				mode := configWord(r)
				c.app.setVoiceEnabled(mode == "voice_only" || mode == "all")
				c.app.setVoiceTTS(mode == "all")
			}
			return nil
		}}
	}
}

// Mtime and MCP revision of the session's config, zero and empty when the read failed
func (c *configSync) readMtime(sid string, then func(mtime float64, rev string) tea.Cmd) tea.Cmd {
	gw := c.gw
	return func() tea.Msg {
		r := quietAsk(gw, tuigateway.ConfigGet, tuigateway.ConfigGetParams{Key: "mtime", SessionID: sid})
		mtime, rev := 0.0, ""
		if r != nil {
			mtime, rev = val(r.Mtime), r.McpRev
		}
		return asyncDoneMsg{run: func() tea.Cmd { return then(mtime, rev) }}
	}
}

// Starts syncing a session's config, stopping the previous session's polls
func (c *configSync) syncSession(sid string) tea.Cmd {
	c.gen++
	c.sid = sid
	if sid == "" {
		return nil
	}
	gen := c.gen
	c.app.setVoiceEnabled(os.Getenv("ICEBATH_VOICE") == "1")
	if c.gw.canonical() {
		c.app.setVoiceTTS(false)
	}
	return tea.Batch(
		c.hydrateVoiceMode(gen, sid),
		c.readMtime(sid, func(mtime float64, rev string) tea.Cmd {
			if !c.current(gen, sid) {
				return nil
			}
			c.mtime, c.mcp.accepted = mtime, rev
			return nil
		}),
		c.hydrateFullConfig(gen, sid),
		c.schedulePoll(gen),
	)
}

func (c *configSync) schedulePoll(gen int) tea.Cmd {
	return tea.Tick(configPollEvery, func(time.Time) tea.Msg { return configPollMsg{gen: gen} })
}

// One poll of the config mtime, reloading MCP servers and the config when they changed
func (c *configSync) onPoll(m configPollMsg) tea.Cmd {
	if m.gen != c.gen || c.sid == "" {
		return nil
	}
	gen, sid := m.gen, c.sid
	poll := c.readMtime(sid, func(next float64, nextMcpRev string) tea.Cmd {
		if !c.current(gen, sid) {
			return nil
		}
		if c.mtime == 0 || math.IsNaN(c.mtime) {
			if next != 0 && !math.IsNaN(next) {
				c.mtime, c.mcp.accepted = next, nextMcpRev
			}
			return nil
		}
		cmds := []tea.Cmd{}
		if nextMcpRev != "" {
			cmds = append(cmds, c.syncMcpReload(sid, nextMcpRev))
		}
		if next == 0 || math.IsNaN(next) || next == c.mtime {
			return tea.Batch(cmds...)
		}
		c.mtime = next
		if nextMcpRev == "" {
			gw := c.gw
			cmds = append(cmds, func() tea.Msg {
				r := quietAsk(gw, tuigateway.ReloadMCP, tuigateway.ReloadMCPParams{SessionID: optionalText(sid), Confirm: true})
				return asyncDoneMsg{run: func() tea.Cmd {
					if r != nil {
						c.turnActivity(tr("status.mcpReloaded"))
					}
					return nil
				}}
			})
		}
		return tea.Batch(append(cmds, c.hydrateFullConfig(gen, sid))...)
	})
	return tea.Batch(poll, c.schedulePoll(gen))
}

// Pushes an info activity line onto the live turn
func (c *configSync) turnActivity(text string) { c.turn.pushActivity(text, "info", "") }

// Reloads MCP servers for a new revision, backing off exponentially after the owner refused it
func (c *configSync) syncMcpReload(sid, nextMcpRev string) tea.Cmd {
	state := &c.mcp
	if nextMcpRev == "" || nextMcpRev == state.accepted || state.inFlight {
		return nil
	}
	var refused *mcpRefusal
	if state.refused != nil && state.refused.rev == nextMcpRev {
		refused = state.refused
	}
	if refused != nil && c.store.nowMillis() < refused.retryAt {
		return nil
	}
	state.inFlight = true
	gw := c.gw
	return func() tea.Msg {
		r := quietAsk(gw, tuigateway.ReloadMCP, tuigateway.ReloadMCPParams{SessionID: optionalText(sid), Confirm: true, Rev: optionalText(nextMcpRev)})
		return asyncDoneMsg{run: func() tea.Cmd {
			state.inFlight = false
			if r != nil && r.Status == "reloaded" {
				state.accepted = firstText(val(r.LoadedRev), nextMcpRev)
				state.refused = nil
				c.turnActivity(tr("status.mcpReloaded"))
				return nil
			}
			failures := 1
			if refused != nil {
				failures = refused.failures + 1
			}
			state.refused = &mcpRefusal{failures: failures, rev: nextMcpRev, retryAt: c.store.nowMillis() + math.Min(mcpRetryBaseMs*math.Pow(2, float64(failures)), mcpRetryMaxMs)}
			return nil
		}}
	}
}
