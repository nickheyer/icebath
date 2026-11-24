package tui

import (
	"slices"
	"strings"

	"github.com/nickheyer/icebath/pkg/tuigateway"
	"github.com/nickheyer/icebath/pkg/ui/txt"

	tea "charm.land/bubbletea/v2"
)

// Busy indicator styles in the order the command offers them
var indicatorNames = []string{"unicode", "emoji", "ascii"}

// Words the first argument of /fast takes: the fast modes in the order the settings registry offers them, status, and the switch words
var fastArgs = []string{"normal", "fast", "ultrafast", "ultra", "auto", "cold", "status", "on", "off", "toggle"}

// Usage of /fast as its help and refusal spell it
var fastUsage = "[" + strings.Join(fastArgs, "|") + "] [--global]"

// Service tier the session info carries for each fast mode
var fastModeTiers = map[string]string{"normal": "", "fast": "priority", "ultrafast": "ultrafast", "auto": "auto", "cold": "cold"}

// Word a config write took, empty for none or a flag
func setWord(res *tuigateway.ConfigSetResult) string {
	if res == nil || res.Value == nil {
		return ""
	}
	return res.Value.Text
}

// Prints the warning a config answer carries
func (r *slashRun) maybeWarn(res *tuigateway.ConfigSetResult) {
	if res.Warning != "" {
		r.sys(warningLine(res.Warning))
	}
}

