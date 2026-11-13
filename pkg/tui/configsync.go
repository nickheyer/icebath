package tui

import (
	"context"
	"math"
	"os"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	tea "charm.land/bubbletea/v2"
)

const (
	configPollEvery = 5000 * time.Millisecond
	mcpRetryBaseMs  = 5000
	mcpRetryMaxMs   = 5 * 60_000
)

var (
	falsyToggle       = regexp.MustCompile(`(?i)^(?:0|false|no|off)$`)
	statusBarAlias    = map[string]string{"bottom": "bottom", "off": "off", "on": "top", "top": "top"}
	busyInputModes    = map[string]bool{"interrupt": true, "queue": true, "steer": true}
	indicatorStyles   = map[string]bool{"ascii": true, "emoji": true, "kaomoji": true, "unicode": true}
	falseyMouse       = map[string]bool{"0": true, "false": true, "no": true, "off": true}
	truthyMouseAll    = map[string]bool{"1": true, "true": true, "yes": true, "on": true, "all": true, "full": true, "any": true}
	leadingIntegerRe  = regexp.MustCompile(`^[` + jsWS + `]*([+-]?\d+)`)
	defaultIndicator  = "kaomoji"
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

// Status bar position of display.tui_statusbar, top by default
func normalizeStatusBar(raw any) string {
	if raw == false {
		return "off"
	}
	if s, isStr := raw.(string); isStr {
		if mode, known := statusBarAlias[strings.ToLower(strings.TrimFunc(s, isJSSpace))]; known {
			return mode
		}
	}
	return "top"
}

// Status bar fields of display.status_bar.fields, nil for every field
func normalizeStatusBarFields(raw any) map[string]bool {
	list, isList := raw.([]any)
	if !isList || len(list) == 0 {
		return nil
	}
	fields := map[string]bool{}
	for _, v := range list {
		if name := strings.ToLower(strings.TrimFunc(jsValueString(v), isJSSpace)); name != "" {
			fields[name] = true
		}
	}
	if len(fields) == 0 {
		return nil
	}
	return fields
}

// Busy input mode of a config value, queue by default
func normalizeBusyInputMode(raw any) string {
	if s, isStr := raw.(string); isStr {
		if v := strings.ToLower(strings.TrimFunc(s, isJSSpace)); busyInputModes[v] {
			return v
		}
	}
	return defaultBusyMode
}

// Status indicator style of a config value, kaomoji by default
func normalizeIndicatorStyle(raw any) string {
	if s, isStr := raw.(string); isStr {
		if v := strings.ToLower(strings.TrimFunc(s, isJSSpace)); indicatorStyles[v] {
			return v
		}
	}
	return defaultIndicator
}

// Mouse tracking mode of display.mouse_tracking, else the legacy display.tui_mouse
func normalizeMouseTracking(display map[string]any) string {
	raw, has := display["mouse_tracking"]
	if !has {
		raw = display["tui_mouse"]
	}
	switch v := raw.(type) {
	case bool:
		if !v {
			return "off"
		}
		return "all"
	case float64:
		if v == 0 {
			return "off"
		}
		return "all"
	case string:
		s := strings.ToLower(strings.TrimFunc(v, isJSSpace))
		switch {
		case falseyMouse[s]:
			return "off"
		case truthyMouseAll[s]:
			return "all"
		case s == "wheel" || s == "scroll":
			return "wheel"
		case s == "button" || s == "buttons" || s == "click":
			return "buttons"
		}
	}
	return "all"
}

// Paste collapse threshold of a config key, a non negative number or integer string, else the default
func pasteThreshold(cfg map[string]any, key string, fallback int) int {
	config, _ := cfg["config"].(map[string]any)
	if config == nil || !truthyJS(cfg["config"]) {
		return fallback
	}
	switch raw := config[key].(type) {
	case float64:
		if !math.IsInf(raw, 0) && !math.IsNaN(raw) && raw >= 0 {
			return int(jsRound(raw))
		}
	case string:
		if m := leadingIntegerRe.FindStringSubmatch(raw); m != nil {
			if n, err := strconv.Atoi(m[1]); err == nil && n >= 0 {
				return n
			}
		}
	}
	return fallback
}

// Config surface the sync sets outside the store, the main model in production
type configApp interface {
	setBellOnComplete(on bool)
	setBellOnPrompt(on bool)
	setVoiceEnabled(on bool)
	setVoiceRecordKey(k voiceRecordKey)
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

// Request whose failure and non object answer both read as nil
func quietCall(gw turnGateway, method string, params map[string]any) map[string]any {
	r, err := gw.request(context.Background(), method, params, 0)
	if err != nil {
		return nil
	}
	return asRPCResult(r)
}

// Whether the session generation is still the one in view
func (c *configSync) current(gen int, sid string) bool { return gen == c.gen && c.store.ui.Sid == sid }

// Applies the display section of a config to the UI state, the busy mode only when applyBusy
func (c *configSync) applyDisplay(cfg map[string]any, applyBusy bool) tea.Cmd {
	d, _ := configPath(cfg, "config", "display").(map[string]any)
	if d == nil {
		d = map[string]any{}
	}
	approvals, _ := configPath(cfg, "config", "approvals").(map[string]any)
	c.app.setBellOnComplete(truthyJS(d["bell_on_complete"]))
	c.app.setBellOnPrompt(truthyJS(d["bell_on_prompt"]))
	cmd := c.store.applyConfiguredTuiTheme(d["tui_theme"])
	if cfg != nil {
		key := defaultVoiceRecordKey
		if raw := configPath(cfg, "config", "voice", "record_key"); truthyJS(raw) {
			key = parseVoiceRecordKey(raw)
		}
		c.app.setVoiceRecordKey(key)
	}
	ui := &c.store.ui
	ui.Battery = truthyJS(d["battery"])
	if applyBusy {
		ui.BusyInputMode = normalizeBusyInputMode(d["busy_input_mode"])
	}
	ui.Compact = truthyJS(d["tui_compact"])
	if cfg != nil {
		ui.DestructiveSlashConfirm = approvals["destructive_slash_confirm"] != false
	}
	statusBar, _ := d["status_bar"].(map[string]any)
	ui.DetailsMode, ui.DetailsModeCommandOverride, ui.FocusView = resolveDetailsMode(d), false, truthyJS(d["focus_view"])
	ui.IndicatorStyle, ui.InlineDiffs, ui.MouseTracking = normalizeIndicatorStyle(d["tui_status_indicator"]), d["inline_diffs"] != false, normalizeMouseTracking(d)
	ui.PasteCollapseLines, ui.PasteCollapseChars = pasteThreshold(cfg, "paste_collapse_threshold", defaultPasteLines), pasteThreshold(cfg, "paste_collapse_char_threshold", defaultPasteChars)
	ui.Sections, ui.ShowReasoning = resolveSections(d["sections"]), truthyJS(d["show_reasoning"])
	ui.StatusBar, ui.StatusBarFields = normalizeStatusBar(d["tui_statusbar"]), normalizeStatusBarFields(statusBar["fields"])
	ui.Streaming, ui.Timestamps = d["streaming"] != false, d["timestamps"] == true
	return cmd
}

// Fetches the full config and the owner's busy mode for the session and applies them while it stays current
func (c *configSync) hydrateFullConfig(gen int, sid string) tea.Cmd {
	gw, canonical := c.gw, c.gw.canonical()
	return func() tea.Msg {
		params := map[string]any{"key": "full", "session_id": sid}
		var cfg, busy map[string]any
		var wg sync.WaitGroup
		wg.Add(1)
		go func() {
			defer wg.Done()
			cfg = quietCall(gw, "config.get", params)
		}()
		if canonical {
			busy = quietCall(gw, "config.get", map[string]any{"key": "busy", "session_id": sid})
		}
		wg.Wait()
		return asyncDoneMsg{run: func() tea.Cmd {
			if !c.current(gen, sid) {
				return nil
			}
			cmd := c.applyDisplay(cfg, !canonical)
			if mode, isStr := busy["value"].(string); canonical && isStr && busyInputModes[mode] {
				c.store.ui.BusyInputMode = mode
			}
			if cfg == nil {
				return cmd
			}
			language := configPath(cfg, "config", "display", "language")
			return tea.Batch(cmd, func() tea.Msg {
				syncTUILocale(func(method string, params map[string]any) (any, error) {
					return gw.request(context.Background(), method, params, 0)
				}, language)
				return nil
			})
		}}
	}
}

// Voice mode of a canonical session, enabled for voice_only and all
func (c *configSync) hydrateVoiceMode(gen int, sid string) tea.Cmd {
	if !c.gw.canonical() {
		return nil
	}
	gw := c.gw
	return func() tea.Msg {
		r := quietCall(gw, "config.get", map[string]any{"key": "voice", "session_id": sid})
		return asyncDoneMsg{run: func() tea.Cmd {
			if r != nil && c.current(gen, sid) {
				c.app.setVoiceEnabled(r["value"] == "voice_only" || r["value"] == "all")
			}
			return nil
		}}
	}
}

// Mtime and MCP revision of the session's config, nil on failure
func (c *configSync) readMtime(sid string, then func(r map[string]any) tea.Cmd) tea.Cmd {
	gw := c.gw
	return func() tea.Msg {
		r := quietCall(gw, "config.get", map[string]any{"key": "mtime", "session_id": sid})
		return asyncDoneMsg{run: func() tea.Cmd { return then(r) }}
	}
}

// Number and text of an mtime answer the way JavaScript converts them
func mtimeOf(r map[string]any) (float64, string) {
	next := 0.0
	if v := r["mtime"]; v != nil {
		next = jsNumberOf(v)
	}
	rev := ""
	if v := r["mcp_rev"]; v != nil {
		rev = jsValueString(v)
	}
	return next, rev
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
	return tea.Batch(
		c.hydrateVoiceMode(gen, sid),
		c.readMtime(sid, func(r map[string]any) tea.Cmd {
			if !c.current(gen, sid) {
				return nil
			}
			c.mtime, c.mcp.accepted = mtimeOf(r)
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
	poll := c.readMtime(sid, func(r map[string]any) tea.Cmd {
		if !c.current(gen, sid) {
			return nil
		}
		next, nextMcpRev := mtimeOf(r)
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
				r := quietCall(gw, "reload.mcp", map[string]any{"session_id": sid, "confirm": true})
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
		r := quietCall(gw, "reload.mcp", map[string]any{"confirm": true, "rev": nextMcpRev, "session_id": sid})
		return asyncDoneMsg{run: func() tea.Cmd {
			state.inFlight = false
			if r["status"] == "reloaded" {
				state.accepted = nextMcpRev
				if truthyJS(r["loaded_rev"]) {
					state.accepted = jsValueString(r["loaded_rev"])
				}
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
