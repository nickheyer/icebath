package tui

import (
	"errors"
	"fmt"
	"maps"
	"math"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	tea "charm.land/bubbletea/v2"
	"github.com/google/uuid"

	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/tuigateway"
	"github.com/nickheyer/icebath/pkg/ui/clip"
	"github.com/nickheyer/icebath/pkg/ui/txt"
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
func (s *tuiStore) mutateCanonicalFrom(gw turnGateway, sid, operation string, build func(snapshot *tuigateway.CanonicalSnapshot) (map[string]any, error), stale func() bool, then func(*mutationOutcome, error) tea.Cmd) tea.Cmd {
	if s.pendingMutations == nil {
		s.pendingMutations = map[string]pendingMutation{}
	}
	key := sid + "\x00" + operation + "\x00snapshot"
	s.retireCanonicalControls(sid, key)
	send := func(params tuigateway.SessionMutateParams) tea.Cmd {
		return call(gw, tuigateway.CanonicalSessionMutate, params, func(result *tuigateway.SessionMutateResult, err error) tea.Cmd {
			if err != nil {
				if hasRefusalReason(err) {
					delete(s.pendingMutations, key)
				}
				return then(nil, err)
			}
			delete(s.pendingMutations, key)
			return then(&mutationOutcome{Result: result, ExpectedGeneration: val(params.ExpectedGeneration)}, nil)
		})
	}
	if pending, has := s.pendingMutations[key]; has {
		return send(pending.params)
	}
	return call(gw, tuigateway.CanonicalSessionResume, tuigateway.CanonicalResumeParams{SessionID: sid}, func(snapshot *tuigateway.CanonicalSnapshot, err error) tea.Cmd {
		if err != nil {
			return then(nil, err)
		}
		if stale() {
			return then(nil, nil)
		}
		payload, err := build(snapshot)
		if err != nil || payload == nil {
			return then(nil, err)
		}
		params := tuigateway.SessionMutateParams{SessionID: sid, RequestID: uuid.NewString(), ExpectedRevision: snapshot.Revision,
			ExpectedGeneration: ptr(snapshot.ExecutionGeneration), Operation: operation, Payload: payload}
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
func lastUserTurn(snapshot *tuigateway.CanonicalSnapshot) *tuigateway.TranscriptMessage {
	rows := snapshot.Messages
	for i := len(rows) - 1; i >= 0; i-- {
		row := rows[i]
		kind := val(row.DisplayKind)
		if row.Role != "user" || (kind != "" && kind != "steer") || row.RowID == nil || val(row.Archived) {
			continue
		}
		content, _ := row.Content.(string)
		marked := false
		for _, marker := range compactionMarkers {
			marked = marked || strings.Contains(content, marker)
		}
		if !marked {
			return &rows[i]
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
	nothing, intent := tr(txt.SlashCmdCoreUndoNothing), "undo"
	if retry {
		nothing, intent = tr(txt.SlashCmdCoreRetryNothing), "retry"
	}
	build := func(snapshot *tuigateway.CanonicalSnapshot) (map[string]any, error) {
		target := lastUserTurn(snapshot)
		if target == nil {
			return nil, nil
		}
		if retry {
			text, sendable := retryText(target.Content)
			if !sendable {
				return nil, errors.New(tr(txt.CanonicalControlsRetryUnsupported))
			}
			if text == "" {
				return nil, nil
			}
		}
		return map[string]any{"target_message_id": *target.RowID}, nil
	}
	return a.store.mutateCanonicalFrom(a.gw, r.sid, "rewind", build, r.stale, func(m *mutationOutcome, err error) tea.Cmd {
		if r.stale() {
			return nil
		}
		if err != nil {
			if busy := rewindBusyText(err, intent); busy != "" {
				r.sys(busy)
				return nil
			}
			r.fail(err)
			return nil
		}
		removed := int64(0)
		if m != nil {
			removed = val(m.Result.RewoundCount)
		}
		if m == nil || removed <= 0 {
			r.sys(nothing)
			return nil
		}
		a.setHistoryItems(dropLastExchange)
		if !retry {
			key := txt.SlashCmdCoreUndoUndidOther
			if removed == 1 {
				key = txt.SlashCmdCoreUndoUndidOne
			}
			r.sys(tr(key, strconv.FormatInt(removed, 10)))
			return r.showUndoRollbackHint(m.Result.TargetMessage)
		}
		var content any
		if m.Result.TargetMessage != nil {
			content = m.Result.TargetMessage.Content
		}
		if text, sendable := retryText(content); sendable && text != "" {
			return a.sendExpanded(text, true, "", submitOpts{})
		}
		r.sys(nothing)
		return nil
	})
}

// Most files the line after an undo names before it counts the rest
const undoHintNamedFiles = 8

// Line naming the files the undone turns changed and pointing at the rollback command, empty for no files
func undoRollbackHintLine(hint *tuigateway.UndoRollbackHint) string {
	if hint == nil || len(hint.Files) == 0 {
		return ""
	}
	files := hint.Files
	named := strings.Join(files[:min(len(files), undoHintNamedFiles)], ", ")
	if len(files) > undoHintNamedFiles {
		named += " " + tr(txt.SlashCmdCoreUndoRollbackHintMore, strconv.Itoa(len(files)-undoHintNamedFiles))
	}
	return tr(txt.SlashCmdCoreUndoRollbackHint, named)
}

// Asks the owner which files the undone turns changed and names them, silent when it knows none or refuses the question
func (r *slashRun) showUndoRollbackHint(target *tuigateway.TranscriptMessage) tea.Cmd {
	if target == nil || target.Timestamp == nil {
		return nil
	}
	params := tuigateway.RollbackListParams{SessionID: r.sid, UndoSince: target.Timestamp, UndoMessageID: target.RowID}
	switch id := target.Extra["id"].(type) {
	case float64:
		if id == math.Trunc(id) {
			params.UndoMessageID = ptr(int64(id))
		}
	case int64:
		params.UndoMessageID = &id
	case int:
		params.UndoMessageID = ptr(int64(id))
	}
	return call(r.a.gw, tuigateway.RollbackList, params, func(res *tuigateway.RollbackListResult, err error) tea.Cmd {
		if err != nil || r.stale() {
			return nil
		}
		if line := undoRollbackHintLine(res.UndoHint); line != "" {
			r.sys(line)
		}
		return nil
	})
}

// Format, file name and redaction flag of the save command's argument, only a trailing word being the flag
func parseSaveArgs(arg string) (format, filename string, redact bool) {
	parts := strings.Fields(arg)
	if n := len(parts); n > 0 {
		word := strings.TrimLeft(parts[n-1], "-")
		if strings.EqualFold(word, "redact") && len(parts[n-1])-len(word) <= 2 {
			redact, parts = true, parts[:n-1]
		}
	}
	format = "json"
	if len(parts) > 0 {
		format = parts[0]
	}
	if len(parts) > 1 {
		filename = parts[1]
	}
	return format, filename, redact
}

// File a session export is written to, a named file under the session folder or the saved sessions of the home
func writeSessionExport(payload *tuigateway.SessionExportResult, filename, cwd string) (string, error) {
	path := filename
	userHome, _ := os.UserHomeDir()
	switch {
	case filename == "":
		name := payload.Filename
		if name == "" {
			name = "icebath_conversation_" + time.Now().Format("20060102_150405") + "." + payload.Format
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
	return path, os.WriteFile(path, []byte(payload.Content), 0o644)
}

// Runs a command at the gateway and shows what it printed
func (r *slashRun) viaGateway(title, empty string) tea.Cmd {
	return ask(r.rpc, r.sys, tuigateway.SlashExec, tuigateway.SlashExecParams{Command: strings.TrimPrefix(r.cmd, "/"), SessionID: r.sid}, func(res *tuigateway.SlashExecResult) tea.Cmd {
		if res == nil {
			return nil
		}
		body := res.Output
		if body == "" {
			body = empty
		}
		return r.a.showOutput(body, title)
	})
}

// Transcript, clipboard, queue and session history commands
func (a *app) transcriptCommands() []slashCommand {
	return []slashCommand{
		{name: "detail", help: "how much of the agent's tool work and thinking you see", usage: "[minimal|normal|detailed|everything] [--session]",
			suggest: func(string) []string { return append(slices.Clone(detailLevels), "--session") }, run: func(r *slashRun) tea.Cmd { return a.detailSlash(r) }},
		{name: "fortune", help: "local fortune", usage: "[random|daily]", run: func(r *slashRun) tea.Cmd {
			switch strings.ToLower(trimJS(r.arg)) {
			case "", "random":
				r.sys(randomFortune())
			case "daily":
				r.sys(dailyFortune(r.sid))
			default:
				r.sys(tr(txt.SlashCmdCoreFortuneUsage))
			}
			return nil
		}},
		{name: "copy", help: "copy the selection, or the nth newest reply (1 is the last)", usage: "[n]", run: func(r *slashRun) tea.Cmd {
			if r.arg == "" {
				if selected := a.wm.Selected(); selected != "" {
					return a.copyText(r, selected, func() string {
						key := txt.SlashCmdCoreCopyCopiedCharsOther
						if utf8.RuneCountInString(selected) == 1 {
							key = txt.SlashCmdCoreCopyCopiedCharsOne
						}
						return tr(key, strconv.Itoa(utf8.RuneCountInString(selected)))
					})
				}
			}
			index, err := strconv.Atoi(trimJS(r.arg))
			if r.arg != "" && (err != nil || index < 1) {
				r.sys(tr(txt.SlashCmdCoreCopyUsage))
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
				at = len(replies) - index
			}
			if at < 0 || at >= len(replies) {
				r.sys(tr(txt.SlashCmdCoreCopyNothingToCopy))
				return nil
			}
			return a.copyText(r, replies[at].Text, func() string { return tr(txt.SlashCmdCoreCopyCopied) })
		}},
		{name: "paste", help: "attach the clipboard image", run: func(r *slashRun) tea.Cmd {
			if r.arg != "" {
				r.sys(tr(txt.SlashCmdCorePasteUsage))
				return nil
			}
			return a.pasteClipboardImage(false)
		}},
		{name: "image", help: "attach an image, or the pages of a PDF, to the next prompt", usage: "<path> [pages]", run: func(r *slashRun) tea.Cmd {
			if trimJS(r.arg) == "" {
				r.sys("usage: /image <path> [pages]")
				return nil
			}
			return a.attachImagePath(r.arg)
		}},
		{name: "prompt", help: "compose your next prompt in $EDITOR", usage: "[text]", run: func(r *slashRun) tea.Cmd {
			var set tea.Cmd
			if r.arg != "" {
				set = a.card.composer.SetValue(r.arg)
			}
			return a.sequence(set, a.openEditor())
		}},
		{name: "terminal-setup", help: "configure IDE terminal keybindings for multiline, undo, redo and copy", usage: "[auto|vscode|cursor|windsurf]",
			suggest: func(string) []string { return []string{"auto", "vscode", "cursor", "windsurf"} }, run: func(r *slashRun) tea.Cmd { return r.terminalSetup() }},
		{name: "logs", help: "icebath's log files, followed and filtered", run: func(r *slashRun) tea.Cmd {
			return tea.Batch(r.ran(), a.openSystem("logs"))
		}},
		{name: "client-log", help: "the last lines this window logged about its connection", usage: "[lines]", run: func(r *slashRun) tea.Cmd {
			n, err := strconv.Atoi(trimJS(r.arg))
			if err != nil || n == 0 {
				n = 20
			}
			text := a.gw.logTail(min(80, max(1, n)))
			if text == "" {
				r.sys(tr(txt.SlashCmdCoreLogsNone))
				return nil
			}
			return a.page(text, tr(txt.SlashCmdCoreLogsPageTitle))
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
				tag := tr(txt.SlashCmdCoreHistoryAgentTag, index)
				if m.Role == "user" {
					tag = tr(txt.SlashCmdCoreHistoryYouTag, index)
				}
				body := trimJS(m.Text)
				switch {
				case body != "":
				case len(m.Tools) == 1:
					body = tr(txt.SlashCmdCoreHistoryToolCallsOne, "1")
				case len(m.Tools) > 1:
					body = tr(txt.SlashCmdCoreHistoryToolCallsOther, strconv.Itoa(len(m.Tools)))
				default:
					body = tr(txt.SlashCmdCoreHistoryEmpty)
				}
				if utf8.RuneCountInString(body) > preview {
					body = strings.TrimRight(string([]rune(body)[:preview]), " \n\t") + "…"
				}
				blocks = append(blocks, "["+tag+"]\n"+body)
			}
			if len(blocks) == 0 {
				r.sys(tr(txt.SlashCmdCoreHistoryNoConversation))
				return r.ran()
			}
			return tea.Batch(r.ran(), a.page(strings.Join(blocks, "\n\n"), tr(txt.SlashCmdCoreHistoryPageTitle)))
		}},
		{name: "save", help: "save the transcript to a file as json, md or html", usage: "[json|md|html] [file] [redact]",
			suggest: func(string) []string { return []string{"json", "md", "html"} }, run: func(r *slashRun) tea.Cmd {
				has := false
				for _, m := range a.history {
					has = has || m.Role == "user" || m.Role == "assistant" || m.Role == "tool"
				}
				if !has {
					r.sys(tr(txt.SlashCmdCoreSaveNoConversation))
					return nil
				}
				if r.sid == "" {
					r.sys(tr(txt.SlashCmdCoreSaveNoActiveSession))
					return nil
				}
				format, filename, redact := parseSaveArgs(r.arg)
				cwd := a.store.sessionCwd()
				if attachURLEnv() != "" {
					cwd = os.Getenv("ICEBATH_CWD")
				}
				if cwd == "" {
					cwd, _ = os.Getwd()
				}
				return ask(r.rpc, a.sys, tuigateway.CanonicalSessionExport, tuigateway.SessionExportParams{SessionID: r.sid, Format: &format, Redact: redact}, func(res *tuigateway.SessionExportResult) tea.Cmd {
					if res == nil {
						return nil
					}
					path, err := writeSessionExport(res, filename, cwd)
					if err != nil {
						r.sys(tr(txt.SlashCmdCoreSaveFailed) + ": " + err.Error())
						return nil
					}
					r.sys(tr(txt.SlashCmdCoreSaveSaved, path))
					return nil
				})
			}},
		{name: "statusbar", help: "status bar position", usage: "[on|off|top|bottom|toggle]", run: func(r *slashRun) tea.Cmd {
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
				r.sys(tr(txt.SlashCmdCoreStatusbarUsage))
				return nil
			}
			a.store.ui.StatusBar = next
			r.sys(tr(txt.SlashCmdCoreStatusbarState, next))
			return r.saveDisplayPref("statusbar", next)
		}},
		{name: "battery", help: "battery indicator in the status bar", usage: "[on|off|status]", run: func(r *slashRun) tea.Cmd {
			mode := strings.ToLower(trimJS(r.arg))
			if mode == "status" || mode == "show" {
				state := onOff(a.store.ui.Battery)
				return call(a.gw, tuigateway.SystemBattery, tuigateway.ProfileParams{}, func(res *tuigateway.SystemBatteryResult, err error) tea.Cmd {
					switch {
					case err != nil:
						r.sys(tr(txt.SlashCmdCoreBatteryState, state))
					case res.Available && res.Percent != nil:
						mark := "🔋"
						if val(res.Plugged) {
							mark = "⚡"
						}
						r.sys(tr(txt.SlashCmdCoreBatteryStatusLive, state, mark, strconv.FormatInt(*res.Percent, 10)))
					default:
						r.sys(tr(txt.SlashCmdCoreBatteryStatusNoBattery, state))
					}
					return nil
				})
			}
			next, ok := flagFromArg(r.arg, a.store.ui.Battery)
			if !ok {
				r.sys(tr(txt.SlashCmdCoreBatteryUsage))
				return nil
			}
			a.store.ui.Battery = next
			if !next {
				a.store.ui.BatteryStatus = nil
			}
			r.sys(tr(txt.SlashCmdCoreBatteryState, onOff(next)))
			return r.saveDisplayPref("battery", onOff(next))
		}},
		{name: "queue", help: "queue a message, or list, edit, move and remove the queued ones", usage: "[<prompt>|list|add <prompt>|edit <n> [prompt]|rm <n>|move <from> <to>|clear]",
			suggest: func(string) []string { return []string{"list", "add", "edit", "rm", "move", "clear"} }, run: func(r *slashRun) tea.Cmd {
				plan := planQueueSlash(r.cmd, r.cmd)
				if plan.kind == "enqueue" {
					a.enqueue(plan.text, plan.display, r.destination)
					r.sys(tr(txt.SlashCmdCoreQueueQueued, previewOf(plan.display)))
					return nil
				}
				return r.applyQueuePlan(plan)
			}},
		{name: "steer", help: "inject a message after the next tool call, no interrupt", usage: "<text>", run: func(r *slashRun) tea.Cmd {
			payload := trimJS(r.arg)
			if payload == "" {
				r.sys(tr(txt.SlashCmdCoreSteerUsage))
				return nil
			}
			if !a.store.ui.Busy || r.sid == "" {
				a.enqueue(payload, payload, r.destination)
				r.sys(tr(txt.SlashCmdCoreSteerNoActiveTurnQueued, previewOf(payload)))
				return nil
			}
			settle := func(status tuigateway.CorrectionStatus) tea.Cmd {
				if status == "queued" {
					r.sys(tr(txt.SlashCmdCoreSteerQueued, previewOf(payload)))
					return nil
				}
				a.enqueue(payload, payload, r.destination)
				r.sys(tr(txt.SlashCmdCoreSteerRejected))
				return nil
			}
			generation := infoOf(a.store.ui.Info).ExecutionGeneration
			if generation == nil {
				r.sys(errorLine(rpcErrorMessage(tuigateway.MissingParam(tuigateway.CanonicalSessionSteer.Name(), "execution_generation"))))
				return nil
			}
			return ask(r.rpc, r.sys, tuigateway.CanonicalSessionSteer, tuigateway.CanonicalCorrectionParams{SessionID: r.sid, ExecutionGeneration: *generation, Text: payload}, func(res *tuigateway.CorrectionAnswer) tea.Cmd {
				switch {
				case res == nil, res.VoiceStopped:
					return nil
				}
				return settle(res.Status)
			})
		}},
		{name: "rewind", help: "rewind to an earlier prompt, edit and resend it or fork from it, also Esc Esc on an empty composer", usage: "[N | fork N]",
			suggest: func(string) []string { return []string{"fork"} }, run: func(r *slashRun) tea.Cmd {
				args := strings.Fields(r.arg)
				fork := len(args) > 0 && strings.EqualFold(args[0], "fork")
				if fork {
					args = args[1:]
				}
				if len(args) == 0 && !fork {
					return tea.Batch(r.ran(), a.openRewindPicker())
				}
				if len(args) != 1 || strings.Trim(args[0], "0123456789") != "" {
					r.sys(tr(txt.SlashCmdCoreRewindUsage))
					return nil
				}
				number, err := strconv.Atoi(args[0])
				if err != nil {
					number = math.MaxInt
				}
				return tea.Batch(r.ran(), a.rewindToPrompt(number, args[0], fork))
			}},
		{name: "undo", help: "undo the last exchange", run: func(r *slashRun) tea.Cmd {
			if r.sid == "" {
				r.sys(tr(txt.SlashCmdCoreUndoNothing))
				return nil
			}
			return r.canonicalRewind(false)
		}},
		{name: "retry", help: "retry the last user message", run: func(r *slashRun) tea.Cmd {
			if r.sid != "" {
				return r.canonicalRewind(true)
			}
			last := a.lastUserMsg
			if last == "" {
				r.sys(tr(txt.SlashCmdCoreRetryNothing))
				return nil
			}
			return a.sendExpanded(last, true, "", submitOpts{})
		}},
	}
}

// Copies text to the system clipboard, through the terminal when the session is remote or no clipboard tool answers
func (a *app) copyText(r *slashRun, text string, done func() string) tea.Cmd {
	if isRemoteShellSession(os.LookupEnv) {
		r.sys(tr(txt.SlashCmdCoreCopySentOsc52))
		return clip.Send(text)
	}
	return func() tea.Msg {
		ok := clip.WriteText(a.ctx, text)
		return asyncDoneMsg{run: func() tea.Cmd {
			if r.stale() {
				return nil
			}
			if ok {
				r.sys(done())
				return nil
			}
			r.sys(tr(txt.SlashCmdCoreCopySentOsc52))
			return clip.Send(text)
		}}
	}
}

// Process, reload, browser, checkpoint, skill and tool commands
func (a *app) opsCommands() []slashCommand {
	return []slashCommand{
		{name: "stop", help: "stop background processes", run: func(r *slashRun) tea.Cmd {
			var interrupt tea.Cmd
			if a.store.ui.Busy && r.sid != "" {
				interrupt = a.interruptTurn()
			}
			params := tuigateway.ProcessStopParams{SessionID: optionalText(r.sid)}
			return tea.Batch(interrupt, ask(r.rpc, r.sys, tuigateway.ProcessStop, params, func(res *tuigateway.ProcessStopResult) tea.Cmd {
				if res == nil {
					return nil
				}
				key := txt.SlashCmdOpsStopStoppedOther
				if res.Killed == 1 {
					key = txt.SlashCmdOpsStopStoppedOne
				}
				r.sys(tr(key, strconv.FormatInt(res.Killed, 10)))
				return nil
			}))
		}},
		{name: "reload-mcp", help: "reload MCP servers in the live session", usage: "[now]", run: func(r *slashRun) tea.Cmd {
			params := tuigateway.ReloadMCPParams{SessionID: optionalText(r.sid), Confirm: strings.ToLower(trimJS(r.arg)) == "now"}
			return ask(r.rpc, r.sys, tuigateway.ReloadMCP, params, func(res *tuigateway.ReloadMCPResult) tea.Cmd {
				if res == nil {
					return nil
				}
				switch message := val(res.Message); {
				case message != "":
					r.sys(message)
				case res.Status == "confirm_required":
					r.sys(tr(txt.SlashCmdOpsReloadMcpConfirmRequired))
				default:
					r.sys(tr(txt.SlashCmdOpsReloadMcpReloaded))
				}
				return nil
			})
		}},
		{name: "reload", help: "read the .env of the home again into the running gateway", run: func(r *slashRun) tea.Cmd {
			params := tuigateway.ReloadEnvParams{SessionID: r.sid}
			return ask(r.rpc, r.sys, tuigateway.ReloadEnv, params, func(res *tuigateway.ReloadEnvResult) tea.Cmd {
				if res == nil {
					return nil
				}
				key := txt.SlashCmdOpsReloadEnvReloadedOther
				if res.Updated == 1 {
					key = txt.SlashCmdOpsReloadEnvReloadedOne
				}
				r.sys(tr(key, res.Updated))
				return nil
			})
		}},
		{name: "browser", help: "connect the agent to your browser, or close the browser holding your profile", usage: "[connect|disconnect|status|close-profile]",
			suggest: func(string) []string { return []string{"connect", "disconnect", "status", "close-profile"} },
			run: func(r *slashRun) tea.Cmd {
				words := strings.Fields(r.arg)
				action := "status"
				if len(words) > 0 {
					action = strings.ToLower(words[0])
				}
				if action == "close-profile" {
					return a.confirmDestructive(tr(txt.SlashCmdOpsBrowserCloseTitle), tr(txt.SlashCmdOpsBrowserCloseDetail), tr(txt.SlashCmdOpsBrowserCloseYes),
						tr(txt.SlashCmdOpsBrowserCloseNo), func() tea.Cmd {
							exe, err := os.Executable()
							if err != nil {
								r.sys(errorLine(err.Error()))
								return nil
							}
							return a.runIcebath(exe, []string{"browser", "close-profile"})
						})
				}
				if action != "connect" && action != "disconnect" && action != "status" {
					r.sys(tr(txt.SlashCmdOpsBrowserUsage))
					return nil
				}
				params := tuigateway.BrowserManageParams{Action: tuigateway.BrowserAction(action), SessionID: r.sid}
				if action == "connect" {
					url := "http://127.0.0.1:9222"
					if len(words) > 1 {
						url = strings.Join(words[1:], " ")
					}
					params.URL = &url
					r.sys(tr(txt.SlashCmdOpsBrowserChecking, url))
				}
				return ask(r.rpc, r.sys, tuigateway.BrowserManage, params, func(res *tuigateway.BrowserManageResult) tea.Cmd {
					if res == nil {
						return nil
					}
					if r.sid == "" {
						for _, message := range res.Messages {
							r.sys(message)
						}
					}
					url := res.URL
					if url == "" {
						url = tr(txt.SlashCmdOpsBrowserUrlUnavailable)
					}
					switch {
					case action == "status" && res.Connected:
						r.sys(tr(txt.SlashCmdOpsBrowserConnected, url))
					case action == "status":
						r.sys(tr(txt.SlashCmdOpsBrowserNotConnected))
					case action == "disconnect":
						r.sys(tr(txt.SlashCmdOpsBrowserDisconnected))
					case res.Connected:
						r.sys(tr(txt.SlashCmdOpsBrowserConnectedLive))
						r.sys(tr(txt.SlashCmdOpsBrowserEndpoint, url))
						r.sys(tr(txt.SlashCmdOpsBrowserNextCallUsesEndpoint))
					}
					return nil
				})
			}},
		{name: "rollback", help: "list, diff, or restore checkpoints", usage: "[list|diff <hash>|<hash> [file] [--all]]", run: func(r *slashRun) tea.Cmd {
			if r.sid == "" {
				r.sys(tr(txt.SlashCmdOpsRollbackNoSession))
				return nil
			}
			words := strings.Fields(r.arg)
			first := ""
			if len(words) > 0 {
				first = words[0]
			}
			switch strings.ToLower(first) {
			case "", "list", "ls":
				return ask(r.rpc, r.sys, tuigateway.RollbackList, tuigateway.RollbackListParams{SessionID: r.sid}, func(res *tuigateway.RollbackListResult) tea.Cmd {
					if res == nil {
						return nil
					}
					if !res.Enabled {
						r.sys(tr(txt.SlashCmdOpsRollbackNotEnabled))
						return nil
					}
					if len(res.Checkpoints) == 0 {
						r.sys(tr(txt.SlashCmdOpsRollbackNoneFound))
						return nil
					}
					rows := make([][2]string, 0, len(res.Checkpoints))
					for i, c := range res.Checkpoints {
						var meta []string
						for _, s := range []string{c.Timestamp, c.Message} {
							if s != "" {
								meta = append(meta, s)
							}
						}
						detail := strings.Join(meta, " · ")
						if detail == "" {
							detail = tr(txt.SlashCmdOpsRollbackNoMetadata)
						}
						rows = append(rows, [2]string{fmt.Sprintf("%d. %s", i+1, c.Hash[:min(len(c.Hash), 10)]), detail})
					}
					a.panel(tr(txt.SlashCmdOpsRollbackListTitle), []panelSection{{Rows: rows}})
					return nil
				})
			case "diff":
				if len(words) < 2 {
					r.sys(tr(txt.SlashCmdOpsRollbackUsageDiff))
					return nil
				}
				return ask(r.rpc, r.sys, tuigateway.RollbackDiff, tuigateway.RollbackDiffParams{Hash: words[1], SessionID: r.sid}, func(res *tuigateway.RollbackDiffResult) tea.Cmd {
					if res == nil {
						return nil
					}
					body := res.Rendered
					if body == "" {
						body = res.Diff
					}
					stat := res.Stat
					if body = trimJS(body); body == "" && stat == "" {
						r.sys(tr(txt.SlashCmdOpsRollbackNoChanges))
						return nil
					}
					text := body
					if stat != "" && body != "" {
						text = stat + "\n\n" + body
					} else if stat != "" {
						text = stat
					}
					return a.page(text, tr(txt.SlashCmdOpsRollbackDiffTitle))
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
			params := tuigateway.RollbackRestoreParams{Hash: first, SessionID: r.sid, FilePath: filePath}
			if filePath == "" && all {
				params.All = ptr(true)
			}
			return ask(r.rpc, r.sys, tuigateway.RollbackRestore, params, func(res *tuigateway.RollbackRestoreResult) tea.Cmd {
				if res == nil {
					return nil
				}
				if !res.Success {
					reason := res.Error
					if reason == "" {
						reason = tr(txt.SlashCmdOpsRollbackUnknownError)
					}
					r.sys(tr(txt.SlashCmdOpsRollbackFailed, reason))
					return nil
				}
				target := filePath
				if target == "" {
					target = tr(txt.SlashCmdOpsRollbackWorkspaceTarget)
				}
				detail := res.Reason
				if detail == "" {
					detail = res.RestoredTo
				}
				if detail == "" {
					detail = tr(txt.SlashCmdOpsRollbackRestoredDetail)
				}
				r.sys(tr(txt.SlashCmdOpsRollbackRestored, target, detail))
				for _, kept := range []struct {
					key   txt.Key
					paths []string
				}{{txt.SlashCmdOpsRollbackKeptUserEdits, res.SkippedUserEdits}, {txt.SlashCmdOpsRollbackKeptOversize, res.SkippedOversize}, {txt.SlashCmdOpsRollbackFailedDeletes, res.FailedDeletes}} {
					if len(kept.paths) > 0 {
						summary := strings.Join(kept.paths[:min(len(kept.paths), 5)], ", ")
						if len(kept.paths) > 5 {
							summary += tr(txt.SlashCmdOpsRollbackMorePaths, len(kept.paths)-5)
						}
						r.sys(tr(kept.key, summary))
					}
				}
				if res.HistoryRemoved != nil && *res.HistoryRemoved > 0 {
					a.setHistoryItems(dropLastExchange)
				}
				return nil
			})
		}},
		{name: "skills", help: "the skills part of the plugins and skills screen, or a skills verb", usage: "[list|inspect|search|install|browse]",
			suggest: func(string) []string { return []string{"list", "inspect", "search", "install", "browse"} }, run: a.skillsSlash},
		{name: "reload-skills", help: "scan installed skills again in the live gateway", run: func(r *slashRun) tea.Cmd {
			return ask(r.rpc, r.sys, tuigateway.SkillsReload, tuigateway.SkillsReloadParams{SessionID: r.sid}, func(res *tuigateway.SkillsReloadResult) tea.Cmd {
				if res == nil {
					return nil
				}
				out := res.Output
				if out == "" {
					out = tr(txt.SlashCmdOpsReloadSkillsReloaded)
				}
				return tea.Batch(a.page(out, tr(txt.SlashCmdOpsReloadSkillsPageTitle)), a.events.loadSlashCatalog(r.sid))
			})
		}},
	}
}

// /skills: the skills hub, or a verb run here or by the background service
func (a *app) skillsSlash(r *slashRun) tea.Cmd {
	text := trimJS(r.arg)
	if text == "" {
		return a.openPlugins("skills")
	}
	sub, query, _ := strings.Cut(text, " ")
	query = trimJS(query)
	manage := func(params tuigateway.SkillsManageParams, then func(res *tuigateway.SkillsManageResult) tea.Cmd) tea.Cmd {
		return ask(r.rpc, r.sys, tuigateway.SkillsManage, params, func(res *tuigateway.SkillsManageResult) tea.Cmd {
			if res == nil {
				return nil
			}
			return then(res)
		})
	}
	switch sub {
	case "list":
		return manage(tuigateway.SkillsManageParams{Action: "list"}, func(res *tuigateway.SkillsManageResult) tea.Cmd {
			if len(res.Skills) == 0 {
				r.sys(tr(txt.UserMessagesSkillsNoneInstalled))
				return nil
			}
			var sections []panelSection
			for _, category := range slices.Sorted(maps.Keys(res.Skills)) {
				title := category
				sections = append(sections, panelSection{Items: res.Skills[category], Title: &title})
			}
			a.panel(tr(txt.SlashCmdOpsSkillsListTitle), sections)
			return nil
		})
	case "inspect":
		if query == "" {
			r.sys(tr(txt.SlashCmdOpsSkillsUsageInspect))
			return nil
		}
		return manage(tuigateway.SkillsManageParams{Action: "inspect", Query: query}, func(res *tuigateway.SkillsManageResult) tea.Cmd {
			info := tuigateway.SkillInspectInfo{}
			if res.Info != nil {
				info = *res.Info
			}
			if info.Name == "" {
				r.sys(tr(txt.SlashCmdOpsSkillsUnknownSkill, query))
				return nil
			}
			sections := []panelSection{{Rows: [][2]string{{tr(txt.SlashCmdOpsSkillsRowName), info.Name}, {tr(txt.SlashCmdOpsSkillsRowCategory), info.Category}, {tr(txt.SlashCmdOpsSkillsRowPath), info.Path}}}}
			if description := info.Description; description != "" {
				sections = append(sections, panelSection{Text: &description})
			}
			a.panel(tr(txt.SlashCmdOpsSkillsInspectTitle), sections)
			return nil
		})
	case "search":
		if query == "" {
			r.sys(tr(txt.SlashCmdOpsSkillsUsageSearch))
			return nil
		}
		return manage(tuigateway.SkillsManageParams{Action: "search", Query: query}, func(res *tuigateway.SkillsManageResult) tea.Cmd {
			if len(res.Results) == 0 {
				r.sys(tr(txt.SlashCmdOpsSkillsNoResults, query))
				return nil
			}
			rows := make([][2]string, 0, len(res.Results))
			for _, hit := range res.Results {
				rows = append(rows, [2]string{hit.Name, hit.Description})
			}
			a.panel(tr(txt.SlashCmdOpsSkillsSearchTitle, query), []panelSection{{Rows: rows}})
			return nil
		})
	case "install":
		if query == "" {
			r.sys(tr(txt.SlashCmdOpsSkillsUsageInstall))
			return nil
		}
		r.sys(tr(txt.SlashCmdOpsSkillsInstalling, query))
		return manage(tuigateway.SkillsManageParams{Action: "install", Query: query}, func(res *tuigateway.SkillsManageResult) tea.Cmd {
			if !res.Installed {
				r.sys(tr(txt.SlashCmdOpsSkillsInstallFailed))
				return nil
			}
			name := res.Name
			if name == "" {
				name = query
			}
			r.sys(tr(txt.SlashCmdOpsSkillsInstalled, name))
			return a.events.loadSlashCatalog(r.sid)
		})
	case "browse":
		pageNum := 1
		if query != "" {
			n, err := strconv.Atoi(query)
			if err != nil || n < 1 {
				r.sys(tr(txt.SlashCmdOpsSkillsUsageBrowse))
				return nil
			}
			pageNum = n
		}
		r.sys(tr(txt.SlashCmdOpsSkillsFetching))
		return manage(tuigateway.SkillsManageParams{Action: "browse", Page: int64(pageNum)}, func(res *tuigateway.SkillsManageResult) tea.Cmd {
			total := strconv.FormatInt(res.Total, 10)
			if len(res.Items) == 0 {
				if res.Total > 0 {
					r.sys(tr(txt.SlashCmdOpsSkillsNoneOnPageWithTotal, strconv.Itoa(pageNum), total))
				} else {
					r.sys(tr(txt.SlashCmdOpsSkillsNoneOnPage, strconv.Itoa(pageNum)))
				}
				return nil
			}
			rows := make([][2]string, 0, len(res.Items))
			for _, item := range res.Items {
				name := item.Name
				if item.Trust != "" {
					name += " · " + item.Trust
				}
				rows = append(rows, [2]string{name, previewOfN(item.Description, 160)})
			}
			var footer []string
			if res.Page > 0 && res.TotalPages > 0 {
				footer = append(footer, tr(txt.SlashCmdOpsSkillsPageOf, strconv.FormatInt(res.Page, 10), strconv.FormatInt(res.TotalPages, 10)))
			}
			if res.Total > 0 {
				footer = append(footer, tr(txt.SlashCmdOpsSkillsSkillsTotal, total))
			}
			if res.Page > 0 && res.Page < res.TotalPages {
				footer = append(footer, tr(txt.SlashCmdOpsSkillsBrowseMore, strconv.FormatInt(res.Page+1, 10)))
			}
			sections := []panelSection{{Rows: rows}}
			if len(footer) > 0 {
				foot := strings.Join(footer, " · ")
				sections = append(sections, panelSection{Text: &foot})
			}
			title := tr(txt.SlashCmdOpsSkillsBrowseTitle)
			if pageNum > 1 {
				title = tr(txt.SlashCmdOpsSkillsBrowseTitlePage, strconv.Itoa(pageNum))
			}
			a.panel(title, sections)
			return nil
		})
	}
	return r.viaGateway(tr(txt.SlashCmdOpsGatewayExecSkillsTitle), tr(txt.SlashCmdOpsGatewayExecSkillsNoOutput))
}

// /plugins: the plugins hub, an update of every plugin, or a verb the background service runs
func (a *app) pluginsSlash(r *slashRun) tea.Cmd {
	switch trimJS(r.arg) {
	case "":
		return a.openPlugins("plugins")
	case "update":
		return r.updateAllPlugins()
	}
	return r.viaGateway(tr(txt.SlashCmdOpsGatewayExecPluginsTitle), tr(txt.SlashCmdOpsGatewayExecPluginsNoOutput))
}

// Wait an update of every plugin gets, ten single update budgets
const pluginsUpdateAllTimeout = 600 * time.Second

// Lines of an update of every plugin, one row per plugin it handled, the skipped ones and what is left to review
func pluginsUpdateAllLines(outcome tuigateway.PluginsManageResult) []string {
	marks := map[tuigateway.PluginUpdateStatus]string{
		"current": tr(txt.SlashCmdOpsPluginsUpdateCurrent), "failed": tr(txt.SlashCmdOpsPluginsUpdateFailed),
		"needs_review": tr(txt.SlashCmdOpsPluginsUpdateNeedsReview), "updated": tr(txt.SlashCmdOpsPluginsUpdateUpdated),
	}
	lines := []string{tr(txt.SlashCmdOpsPluginsUpdateTitle)}
	review := false
	for _, row := range outcome.Results {
		mark := marks[row.Status]
		if mark == "" {
			mark = string(row.Status)
		}
		line := "  " + mark + "  " + row.Name
		if row.Detail != "" {
			line += "  (" + row.Detail + ")"
		}
		lines = append(lines, line)
		review = review || row.Status == "needs_review"
	}
	for _, skip := range outcome.Skipped {
		lines = append(lines, "  - "+tr(txt.SlashCmdOpsPluginsUpdateSkipped, skip.Name, skip.Reason))
	}
	if len(outcome.Results) == 0 {
		lines = append(lines, "  "+tr(txt.SlashCmdOpsPluginsUpdateNone))
	}
	if review {
		lines = append(lines, "  "+tr(txt.SlashCmdOpsPluginsUpdateReview))
	}
	return lines
}

// Checks every installed plugin and updates the ones that can move, through the typed verb so a long clone or scan is not cut off
func (r *slashRun) updateAllPlugins() tea.Cmd {
	r.sys(tr(txt.SlashCmdOpsPluginsUpdateRunning))
	params := tuigateway.PluginsManageParams{Action: "update_all"}
	return callWithin(r.a.gw, tuigateway.PluginsManage, params, pluginsUpdateAllTimeout, func(res *tuigateway.PluginsManageResult, err error) tea.Cmd {
		if r.stale() {
			return nil
		}
		if err != nil {
			r.fail(err)
			return nil
		}
		return r.a.page(strings.Join(pluginsUpdateAllLines(*res), "\n"), tr(txt.SlashCmdOpsGatewayExecPluginsTitle))
	})
}

// First n characters of a text
func previewOfN(text string, n int) string {
	if utf8.RuneCountInString(text) <= n {
		return text
	}
	return string([]rune(text)[:n])
}
