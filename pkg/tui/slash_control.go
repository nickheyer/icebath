package tui

import (
	"errors"
	"fmt"
	"github.com/nickheyer/icebath/pkg/tuigateway"
	"os"
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
func (r *slashRun) printCompressSummary(summary *tuigateway.CompressionSummary) {
	if summary == nil {
		summary = &tuigateway.CompressionSummary{}
	}
	switch {
	case summary.Headline == "":
		r.sys(tr("canonical.controls.compressed"))
	case summary.Noop:
		r.sys(summary.Headline)
	default:
		r.sys("✓ " + summary.Headline)
	}
	for _, line := range []string{summary.TokenLine, val(summary.Note)} {
		if line != "" {
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
			child := val(result.BranchedSessionID)
			if child == "" {
				r.fail(errors.New(tr("session.common.invalidResponse", "branch")))
				return nil
			}
			return a.resumeByID(child, func(err error) tea.Cmd {
				if err != nil {
					a.uncaught(err)
					return nil
				}
				return call(a.gw, tuigateway.CanonicalSessionInfo, tuigateway.CanonicalSessionInfoParams{SessionID: child}, func(info *tuigateway.SessionInfo, _ error) tea.Cmd {
					if a.store.ui.Sid != child {
						return nil
					}
					title := trimJS(val(infoOf(info).Title))
					for _, fallback := range []string{trimJS(r.arg), child} {
						if title == "" {
							title = fallback
						}
					}
					a.sys(tr("slashCmd.session.branch.branched", title))
					return nil
				})
			})
		}
		switch val(result.Status) {
		case "preview":
			r.sys(strings.Join(result.Lines, "\n"))
			return nil
		case "noop":
			r.printCompressSummary(result.Summary)
			return nil
		}
		return call(a.gw, tuigateway.CanonicalSessionResume, tuigateway.CanonicalResumeParams{SessionID: r.sid, DisplayProjection: true}, func(snapshot *tuigateway.CanonicalSnapshot, err error) tea.Cmd {
			if r.stale() || a.store.ui.Busy != busy || a.store.ui.Info != baseline {
				return nil
			}
			if err != nil {
				r.fail(err)
				return nil
			}
			attached := snapshotAttachment(snapshot)
			info := patchedInfo(baseline, *attached.Info)
			if !sameStamp(info.AuthorityEpoch, infoOf(baseline).AuthorityEpoch) || val(info.ExecutionGeneration) < val(result.ExecutionGeneration) {
				r.fail(errors.New(tr("canonical.controls.compressedStale")))
				return nil
			}
			a.setHistoryItems(func([]msg) []msg {
				return append([]msg{introMsg(info)}, toTranscriptMessages(attached.Messages)...)
			})
			usage := *usageOf(a.store.ui.Usage)
			compressions, estimated, emptied, unused := val(usage.Compressions)+1, false, int64(0), int64(0)
			usage.Compressions, usage.ContextEstimated, usage.ContextPercent, usage.ContextUsed = &compressions, &estimated, &emptied, &unused
			a.store.ui.Info, a.store.ui.Usage = info, &usage
			a.card.view.invalidate()
			r.printCompressSummary(result.Summary)
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

// Compress, branch and the commands that inspect the terminal UI itself
func (a *app) controlCommands() []slashCommand {
	return []slashCommand{
		{name: "compress", help: "compress the transcript", usage: "[focus]", run: func(r *slashRun) tea.Cmd {
			if a.gw.canonical() {
				return r.canonicalControl("compress")
			}
			params := tuigateway.SessionCompressParams{SessionID: r.sid}
			if r.arg != "" {
				params.FocusTopic = &r.arg
			}
			return ask(r.rpc, a.sys, tuigateway.SessionCompress, params, func(res *tuigateway.SessionCompressResult) tea.Cmd {
				if res == nil {
					return nil
				}
				info := a.store.ui.Info
				if res.Info != nil {
					info = patchedInfo(info, *res.Info)
					a.store.ui.Info = info
				}
				if res.Messages != nil {
					rows := toTranscriptMessages(res.Messages)
					if info != nil {
						rows = append([]msg{introMsg(info)}, rows...)
					}
					a.setHistoryItems(func([]msg) []msg { return rows })
				}
				if res.Usage != nil {
					a.store.ui.Usage = mergeUsageStable(a.store.ui.Usage, res.Usage)
				}
				if res.Summary != nil && res.Summary.Headline != "" {
					r.printCompressSummary(res.Summary)
					return nil
				}
				removed := val(res.Removed)
				if removed <= 0 {
					r.sys(tr("slashCmd.session.compress.nothing"))
					return nil
				}
				key := "slashCmd.session.compress.compressedOther"
				if removed == 1 {
					key = "slashCmd.session.compress.compressedOne"
				}
				suffix := ""
				if res.Usage != nil && res.Usage.Total > 0 {
					suffix = tr("slashCmd.session.compress.tokSuffix", compactNumber(float64(res.Usage.Total)))
				}
				r.sys(tr(key, strconv.FormatInt(removed, 10)) + suffix)
				return nil
			})
		}},
		{name: "branch", aliases: []string{"fork"}, help: "branch the session", usage: "[title]", run: func(r *slashRun) tea.Cmd {
			if a.gw.canonical() {
				return r.canonicalControl("branch")
			}
			return ask(r.rpc, a.sys, tuigateway.SessionBranch, tuigateway.SessionBranchParams{SessionID: r.sid, Name: &r.arg}, func(res *tuigateway.SessionBranchResult) tea.Cmd {
				if res == nil || res.SessionID == "" {
					return nil
				}
				r.sys(tr("slashCmd.session.branch.branched", res.Title))
				return a.resumeByID(res.SessionID, nil)
			})
		}},
		{name: "setup", help: "configure a section: model, aux, agent, tts, terminal, tools, gateway, or start over with quick, blank or import", usage: "[section] [provider]", suggest: func(string) []string {
			keys := []string{}
			for _, s := range setupSections() {
				keys = append(keys, s.key)
			}
			return keys
		}, run: func(r *slashRun) tea.Cmd { return a.openSetup(trimJS(r.arg)) }},
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
					saved := detail(res)
					if truthyJS(res["disabled_persisted"]) {
						saved += " · " + tr("slashCmd.wake.disabledInConfig")
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
		{name: "usage", help: "session usage", run: func(r *slashRun) tea.Cmd {
			if r.sid == "" {
				r.sys(tr("slashCmd.core.status.noActiveSession"))
				return nil
			}
			if !a.gw.canonical() {
				return a.sessionUsage(r)
			}
			return tea.Batch(r.ran(), ask(r.rpc, a.sys, tuigateway.CanonicalSessionStats, tuigateway.SessionStatsParams{SessionID: r.sid}, func(res *tuigateway.SessionStats) tea.Cmd {
				if res == nil {
					return nil
				}
				var counters tuigateway.SessionUsagePayload
				if err := tuigateway.Loose(wireParams(res), &counters); err != nil {
					r.sys("error: " + tr("session.common.invalidResponse", "session.stats") + " " + err.Error())
					return nil
				}
				usage := usageOf(usageOfCounters(counters))
				if usage.Calls == 0 {
					r.sys(tr("slashCmd.session.usage.noCalls"))
				} else {
					a.store.ui.Usage = mergeUsageStable(a.store.ui.Usage, usage)
					a.panel(tr("slashCmd.session.usage.usageTitle"), usageTotalsSections(usage))
				}
				return nil
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
				return requestSessions(a.gw, true, "", func(sessions []listedSession, err error) tea.Cmd {
					if err != nil {
						r.fail(err)
						return nil
					}
					for _, s := range sessions {
						if s.ID == r.sid || s.SessionID == r.sid {
							show(s.Title)
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
					if set := trimJS(val(m.Result.Title)); set != "" {
						next = set
					}
					a.store.ui.SessionTitle = next
					r.sys(tr("slashCmd.core.title.set", next, ""))
					return nil
				})
			case title == "":
				return ask(r.rpc, a.sys, tuigateway.SessionTitle, tuigateway.SessionTitleParams{SessionID: r.sid}, func(res *tuigateway.SessionTitleResult) tea.Cmd {
					if res != nil {
						show(res.Title)
					}
					return nil
				})
			}
			return ask(r.rpc, a.sys, tuigateway.SessionTitle, tuigateway.SessionTitleParams{SessionID: r.sid, Title: &title}, func(res *tuigateway.SessionTitleResult) tea.Cmd {
				if res == nil {
					return nil
				}
				next := title
				if set := trimJS(res.Title); set != "" {
					next = set
				}
				suffix := ""
				if val(res.Pending) {
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
			stamp := strings.NewReplacer(":", "-", ".", "-").Replace(time.Now().UTC().Format("2006-01-02T15:04:05.000Z"))
			path := filepath.Join(dir, fmt.Sprintf("icebath-%s-%d-manual.pprof", stamp, os.Getpid()))
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
			r.sys(countNoun(len(widgets), "widget") + ": " + strings.Join(names, " "))
			return nil
		}},
	}
}
