package tui

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	tea "charm.land/bubbletea/v2"
	"github.com/google/uuid"

	"github.com/nickheyer/icebath/pkg/home"
)

// Markers of rows the compaction wrote, never the target of an undo
var compactionMarkers = []string{"[CONTEXT COMPACTION", "[CONTEXT SUMMARY]:", "[END OF PRIOR CONTEXT"}

// First fifty characters of a queued or steered prompt for its confirmation line
func previewOf(text string) string {
	if utf8.RuneCountInString(text) <= 50 {
		return text
	}
	return string([]rune(text)[:50]) + "…"
}

// Compare and swap mutation whose payload is built from the session snapshot, nothing sent when the builder finds no target
func (s *tuiStore) mutateCanonicalFrom(gw turnGateway, sid, operation string, build func(snapshot map[string]any) (map[string]any, error), stale func() bool, then func(*mutationOutcome, error) tea.Cmd) tea.Cmd {
	if s.pendingMutations == nil {
		s.pendingMutations = map[string]pendingMutation{}
	}
	key := sid + "\x00" + operation + "\x00snapshot"
	s.retireCanonicalControls(sid, key)
	send := func(params map[string]any) tea.Cmd {
		return gatewayCall(gw, "session.mutate", params, func(v any, err error) tea.Cmd {
			result := asRPCResult(v)
			if err == nil && result == nil {
				err = errors.New(tr("session.common.invalidResponse", "session.mutate"))
			}
			if err != nil {
				if hasRefusalReason(err) {
					delete(s.pendingMutations, key)
				}
				return then(nil, err)
			}
			delete(s.pendingMutations, key)
			return then(&mutationOutcome{Result: result, ExpectedGeneration: jsNumberOf(params["expected_generation"])}, nil)
		})
	}
	if pending, has := s.pendingMutations[key]; has {
		return send(pending.params)
	}
	return gatewayCall(gw, "session.resume", map[string]any{"session_id": sid}, func(v any, err error) tea.Cmd {
		if err != nil {
			return then(nil, err)
		}
		snapshot := asRPCResult(v)
		if stale() {
			return then(nil, nil)
		}
		if !isSafeInteger(snapshot["revision"]) || !isSafeInteger(snapshot["execution_generation"]) {
			return then(nil, errors.New(tr("canonical.controls.identityUnavailable")))
		}
		payload, err := build(snapshot)
		if err != nil || payload == nil {
			return then(nil, err)
		}
		params := map[string]any{"session_id": sid, "request_id": uuid.NewString(), "expected_revision": snapshot["revision"],
			"expected_generation": snapshot["execution_generation"], "operation": operation, "payload": payload}
		s.pendingMutations[key] = pendingMutation{sid: sid, params: params}
		return send(params)
	})
}

// Transcript without its last exchange, the newest user row with the replies, tool rows and diffs after it, notes kept
func dropLastExchange(items []msg) []msg {
	last := -1
	for i, m := range items {
		if m.Role == "user" {
			last = i
		}
	}
	if last < 0 {
		return items
	}
	out := append([]msg(nil), items[:last]...)
	for _, m := range items[last+1:] {
		if m.Role == "assistant" || m.Role == "tool" || m.Kind == "trail" || m.Kind == "diff" {
			continue
		}
		out = append(out, m)
	}
	return out
}

// Newest row a user typed, compaction summaries and archived rows passed over
func lastUserTurn(snapshot map[string]any) map[string]any {
	rows, _ := snapshot["stored_messages"].([]any)
	if rows == nil {
		rows, _ = snapshot["messages"].([]any)
	}
	for i := len(rows) - 1; i >= 0; i-- {
		row, _ := rows[i].(map[string]any)
		kind, _ := row["display_kind"].(string)
		if row["role"] != "user" || (kind != "" && kind != "steer") || !isSafeInteger(row["row_id"]) || truthyJS(row["archived"]) {
			continue
		}
		content, _ := row["content"].(string)
		marked := false
		for _, marker := range compactionMarkers {
			marked = marked || strings.Contains(content, marker)
		}
		if !marked {
			return row
		}
	}
	return nil
}

// Text of a user row that can be sent again, false when it holds parts other than text
func retryText(content any) (string, bool) {
	parts, isList := content.([]any)
	if text, isText := content.(string); isText {
		parts, isList = []any{text}, true
	}
	if !isList {
		return "", false
	}
	var b strings.Builder
	for _, part := range parts {
		switch p := part.(type) {
		case string:
			b.WriteString(p)
		case map[string]any:
			kind, _ := p["type"].(string)
			text, isText := p["text"].(string)
			if (kind != "input_text" && kind != "output_text" && kind != "text") || !isText || len(p) != 2 {
				return "", false
			}
			b.WriteString(text)
		default:
			return "", false
		}
	}
	if trimJS(b.String()) == "" {
		return "", true
	}
	return b.String(), true
}

