package tui

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"runtime/pprof"
	"strconv"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/ui/render"
)

// Prints the report of a compress, its headline, token line and note
func (r *slashRun) printCompressSummary(summary map[string]any) {
	headline, _ := summary["headline"].(string)
	switch {
	case headline == "":
		r.sys(tr("canonical.controls.compressed"))
	case truthyJS(summary["noop"]):
		r.sys(headline)
	default:
		r.sys("✓ " + headline)
	}
	for _, key := range []string{"token_line", "note"} {
		if line, _ := summary[key].(string); line != "" {
			r.sys("  " + line)
		}
	}
}

// Branches or compresses the session through the canonical mutation and shows what the owner did
func (r *slashRun) canonicalControl(operation string) tea.Cmd {
	a := r.a
	if r.sid == "" {
		r.sys(tr("slashCmd.core.status.noActiveSession"))
		return nil
	}
	baseline, busy := a.store.ui.Info, a.store.ui.Busy
	return a.store.mutateCanonicalSession(a.gw, r.sid, operation, trimJS(r.arg), r.stale, "", func(m *mutationOutcome, err error) tea.Cmd {
		if r.stale() {
			return nil
		}
		if err != nil {
			r.fail(err)
			return nil
		}
		if m == nil || a.store.isSupersededControl(operation, m.Result, m.ExpectedGeneration, baseline) {
			return nil
		}
		result := m.Result
		if operation == "branch" {
			child, _ := result["branched_session_id"].(string)
			if child == "" {
				r.fail(errors.New(tr("session.common.invalidResponse", "branch")))
				return nil
			}
			return a.resumeByID(child, func(err error) tea.Cmd {
				if err != nil {
					a.uncaught(err)
					return nil
				}
				return gatewayCall(a.gw, "session.info", map[string]any{"session_id": child}, func(v any, _ error) tea.Cmd {
					if a.store.ui.Sid != child {
						return nil
					}
					title, _ := asRPCResult(v)["title"].(string)
					for _, fallback := range []string{trimJS(r.arg), child} {
						if trimJS(title) == "" {
							title = fallback
						}
					}
					a.sys(tr("slashCmd.session.branch.branched", trimJS(title)))
					return nil
				})
			})
		}
		summary, _ := result["summary"].(map[string]any)
		switch result["status"] {
		case "preview":
			r.sys(strings.Join(stringList(result["lines"]), "\n"))
			return nil
		case "noop":
			r.printCompressSummary(summary)
			return nil
		}
		return gatewayCall(a.gw, "session.resume", map[string]any{"session_id": r.sid}, func(v any, err error) tea.Cmd {
			if r.stale() || a.store.ui.Busy != busy || !sameMap(a.store.ui.Info, baseline) {
				return nil
			}
			snapshot := asRPCResult(v)
			messages, isList := snapshot["messages"].([]any)
			if err != nil || !isList {
				if err == nil {
					err = errors.New(tr("session.common.invalidResponse", "compressed transcript"))
				}
				r.fail(err)
				return nil
			}
			info := map[string]any{}
			for k, v := range baseline {
				info[k] = v
			}
			fresh, _ := snapshot["info"].(map[string]any)
			for k, v := range fresh {
				info[k] = v
			}
			if info["execution_epoch"] != baseline["execution_epoch"] || jsNumberOf(info["execution_generation"]) < jsNumberOf(result["execution_generation"]) {
				r.fail(errors.New(tr("canonical.controls.compressedStale")))
				return nil
			}
			a.setHistoryItems(func([]msg) []msg { return append([]msg{introMsg(info)}, toTranscriptMessages(messages)...) })
			usage := map[string]any{}
			for k, v := range a.store.ui.Usage {
				usage[k] = v
			}
			prior, _ := usage["compressions"].(float64)
			usage["compressions"], usage["context_estimated"], usage["context_percent"], usage["context_used"] = prior+1, false, float64(0), float64(0)
			a.store.ui.Info, a.store.ui.Usage = info, usage
			a.card.view.invalidate()
			r.printCompressSummary(summary)
			return nil
		})
	})
}

