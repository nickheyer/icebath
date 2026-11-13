package tui

import (
	"strings"

	tea "charm.land/bubbletea/v2"
)

// Busy indicator styles in the order the command offers them
var indicatorNames = []string{"kaomoji", "emoji", "unicode", "ascii"}

// Word of a fast mode value
func fastModeWord(v any) string {
	if v == "fast" || v == "ultrafast" {
		return v.(string)
	}
	return "normal"
}

// Prints the warning a config answer carries
func (r *slashRun) maybeWarn(res map[string]any) {
	if warning, _ := res["warning"].(string); warning != "" {
		r.sys("warning: " + warning)
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
			return r.rpc("prompt.background", map[string]any{"session_id": nilIfEmpty(r.sid), "text": r.arg}, func(res map[string]any) tea.Cmd {
				task, _ := res["task_id"].(string)
				if task == "" {
					return nil
				}
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
			return r.rpc("prompt.btw", map[string]any{"session_id": nilIfEmpty(r.sid), "text": r.arg}, func(res map[string]any) tea.Cmd {
				if task, _ := res["task_id"].(string); task != "" {
					r.sys(tr("slashCmd.session.btw.answering", task))
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
				return a.store.switchCanonicalModel(a.gw, r.sid, arg, r.sys, nil, "", func(err error) tea.Cmd {
					if err != nil && !r.stale() {
						r.fail(err)
					}
					return nil
				})
			}
			value := arg
			if strings.Contains(arg, tuiSessionModelFlag) {
				value = sessionScopedModelArg(arg)
			}
			var switchModel func(confirmed bool) tea.Cmd
			switchModel = func(confirmed bool) tea.Cmd {
				return r.rpc("config.set", map[string]any{"confirm_expensive_model": confirmed, "key": "model", "session_id": nilIfEmpty(r.sid), "value": value}, func(res map[string]any) tea.Cmd {
					if truthyJS(res["confirm_required"]) {
						detail := tr("slashCmd.session.model.expensiveDetail")
						for _, k := range []string{"warning", "confirm_message"} {
							if s, _ := res[k].(string); s != "" {
								detail = s
							}
						}
						a.store.overlay.Confirm = &confirmReq{CancelLabel: tr("slashCmd.session.model.cancel"), ConfirmLabel: tr("slashCmd.session.model.switchAnyway"),
							Danger: true, Detail: detail, OnConfirm: func() tea.Cmd { return switchModel(true) }, Title: tr("slashCmd.session.model.expensiveTitle")}
						return nil
					}
					model, _ := res["value"].(string)
					if model == "" {
						r.sys(tr("slashCmd.session.model.invalidResponse"))
						return nil
					}
					if truthyJS(res["deferred"]) {
						r.sys(tr("slashCmd.session.model.switchedDeferred", model))
					} else {
						r.sys(tr("slashCmd.session.model.switched", model))
					}
					r.maybeWarn(res)
					info := map[string]any{"skills": map[string]any{}, "tools": map[string]any{}}
					for k, v := range a.store.ui.Info {
						info[k] = v
					}
					info["model"] = model
					a.store.ui.Info = info
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
				return r.rpc("slash.exec", map[string]any{"command": "personality", "session_id": nilIfEmpty(r.sid)}, func(res map[string]any) tea.Cmd {
					if out, _ := res["output"].(string); out != "" {
						r.sys(out)
					}
					return nil
				})
			}
			return r.rpc("config.set", map[string]any{"key": "personality", "session_id": nilIfEmpty(r.sid), "value": r.arg}, func(res map[string]any) tea.Cmd {
				reset := truthyJS(res["history_reset"])
				if reset {
					info, _ := res["info"].(map[string]any)
					a.life.resetVisibleHistory(info)
				}
				value, _ := res["value"].(string)
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
				return r.rpc("config.get", map[string]any{"key": "theme"}, func(res map[string]any) tea.Cmd {
					current, _ := res["value"].(string)
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
			return r.rpc("config.set", map[string]any{"key": "theme", "value": value}, func(res map[string]any) tea.Cmd {
				if _, has := res["value"]; !has {
					return nil
				}
				r.sys(tr("slashCmd.session.theme.switched", value))
				return a.store.applyConfiguredTuiTheme(value)
			})
		}},
		{name: "skin", help: "switch the theme skin", usage: "[name]", run: func(r *slashRun) tea.Cmd {
			if r.arg == "" {
				return r.rpc("config.get", map[string]any{"key": "skin"}, func(res map[string]any) tea.Cmd {
					current, _ := res["value"].(string)
					if current == "" {
						current = tr("slashCmd.session.skin.defaultValue")
					}
					r.sys(tr("slashCmd.session.skin.current", current))
					return nil
				})
			}
			return r.rpc("config.set", map[string]any{"key": "skin", "value": r.arg}, func(res map[string]any) tea.Cmd {
				if value, _ := res["value"].(string); value != "" {
					r.sys(tr("slashCmd.session.skin.switched", value))
				}
				return nil
			})
		}},
		{name: "indicator", help: "pick the busy indicator", usage: "[" + strings.Join(indicatorNames, "|") + "]", suggest: func(string) []string { return indicatorNames }, run: func(r *slashRun) tea.Cmd {
			value := strings.ToLower(trimJS(r.arg))
			if value == "" {
				return r.rpc("config.get", map[string]any{"key": "indicator"}, func(res map[string]any) tea.Cmd {
					current, _ := res["value"].(string)
					if current == "" {
						current = defaultIndicator
					}
					r.sys(tr("slashCmd.session.indicator.current", current))
					return nil
				})
			}
			if !indicatorStyles[value] {
				r.sys(tr("slashCmd.session.indicator.usage", strings.Join(indicatorNames, "|")))
				return nil
			}
			return r.rpc("config.set", map[string]any{"key": "indicator", "value": value}, func(res map[string]any) tea.Cmd {
				if set, _ := res["value"].(string); set != "" {
					a.store.ui.IndicatorStyle = value
					r.sys(tr("slashCmd.session.indicator.switched", set))
				}
				return nil
			})
		}},
		{name: "yolo", help: "toggle yolo mode, approvals off for this session", run: func(r *slashRun) tea.Cmd {
			return r.rpc("config.set", map[string]any{"key": "yolo", "session_id": nilIfEmpty(r.sid)}, func(res map[string]any) tea.Cmd {
				if res["value"] == "1" {
					r.sys(tr("slashCmd.session.yolo.on"))
				} else {
					r.sys(tr("slashCmd.session.yolo.off"))
				}
				return nil
			})
		}},
		{name: "reasoning", help: "inspect or set reasoning effort", usage: "[effort|show|hide] [--global]", run: func(r *slashRun) tea.Cmd {
			if r.arg == "" {
				return tea.Batch(r.ran(), r.rpc("config.get", map[string]any{"key": "reasoning", "session_id": nilIfEmpty(r.sid)}, func(res map[string]any) tea.Cmd {
					value, _ := res["value"].(string)
					display, _ := res["display"].(string)
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
			params := map[string]any{"key": "reasoning", "session_id": r.sid, "value": value}
			if scope != "" {
				params["scope"] = scope
			}
			return r.rpc("config.set", params, func(res map[string]any) tea.Cmd {
				set, _ := res["value"].(string)
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
					return r.rpc("config.get", map[string]any{"key": "fast", "session_id": nilIfEmpty(r.sid)}, func(res map[string]any) tea.Cmd {
						r.sys(tr("slashCmd.session.fast.mode", fastModeWord(res["value"])))
						return nil
					})
				case "normal", "fast", "ultrafast", "on", "off", "toggle":
				default:
					r.sys(tr("slashCmd.session.fast.usage"))
					return nil
				}
				return r.rpc("config.set", map[string]any{"key": "fast", "session_id": nilIfEmpty(r.sid), "value": mode}, func(res map[string]any) tea.Cmd {
					next := fastModeWord(res["value"])
					r.sys(tr("slashCmd.session.fast.mode", next))
					if a.store.ui.Info != nil {
						info := map[string]any{}
						for k, v := range a.store.ui.Info {
							info[k] = v
						}
						info["fast"] = next != "normal"
						info["service_tier"] = map[string]string{"fast": "priority", "normal": "", "ultrafast": "ultrafast"}[next]
						a.store.ui.Info = info
					}
					return nil
				})
			}},
		{name: "busy", help: "control what enter does while a turn runs", usage: "[queue|steer|interrupt|status]",
			suggest: func(string) []string { return []string{"queue", "steer", "interrupt", "status"} },
			run: func(r *slashRun) tea.Cmd {
				mode := strings.ToLower(trimJS(r.arg))
				params := map[string]any{"key": "busy"}
				if a.gw.canonical() {
					params["session_id"] = nilIfEmpty(r.sid)
				}
				switch mode {
				case "", "status":
					return r.rpc("config.get", params, func(res map[string]any) tea.Cmd {
						current, _ := res["value"].(string)
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
				params["value"] = mode
				return r.rpc("config.set", params, func(res map[string]any) tea.Cmd {
					next, _ := res["value"].(string)
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
			return r.rpc("config.set", map[string]any{"key": "verbose", "session_id": nilIfEmpty(r.sid), "value": value}, func(res map[string]any) tea.Cmd {
				if set, _ := res["value"].(string); set != "" {
					r.sys(tr("slashCmd.session.verbose.current", set))
				}
				return nil
			})
		}},
	}
}