// Undoes the last exchange on the canonical wire, sending the prompt again for a retry
func (r *slashRun) canonicalRewind(retry bool) tea.Cmd {
	a := r.a
	nothing := tr("slashCmd.core.undo.nothing")
	if retry {
		nothing = tr("slashCmd.core.retry.nothing")
	}
	build := func(snapshot map[string]any) (map[string]any, error) {
		target := lastUserTurn(snapshot)
		if target == nil {
			return nil, nil
		}
		if retry {
			text, sendable := retryText(target["content"])
			if !sendable {
				return nil, errors.New(tr("canonical.controls.retryUnsupported"))
			}
			if text == "" {
				return nil, nil
			}
		}
		return map[string]any{"target_message_id": target["row_id"]}, nil
	}
	return a.store.mutateCanonicalFrom(a.gw, r.sid, "rewind", build, r.stale, func(m *mutationOutcome, err error) tea.Cmd {
		if r.stale() {
			return nil
		}
		if err != nil {
			r.fail(err)
			return nil
		}
		removed := 0.0
		if m != nil {
			removed = jsNumberOf(m.Result["rewound_count"])
		}
		if m == nil || removed <= 0 {
			r.sys(nothing)
			return nil
		}
		a.setHistoryItems(dropLastExchange)
		if !retry {
			key := "slashCmd.core.undo.undidOther"
			if removed == 1 {
				key = "slashCmd.core.undo.undidOne"
			}
			r.sys(tr(key, jsString(removed)))
			return nil
		}
		target, _ := m.Result["target_message"].(map[string]any)
		if text, sendable := retryText(target["content"]); sendable && text != "" {
			return a.sendExpanded(text, true, "", submitOpts{})
		}
		r.sys(nothing)
		return nil
	})
}

// File a session export is written to, a named file under the session folder or the saved sessions of the home
func writeSessionExport(payload map[string]any, filename, cwd string) (string, error) {
	content, _ := payload["content"].(string)
	format, _ := payload["format"].(string)
	path := filename
	userHome, _ := os.UserHomeDir()
	switch {
	case filename == "":
		name, _ := payload["filename"].(string)
		if name == "" {
			name = "icebath_conversation_" + time.Now().Format("20060102_150405") + "." + format
		}
		path = filepath.Join(home.Process(), "sessions", "saved", filepath.Base(name))
	case filename == "~" || strings.HasPrefix(filename, "~/"):
		path = filepath.Join(userHome, strings.TrimPrefix(filename, "~"))
	case !filepath.IsAbs(filename):
		path = filepath.Join(cwd, filename)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return "", err
	}
	return path, os.WriteFile(path, []byte(content), 0o644)
}

// Runs a command through the gateway's own command worker and shows what it printed
func (r *slashRun) viaWorker(title, empty string) tea.Cmd {
	return r.rpc("slash.exec", map[string]any{"command": strings.TrimPrefix(r.cmd, "/"), "session_id": nilIfEmpty(r.sid)}, func(res map[string]any) tea.Cmd {
		body, _ := res["output"].(string)
		if body == "" {
			body = empty
		}
		if warning, _ := res["warning"].(string); warning != "" {
			body = tr("slashCmd.ops.slashWorker.warning", warning) + "\n" + body
		}
		return r.a.showOutput(body, title)
	})
}

// Strings of a list answer field
func stringList(v any) []string {
	list, _ := v.([]any)
	out := make([]string, 0, len(list))
	for _, item := range list {
		if s, isStr := item.(string); isStr {
			out = append(out, s)
		}
	}
	return out
}