// Whole number with a comma between every three digits
func groupDigits(n float64) string {
	digits := strconv.FormatInt(int64(n), 10)
	sign := ""
	if strings.HasPrefix(digits, "-") {
		sign, digits = "-", digits[1:]
	}
	for i := len(digits) - 3; i > 0; i -= 3 {
		digits = digits[:i] + "," + digits[i:]
	}
	return sign + digits
}

// Time left until a usage window resets, soon inside the last minute
func resetsIn(resetsAtMs, nowMs float64) string {
	left := time.Duration(resetsAtMs-nowMs) * time.Millisecond
	switch {
	case resetsAtMs <= 0 || left <= 0:
		return ""
	case left < time.Minute:
		return "resets soon"
	case left < time.Hour:
		return fmt.Sprintf("resets in %dm", int(left.Minutes()))
	case left < 24*time.Hour:
		return fmt.Sprintf("resets in %dh %dm", int(left.Hours()), int(left.Minutes())%60)
	}
	return fmt.Sprintf("resets in %dd %dh", int(left.Hours())/24, int(left.Hours())%24)
}

// Gauge rows of a plan with usage windows, each window with its share used and when it resets
func planUsageLines(plan map[string]any, nowMs float64) []string {
	if plan["kind"] != "usage_window" {
		return nil
	}
	gauges, _ := plan["gauges"].([]any)
	type row struct{ label, rest string }
	var rows []row
	width := 0
	for _, raw := range gauges {
		g, _ := raw.(map[string]any)
		left, isNum := g["left_fraction"].(float64)
		if !isNum {
			continue
		}
		used := min(max(1-left, 0), 1)
		filled := int(used*10 + 0.5)
		if used > 0 {
			filled = max(filled, 1)
		}
		if used < 1 {
			filled = min(filled, 9)
		}
		pct := "0%"
		switch points := int(used*10000 + 0.000001); {
		case points >= 100:
			pct = strconv.Itoa(points/100) + "%"
		case points > 0:
			pct = "<1%"
		}
		rest := "[" + strings.Repeat("|", filled) + strings.Repeat(" ", 10-filled) + "]  " + pct + " used"
		resets, _ := g["resets_at_ms"].(float64)
		if when := resetsIn(resets, nowMs); when != "" {
			rest += "  ·  " + when
		}
		label, _ := g["title"].(string)
		if label == "" {
			label, _ = g["label"].(string)
		}
		width = max(width, len([]rune(label)))
		rows = append(rows, row{label, rest})
	}
	if len(rows) == 0 {
		return nil
	}
	var lines []string
	if name, _ := plan["plan"].(string); name != "" {
		lines = append(lines, name)
	}
	for _, r := range rows {
		lines = append(lines, r.label+strings.Repeat(" ", width-len([]rune(r.label)))+"  "+r.rest)
	}
	return lines
}

