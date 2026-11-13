package tui

import (
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/nickheyer/icebath/pkg/ui/cards"
	"github.com/nickheyer/icebath/pkg/ui/keys"
	"github.com/nickheyer/icebath/pkg/ui/render"
	"github.com/nickheyer/icebath/pkg/ui/wm"
)

// Keymap contexts of the list windows of the terminal UI
const (
	sessionsContext = "tui.sessions"
	agentsContext   = "tui.agents"
	skillsContext   = "tui.skills"
	pluginsContext  = "tui.plugins"
)

// Keys of the list windows, registered with the keymap of the app
func registerListKeys(km *keys.Map) {
	for name, doc := range map[string]string{sessionsContext: "session switcher", agentsContext: "agents window", skillsContext: "skills hub", pluginsContext: "plugins hub"} {
		if keys.ContextDocs[name] == "" {
			_ = keys.AddContext(name, doc)
		}
	}
	for _, a := range []keys.Action{
		{ID: "tui.sessions.open", Context: sessionsContext, Label: "open", Doc: "switch to the session", Default: []string{"enter"}},
		{ID: "tui.sessions.delete", Context: sessionsContext, Label: "delete", Doc: "delete the stored session after a confirmation", Default: []string{"d"}},
		{ID: "tui.sessions.new", Context: sessionsContext, Label: "new", Doc: "start a new session beside this one", Default: []string{"n"}},
		{ID: "tui.agents.tail", Context: agentsContext, Label: "transcript", Doc: "read the newest output of the agent", Default: []string{"enter"}},
		{ID: "tui.agents.stop", Context: agentsContext, Label: "stop", Doc: "interrupt the agent", Default: []string{"x"}},
		{ID: "tui.agents.steer", Context: agentsContext, Label: "steer", Doc: "send guidance to the agent", Default: []string{"s"}},
		{ID: "tui.agents.pause", Context: agentsContext, Label: "pause", Doc: "pause or resume spawning of new agents", Default: []string{"p"}},
		{ID: "tui.skills.inspect", Context: skillsContext, Label: "inspect", Doc: "show or hide what the skill does", Default: []string{"enter"}},
		{ID: "tui.skills.install", Context: skillsContext, Label: "install", Doc: "install the skill", Default: []string{"i"}},
		{ID: "tui.plugins.toggle", Context: pluginsContext, Label: "toggle", Doc: "enable or disable the plugin", Default: []string{"enter", "space"}},
	} {
		km.Register(a)
	}
}

// Environment the list windows read, the toolkit palette and keymap of the app
func (a *app) cardEnv() *cards.Env {
	return &cards.Env{Theme: a.toolkit, Keys: a.keys, Ctx: a.ctx, WorkDir: a.store.sessionCwd}
}

// Clock or date of an epoch second, the clock for today
func whenText(epochSeconds float64) string {
	if epochSeconds <= 0 {
		return ""
	}
	t := time.Unix(int64(epochSeconds), 0).In(clockLocation())
	if now := time.Now().In(clockLocation()); now.YearDay() == t.YearDay() && now.Year() == t.Year() {
		return t.Format("15:04")
	}
	return t.Format("Jan 2")
}

// Opens the model picker over every provider and model the gateway offers, typed text narrowing it
func (a *app) openModelPicker() tea.Cmd { return a.loadModelPicker(false) }