// Transcript, clipboard, queue and session history commands
func (a *app) transcriptCommands() []slashCommand {
	modes := func(string) []string { return []string{"hidden", "collapsed", "expanded", "cycle"} }
	return []slashCommand{
		{name: "details", aliases: []string{"detail"}, help: "control agent detail visibility, global or per section", usage: "[hidden|collapsed|expanded|cycle]", suggest: modes, run: func(r *slashRun) tea.Cmd {
			ui := &a.store.ui
			if r.arg == "" {
				return gatewayCall(a.gw, "config.get", map[string]any{"key": "details_mode"}, func(v any, err error) tea.Cmd {
					if r.stale() {
						return nil
					}
					mode := ui.DetailsMode
					if parsed := parseDetailsMode(asRPCResult(v)["value"]); err == nil && parsed != "" {
						mode = parsed
						ui.DetailsMode, ui.DetailsModeCommandOverride = mode, false
						a.card.view.invalidate()
					}
					var overrides []string
					for _, s := range sectionNames {
						if ui.Sections[s] != "" {
							overrides = append(overrides, s+"="+ui.Sections[s])
						}
					}
					suffix := ""
					if len(overrides) > 0 {
						suffix = "  (" + strings.Join(overrides, " ") + ")"
					}
					r.sys(tr("slashCmd.core.details.current", mode, suffix))
					return nil
				})
			}
			words := strings.Fields(strings.ToLower(r.arg))
			first := words[0]
			if len(words) > 1 && isSectionName(first) {
				second := words[1]
				reset := second == "reset" || second == "clear" || second == "default"
				mode := parseDetailsMode(second)
				if !reset && mode == "" {
					r.sys(tr("slashCmd.core.details.sectionUsage"))
					return nil
				}
				sections := map[string]string{}
				for k, v := range ui.Sections {
					if k != first {
						sections[k] = v
					}
				}
				shown := tr("slashCmd.core.details.reset")
				if !reset {
					sections[first], shown = mode, mode
				}
				ui.Sections = sections
				a.card.view.invalidate()
				r.sys(tr("slashCmd.core.details.section", first, shown))
				return r.saveDisplayPref("details_mode."+first, mode)
			}
			next := parseDetailsMode(first)
			if first == "cycle" || first == "toggle" {
				next = nextDetailsMode(ui.DetailsMode)
			}
			if next == "" {
				r.sys(tr("slashCmd.core.details.usage"))
				return nil
			}
			sections := map[string]string{}
			for _, s := range sectionNames {
				sections[s] = next
			}
			ui.DetailsMode, ui.DetailsModeCommandOverride, ui.Sections = next, true, sections
			a.card.view.invalidate()
			r.sys(tr("slashCmd.core.details.current", next, ""))
			return r.saveDisplayPref("details_mode", next)
		}},
		{name: "fortune", help: "local fortune", usage: "[random|daily]", run: func(r *slashRun) tea.Cmd {
			switch strings.ToLower(trimJS(r.arg)) {
			case "", "random":
				r.sys(randomFortune())
			case "daily", "stable", "today":
				r.sys(dailyFortune(r.sid))
			default:
				r.sys(tr("slashCmd.core.fortune.usage"))
			}
			return nil
		}},
		{name: "copy", help: "copy the selection or an assistant message", usage: "[n]", run: func(r *slashRun) tea.Cmd {
			if r.arg == "" {
				if selected := a.wm.Selected(); selected != "" {
					return a.copyText(r, selected, func() string {
						key := "slashCmd.core.copy.copiedCharsOther"
						if utf8.RuneCountInString(selected) == 1 {
							key = "slashCmd.core.copy.copiedCharsOne"
						}
						return tr(key, strconv.Itoa(utf8.RuneCountInString(selected)))
					})
				}
			}
			index, err := strconv.Atoi(trimJS(r.arg))
			if r.arg != "" && err != nil {
				r.sys(tr("slashCmd.core.copy.usage"))
				return nil
			}
			var replies []msg
			for _, m := range a.history {
				if m.Role == "assistant" {
					replies = append(replies, m)
				}
			}
			at := len(replies) - 1
			if r.arg != "" {
				at = min(index, len(replies)) - 1
			}
			if at < 0 || at >= len(replies) {
				r.sys(tr("slashCmd.core.copy.nothingToCopy"))
				return nil
			}
			return a.copyText(r, replies[at].Text, func() string { return tr("slashCmd.core.copy.copied") })
		}},
		{name: "paste", help: "attach the clipboard image", run: func(r *slashRun) tea.Cmd {
			if r.arg != "" {
				r.sys(tr("slashCmd.core.paste.usage"))
				return nil
			}
			return a.pasteClipboardImage(false)
		}},
		{name: "image", help: "attach an image file to the next prompt", usage: "<path>", run: func(r *slashRun) tea.Cmd {
			if trimJS(r.arg) == "" {
				r.sys("usage: /image <path>")
				return nil
			}
			return a.attachImagePath(r.arg)
		}},
		{name: "prompt", aliases: []string{"compose"}, help: "compose your next prompt in $EDITOR", usage: "[text]", run: func(r *slashRun) tea.Cmd {
			var set tea.Cmd
			if r.arg != "" {
				set = a.card.composer.SetValue(r.arg)
			}
			return tea.Sequence(set, a.openEditor())
		}},
		{name: "logs", help: "view gateway logs", usage: "[lines]", run: func(r *slashRun) tea.Cmd {
			n, err := strconv.Atoi(trimJS(r.arg))
			if err != nil || n == 0 {
				n = 20
			}
			text := a.gw.logTail(min(80, max(1, n)))
			if text == "" {
				r.sys(tr("slashCmd.core.logs.none"))
				return nil
			}
			return a.page(text, tr("slashCmd.core.logs.pageTitle"))
		}},
		{name: "history", help: "view the current transcript, user and assistant messages", usage: "[chars]", run: func(r *slashRun) tea.Cmd {
			preview, err := strconv.Atoi(trimJS(r.arg))
			if err != nil || preview == 0 {
				preview = 400
			}
			preview = max(80, preview)
			var blocks []string
			for _, m := range a.history {
				if m.Role != "user" && m.Role != "assistant" {
					continue
				}
				index := strconv.Itoa(len(blocks) + 1)
				tag := tr("slashCmd.core.history.hermesTag", index)
				if m.Role == "user" {
					tag = tr("slashCmd.core.history.youTag", index)
				}
				body := trimJS(m.Text)
				switch {
				case body != "":
				case len(m.Tools) == 1:
					body = tr("slashCmd.core.history.toolCallsOne", "1")
				case len(m.Tools) > 1:
					body = tr("slashCmd.core.history.toolCallsOther", strconv.Itoa(len(m.Tools)))
				default:
					body = tr("slashCmd.core.history.empty")
				}
				if utf8.RuneCountInString(body) > preview {
					body = strings.TrimRight(string([]rune(body)[:preview]), " \n\t") + "…"
				}
				blocks = append(blocks, "["+tag+"]\n"+body)
			}
			if len(blocks) == 0 {
				r.sys(tr("slashCmd.core.history.noConversation"))
				return r.ran()
			}
			return tea.Batch(r.ran(), a.page(strings.Join(blocks, "\n\n"), tr("slashCmd.core.history.pageTitle")))
		}},
		{name: "save", help: "save the current transcript to a file", usage: "[json|md] [file] [redact]", run: func(r *slashRun) tea.Cmd {
			has := false
			for _, m := range a.history {
				has = has || m.Role == "user" || m.Role == "assistant" || m.Role == "tool"
			}
			if !has {
				r.sys(tr("slashCmd.core.save.noConversation"))
				return nil
			}
			if r.sid == "" {
				r.sys(tr("slashCmd.core.save.noActiveSession"))
				return nil
			}
			if !a.gw.canonical() {
				return r.rpc("session.save", map[string]any{"session_id": r.sid}, func(res map[string]any) tea.Cmd {
					if file, _ := res["file"].(string); file != "" {
						r.sys(tr("slashCmd.core.save.saved", file))
					} else {
						r.sys(tr("slashCmd.core.save.failed"))
					}
					return nil
				})
			}
			parts := strings.Fields(r.arg)
			redact := false
			if n := len(parts); n > 0 && strings.EqualFold(strings.TrimLeft(parts[n-1], "-"), "redact") {
				redact, parts = true, parts[:n-1]
			}
			format, filename := "json", ""
			if len(parts) > 0 {
				format = parts[0]
			}
			if len(parts) > 1 {
				filename = parts[1]
			}
			cwd := a.store.sessionCwd()
			if attachURLEnv() != "" {
				cwd = os.Getenv("ICEBATH_CWD")
			}
			if cwd == "" {
				cwd, _ = os.Getwd()
			}
			return r.rpc("session.export", map[string]any{"format": format, "redact": redact, "session_id": r.sid}, func(res map[string]any) tea.Cmd {
				path, err := writeSessionExport(res, filename, cwd)
				if err != nil {
					r.sys(tr("slashCmd.core.save.failed") + ": " + err.Error())
					return nil
				}
				r.sys(tr("slashCmd.core.save.saved", path))
				return nil
			})
		}},
		{name: "focus", help: "show only your prompt and the final response", usage: "[on|off|status]", run: func(r *slashRun) tea.Cmd {
			mode := strings.ToLower(trimJS(r.arg))
			current := a.store.ui.FocusView
			if mode == "status" || mode == "show" || mode == "?" {
				if current {
					r.sys(tr("slashCmd.core.focus.statusOn"))
				} else {
					r.sys(tr("slashCmd.core.focus.statusOff"))
				}
				return nil
			}
			next, ok := flagFromArg(mode, current)
			if !ok {
				r.sys(tr("slashCmd.core.focus.usage"))
				return nil
			}
			a.store.ui.FocusView = next
			a.card.view.invalidate()
			if next {
				r.sys(tr("slashCmd.core.focus.enabled"))
			} else {
				r.sys(tr("slashCmd.core.focus.disabled"))
			}
			return r.saveDisplayPref("focus", onOff(next))
		}},
		{name: "statusbar", aliases: []string{"sb"}, help: "status bar position", usage: "[on|off|top|bottom|toggle]", run: func(r *slashRun) tea.Cmd {
			mode := strings.ToLower(trimJS(r.arg))
			next := ""
			switch mode {
			case "", "toggle":
				next = "off"
				if a.store.ui.StatusBar == "off" {
					next = "top"
				}
			case "on", "top":
				next = "top"
			case "off", "bottom":
				next = mode
			}
			if next == "" {
				r.sys(tr("slashCmd.core.statusbar.usage"))
				return nil
			}
			a.store.ui.StatusBar = next
			r.sys(tr("slashCmd.core.statusbar.state", next))
			return r.saveDisplayPref("statusbar", next)
		}},
		{name: "battery", help: "battery indicator in the status bar", usage: "[on|off|status]", run: func(r *slashRun) tea.Cmd {
			mode := strings.ToLower(trimJS(r.arg))
			if mode == "status" || mode == "show" {
				state := onOff(a.store.ui.Battery)
				return gatewayCall(a.gw, "system.battery", map[string]any{}, func(v any, err error) tea.Cmd {
					res := asRPCResult(v)
					percent, hasPercent := res["percent"].(float64)
					switch {
					case err != nil:
						r.sys(tr("slashCmd.core.battery.state", state))
					case truthyJS(res["available"]) && hasPercent:
						mark := "🔋"
						if truthyJS(res["plugged"]) {
							mark = "⚡"
						}
						r.sys(tr("slashCmd.core.battery.statusLive", state, mark, jsString(percent)))
					default:
						r.sys(tr("slashCmd.core.battery.statusNoBattery", state))
					}
					return nil
				})
			}
			next, ok := flagFromArg(r.arg, a.store.ui.Battery)
			if !ok {
				r.sys(tr("slashCmd.core.battery.usage"))
				return nil
			}
			a.store.ui.Battery = next
			if !next {
				a.store.ui.BatteryStatus = nil
			}
			r.sys(tr("slashCmd.core.battery.state", onOff(next)))
			return r.saveDisplayPref("battery", onOff(next))
		}},
		{name: "queue", aliases: []string{"q"}, help: "inspect or enqueue a message", usage: "[text|edit <n>|drop <n>]", run: func(r *slashRun) tea.Cmd {
			shown, err := a.queue.queuedDisplay()
			if err != nil {
				r.sys("error: " + err.Error())
				return nil
			}
			words := strings.Fields(r.arg)
			if len(words) == 2 && (words[0] == "edit" || words[0] == "drop") {
				n, err := strconv.Atoi(words[1])
				if err != nil || n < 1 || n > len(shown) {
					r.sys(fmt.Sprintf("usage: /queue %s <1-%d>", words[0], len(shown)))
					return nil
				}
				if words[0] == "drop" {
					cmd, err := a.queue.removeQ(n - 1)
					if err != nil {
						r.sys("error: " + err.Error())
					}
					return cmd
				}
				draft, err := a.queue.queueDraft(n - 1)
				if err != nil {
					r.sys("error: " + err.Error())
					return nil
				}
				index := n - 1
				if err := a.queue.setQueueEdit(&index); err != nil {
					r.sys("error: " + err.Error())
					return nil
				}
				a.queueEdit = &index
				return a.card.composer.SetValue(draft)
			}
			key := "slashCmd.core.queue.countOther"
			if len(shown) == 1 {
				key = "slashCmd.core.queue.countOne"
			}
			r.sys(tr(key, strconv.Itoa(len(shown))))
			return nil
		}},
		{name: "steer", aliases: []string{"s"}, help: "inject a message after the next tool call, no interrupt", usage: "<text>", run: func(r *slashRun) tea.Cmd {
			payload := trimJS(r.arg)
			if payload == "" {
				r.sys(tr("slashCmd.core.steer.usage"))
				return nil
			}
			if !a.store.ui.Busy || r.sid == "" {
				a.enqueue(payload, payload, r.destination)
				r.sys(tr("slashCmd.core.steer.noActiveTurnQueued", previewOf(payload)))
				return nil
			}
			params := map[string]any{"session_id": r.sid, "text": payload}
			if generation, has := a.store.ui.Info["execution_generation"]; has && a.gw.canonical() {
				params["execution_generation"] = generation
			}
			return r.rpc("session.steer", params, func(res map[string]any) tea.Cmd {
				if res["status"] == "queued" {
					r.sys(tr("slashCmd.core.steer.queued", previewOf(payload)))
					return nil
				}
				a.enqueue(payload, payload, r.destination)
				r.sys(tr("slashCmd.core.steer.rejected"))
				return nil
			})
		}},
		{name: "undo", help: "undo the last exchange", run: func(r *slashRun) tea.Cmd {
			if r.sid == "" {
				r.sys(tr("slashCmd.core.undo.nothing"))
				return nil
			}
			if a.gw.canonical() {
				return r.canonicalRewind(false)
			}
			return r.rpc("session.undo", map[string]any{"session_id": r.sid}, func(res map[string]any) tea.Cmd {
				removed, _ := res["removed"].(float64)
				if removed <= 0 {
					r.sys(tr("slashCmd.core.undo.nothing"))
					return nil
				}
				a.setHistoryItems(dropLastExchange)
				key := "slashCmd.core.undo.undidOther"
				if removed == 1 {
					key = "slashCmd.core.undo.undidOne"
				}
				r.sys(tr(key, jsString(removed)))
				return nil
			})
		}},
		{name: "retry", help: "retry the last user message", run: func(r *slashRun) tea.Cmd {
			if r.sid != "" && a.gw.canonical() {
				return r.canonicalRewind(true)
			}
			last := a.lastUserMsg
			if last == "" {
				r.sys(tr("slashCmd.core.retry.nothing"))
				return nil
			}
			if r.sid == "" {
				return a.sendExpanded(last, true, "", submitOpts{})
			}
			return r.rpc("session.undo", map[string]any{"intent": "retry", "session_id": r.sid}, func(res map[string]any) tea.Cmd {
				if removed, _ := res["removed"].(float64); removed <= 0 {
					r.sys(tr("slashCmd.core.retry.nothing"))
					return nil
				}
				a.setHistoryItems(dropLastExchange)
				return a.sendExpanded(last, true, "", submitOpts{})
			})
		}},
	}
}