// Session commands, background prompts, the model, session switching and the per session switches
func (a *app) sessionCommands() []slashCommand {
	return []slashCommand{
		{name: "bg", help: "launch a background prompt", usage: "<prompt>", run: func(r *slashRun) tea.Cmd {
			if r.arg == "" {
				r.sys(tr(txt.SlashCmdSessionBgUsage))
				return nil
			}
			return ask(r.rpc, r.sys, tuigateway.PromptBackground, tuigateway.SideAgentParams{SessionID: r.sid, Text: r.arg}, func(res *tuigateway.TaskIDResult) tea.Cmd {
				if res == nil || res.TaskID == "" {
					return nil
				}
				task := res.TaskID
				tasks := map[string]bool{task: true}
				for k := range a.store.ui.BgTasks {
					tasks[k] = true
				}
				a.store.ui.BgTasks = tasks
				r.sys(tr(txt.SlashCmdSessionBgStarted, task))
				return nil
			})
		}},
		{name: "btw", help: "ask a side question about this conversation", usage: "<question>", run: func(r *slashRun) tea.Cmd {
			if r.arg == "" {
				r.sys(tr(txt.SlashCmdSessionBtwUsage))
				return nil
			}
			return ask(r.rpc, r.sys, tuigateway.PromptBtw, tuigateway.SideAgentParams{SessionID: r.sid, Text: r.arg}, func(res *tuigateway.TaskIDResult) tea.Cmd {
				if res != nil && res.TaskID != "" {
					r.sys(tr(txt.SlashCmdSessionBtwAnswering, res.TaskID))
				}
				return nil
			})
		}},
		{name: "model", help: "change or show the model", usage: "[model] [--provider name] [--global]", run: func(r *slashRun) tea.Cmd {
			arg := trimJS(r.arg)
			switch {
			case arg == "":
				return a.loadModelPicker(false)
			case arg == "--refresh":
				return a.loadModelPicker(true)
			case r.sid == "":
				r.sys(tr(txt.SlashCmdCoreStatusNoActiveSession))
				return nil
			}
			return r.canonicalModel(arg, "")
		}},
		{name: "personality", help: "switch personality for this session", usage: "[name]", run: func(r *slashRun) tea.Cmd {
			if r.arg == "" {
				return ask(r.rpc, r.sys, tuigateway.SlashExec, tuigateway.SlashExecParams{Command: "personality", SessionID: r.sid}, func(res *tuigateway.SlashExecResult) tea.Cmd {
					if res != nil && res.Output != "" {
						r.sys(res.Output)
					}
					return nil
				})
			}
			return ask(r.rpc, r.sys, tuigateway.ConfigSet, tuigateway.ConfigSetParams{Key: "personality", SessionID: r.sid, Value: tuigateway.TextValue(r.arg)}, func(res *tuigateway.ConfigSetResult) tea.Cmd {
				if res == nil {
					return nil
				}
				reset := res.HistoryReset
				if reset {
					a.life.resetVisibleHistory(res.Info)
				}
				value := setWord(res)
				if value == "" {
					value = tr(txt.SlashCmdSessionPersonalityDefaultValue)
				}
				if reset {
					r.sys(tr(txt.SlashCmdSessionPersonalityChangedCleared, value))
				} else {
					r.sys(tr(txt.SlashCmdSessionPersonalityChanged, value))
				}
				r.maybeWarn(res)
				return nil
			})
		}},
		{name: "theme", help: "light, dark, or match the terminal", usage: "[auto|light|dark]", suggest: func(string) []string { return []string{"auto", "light", "dark"} }, run: func(r *slashRun) tea.Cmd {
			value := strings.ToLower(trimJS(r.arg))
			if value == "" {
				return ask(r.rpc, r.sys, tuigateway.ConfigGet, tuigateway.ConfigGetParams{Key: "theme"}, func(res *tuigateway.ConfigGetResult) tea.Cmd {
					current := configWord(res)
					if current == "" {
						current = "auto"
					}
					r.sys("theme: " + current)
					return nil
				})
			}
			if value != "auto" && value != "light" && value != "dark" {
				r.sys(tr(txt.SlashCmdSessionThemeUsage))
				return nil
			}
			return ask(r.rpc, r.sys, tuigateway.ConfigSet, tuigateway.ConfigSetParams{Key: "theme", Value: tuigateway.TextValue(value)}, func(res *tuigateway.ConfigSetResult) tea.Cmd {
				if res == nil || res.Value == nil {
					return nil
				}
				r.sys(tr(txt.SlashCmdSessionThemeSwitched, value))
				return a.store.applyConfiguredTuiTheme(tuigateway.ThemeMode(value))
			})
		}},
		{name: "skin", help: "the color palette and symbols drawn on top of the theme", usage: "[name]", run: func(r *slashRun) tea.Cmd {
			if r.arg == "" {
				return ask(r.rpc, r.sys, tuigateway.ConfigGet, tuigateway.ConfigGetParams{Key: "skin"}, func(res *tuigateway.ConfigGetResult) tea.Cmd {
					current := configWord(res)
					if current == "" {
						current = tr(txt.SlashCmdSessionSkinDefaultValue)
					}
					r.sys(tr(txt.SlashCmdSessionSkinCurrent, current))
					return nil
				})
			}
			return ask(r.rpc, r.sys, tuigateway.ConfigSet, tuigateway.ConfigSetParams{Key: "skin", Value: tuigateway.TextValue(r.arg)}, func(res *tuigateway.ConfigSetResult) tea.Cmd {
				if value := setWord(res); value != "" {
					r.sys(tr(txt.SlashCmdSessionSkinSwitched, value))
				}
				return nil
			})
		}},
		{name: "indicator", help: "pick the busy indicator", usage: "[" + strings.Join(indicatorNames, "|") + "]", suggest: func(string) []string { return indicatorNames }, run: func(r *slashRun) tea.Cmd {
			value := strings.ToLower(trimJS(r.arg))
			if value == "" {
				return ask(r.rpc, r.sys, tuigateway.ConfigGet, tuigateway.ConfigGetParams{Key: "indicator"}, func(res *tuigateway.ConfigGetResult) tea.Cmd {
					current := configWord(res)
					if current == "" {
						current = defaultIndicator
					}
					r.sys(tr(txt.SlashCmdSessionIndicatorCurrent, current))
					return nil
				})
			}
			if !slices.Contains(indicatorNames, value) {
				r.sys(tr(txt.SlashCmdSessionIndicatorUsage, strings.Join(indicatorNames, "|")))
				return nil
			}
			return ask(r.rpc, r.sys, tuigateway.ConfigSet, tuigateway.ConfigSetParams{Key: "indicator", Value: tuigateway.TextValue(value)}, func(res *tuigateway.ConfigSetResult) tea.Cmd {
				if set := setWord(res); set != "" {
					a.store.ui.IndicatorStyle = value
					r.sys(tr(txt.SlashCmdSessionIndicatorSwitched, set))
				}
				return nil
			})
		}},
		{name: "approvals", help: "whether the agent asks before risky commands, for good or --session", usage: "[manual|smart|off] [--session]",
			suggest: func(string) []string { return []string{"manual", "smart", "off", "--session"} }, run: func(r *slashRun) tea.Cmd { return a.approvalsSlash(r) }},
		{name: "pause", help: "emergency stop: no new work starts until /resume", usage: "[reason]", run: func(r *slashRun) tea.Cmd {
			return r.forward(strings.TrimSpace("pause "+trimJS(r.arg)), tr(txt.SlashCmdSessionPauseTitle))
		}},
		{name: "resume", help: "lift the emergency stop /pause set", run: func(r *slashRun) tea.Cmd {
			return r.forward("pause off", tr(txt.SlashCmdSessionPauseTitle))
		}},
		{name: "yolo", help: "approvals off for this session, or on again; /approvals sets the lasting mode", run: func(r *slashRun) tea.Cmd {
			return ask(r.rpc, r.sys, tuigateway.ConfigSet, tuigateway.ConfigSetParams{Key: "yolo", SessionID: r.sid}, func(res *tuigateway.ConfigSetResult) tea.Cmd {
				if res == nil {
					return nil
				}
				a.store.ui.Yolo = setWord(res) == "1"
				if a.store.ui.Yolo {
					r.sys(tr(txt.SlashCmdSessionYoloOn))
				} else {
					r.sys(tr(txt.SlashCmdSessionYoloOff))
				}
				return nil
			})
		}},
		{name: "reasoning", help: "how long the model thinks before answering", usage: "[none|minimal|low|medium|high|xhigh|max|ultra] [--global]",
			suggest: func(string) []string { return append(append([]string{"none"}, reasoningLadder...), "--global") }, run: func(r *slashRun) tea.Cmd {
				if r.arg == "" {
					return tea.Batch(r.ran(), ask(r.rpc, r.sys, tuigateway.ConfigGet, tuigateway.ConfigGetParams{Key: "reasoning", SessionID: r.sid}, func(res *tuigateway.ConfigGetResult) tea.Cmd {
						if value := configWord(res); value != "" {
							r.sys(tr(txt.SlashCmdSessionReasoningCurrent, value))
						}
						return nil
					}))
				}
				scope := ""
				var values []string
				for _, part := range strings.Fields(r.arg) {
					switch strings.ToLower(part) {
					case "--global":
						scope = "global"
					case "--session":
						if scope == "" {
							scope = "session"
						}
					default:
						values = append(values, part)
					}
				}
				value := strings.Join(values, " ")
				if effort := strings.ToLower(trimJS(value)); r.sid != "" && scope != "global" && canonicalReasoningEfforts[effort] {
					return a.store.mutateCanonical(a.gw, r.sid, "reasoning", map[string]any{"effort": effort}, r.stale, func(m *mutationOutcome, err error) tea.Cmd {
						if r.stale() {
							return nil
						}
						if err != nil {
							r.fail(err)
							return nil
						}
						if m == nil {
							return nil
						}
						r.sys(tr(txt.SlashCmdSessionReasoningCurrent, effort))
						return r.ran()
					})
				}
				params := tuigateway.ConfigSetParams{Key: "reasoning", SessionID: r.sid, Value: tuigateway.TextValue(value), Scope: tuigateway.ConfigWriteScope(scope)}
				return ask(r.rpc, r.sys, tuigateway.ConfigSet, params, func(res *tuigateway.ConfigSetResult) tea.Cmd {
					set := setWord(res)
					if set == "" {
						return nil
					}
					r.sys(tr(txt.SlashCmdSessionReasoningCurrent, set))
					return nil
				})
			}},
		{name: "fast", help: "pay more for faster replies; --global sets it for every session", usage: fastUsage,
			suggest: func(string) []string { return append(append([]string(nil), fastArgs...), "--global") },
			suggestAfter: func(done []string) []string {
				switch {
				case len(done) != 1:
					return nil
				case strings.EqualFold(done[0], "--global"):
					return fastArgs
				}
				return []string{"--global"}
			},
			run: func(r *slashRun) tea.Cmd { return a.fastSlash(r) }},
		{name: "busy", help: "control what enter does while a turn runs", usage: "[queue|steer|interrupt|status]",
			suggest: func(string) []string { return []string{"queue", "steer", "interrupt", "status"} },
			run: func(r *slashRun) tea.Cmd {
				mode := strings.ToLower(trimJS(r.arg))
				sid := r.sid
				switch mode {
				case "", "status":
					return ask(r.rpc, r.sys, tuigateway.ConfigGet, tuigateway.ConfigGetParams{Key: "busy", SessionID: sid}, func(res *tuigateway.ConfigGetResult) tea.Cmd {
						if res == nil {
							return nil
						}
						current := configWord(res)
						if current == "" {
							current = "interrupt"
						}
						r.sys(tr(txt.SlashCmdSessionBusyMode, current))
						return nil
					})
				case "queue", "steer", "interrupt":
				default:
					r.sys(tr(txt.SlashCmdSessionBusyUsage))
					return nil
				}
				return ask(r.rpc, r.sys, tuigateway.ConfigSet, tuigateway.ConfigSetParams{Key: "busy", SessionID: sid, Value: tuigateway.TextValue(mode)}, func(res *tuigateway.ConfigSetResult) tea.Cmd {
					if res == nil {
						return nil
					}
					next := setWord(res)
					if next == "" {
						next = mode
					}
					if next == "queue" || next == "steer" || next == "interrupt" {
						a.store.ui.BusyInputMode = next
					}
					r.sys(tr(txt.SlashCmdSessionBusyMode, next))
					return nil
				})
			}},
	}
}