func (a *app) loadModelPicker(refresh bool) tea.Cmd {
	params := map[string]any{"include_unconfigured": true}
	sid := a.store.ui.Sid
	if sid != "" {
		params["session_id"] = sid
	}
	if refresh {
		params["refresh"] = true
	}
	a.store.ui.Status = "loading models…"
	return a.events.rpc("model.options", params, func(r map[string]any) tea.Cmd {
		a.store.ui.Status = a.store.statusFromBusy()
		if r == nil {
			return nil
		}
		providers, _ := r["providers"].([]any)
		current := infoText(a.store.ui.Info, "model")
		var items []wm.Item
		for _, raw := range providers {
			p, _ := raw.(map[string]any)
			slug, _ := p["slug"].(string)
			name, _ := p["name"].(string)
			if name == "" {
				name = slug
			}
			if p["authenticated"] == false {
				keyEnv, _ := p["key_env"].(string)
				detail, _ := p["warning"].(string)
				if keyEnv == "" {
					items = append(items, wm.Item{ID: "provider:" + slug, Title: "○ " + name, Detail: detail, Run: func() tea.Cmd {
						a.sys(name + ": " + detail)
						return nil
					}})
					continue
				}
				items = append(items, wm.Item{ID: "provider:" + slug, Title: "○ " + name, Detail: "paste " + keyEnv + " to activate", Run: func() tea.Cmd {
					return a.wm.ShowOverlay(wm.NewPrompt(a.toolkit, keyEnv+" for "+name, "", func(key string) tea.Cmd {
						if trimJS(key) == "" {
							return nil
						}
						save := map[string]any{"slug": slug, "api_key": trimJS(key)}
						if sid != "" {
							save["session_id"] = sid
						}
						return a.events.rpc("model.save_key", save, func(res map[string]any) tea.Cmd {
							if res == nil {
								return nil
							}
							a.sys(name + " connected")
							return a.loadModelPicker(false)
						})
					}))
				}})
				continue
			}
			for _, model := range stringList(p["models"]) {
				title := model
				if model == current && truthyJS(p["is_current"]) {
					title = "● " + model
				}
				items = append(items, wm.Item{ID: slug + "/" + model, Title: title, Detail: name, Run: func() tea.Cmd {
					return a.slash("/model "+model+" --provider "+slug+" "+tuiSessionModelFlag, nil, false)
				}})
			}
			items = append(items, wm.Item{ID: "disconnect:" + slug, Title: "disconnect " + name, Detail: "remove the saved credentials of this provider", Run: func() tea.Cmd {
				drop := map[string]any{"slug": slug}
				if sid != "" {
					drop["session_id"] = sid
				}
				return a.events.rpc("model.disconnect", drop, func(res map[string]any) tea.Cmd {
					if truthyJS(res["disconnected"]) {
						a.sys(name + " disconnected")
					}
					return nil
				})
			}})
		}
		if len(items) == 0 {
			a.sys("no models are available, run `icebath model` to configure a provider")
			return nil
		}
		p := wm.NewPalette(a.toolkit, items)
		p.Placeholder = "model"
		return a.wm.ShowOverlay(p)
	})
}

// One row of the session switcher
type sessionRow struct {
	id      string
	title   string
	preview string
	status  string
	when    float64
	count   float64
	live    bool
	current bool
}