// Compress, branch and the commands that inspect the terminal UI itself
func (a *app) controlCommands() []slashCommand {
	return []slashCommand{
		{name: "compress", help: "compress the transcript", usage: "[focus]", run: func(r *slashRun) tea.Cmd {
			if a.gw.canonical() {
				return r.canonicalControl("compress")
			}
			params := map[string]any{"session_id": nilIfEmpty(r.sid)}
			if r.arg != "" {
				params["focus_topic"] = r.arg
			}
			return r.rpc("session.compress", params, func(res map[string]any) tea.Cmd {
				info := a.store.ui.Info
				if fresh, _ := res["info"].(map[string]any); fresh != nil {
					merged := map[string]any{}
					for k, v := range info {
						merged[k] = v
					}
					for k, v := range fresh {
						merged[k] = v
					}
					info = merged
					a.store.ui.Info = info
				}
				if messages, isList := res["messages"].([]any); isList {
					rows := toTranscriptMessages(messages)
					if info != nil {
						rows = append([]msg{introMsg(info)}, rows...)
					}
					a.setHistoryItems(func([]msg) []msg { return rows })
				}
				usage, _ := res["usage"].(map[string]any)
				if usage != nil {
					merged := map[string]any{}
					for k, v := range a.store.ui.Usage {
						merged[k] = v
					}
					for k, v := range usage {
						merged[k] = v
					}
					a.store.ui.Usage = merged
				}
				if summary, _ := res["summary"].(map[string]any); truthyJS(summary["headline"]) {
					r.printCompressSummary(summary)
					return nil
				}
				removed, _ := res["removed"].(float64)
				if removed <= 0 {
					r.sys(tr("slashCmd.session.compress.nothing"))
					return nil
				}
				key := "slashCmd.session.compress.compressedOther"
				if removed == 1 {
					key = "slashCmd.session.compress.compressedOne"
				}
				suffix := ""
				if total, _ := usage["total"].(float64); total > 0 {
					suffix = tr("slashCmd.session.compress.tokSuffix", compactNumber(total))
				}
				r.sys(tr(key, jsString(removed)) + suffix)
				return nil
			})
		}},
		{name: "branch", aliases: []string{"fork"}, help: "branch the session", usage: "[title]", run: func(r *slashRun) tea.Cmd {
			if a.gw.canonical() {
				return r.canonicalControl("branch")
			}
			return r.rpc("session.branch", map[string]any{"name": r.arg, "session_id": nilIfEmpty(r.sid)}, func(res map[string]any) tea.Cmd {
				child, _ := res["session_id"].(string)
				if child == "" {
					return nil
				}
				title, _ := res["title"].(string)
				r.sys(tr("slashCmd.session.branch.branched", title))
				return a.resumeByID(child, nil)
			})
		}},
		{name: "setup", help: "run the full setup wizard, the terminal handed to `icebath setup` until it exits", usage: "[section]", run: func(r *slashRun) tea.Cmd {
			self, err := os.Executable()
			if err != nil {
				r.sys("error: " + err.Error())
				return nil
			}
			return tea.ExecProcess(exec.Command(self, append([]string{"setup"}, strings.Fields(r.arg)...)...), func(runErr error) tea.Msg {
				return asyncDoneMsg{run: func() tea.Cmd {
					var exit *exec.ExitError
					switch {
					case errors.As(runErr, &exit):
						a.sys(tr("session.handoff.exitedWithCode", "setup", exit.ExitCode()))
					case runErr != nil:
						a.sys("error: " + runErr.Error())
					default:
						a.sys(tr("slashCmd.setup.setup.done"))
					}
					if a.store.ui.Sid == "" && runErr == nil {
						return tea.Batch(tea.ClearScreen, a.newSession(nil))
					}
					return tea.ClearScreen
				}}
			})
		}},
		{name: "wake", help: "toggle the wake word listener", usage: "[on|off|status]", suggest: func(string) []string { return []string{"on", "off", "status"} }, run: func(r *slashRun) tea.Cmd {
			text := func(res map[string]any, key string) string {
				s, _ := res[key].(string)
				return s
			}
			withHint := func(line, hint string) string {
				if trimJS(hint) != "" {
					return tr("slashCmd.wake.withHint", line, trimJS(hint))
				}
				return line
			}
			detail := func(res map[string]any) string {
				out := ""
				if phrase := text(res, "phrase"); phrase != "" {
					out = " " + tr("slashCmd.wake.forPhrase", phrase)
				}
				if provider := text(res, "provider"); provider != "" {
					out += " · " + provider
				}
				return out
			}
			switch strings.ToLower(trimJS(r.arg)) {
			case "on":
				a.store.wakeUserDisabled = false
				return r.rpc("wake.start", map[string]any{"persist": true, "surface": "tui"}, func(res map[string]any) tea.Cmd {
					if !truthyJS(res["started"]) {
						reason := text(res, "reason")
						base := reason
						if key, known := map[string]string{"disabled": "disabled", "disabled_for_surface": "disabledForSurface", "not_owner": "notOwner", "owned": "owned", "unavailable": "unavailable"}[reason]; known {
							base = tr("slashCmd.wake.reason." + key)
						} else if reason == "" {
							base = tr("slashCmd.wake.reason.unknown")
						}
						if owner := text(res, "owner_surface"); owner != "" {
							base += " (" + tr("slashCmd.wake.ownedBy", owner) + ")"
						}
						r.sys(withHint(tr("slashCmd.wake.notStarted", base), text(res, "hint")))
						return nil
					}
					saved := ""
					if truthyJS(res["enabled_persisted"]) {
						saved = " · " + tr("slashCmd.wake.enabledInConfig")
					}
					r.sys(tr("slashCmd.wake.listening", detail(res)+saved))
					return nil
				})
			case "off":
				a.store.wakeUserDisabled = true
				return r.rpc("wake.stop", map[string]any{"persist": true}, func(res map[string]any) tea.Cmd {
					saved := ""
					if truthyJS(res["disabled_persisted"]) {
						saved = " · " + tr("slashCmd.wake.disabledInConfig")
					}
					if truthyJS(res["stopped"]) {
						r.sys(tr("slashCmd.wake.listenerOff", saved))
						return nil
					}
					reason := text(res, "reason")
					switch reason {
					case "not_owner":
						reason = tr("slashCmd.wake.notOwnerStop")
					case "":
						reason = tr("slashCmd.wake.notRunning")
					}
					r.sys(tr("slashCmd.wake.nothingToStop", reason, saved))
					return nil
				})
			case "", "status":
				return r.rpc("wake.status", map[string]any{}, func(res map[string]any) tea.Cmd {
					details := detail(res)
					switch {
					case truthyJS(res["listening"]) && truthyJS(res["audio_silent"]):
						r.sys(withHint(tr("slashCmd.wake.listening", details)+" · "+tr("slashCmd.wake.micSilent"), text(res, "hint")))
					case truthyJS(res["listening"]):
						r.sys(tr("slashCmd.wake.listening", details))
					case text(res, "owner_surface") != "" && !truthyJS(res["owned_by_caller"]):
						r.sys(tr("slashCmd.wake.offOwnedBy", text(res, "owner_surface"), details))
					case res["available"] == false:
						r.sys(withHint(tr("slashCmd.wake.unavailable"), text(res, "hint")))
					default:
						r.sys(tr("slashCmd.wake.off", details))
					}
					return nil
				})
			}
			r.sys(tr("slashCmd.wake.usage"))
			return nil
		}},
		{name: "usage", help: "session usage and plan windows", run: func(r *slashRun) tea.Cmd {
			if r.sid == "" || !a.gw.canonical() {
				return r.viaWorker("usage", tr("slashCmd.session.usage.noCalls"))
			}
			return tea.Batch(r.ran(), r.rpc("session.stats", map[string]any{"session_id": r.sid}, func(res map[string]any) tea.Cmd {
				usage := usageFromTick(res)
				number := func(key string) float64 {
					n, _ := usage[key].(float64)
					return n
				}
				if number("calls") == 0 {
					r.sys(tr("slashCmd.session.usage.noCalls"))
				} else {
					merged := map[string]any{}
					for k, v := range a.store.ui.Usage {
						merged[k] = v
					}
					for k, v := range usage {
						merged[k] = v
					}
					a.store.ui.Usage = merged
					model, _ := usage["model"].(string)
					sections := []panelSection{{Rows: [][2]string{
						{tr("slashCmd.session.usage.rowModel"), model}, {tr("slashCmd.session.usage.rowInputTokens"), groupDigits(number("input"))},
						{tr("slashCmd.session.usage.rowOutputTokens"), groupDigits(number("output"))}, {tr("slashCmd.session.usage.rowTotalTokens"), groupDigits(number("total"))},
						{tr("slashCmd.session.usage.rowApiCalls"), groupDigits(number("calls"))},
					}}}
					if number("context_max") > 0 {
						mark := ""
						if truthyJS(usage["context_estimated"]) {
							mark = "~"
						}
						text := tr("slashCmd.session.usage.context", mark+groupDigits(number("context_used")), groupDigits(number("context_max")), mark+jsString(number("context_percent")))
						sections = append(sections, panelSection{Text: &text})
					}
					if n := number("compressions"); n > 0 {
						text := tr("slashCmd.session.usage.compressions", jsString(n))
						sections = append(sections, panelSection{Text: &text})
					}
					a.panel(tr("slashCmd.session.usage.usageTitle"), sections)
				}
				r.sys(tr("slashCmd.session.usage.cta"))
				return gatewayCall(a.gw, "billing.state", map[string]any{}, func(v any, err error) tea.Cmd {
					plan, _ := asRPCResult(v)["plan_usage"].(map[string]any)
					if err != nil || r.stale() {
						return nil
					}
					for _, line := range planUsageLines(plan, a.store.nowMillis()) {
						r.sys(line)
					}
					return nil
				})
			}))
		}},
		{name: "voice", help: "voice mode", usage: "[on|off|tts|status]", suggest: func(string) []string { return []string{"on", "off", "tts", "status"} }, run: func(r *slashRun) tea.Cmd {
			action := strings.ToLower(trimJS(r.arg))
			if action != "on" && action != "off" && action != "tts" {
				action = "status"
			}
			if a.gw.canonical() && action == "tts" {
				r.sys(tr("canonical.controls.notAvailable", "voice tts"))
				return nil
			}
			if a.gw.canonical() && r.sid != "" {
				return r.rpc("slash.exec", map[string]any{"command": "voice " + action, "session_id": r.sid}, func(res map[string]any) tea.Cmd {
					if out, _ := res["output"].(string); trimJS(out) != "" {
						r.sys(trimJS(out))
					}
					return r.rpc("config.get", map[string]any{"key": "voice", "session_id": r.sid}, func(cfg map[string]any) tea.Cmd {
						a.voiceEnabled = cfg["value"] == "voice_only" || cfg["value"] == "all"
						return nil
					})
				})
			}
			return r.rpc("voice.toggle", map[string]any{"action": action}, func(res map[string]any) tea.Cmd {
				a.voiceEnabled = truthyJS(res["enabled"])
				key := a.voiceKey
				if raw, _ := res["record_key"].(string); raw != "" {
					key = parseVoiceRecordKey(raw)
					a.setVoiceRecordKey(key)
				}
				label := a.keys.Display("tui.voice")
				onOffWord := func(on bool) string {
					if on {
						return tr("slashCmd.session.voice.on")
					}
					return tr("slashCmd.session.voice.off")
				}
				switch {
				case action == "status":
					r.sys(tr("slashCmd.session.voice.statusTitle"))
					r.sys(tr("slashCmd.session.voice.statusMode", onOffWord(a.voiceEnabled)))
					r.sys(tr("slashCmd.session.voice.statusTts", onOffWord(truthyJS(res["tts"]))))
					r.sys(tr("slashCmd.session.voice.statusRecordKey", label))
					if details, _ := res["details"].(string); details != "" {
						r.sys("")
						r.sys(tr("slashCmd.session.voice.requirements"))
						for _, line := range strings.Split(details, "\n") {
							if trimJS(line) != "" {
								r.sys("    " + line)
							}
						}
					}
				case action == "tts" && truthyJS(res["tts"]):
					r.sys(tr("slashCmd.session.voice.ttsEnabled"))
				case action == "tts":
					r.sys(tr("slashCmd.session.voice.ttsDisabled"))
				case a.voiceEnabled:
					if truthyJS(res["tts"]) {
						r.sys(tr("slashCmd.session.voice.enabledWithTts"))
					} else {
						r.sys(tr("slashCmd.session.voice.enabled"))
					}
					r.sys(tr("slashCmd.session.voice.recordHint", label))
					if hint, _ := res["stop_hint"].(string); hint != "" {
						r.sys("  " + hint)
					}
					r.sys(tr("slashCmd.session.voice.ttsHint"))
					r.sys(tr("slashCmd.session.voice.offHint"))
				default:
					r.sys(tr("slashCmd.session.voice.disabled"))
				}
				return nil
			})
		}},
		{name: "title", help: "set or show the session title", usage: "[title]", run: func(r *slashRun) tea.Cmd {
			if r.sid == "" {
				r.sys(tr("slashCmd.core.title.noActiveSession"))
				return nil
			}
			title := trimJS(r.arg)
			show := func(current string) {
				if trimJS(current) == "" {
					r.sys(tr("slashCmd.core.title.none"))
					return
				}
				r.sys(tr("slashCmd.core.title.current", trimJS(current)))
			}
			switch {
			case a.gw.canonical() && title == "":
				return r.rpc("session.list", map[string]any{"limit": 200}, func(res map[string]any) tea.Cmd {
					sessions, _ := res["sessions"].([]any)
					for _, raw := range sessions {
						if row, _ := raw.(map[string]any); row["id"] == r.sid || row["session_id"] == r.sid {
							current, _ := row["title"].(string)
							show(current)
							return nil
						}
					}
					show("")
					return nil
				})
			case a.gw.canonical():
				return a.store.mutateCanonical(a.gw, r.sid, "rename", map[string]any{"title": title}, r.stale, func(m *mutationOutcome, err error) tea.Cmd {
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
					next := title
					if set, _ := m.Result["title"].(string); trimJS(set) != "" {
						next = trimJS(set)
					}
					a.store.ui.SessionTitle = next
					r.sys(tr("slashCmd.core.title.set", next, ""))
					return nil
				})
			case title == "":
				return r.rpc("session.title", map[string]any{"session_id": r.sid}, func(res map[string]any) tea.Cmd {
					current, _ := res["title"].(string)
					show(current)
					return nil
				})
			}
			return r.rpc("session.title", map[string]any{"session_id": r.sid, "title": title}, func(res map[string]any) tea.Cmd {
				next := title
				if set, _ := res["title"].(string); trimJS(set) != "" {
					next = trimJS(set)
				}
				suffix := ""
				if truthyJS(res["pending"]) {
					suffix = tr("slashCmd.core.title.queuedSuffix")
				}
				a.store.ui.SessionTitle = next
				r.sys(tr("slashCmd.core.title.set", next, suffix))
				return nil
			})
		}},
		{name: "theme-info", help: "show the colors and brand of the active theme", run: func(r *slashRun) tea.Cmd {
			t := a.store.ui.Theme
			rows := [][2]string{{"brand", t.Brand.Name}, {"prompt", t.Brand.Prompt}, {"terminal background", a.store.themes.terminalBg}, {"terminal foreground", a.store.themes.terminalFg}}
			for _, slot := range []string{"primary", "accent", "border", "text", "muted", "label", "ok", "warn", "error", "tool", "thinking", "prompt", "diffAdded", "diffRemoved"} {
				if tone := t.Color.slot(slot); tone != nil {
					rows = append(rows, [2]string{slot, *tone})
				}
			}
			a.panel("Theme", []panelSection{{Rows: rows}})
			return nil
		}},
		{name: "mem", help: "show the memory the terminal UI holds", run: func(r *slashRun) tea.Cmd {
			var stats runtime.MemStats
			runtime.ReadMemStats(&stats)
			a.panel("Memory", []panelSection{{Rows: [][2]string{
				{"heap in use", render.Bytes(int64(stats.HeapInuse))}, {"heap objects", fmt.Sprint(stats.HeapObjects)}, {"from the system", render.Bytes(int64(stats.Sys))},
				{"collections", fmt.Sprint(stats.NumGC)}, {"goroutines", fmt.Sprint(runtime.NumGoroutine())}, {"transcript rows", fmt.Sprint(len(a.history))},
				{"rendered blocks kept", fmt.Sprint(len(a.card.view.cache))},
			}}})
			return nil
		}},
		{name: "heapdump", help: "write a heap profile of the terminal UI to a file", run: func(r *slashRun) tea.Cmd {
			dir := filepath.Join(home.Process(), "logs")
			if err := os.MkdirAll(dir, 0o755); err != nil {
				r.sys("error: " + err.Error())
				return nil
			}
			path := filepath.Join(dir, "tui-heap-"+time.Now().Format("20060102-150405")+".pprof")
			file, err := os.Create(path)
			if err != nil {
				r.sys("error: " + err.Error())
				return nil
			}
			defer file.Close()
			runtime.GC()
			if err := pprof.WriteHeapProfile(file); err != nil {
				r.sys("error: " + err.Error())
				return nil
			}
			r.sys("heap profile written to " + path + ", read it with `go tool pprof`")
			return nil
		}},
		{name: "widgets-reload", help: "read the widgets of plugins again", run: func(r *slashRun) tea.Cmd {
			widgets := a.widgets.list()
			names := make([]string, len(widgets))
			for i, w := range widgets {
				names[i] = "/" + w.ID
			}
			a.card.composer.SetCommands(a.composerCommands())
			r.sys(countNoun(len(widgets), "widget") + ": " + strings.Join(names, " "))
			return nil
		}},
	}
}