// Detail levels the /detail command takes, the least shown first
var detailLevels = []string{"minimal", "normal", "detailed", "everything"}

// Fold state every part of the transcript opens to at each detail level
var detailFolds = map[string]string{"minimal": "hidden", "normal": "collapsed", "detailed": "expanded", "everything": "expanded"}

// Words of a setting's value as its row names them, the value itself when no choice holds it
func detailLabel(row tuigateway.SettingsRow) string {
	value, _ := row.Value.(string)
	for _, c := range row.Choices {
		if c.Value == value {
			return c.Label
		}
	}
	return value
}

// /detail: says the detail level, or sets it for every session or with --session for this one, the transcript following at once
func (a *app) detailSlash(r *slashRun) tea.Cmd {
	level, scope := "", "global"
	for _, word := range strings.Fields(strings.ToLower(r.arg)) {
		switch {
		case word == "--session":
			scope = "session"
		case level == "" && slices.Contains(detailLevels, word):
			level = word
		default:
			r.sys(tr(txt.SlashCmdCoreDetailUsage))
			return nil
		}
	}
	if scope == "session" && r.sid == "" {
		r.sys(tr(txt.SlashCmdCoreStatusNoActiveSession))
		return nil
	}
	sid := optionalText(r.sid)
	if level == "" {
		params := tuigateway.SettingsListParams{SessionID: sid, Keys: []string{"display.detail_level"}}
		return ask(r.rpc, r.sys, tuigateway.SettingsList, params, func(res *tuigateway.SettingsListResult) tea.Cmd {
			if res != nil && len(res.Rows) > 0 {
				r.sys(tr(txt.SlashCmdCoreDetailCurrent, detailLabel(res.Rows[0])))
			}
			return r.ran()
		})
	}
	// The transcript follows at once, the saved level coming back with the next read of the config should the write fail
	fold := detailFolds[level]
	sections := map[string]string{}
	for _, s := range sectionNames {
		sections[s] = fold
	}
	ui := &a.store.ui
	ui.DetailsMode, ui.DetailsModeCommandOverride, ui.Sections, ui.FocusView = fold, true, sections, false
	a.card.view.invalidate()
	params := tuigateway.SettingsSetParams{SessionID: sid, Key: "display.detail_level", Value: level, Scope: &scope}
	return ask(r.rpc, r.sys, tuigateway.SettingsSet, params, func(res *tuigateway.SettingsWriteResult) tea.Cmd {
		if res == nil {
			return nil
		}
		key := txt.SlashCmdCoreDetailSet
		if scope == "session" {
			key = txt.SlashCmdCoreDetailSetSession
		}
		r.sys(tr(key, detailLabel(res.Row)))
		return r.ran()
	})
}