// Session switcher, the live sessions above the stored ones, with open, delete and new
func (a *app) openSessionSwitcher() tea.Cmd {
	var rows []sessionRow
	load := func(done func() tea.Cmd) tea.Cmd {
		current := a.store.ui.Sid
		stored := a.store.ui.StoredSid
		method, params := "session.active_list", map[string]any{"current_session_id": current}
		if a.gw.canonical() {
			method, params = "session.list", map[string]any{"limit": 200, "scope": "live"}
		}
		return gatewayCall(a.gw, method, params, func(liveRaw any, liveErr error) tea.Cmd {
			return gatewayCall(a.gw, "session.list", map[string]any{"limit": 200, "scope": "stored"}, func(histRaw any, histErr error) tea.Cmd {
				if liveErr != nil && histErr != nil {
					a.sys("error: " + rpcErrorMessage(liveErr))
					return nil
				}
				seen := map[string]bool{}
				rows = rows[:0]
				add := func(raw any, live bool) {
					list, _ := asRPCResult(raw)["sessions"].([]any)
					for _, item := range list {
						s, _ := item.(map[string]any)
						id, _ := s["id"].(string)
						running := truthyJS(s["running"])
						if id == "" || seen[id] || (live && a.gw.canonical() && !running && id != current && id != stored) {
							continue
						}
						seen[id] = true
						row := sessionRow{id: id, live: live, current: id == current || id == stored || truthyJS(s["current"])}
						row.title, _ = s["title"].(string)
						row.preview, _ = s["preview"].(string)
						row.status, _ = s["status"].(string)
						if row.status == "" && running {
							row.status = "running"
						}
						row.when, _ = s["last_active"].(float64)
						if row.when == 0 {
							row.when, _ = s["started_at"].(float64)
						}
						row.count, _ = s["message_count"].(float64)
						rows = append(rows, row)
					}
				}
				if liveErr == nil {
					add(liveRaw, true)
				}
				if histErr == nil {
					add(histRaw, false)
				}
				sort.SliceStable(rows, func(i, j int) bool {
					if rows[i].live != rows[j].live {
						return rows[i].live
					}
					return rows[i].when > rows[j].when
				})
				return done()
			})
		})
	}
	var list *cards.List
	list = cards.NewList(a.cardEnv(), cards.ListSpec{
		Title: "sessions", Glyph: a.toolkit.Glyphs.Sessions, Key: "tui:sessions", Context: sessionsContext, Primary: "tui.sessions.open",
		Columns: []render.Column{{Title: ""}, {Title: "session", Flex: 2, Max: 48}, {Title: "last message", Flex: 3}, {Title: "msgs", Right: true}, {Title: "when", Right: true}},
		Rows: func() [][]string {
			out := make([][]string, len(rows))
			for i, r := range rows {
				mark := " "
				switch {
				case r.current:
					mark = "●"
				case r.live:
					mark = "◌"
				}
				title := r.title
				if title == "" {
					title = sessionLabel(r.id)
				}
				if r.status != "" && r.live {
					title += " · " + r.status
				}
				count := ""
				if r.count > 0 {
					count = jsString(r.count)
				}
				out[i] = []string{mark, title, strings.ReplaceAll(r.preview, "\n", " "), count, whenText(r.when)}
			}
			return out
		},
		Free:  map[string]bool{"tui.sessions.new": true},
		Empty: func() string { return "no sessions yet, n starts one" },
		Actions: map[string]func(l *cards.List, i int) tea.Cmd{
			"tui.sessions.open": func(l *cards.List, i int) tea.Cmd {
				row := rows[i]
				if row.current {
					return wm.RequestDismissOf(l)
				}
				if a.life.guardBusySessionSwitch(tr("slashCmd.session.sessions.busyGuardAction")) {
					return wm.RequestDismissOf(l)
				}
				if row.live && !a.gw.canonical() {
					return tea.Batch(wm.RequestDismissOf(l), a.life.activateLiveSession(row.id))
				}
				return tea.Batch(wm.RequestDismissOf(l), a.resumeByID(row.id, nil))
			},
			"tui.sessions.new": func(l *cards.List, _ int) tea.Cmd {
				return tea.Batch(wm.RequestDismissOf(l), a.life.newLiveSession("", "", nil))
			},
			"tui.sessions.delete": func(l *cards.List, i int) tea.Cmd {
				row := rows[i]
				if row.current {
					return wm.Notify("the session in view stays, switch away from it first", wm.Warn)
				}
				name := row.title
				if name == "" {
					name = sessionLabel(row.id)
				}
				l.Confirm("delete "+name+"?", func() tea.Cmd {
					return a.events.rpc("session.delete", map[string]any{"session_id": row.id}, func(r map[string]any) tea.Cmd {
						if r == nil {
							return nil
						}
						return load(func() tea.Cmd { l.Refresh(); return nil })
					})
				})
				return nil
			},
		},
	})
	return load(func() tea.Cmd { return a.wm.ShowOverlay(list) })
}

