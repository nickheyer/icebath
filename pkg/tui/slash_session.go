package tui

import (
	"slices"
	"strings"

	"github.com/nickheyer/icebath/pkg/tuigateway"

	tea "charm.land/bubbletea/v2"
)

// Busy indicator styles in the order the command offers them
var indicatorNames = []string{"unicode", "emoji", "ascii"}

// Word of a fast mode value
func fastModeWord(word string) string {
	if word == "fast" || word == "ultrafast" {
		return word
	}
	return "normal"
}

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
		r.sys("warning: " + res.Warning)
	}
}

// Session commands, background prompts, the model, session switching and the per session switches
func (a *app) sessionCommands() []slashCommand {
	return []slashCommand{
		{name: "bg", aliases: []string{"background"}, help: "launch a background prompt", usage: "<prompt>", run: func(r *slashRun) tea.Cmd {
			if r.arg == "" {
				r.sys(tr("slashCmd.session.bg.usage"))
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
				r.sys(tr("slashCmd.session.bg.started", task))
				return nil
			})
		}},
		{name: "btw", help: "ask a side question about this conversation", usage: "<question>", run: func(r *slashRun) tea.Cmd {
			if r.arg == "" {
				r.sys(tr("slashCmd.session.btw.usage"))
				return nil
			}
			return ask(r.rpc, r.sys, tuigateway.PromptBtw, tuigateway.SideAgentParams{SessionID: r.sid, Text: r.arg}, func(res *tuigateway.TaskIDResult) tea.Cmd {
				if res != nil && res.TaskID != "" {
					r.sys(tr("slashCmd.session.btw.answering", res.TaskID))
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
			case a.gw.canonical():
				if r.sid == "" {
					r.sys(tr("slashCmd.core.status.noActiveSession"))
					return nil
				}
				return r.canonicalModel(arg, "")
			}
			value := arg
			if strings.Contains(arg, tuiSessionModelFlag) {
				value = sessionScopedModelArg(arg)
			}
			var switchModel func(confirmed bool) tea.Cmd
			switchModel = func(confirmed bool) tea.Cmd {
				params := tuigateway.ConfigSetParams{ConfirmExpensiveModel: confirmed, Key: "model", SessionID: r.sid, Value: tuigateway.TextValue(value)}
				return ask(r.rpc, r.sys, tuigateway.ConfigSet, params, func(res *tuigateway.ConfigSetResult) tea.Cmd {
					if res == nil {
						return nil
					}
					if res.ConfirmRequired {
						detail := tr("slashCmd.session.model.expensiveDetail")
						for _, s := range []string{res.Warning, res.ConfirmMessage} {
							if s != "" {
								detail = s
							}
						}
						a.store.overlay.Confirm = &confirmReq{CancelLabel: tr("slashCmd.session.model.cancel"), ConfirmLabel: tr("slashCmd.session.model.switchAnyway"),
							Danger: true, Detail: detail, OnConfirm: func() tea.Cmd { return switchModel(true) }, Title: tr("slashCmd.session.model.expensiveTitle")}
						return nil
					}
					model := setWord(res)
					if model == "" {
						r.sys(misfitText(tr("slashCmd.session.model.invalidResponse")))
						return nil
					}
					if res.Deferred {
						r.sys(tr("slashCmd.session.model.switchedDeferred", model))
					} else {
						r.sys(tr("slashCmd.session.model.switched", model))
					}
					r.maybeWarn(res)
					a.store.ui.Info = patchedInfo(a.store.ui.Info, tuigateway.SessionInfo{Model: &model})
					return nil
				})
			}
			return switchModel(false)
		}},
		{name: "sessions", aliases: []string{"switch", "session", "resume"}, help: "browse, switch, or resume sessions", usage: "[new|session id]", run: func(r *slashRun) tea.Cmd {
			arg := trimJS(r.arg)
			switch {
			case strings.EqualFold(arg, "new"):
				return a.life.newLiveSession("", "", nil)
			case arg != "":
				if a.life.guardBusySessionSwitch(tr("slashCmd.session.sessions.busyGuardAction")) {
					return nil
				}
				return a.resumeByID(arg, nil)
			}
			return a.openSessionSwitcher()
		}},
		{name: "personality", help: "switch personality for this session", usage: "[name]", run: func(r *slashRun) tea.Cmd {
			if r.arg == "" {
				if !a.gw.canonical() {
					return nil
				}
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
					value = tr("slashCmd.session.personality.defaultValue")
				}
				if reset {
					r.sys(tr("slashCmd.session.personality.changedCleared", value))
				} else {
					r.sys(tr("slashCmd.session.personality.changed", value))
				}
				r.maybeWarn(res)
				return nil
			})
		}},
		{name: "theme", help: "terminal UI color mode", usage: "[auto|light|dark]", suggest: func(string) []string { return []string{"auto", "light", "dark"} }, run: func(r *slashRun) tea.Cmd {
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
				r.sys(tr("slashCmd.session.theme.usage"))
				return nil
			}
			return ask(r.rpc, r.sys, tuigateway.ConfigSet, tuigateway.ConfigSetParams{Key: "theme", Value: tuigateway.TextValue(value)}, func(res *tuigateway.ConfigSetResult) tea.Cmd {
				if res == nil || res.Value == nil {
					return nil
				}
				r.sys(tr("slashCmd.session.theme.switched", value))
				return a.store.applyConfiguredTuiTheme(tuigateway.ThemeMode(value))
			})
		}},
		{name: "skin", help: "switch the theme skin", usage: "[name]", run: func(r *slashRun) tea.Cmd {
			if r.arg == "" {
				return ask(r.rpc, r.sys, tuigateway.ConfigGet, tuigateway.ConfigGetParams{Key: "skin"}, func(res *tuigateway.ConfigGetResult) tea.Cmd {
					current := configWord(res)
					if current == "" {
						current = tr("slashCmd.session.skin.defaultValue")
					}
					r.sys(tr("slashCmd.session.skin.current", current))
					return nil
				})
			}
			return ask(r.rpc, r.sys, tuigateway.ConfigSet, tuigateway.ConfigSetParams{Key: "skin", Value: tuigateway.TextValue(r.arg)}, func(res *tuigateway.ConfigSetResult) tea.Cmd {
				if value := setWord(res); value != "" {
					r.sys(tr("slashCmd.session.skin.switched", value))
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
					r.sys(tr("slashCmd.session.indicator.current", current))
					return nil
				})
			}
			if !slices.Contains(indicatorNames, value) {
				r.sys(tr("slashCmd.session.indicator.usage", strings.Join(indicatorNames, "|")))
				return nil
			}
			return ask(r.rpc, r.sys, tuigateway.ConfigSet, tuigateway.ConfigSetParams{Key: "indicator", Value: tuigateway.TextValue(value)}, func(res *tuigateway.ConfigSetResult) tea.Cmd {
				if set := setWord(res); set != "" {
					a.store.ui.IndicatorStyle = value
					r.sys(tr("slashCmd.session.indicator.switched", set))
				}
				return nil
			})
		}},
		{name: "yolo", help: "toggle yolo mode, approvals off for this session", run: func(r *slashRun) tea.Cmd {
			return ask(r.rpc, r.sys, tuigateway.ConfigSet, tuigateway.ConfigSetParams{Key: "yolo", SessionID: r.sid}, func(res *tuigateway.ConfigSetResult) tea.Cmd {
				if res == nil {
					return nil
				}
				if setWord(res) == "1" {
					r.sys(tr("slashCmd.session.yolo.on"))
				} else {
					r.sys(tr("slashCmd.session.yolo.off"))
				}
				return nil
			})
		}},
		{name: "reasoning", help: "inspect or set reasoning effort", usage: "[effort|show|hide] [--global]", run: func(r *slashRun) tea.Cmd {
			if r.arg == "" {
				return tea.Batch(r.ran(), ask(r.rpc, r.sys, tuigateway.ConfigGet, tuigateway.ConfigGetParams{Key: "reasoning", SessionID: r.sid}, func(res *tuigateway.ConfigGetResult) tea.Cmd {
					if res == nil {
						return nil
					}
					value, display := configWord(res), res.Display
					if display == "" {
						display = "hide"
					}
					if value != "" {
						r.sys(tr("slashCmd.session.reasoning.currentWithDisplay", value, display))
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
			if effort := strings.ToLower(trimJS(value)); a.gw.canonical() && r.sid != "" && scope != "global" && canonicalReasoningEfforts[effort] {
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
					r.sys(tr("slashCmd.session.reasoning.current", effort))
					return r.ran()
				})
			}
			params := tuigateway.ConfigSetParams{Key: "reasoning", SessionID: r.sid, Value: tuigateway.TextValue(value), Scope: tuigateway.ConfigWriteScope(scope)}
			return ask(r.rpc, r.sys, tuigateway.ConfigSet, params, func(res *tuigateway.ConfigSetResult) tea.Cmd {
				set := setWord(res)
				if set == "" {
					return nil
				}
				if set == "hide" || set == "show" {
					sections := map[string]string{}
					for k, v := range a.store.ui.Sections {
						sections[k] = v
					}
					sections["thinking"] = "hidden"
					if set == "show" {
						sections["thinking"] = "expanded"
					}
					a.store.ui.Sections, a.store.ui.ShowReasoning = sections, set == "show"
					a.card.view.invalidate()
				}
				r.sys(tr("slashCmd.session.reasoning.current", set))
				return nil
			})
		}},
		{name: "fast", help: "toggle fast mode", usage: "[normal|fast|ultrafast|status|on|off|toggle]",
			suggest: func(string) []string { return []string{"normal", "fast", "ultrafast", "status", "on", "off", "toggle"} },
			run: func(r *slashRun) tea.Cmd {
				mode := strings.ToLower(trimJS(r.arg))
				switch mode {
				case "", "status":
					return ask(r.rpc, r.sys, tuigateway.ConfigGet, tuigateway.ConfigGetParams{Key: "fast", SessionID: r.sid}, func(res *tuigateway.ConfigGetResult) tea.Cmd {
						if res != nil {
							r.sys(tr("slashCmd.session.fast.mode", fastModeWord(configWord(res))))
						}
						return nil
					})
				case "normal", "fast", "ultrafast", "on", "off", "toggle":
				default:
					r.sys(tr("slashCmd.session.fast.usage"))
					return nil
				}
				return ask(r.rpc, r.sys, tuigateway.ConfigSet, tuigateway.ConfigSetParams{Key: "fast", SessionID: r.sid, Value: tuigateway.TextValue(mode)}, func(res *tuigateway.ConfigSetResult) tea.Cmd {
					if res == nil {
						return nil
					}
					next := fastModeWord(setWord(res))
					r.sys(tr("slashCmd.session.fast.mode", next))
					if a.store.ui.Info != nil {
						fast, tier := next != "normal", map[string]string{"fast": "priority", "normal": "", "ultrafast": "ultrafast"}[next]
						a.store.ui.Info = patchedInfo(a.store.ui.Info, tuigateway.SessionInfo{Fast: &fast, ServiceTier: &tier})
					}
					return nil
				})
			}},
		{name: "busy", help: "control what enter does while a turn runs", usage: "[queue|steer|interrupt|status]",
			suggest: func(string) []string { return []string{"queue", "steer", "interrupt", "status"} },
			run: func(r *slashRun) tea.Cmd {
				mode := strings.ToLower(trimJS(r.arg))
				sid := ""
				if a.gw.canonical() {
					sid = r.sid
				}
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
						r.sys(tr("slashCmd.session.busy.mode", current))
						return nil
					})
				case "queue", "steer", "interrupt":
				default:
					r.sys(tr("slashCmd.session.busy.usage"))
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
					r.sys(tr("slashCmd.session.busy.mode", next))
					return nil
				})
			}},
		{name: "verbose", help: "cycle verbose tool output", usage: "[mode]", run: func(r *slashRun) tea.Cmd {
			value := r.arg
			if value == "" {
				value = "cycle"
			}
			return ask(r.rpc, r.sys, tuigateway.ConfigSet, tuigateway.ConfigSetParams{Key: "verbose", SessionID: r.sid, Value: tuigateway.TextValue(value)}, func(res *tuigateway.ConfigSetResult) tea.Cmd {
				if set := setWord(res); set != "" {
					r.sys(tr("slashCmd.session.verbose.current", set))
				}
				return nil
			})
		}},
	}
}