// Runs a command line at the background service, as the owner of this window, and shows what it printed
func (r *slashRun) forward(command, title string) tea.Cmd {
	return ask(r.rpc, r.sys, tuigateway.SlashExec, tuigateway.SlashExecParams{Command: command, SessionID: r.sid}, func(res *tuigateway.SlashExecResult) tea.Cmd {
		if res == nil {
			return nil
		}
		body := res.Output
		if body == "" {
			body = tr(txt.SlashCmdCoreNoOutputFor, command)
		}
		return tea.Batch(r.ran(), r.a.showOutput(body, title))
	})
}

// /approvals: says the approval mode and whether this session skips it, sets the mode for good, or with --session asks or skips for this
// session alone, the same switch /yolo flips
func (a *app) approvalsSlash(r *slashRun) tea.Cmd {
	mode, session := "", false
	for _, word := range strings.Fields(strings.ToLower(r.arg)) {
		switch word {
		case "--session":
			session = true
		case "manual", "smart", "off":
			mode = word
		default:
			r.sys(tr(txt.SlashCmdSessionApprovalsUsage))
			return nil
		}
	}
	sid := optionalText(r.sid)
	switch {
	case mode == "":
		params := tuigateway.SettingsListParams{SessionID: sid, Keys: []string{"approvals.mode"}}
		return ask(r.rpc, r.sys, tuigateway.SettingsList, params, func(res *tuigateway.SettingsListResult) tea.Cmd {
			if res == nil || len(res.Rows) == 0 {
				return nil
			}
			key := txt.SlashCmdSessionApprovalsCurrent
			if a.store.ui.Yolo {
				key = txt.SlashCmdSessionApprovalsCurrentYolo
			}
			r.sys(tr(key, detailLabel(res.Rows[0])))
			return r.ran()
		})
	case session && r.sid == "":
		r.sys(tr(txt.SlashCmdCoreStatusNoActiveSession))
		return nil
	case session:
		skip := "off"
		if mode == "off" {
			skip = "on"
		}
		return ask(r.rpc, r.sys, tuigateway.ConfigSet, tuigateway.ConfigSetParams{Key: "yolo", SessionID: r.sid, Value: tuigateway.TextValue(skip)}, func(res *tuigateway.ConfigSetResult) tea.Cmd {
			if res == nil {
				return nil
			}
			a.store.ui.Yolo = setWord(res) == "1"
			if a.store.ui.Yolo {
				r.sys(tr(txt.SlashCmdSessionYoloOn))
			} else {
				r.sys(tr(txt.SlashCmdSessionYoloOff))
			}
			return r.ran()
		})
	}
	scope := "global"
	params := tuigateway.SettingsSetParams{SessionID: sid, Key: "approvals.mode", Value: mode, Scope: &scope}
	return ask(r.rpc, r.sys, tuigateway.SettingsSet, params, func(res *tuigateway.SettingsWriteResult) tea.Cmd {
		if res == nil {
			return nil
		}
		r.sys(tr(txt.SlashCmdSessionApprovalsSet, detailLabel(res.Row)))
		return r.ran()
	})
}