// Agents of the session with their state, the newest output of one, and stop, steer and pause controls
func (a *app) openAgents() tea.Cmd {
	roster := func() []subagentProgress { return a.store.currentAgentRoster() }
	sid := a.store.ui.Sid
	list := cards.NewList(a.cardEnv(), cards.ListSpec{
		Title: "agents", Glyph: a.toolkit.Glyphs.Agent, Key: "tui:agents", Context: agentsContext, Primary: "tui.agents.tail", Follow: true,
		Columns: []render.Column{{Title: "state"}, {Title: "goal", Flex: 4}, {Title: "model", Flex: 1, Max: 28}, {Title: "tools", Right: true}, {Title: "time", Right: true}},
		Rows: func() [][]string {
			agents := roster()
			out := make([][]string, len(agents))
			for i, s := range agents {
				elapsed := ""
				if s.DurationSeconds != nil {
					elapsed = render.Elapsed(time.Duration(*s.DurationSeconds * float64(time.Second)).Truncate(time.Second))
				}
				out[i] = []string{s.Status, strings.Repeat("  ", s.Depth) + strings.ReplaceAll(s.Goal, "\n", " "), s.Model, fmt.Sprint(s.ToolCount), elapsed}
			}
			return out
		},
		Detail: func(i int) []string {
			agents := roster()
			if i < 0 || i >= len(agents) {
				return nil
			}
			s := agents[i]
			lines := []string{s.Goal}
			if s.Summary != "" {
				lines = append(lines, "", s.Summary)
			}
			for _, note := range s.Notes {
				lines = append(lines, "· "+note)
			}
			for _, out := range s.OutputTail {
				lines = append(lines, out.Tool+": "+out.Preview)
			}
			return lines
		},
		Free: map[string]bool{"tui.agents.pause": true},
		Empty: func() string {
			if a.store.delegation.Paused {
				return "no agents are running, spawning is paused"
			}
			return "no agents are running in this session"
		},
		Actions: map[string]func(l *cards.List, i int) tea.Cmd{
			"tui.agents.tail": func(l *cards.List, i int) tea.Cmd {
				agent := roster()[i]
				return gatewayCall(a.gw, "subagent.tail", map[string]any{"session_id": sid, "subagent_id": agent.ID}, func(v any, err error) tea.Cmd {
					r := asRPCResult(v)
					text, _ := r["text"].(string)
					switch {
					case err != nil:
						return wm.Notify(rpcErrorMessage(err), wm.Error)
					case !truthyJS(r["available"]) || text == "":
						return l.ToggleDetail()
					}
					return a.page(text, "agent · "+truncateEnd(agent.Goal, 40))
				})
			},
			"tui.agents.stop": func(l *cards.List, i int) tea.Cmd {
				agent := roster()[i]
				return a.events.rpc("subagent.interrupt", map[string]any{"session_id": sid, "subagent_id": agent.ID}, func(r map[string]any) tea.Cmd {
					if r != nil && !truthyJS(r["found"]) {
						return wm.Notify("the agent already finished", wm.Info)
					}
					return wm.Notify("agent interrupted", wm.Success)
				})
			},
			"tui.agents.steer": func(l *cards.List, i int) tea.Cmd {
				agent := roster()[i]
				return a.wm.ShowOverlay(wm.NewPrompt(a.toolkit, "guidance for the agent", "", func(text string) tea.Cmd {
					if trimJS(text) == "" {
						return nil
					}
					return a.events.rpc("subagent.steer", map[string]any{"session_id": sid, "subagent_id": agent.ID, "text": text}, func(r map[string]any) tea.Cmd {
						if r != nil {
							a.sys("guidance queued for the agent at its next tool boundary")
						}
						return nil
					})
				}))
			},
			"tui.agents.pause": func(l *cards.List, _ int) tea.Cmd {
				return a.events.rpc("delegation.pause", map[string]any{"paused": !a.store.delegation.Paused}, func(r map[string]any) tea.Cmd {
					if r == nil {
						return nil
					}
					a.store.applyDelegationStatus(map[string]any{"paused": r["paused"]})
					if truthyJS(r["paused"]) {
						return wm.Notify(tr("slashCmd.ops.agents.paused"), wm.Info)
					}
					return wm.Notify(tr("slashCmd.ops.agents.resumed"), wm.Info)
				})
			},
		},
	})
	a.events.refreshDelegationStatus(true)
	return a.wm.ShowOverlay(list)
}

// Lists what a hub shows, an owner without the hub method answering through its command worker in the pager
func (a *app) hubRequest(method, command string, show func(r map[string]any) tea.Cmd) tea.Cmd {
	sid := a.store.ui.Sid
	return gatewayCall(a.gw, method, map[string]any{"action": "list"}, func(v any, err error) tea.Cmd {
		var refused *rpcError
		if errors.As(err, &refused) && refused.HasCode && refused.Code == -32601 {
			return gatewayCall(a.gw, "slash.exec", map[string]any{"command": command, "session_id": nilIfEmpty(sid)}, func(v any, err error) tea.Cmd {
				if err != nil {
					a.sys("error: " + describeSlashExecError(command, err))
					return nil
				}
				out, _ := asRPCResult(v)["output"].(string)
				if out == "" {
					out = "/" + command + ": no output"
				}
				return a.showOutput(out, command)
			})
		}
		r := asRPCResult(v)
		switch {
		case err != nil:
			a.sys("error: " + rpcErrorMessage(err))
			return nil
		case r == nil:
			a.sys("error: " + tr("session.common.invalidResponse", method))
			return nil
		}
		return show(r)
	})
}