// Copies text to the system clipboard, through the terminal when the session is remote or no clipboard tool answers
func (a *app) copyText(r *slashRun, text string, done func() string) tea.Cmd {
	if isRemoteShellSession(os.LookupEnv) {
		r.sys(tr("slashCmd.core.copy.sentOsc52"))
		return tea.SetClipboard(text)
	}
	return func() tea.Msg {
		ok := writeClipboardText(a.ctx, text)
		return asyncDoneMsg{run: func() tea.Cmd {
			if r.stale() {
				return nil
			}
			if ok {
				r.sys(done())
				return nil
			}
			r.sys(tr("slashCmd.core.copy.sentOsc52"))
			return tea.SetClipboard(text)
		}}
	}
}

// Process, reload, browser, checkpoint, skill and tool commands
func (a *app) opsCommands() []slashCommand {
	return []slashCommand{
		{name: "stop", help: "stop background processes", run: func(r *slashRun) tea.Cmd {
			var interrupt tea.Cmd
			if a.gw.canonical() && a.store.ui.Busy && r.sid != "" {
				interrupt = a.interruptTurn()
			}
			params := map[string]any{}
			if r.sid != "" {
				params["session_id"] = r.sid
			}
			return tea.Batch(interrupt, r.rpc("process.stop", params, func(res map[string]any) tea.Cmd {
				killed, _ := res["killed"].(float64)
				key := "slashCmd.ops.stop.stoppedOther"
				if killed == 1 {
					key = "slashCmd.ops.stop.stoppedOne"
				}
				r.sys(tr(key, jsString(killed)))
				return nil
			}))
		}},
		{name: "reload-mcp", aliases: []string{"reload_mcp"}, help: "reload MCP servers in the live session", usage: "[now|always]", run: func(r *slashRun) tea.Cmd {
			arg := strings.ToLower(trimJS(r.arg))
			confirm := arg == "now" || arg == "approve" || arg == "once" || arg == "yes" || arg == "always"
			params := map[string]any{"session_id": nilIfEmpty(r.sid)}
			if confirm {
				params["confirm"] = true
			}
			always := arg == "always" && !a.gw.canonical()
			if always {
				params["always"] = true
			}
			return r.rpc("reload.mcp", params, func(res map[string]any) tea.Cmd {
				message, _ := res["message"].(string)
				switch {
				case a.gw.canonical() && message != "":
					r.sys(message)
				case res["status"] == "confirm_required":
					if message == "" {
						message = tr("slashCmd.ops.reloadMcp.confirmRequired")
					}
					r.sys(message)
				case res["status"] == "reloaded" && always:
					r.sys(tr("slashCmd.ops.reloadMcp.reloadedAlways"))
				case res["status"] == "reloaded" || a.gw.canonical():
					r.sys(tr("slashCmd.ops.reloadMcp.reloaded"))
				default:
					r.sys(tr("slashCmd.ops.reloadMcp.complete"))
				}
				return nil
			})
		}},
		{name: "reload", help: "read the .env of the home again into the running gateway", run: func(r *slashRun) tea.Cmd {
			params := map[string]any{}
			if a.gw.canonical() && r.sid != "" {
				params["session_id"] = r.sid
			}
			return r.rpc("reload.env", params, func(res map[string]any) tea.Cmd {
				n, _ := res["updated"].(float64)
				key := "slashCmd.ops.reloadEnv.reloadedOther"
				if n == 1 {
					key = "slashCmd.ops.reloadEnv.reloadedOne"
				}
				r.sys(tr(key, jsString(n)))
				return nil
			})
		}},
		{name: "browser", help: "manage the agent browser", usage: "[connect|disconnect|status]",
			suggest: func(string) []string { return []string{"connect", "disconnect", "status"} },
			run: func(r *slashRun) tea.Cmd {
				words := strings.Fields(r.arg)
				action := "status"
				if len(words) > 0 {
					action = strings.ToLower(words[0])
				}
				if action != "connect" && action != "disconnect" && action != "status" {
					r.sys(tr("slashCmd.ops.browser.usage"))
					return nil
				}
				params := map[string]any{"action": action, "session_id": nilIfEmpty(r.sid)}
				if action == "connect" {
					url := "http://127.0.0.1:9222"
					if len(words) > 1 {
						url = strings.Join(words[1:], " ")
					}
					params["url"] = url
					r.sys(tr("slashCmd.ops.browser.checking", url))
				}
				return r.rpc("browser.manage", params, func(res map[string]any) tea.Cmd {
					if r.sid == "" {
						for _, message := range stringList(res["messages"]) {
							r.sys(message)
						}
					}
					url, _ := res["url"].(string)
					if url == "" {
						url = tr("slashCmd.ops.browser.urlUnavailable")
					}
					switch {
					case action == "status" && truthyJS(res["connected"]):
						r.sys(tr("slashCmd.ops.browser.connected", url))
					case action == "status":
						r.sys(tr("slashCmd.ops.browser.notConnected"))
					case action == "disconnect":
						r.sys(tr("slashCmd.ops.browser.disconnected"))
					case truthyJS(res["connected"]):
						r.sys(tr("slashCmd.ops.browser.connectedLive"))
						r.sys(tr("slashCmd.ops.browser.endpoint", url))
						r.sys(tr("slashCmd.ops.browser.nextCallUsesEndpoint"))
					}
					return nil
				})
			}},
		{name: "rollback", help: "list, diff, or restore checkpoints", usage: "[list|diff <hash>|<hash> [file] [--all]]", run: func(r *slashRun) tea.Cmd {
			if r.sid == "" {
				r.sys(tr("slashCmd.ops.rollback.noSession"))
				return nil
			}
			words := strings.Fields(r.arg)
			first := ""
			if len(words) > 0 {
				first = words[0]
			}
			switch strings.ToLower(first) {
			case "", "list", "ls":
				return r.rpc("rollback.list", map[string]any{"session_id": r.sid}, func(res map[string]any) tea.Cmd {
					if !truthyJS(res["enabled"]) {
						r.sys(tr("slashCmd.ops.rollback.notEnabled"))
						return nil
					}
					checkpoints, _ := res["checkpoints"].([]any)
					if len(checkpoints) == 0 {
						r.sys(tr("slashCmd.ops.rollback.noneFound"))
						return nil
					}
					rows := make([][2]string, 0, len(checkpoints))
					for i, raw := range checkpoints {
						c, _ := raw.(map[string]any)
						hash, _ := c["hash"].(string)
						var meta []string
						for _, k := range []string{"timestamp", "message"} {
							if s, _ := c[k].(string); s != "" {
								meta = append(meta, s)
							}
						}
						detail := strings.Join(meta, " · ")
						if detail == "" {
							detail = tr("slashCmd.ops.rollback.noMetadata")
						}
						rows = append(rows, [2]string{fmt.Sprintf("%d. %s", i+1, hash[:min(len(hash), 10)]), detail})
					}
					a.panel(tr("slashCmd.ops.rollback.listTitle"), []panelSection{{Rows: rows}})
					return nil
				})
			case "diff":
				if len(words) < 2 {
					r.sys(tr("slashCmd.ops.rollback.usageDiff"))
					return nil
				}
				return r.rpc("rollback.diff", map[string]any{"hash": words[1], "session_id": r.sid}, func(res map[string]any) tea.Cmd {
					body, _ := res["rendered"].(string)
					if body == "" {
						body, _ = res["diff"].(string)
					}
					stat, _ := res["stat"].(string)
					if body = trimJS(body); body == "" && stat == "" {
						r.sys(tr("slashCmd.ops.rollback.noChanges"))
						return nil
					}
					text := body
					if stat != "" && body != "" {
						text = stat + "\n\n" + body
					} else if stat != "" {
						text = stat
					}
					return a.page(text, tr("slashCmd.ops.rollback.diffTitle"))
				})
			}
			var files []string
			all := false
			for _, w := range words[1:] {
				if lower := strings.ToLower(w); lower == "--all" || lower == "--force" {
					all = true
					continue
				}
				files = append(files, w)
			}
			filePath := strings.Join(files, " ")
			params := map[string]any{"hash": first, "session_id": r.sid}
			if filePath != "" {
				params["file_path"] = filePath
			} else if all {
				params["all"] = true
			}
			return r.rpc("rollback.restore", params, func(res map[string]any) tea.Cmd {
				text := func(keys ...string) string {
					for _, k := range keys {
						if s, _ := res[k].(string); s != "" {
							return s
						}
					}
					return ""
				}
				if !truthyJS(res["success"]) {
					reason := text("error", "message")
					if reason == "" {
						reason = tr("slashCmd.ops.rollback.unknownError")
					}
					r.sys(tr("slashCmd.ops.rollback.failed", reason))
					return nil
				}
				target := filePath
				if target == "" {
					target = tr("slashCmd.ops.rollback.workspaceTarget")
				}
				detail := text("reason", "message", "restored_to")
				if detail == "" {
					detail = tr("slashCmd.ops.rollback.restoredDetail")
				}
				r.sys(tr("slashCmd.ops.rollback.restored", target, detail))
				for _, pair := range [][2]string{{"keptUserEdits", "skipped_user_edits"}, {"keptOversize", "skipped_oversize"}, {"failedDeletes", "failed_deletes"}} {
					if paths := stringList(res[pair[1]]); len(paths) > 0 {
						summary := strings.Join(paths[:min(len(paths), 5)], ", ")
						if len(paths) > 5 {
							summary += tr("slashCmd.ops.rollback.morePaths", len(paths)-5)
						}
						r.sys(tr("slashCmd.ops.rollback."+pair[0], summary))
					}
				}
				if removed, _ := res["history_removed"].(float64); removed > 0 {
					a.setHistoryItems(dropLastExchange)
				}
				return nil
			})
		}},
		{name: "reload-skills", aliases: []string{"reload_skills"}, help: "scan installed skills again in the live gateway", run: func(r *slashRun) tea.Cmd {
			params := map[string]any{}
			if r.sid != "" {
				params["session_id"] = r.sid
			}
			return r.rpc("skills.reload", params, func(res map[string]any) tea.Cmd {
				out, _ := res["output"].(string)
				if out == "" {
					out = tr("slashCmd.ops.reloadSkills.reloaded")
				}
				return tea.Batch(a.page(out, tr("slashCmd.ops.reloadSkills.pageTitle")), a.events.loadSlashCatalog(r.sid))
			})
		}},
		{name: "skills", help: "browse, inspect, install skills", usage: "[list|inspect|search|install|browse]",
			suggest: func(string) []string { return []string{"list", "inspect", "search", "install", "browse"} },
			run: func(r *slashRun) tea.Cmd {
				text := trimJS(r.arg)
				if text == "" {
					return a.openSkillsHub()
				}
				sub, query, _ := strings.Cut(text, " ")
				query = trimJS(query)
				switch sub {
				case "list":
					return r.rpc("skills.manage", map[string]any{"action": "list"}, func(res map[string]any) tea.Cmd {
						skills, _ := res["skills"].(map[string]any)
						if len(skills) == 0 {
							r.sys(tr("userMessages.skills.noneInstalled"))
							return nil
						}
						var sections []panelSection
						for _, category := range sortedKeys(skills) {
							title := category
							sections = append(sections, panelSection{Items: stringList(skills[category]), Title: &title})
						}
						a.panel(tr("slashCmd.ops.skills.listTitle"), sections)
						return nil
					})
				case "inspect":
					if query == "" {
						r.sys(tr("slashCmd.ops.skills.usageInspect"))
						return nil
					}
					return r.rpc("skills.manage", map[string]any{"action": "inspect", "query": query}, func(res map[string]any) tea.Cmd {
						info, _ := res["info"].(map[string]any)
						name, _ := info["name"].(string)
						if name == "" {
							r.sys(tr("slashCmd.ops.skills.unknownSkill", query))
							return nil
						}
						category, _ := info["category"].(string)
						path, _ := info["path"].(string)
						sections := []panelSection{{Rows: [][2]string{{tr("slashCmd.ops.skills.rowName"), name}, {tr("slashCmd.ops.skills.rowCategory"), category}, {tr("slashCmd.ops.skills.rowPath"), path}}}}
						if description, _ := info["description"].(string); description != "" {
							sections = append(sections, panelSection{Text: &description})
						}
						a.panel(tr("slashCmd.ops.skills.inspectTitle"), sections)
						return nil
					})
				case "search":
					if query == "" {
						r.sys(tr("slashCmd.ops.skills.usageSearch"))
						return nil
					}
					return r.rpc("skills.manage", map[string]any{"action": "search", "query": query}, func(res map[string]any) tea.Cmd {
						results, _ := res["results"].([]any)
						if len(results) == 0 {
							r.sys(tr("slashCmd.ops.skills.noResults", query))
							return nil
						}
						rows := make([][2]string, 0, len(results))
						for _, raw := range results {
							s, _ := raw.(map[string]any)
							name, _ := s["name"].(string)
							description, _ := s["description"].(string)
							rows = append(rows, [2]string{name, description})
						}
						a.panel(tr("slashCmd.ops.skills.searchTitle", query), []panelSection{{Rows: rows}})
						return nil
					})
				case "install":
					if query == "" {
						r.sys(tr("slashCmd.ops.skills.usageInstall"))
						return nil
					}
					r.sys(tr("slashCmd.ops.skills.installing", query))
					return r.rpc("skills.manage", map[string]any{"action": "install", "query": query}, func(res map[string]any) tea.Cmd {
						if !truthyJS(res["installed"]) {
							r.sys(tr("slashCmd.ops.skills.installFailed"))
							return nil
						}
						name, _ := res["name"].(string)
						if name == "" {
							name = query
						}
						r.sys(tr("slashCmd.ops.skills.installed", name))
						return a.events.loadSlashCatalog(r.sid)
					})
				case "browse":
					pageNum := 1
					if query != "" {
						n, err := strconv.Atoi(query)
						if err != nil || n < 1 {
							r.sys(tr("slashCmd.ops.skills.usageBrowse"))
							return nil
						}
						pageNum = n
					}
					r.sys(tr("slashCmd.ops.skills.fetching"))
					return r.rpc("skills.manage", map[string]any{"action": "browse", "page": pageNum}, func(res map[string]any) tea.Cmd {
						items, _ := res["items"].([]any)
						total, _ := res["total"].(float64)
						if len(items) == 0 {
							if total > 0 {
								r.sys(tr("slashCmd.ops.skills.noneOnPageWithTotal", strconv.Itoa(pageNum), jsString(total)))
							} else {
								r.sys(tr("slashCmd.ops.skills.noneOnPage", strconv.Itoa(pageNum)))
							}
							return nil
						}
						rows := make([][2]string, 0, len(items))
						for _, raw := range items {
							s, _ := raw.(map[string]any)
							name, _ := s["name"].(string)
							if trust, _ := s["trust"].(string); trust != "" {
								name += " · " + trust
							}
							description, _ := s["description"].(string)
							rows = append(rows, [2]string{name, previewOfN(description, 160)})
						}
						page, _ := res["page"].(float64)
						pages, _ := res["total_pages"].(float64)
						var footer []string
						if page > 0 && pages > 0 {
							footer = append(footer, tr("slashCmd.ops.skills.pageOf", jsString(page), jsString(pages)))
						}
						if total > 0 {
							footer = append(footer, tr("slashCmd.ops.skills.skillsTotal", jsString(total)))
						}
						if page > 0 && page < pages {
							footer = append(footer, tr("slashCmd.ops.skills.browseMore", jsString(page+1)))
						}
						sections := []panelSection{{Rows: rows}}
						if len(footer) > 0 {
							foot := strings.Join(footer, " · ")
							sections = append(sections, panelSection{Text: &foot})
						}
						title := tr("slashCmd.ops.skills.browseTitle")
						if pageNum > 1 {
							title = tr("slashCmd.ops.skills.browseTitlePage", strconv.Itoa(pageNum))
						}
						a.panel(title, sections)
						return nil
					})
				}
				return r.viaWorker(tr("slashCmd.ops.slashWorker.skillsTitle"), tr("slashCmd.ops.slashWorker.skillsNoOutput"))
			}},
		{name: "plugins", help: "view and toggle plugins", usage: "[enable|disable <name>]", run: func(r *slashRun) tea.Cmd {
			if trimJS(r.arg) == "" {
				return a.openPluginsHub()
			}
			return r.viaWorker(tr("slashCmd.ops.slashWorker.pluginsTitle"), tr("slashCmd.ops.slashWorker.pluginsNoOutput"))
		}},
		{name: "agents", aliases: []string{"tasks"}, help: "open the agents of the session, with stop and pause controls", usage: "[pause|resume|status]",
			suggest: func(string) []string { return []string{"pause", "resume", "status"} },
			run: func(r *slashRun) tea.Cmd {
				switch sub := strings.ToLower(trimJS(r.arg)); sub {
				case "pause", "resume", "unpause":
					return r.rpc("delegation.pause", map[string]any{"paused": sub == "pause"}, func(res map[string]any) tea.Cmd {
						a.store.applyDelegationStatus(map[string]any{"paused": res["paused"]})
						if truthyJS(res["paused"]) {
							r.sys(tr("slashCmd.ops.agents.paused"))
						} else {
							r.sys(tr("slashCmd.ops.agents.resumed"))
						}
						return nil
					})
				case "status":
					d := a.store.delegation
					state := tr("slashCmd.ops.agents.stateActive")
					if d.Paused {
						state = tr("slashCmd.ops.agents.statePaused")
					}
					limit := func(v *float64) string {
						if v == nil {
							return "?"
						}
						return jsString(*v)
					}
					r.sys(tr("slashCmd.ops.agents.status", state, limit(d.MaxSpawnDepth), limit(d.MaxConcurrentChildren)))
					return nil
				}
				return a.openAgents()
			}},
	}
}

// First n characters of a text
func previewOfN(text string, n int) string {
	if utf8.RuneCountInString(text) <= n {
		return text
	}
	return string([]rune(text)[:n])
}

// Keys of an object in order
func sortedKeys(m map[string]any) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