// /fast: says the fast mode of this session or with --global the one every session starts with, or sets it for this session or with
// --global for every session, this one following
func (a *app) fastSlash(r *slashRun) tea.Cmd {
	mode, global := "", false
	for _, word := range strings.Fields(strings.ToLower(r.arg)) {
		switch {
		case word == "--global" && !global:
			global = true
		case mode == "" && slices.Contains(fastArgs, word):
			mode = word
		default:
			r.sys(tr(txt.SlashCmdSessionFastUsage, fastUsage))
			return nil
		}
	}
	if mode == "" || mode == "status" {
		sid, key := r.sid, txt.SlashCmdSessionFastMode
		if global || r.sid == "" {
			sid, key = "", txt.SlashCmdSessionFastModeGlobal
		}
		return ask(r.rpc, r.sys, tuigateway.ConfigGet, tuigateway.ConfigGetParams{Key: "fast", SessionID: sid}, func(res *tuigateway.ConfigGetResult) tea.Cmd {
			if res == nil {
				return nil
			}
			current := configWord(res)
			if current == "" {
				current = "normal"
			}
			r.sys(tr(key, current))
			return r.ran()
		})
	}
	if !global && r.sid == "" {
		r.sys(tr(txt.SlashCmdSessionFastNoSession))
		return nil
	}
	scope, key := tuigateway.ConfigWriteScope("session"), txt.SlashCmdSessionFastSetSession
	if global {
		scope, key = "global", txt.SlashCmdSessionFastSetGlobal
	}
	params := tuigateway.ConfigSetParams{Key: "fast", SessionID: r.sid, Value: tuigateway.TextValue(mode), Scope: scope}
	return ask(r.rpc, r.sys, tuigateway.ConfigSet, params, func(res *tuigateway.ConfigSetResult) tea.Cmd {
		next := setWord(res)
		if next == "" {
			return nil
		}
		r.sys(tr(key, next))
		if tier, known := fastModeTiers[next]; known && a.store.ui.Info != nil && r.sid != "" {
			fast := tier == "priority" || tier == "ultrafast"
			a.store.ui.Info = patchedInfo(a.store.ui.Info, tuigateway.SessionInfo{Fast: &fast, ServiceTier: &tier})
		}
		return r.ran()
	})
}