// Skills hub, every installed skill by category with what it does and an install action
func (a *app) openSkillsHub() tea.Cmd {
	return a.hubRequest("skills.manage", "skills", func(r map[string]any) tea.Cmd {
		skills, _ := r["skills"].(map[string]any)
		type skillRow struct{ category, name string }
		var rows []skillRow
		for _, category := range sortedKeys(skills) {
			for _, name := range stringList(skills[category]) {
				rows = append(rows, skillRow{category, name})
			}
		}
		details := map[string][]string{}
		list := cards.NewList(a.cardEnv(), cards.ListSpec{
			Title: "skills", Glyph: a.toolkit.Glyphs.Tools, Key: "tui:skills", Context: skillsContext, Primary: "tui.skills.inspect",
			Columns: []render.Column{{Title: "skill", Flex: 3}, {Title: "category", Flex: 2}},
			Rows: func() [][]string {
				out := make([][]string, len(rows))
				for i, s := range rows {
					out[i] = []string{s.name, s.category}
				}
				return out
			},
			Detail: func(i int) []string {
				if i < 0 || i >= len(rows) {
					return nil
				}
				if lines, loaded := details[rows[i].name]; loaded {
					return lines
				}
				return []string{"loading…"}
			},
			Empty: func() string { return tr("userMessages.skills.noneInstalled") },
			Actions: map[string]func(l *cards.List, i int) tea.Cmd{
				"tui.skills.inspect": func(l *cards.List, i int) tea.Cmd {
					name := rows[i].name
					toggle := l.ToggleDetail()
					if _, loaded := details[name]; loaded {
						return toggle
					}
					return tea.Batch(toggle, a.events.rpc("skills.manage", map[string]any{"action": "inspect", "query": name}, func(res map[string]any) tea.Cmd {
						info, _ := res["info"].(map[string]any)
						description, _ := info["description"].(string)
						path, _ := info["path"].(string)
						details[name] = append(strings.Split(description, "\n"), "", path)
						return nil
					}))
				},
				"tui.skills.install": func(l *cards.List, i int) tea.Cmd {
					name := rows[i].name
					return a.events.rpc("skills.manage", map[string]any{"action": "install", "query": name}, func(res map[string]any) tea.Cmd {
						if res == nil {
							return nil
						}
						return tea.Batch(wm.Notify(tr("slashCmd.ops.skills.installed", name), wm.Success), a.events.loadSlashCatalog(a.store.ui.Sid))
					})
				},
			},
		})
		return a.wm.ShowOverlay(list)
	})
}

// Plugins hub, every plugin with its state, toggled in place
func (a *app) openPluginsHub() tea.Cmd {
	return a.hubRequest("plugins.manage", "plugins", func(r map[string]any) tea.Cmd {
		plugins, _ := r["plugins"].([]any)
		text := func(p map[string]any, key string) string {
			s, _ := p[key].(string)
			return s
		}
		list := cards.NewList(a.cardEnv(), cards.ListSpec{
			Title: "plugins", Glyph: a.toolkit.Glyphs.Ext, Key: "tui:plugins", Context: pluginsContext, Primary: "tui.plugins.toggle",
			Columns: []render.Column{{Title: "on"}, {Title: "plugin", Flex: 2}, {Title: "source"}, {Title: "what it adds", Flex: 4}},
			Rows: func() [][]string {
				out := make([][]string, len(plugins))
				for i, raw := range plugins {
					p, _ := raw.(map[string]any)
					mark := "○"
					if truthyJS(p["enabled"]) {
						mark = "●"
					}
					out[i] = []string{mark, text(p, "name"), text(p, "source"), strings.ReplaceAll(text(p, "description"), "\n", " ")}
				}
				return out
			},
			Empty: func() string { return "no plugins are installed" },
			Actions: map[string]func(l *cards.List, i int) tea.Cmd{
				"tui.plugins.toggle": func(l *cards.List, i int) tea.Cmd {
					p, _ := plugins[i].(map[string]any)
					name := text(p, "name")
					return a.events.rpc("plugins.manage", map[string]any{"action": "toggle", "enable": !truthyJS(p["enabled"]), "name": name}, func(res map[string]any) tea.Cmd {
						if updated, _ := res["plugin"].(map[string]any); updated != nil {
							plugins[i] = updated
						} else if res != nil {
							p["enabled"] = !truthyJS(p["enabled"])
						}
						l.Refresh()
						return nil
					})
				},
			},
		})
		return a.wm.ShowOverlay(list)
	})
}
