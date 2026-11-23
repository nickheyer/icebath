package tui

import (
	"errors"
	"os"
	"regexp"
	"strconv"
	"strings"

	tea "charm.land/bubbletea/v2"

	"github.com/nickheyer/icebath/pkg/tuigateway"
	"github.com/nickheyer/icebath/pkg/ui/cards"
	"github.com/nickheyer/icebath/pkg/ui/keys"
	"github.com/nickheyer/icebath/pkg/ui/render"
	"github.com/nickheyer/icebath/pkg/ui/txt"
	"github.com/nickheyer/icebath/pkg/ui/wm"
)

// Keymap context of the list of earlier prompts
const rewindContext = "tui.rewind"

// Image notes a surface puts around a typed prompt for the model, the vision descriptions ahead of it and the native hints after it
var (
	imageNoteRe   = regexp.MustCompile(`^\[The user (?:sent|attached) an image[\s\S]*?image_url: ([^\n]+?)(?: ~)?\](?:\n\n|$)`)
	nativeHintsRe = regexp.MustCompile(`\n\n((?:\[Image attached(?: at)?: [^\n]+\]\n?)+)$`)
	nativeHintRe  = regexp.MustCompile(`\[Image attached(?: at)?: ([^\n]+)\]`)
)

// Caption a surface sends for an image that came with no text
const defaultImageCaption = "What do you see in this image?"

// Earlier prompt a rewind can return to, its turn number counting every prompt of the session
type rewindTarget struct {
	rowID   int64
	through int64
	turn    int
	text    string
}

// Prompt put back into the composer whose send rewinds the session to just before it
type pendingRewind struct {
	sid    string
	target rewindTarget
}

// Keys of the list of earlier prompts, registered with the keymap of the app
func registerRewindKeys(km *keys.Map) {
	if keys.ContextDocs[rewindContext] == "" {
		if err := keys.AddContext(rewindContext, "earlier prompts to rewind to"); err != nil {
			panic(err)
		}
	}
	for _, a := range []keys.Action{
		{ID: "tui.rewind.edit", Context: rewindContext, Label: "edit", Doc: "put the prompt back into the composer, sending it rewinds the session to just before it", Default: []string{"enter"}},
		{ID: "tui.rewind.fork", Context: rewindContext, Label: "fork", Doc: "open a new session holding everything before the prompt, with the prompt ready to edit", Default: []string{"f"}},
	} {
		km.Register(a)
	}
}

// What the user typed and the images its notes name, text without notes unchanged
func splitImageNotes(text string) (string, []string) {
	var images []string
	rest := text
	for {
		m := imageNoteRe.FindStringSubmatchIndex(rest)
		if m == nil {
			break
		}
		images = append(images, strings.TrimSpace(rest[m[2]:m[3]]))
		rest = rest[m[1]:]
	}
	if len(images) > 0 {
		return rest, images
	}
	m := nativeHintsRe.FindStringSubmatchIndex(rest)
	if m == nil {
		return text, nil
	}
	for _, hit := range nativeHintRe.FindAllStringSubmatch(rest[m[2]:m[3]], -1) {
		images = append(images, strings.TrimSpace(hit[1]))
	}
	rest = rest[:m[0]]
	if strings.TrimSpace(rest) == defaultImageCaption {
		rest = ""
	}
	return rest, images
}

// Whether a row is a prompt the user typed, compaction summaries and archived rows passed over
func isUserTurn(row *tuigateway.TranscriptMessage) bool {
	kind := val(row.DisplayKind)
	if row.Role != "user" || (kind != "" && kind != "steer") || row.RowID == nil || val(row.Archived) {
		return false
	}
	content, _ := row.Content.(string)
	for _, marker := range compactionMarkers {
		if strings.Contains(content, marker) {
			return false
		}
	}
	return true
}

// Earlier prompts of a snapshot oldest first and the number of prompts it holds, a prompt with media keeping its number without being offered
func rewindTargets(snapshot *tuigateway.CanonicalSnapshot) ([]rewindTarget, int) {
	var targets []rewindTarget
	var through int64
	turn := 0
	for i := range snapshot.Messages {
		row := &snapshot.Messages[i]
		if isUserTurn(row) {
			turn++
			if text, sendable := retryText(row.Content); sendable && text != "" {
				targets = append(targets, rewindTarget{rowID: *row.RowID, through: through, turn: turn, text: text})
			}
		}
		if row.RowID != nil && *row.RowID > 0 && !val(row.Archived) {
			through = *row.RowID
		}
	}
	return targets, turn
}

// Text the owner refused a rewind with while the session runs a turn or a compression, empty for any other failure
func rewindBusyText(err error, intent string) string {
	var rerr *rpcError
	if !errors.As(err, &rerr) {
		return ""
	}
	if reason, _ := rerr.Data["reason"].(string); reason != "session_busy" {
		return ""
	}
	if field, _ := rerr.Data["field"].(string); field == "compression" {
		return tr(txt.SlashCmdCoreBusyCompressing, intent)
	}
	return tr(txt.SlashCmdCoreBusyReplying, intent)
}

// Whether every image file can be read from this machine
func imagesReadable(paths []string) bool {
	for _, path := range paths {
		f, err := os.Open(path)
		if err != nil {
			return false
		}
		f.Close()
	}
	return true
}

// Puts a picked prompt back into the composer as it was typed, its images attached again where this machine can read them
func (a *app) prefillPrompt(stored string) tea.Cmd {
	text, images := splitImageNotes(stored)
	if len(images) > 0 && attachURLEnv() != "" && !imagesReadable(images) {
		a.sys(tr(txt.SlashCmdCoreRewindImagesKeptAsNotes, len(images)))
		text, images = stored, nil
	}
	if draft := a.card.composer.Value(); trimJS(draft) != "" {
		text = strings.TrimRightFunc(text, isJSSpace) + " " + draft
	}
	filled := a.card.composer.SetValue(text)
	return tea.Batch(filled, a.restoreImages(images))
}

// Drops the armed edit, saying so when one was armed and the cancel was asked for
func (a *app) cancelPendingRewind(announce bool) bool {
	// A rewind on its way to the owner no longer arms the edit again when it fails
	a.rewindFlight = nil
	if a.rewindPending == nil {
		return false
	}
	a.rewindPending = nil
	if announce {
		a.sys(tr(txt.SlashCmdCoreRewindCancelled))
	}
	return true
}

// Number of prompts in the transcript on screen, read without asking the owner
func (a *app) promptsOnScreen() int {
	n := 0
	for _, m := range a.history {
		if m.Role == "user" && m.Kind == "" {
			n++
		}
	}
	return n
}

// One escape on an idle, empty composer, the first arming the second that opens the earlier prompts or drops an armed edit whose text was erased,
// false when the escape is not one of those
func (a *app) backtrackEscape() (tea.Cmd, bool) {
	ui := a.store.ui
	if ui.Busy || ui.Sid == "" || a.dialogs.waiting() || a.queue.queueEditIndex() != nil || a.card.composer.Value() != "" {
		a.rewindPrimed = ""
		return nil, false
	}
	armed := a.rewindPending != nil
	if a.rewindPrimed != ui.Sid {
		a.rewindPrimed = ui.Sid
		a.rewindPrimeCount = 0
		if !armed {
			a.rewindPrimeCount = a.promptsOnScreen()
		}
		return nil, true
	}
	a.rewindPrimed = ""
	if armed {
		a.cancelPendingRewind(true)
		return nil, true
	}
	return a.openRewindPicker(), true
}

// Line above the composer while an escape is armed or an edit waits to be sent, empty otherwise
func (a *app) rewindHintRows(rc renderCtx) []string {
	switch {
	case a.rewindPending != nil && a.rewindPending.sid == a.store.ui.Sid:
		return []string{"  " + toneStyle(rc.t.Color.Accent).Render(tr(txt.PromptRewindPending, strconv.Itoa(a.rewindPending.target.turn)))}
	case a.rewindPrimed != "" && a.rewindPrimed == a.store.ui.Sid && a.rewindPrimeCount > 0:
		return []string{"  " + toneStyle(rc.t.Color.Muted).Render(tr(txt.PromptRewindPrime))}
	}
	return nil
}

// Reads the earlier prompts of the session in view from the owner's rows and hands them on with the number of prompts, saying why when no rewind can start
func (a *app) withRewindTargets(then func(sid string, targets []rewindTarget, turns int) tea.Cmd) tea.Cmd {
	a.rewindPrimed = ""
	a.cancelPendingRewind(false)
	sid := a.store.ui.Sid
	switch {
	case sid == "":
		a.sys(tr(txt.SlashCmdCoreRewindNothing))
		return nil
	case a.store.ui.Busy:
		a.sys(tr(txt.SlashCmdCoreBusyReplying, "rewind"))
		return nil
	}
	return call(a.gw, tuigateway.CanonicalSessionResume, tuigateway.CanonicalResumeParams{SessionID: sid}, func(snapshot *tuigateway.CanonicalSnapshot, err error) tea.Cmd {
		if a.store.ui.Sid != sid {
			return nil
		}
		if err != nil {
			a.sys(errorLine(rpcErrorMessage(err)))
			return nil
		}
		targets, turns := rewindTargets(snapshot)
		return then(sid, targets, turns)
	})
}

// Puts a prompt back into the composer with the rewind to it armed, the next send cutting the session back to just before it
func (a *app) armRewind(sid string, target rewindTarget) tea.Cmd {
	prefilled := a.prefillPrompt(target.text)
	a.rewindPending = &pendingRewind{sid: sid, target: target}
	return prefilled
}

// Runs /rewind with a prompt number, the prompt put back for editing or a session forked from before it
func (a *app) rewindToPrompt(number int, typed string, fork bool) tea.Cmd {
	return a.withRewindTargets(func(sid string, targets []rewindTarget, turns int) tea.Cmd {
		switch {
		case turns == 0:
			a.sys(tr(txt.SlashCmdCoreRewindNothing))
			return nil
		case number < 1 || number > turns:
			a.sys(tr(txt.SlashCmdCoreRewindInvalid, typed, strconv.Itoa(turns)))
			return nil
		}
		for _, target := range targets {
			if target.turn != number {
				continue
			}
			if fork {
				return a.forkFromPrompt(sid, target)
			}
			return a.armRewind(sid, target)
		}
		a.sys(tr(txt.SlashCmdCoreRewindNotEditable, strconv.Itoa(number)))
		return nil
	})
}

// Opens the list of earlier prompts of the session in view
func (a *app) openRewindPicker() tea.Cmd {
	return a.withRewindTargets(func(sid string, targets []rewindTarget, _ int) tea.Cmd {
		if len(targets) == 0 {
			a.sys(tr(txt.SlashCmdCoreRewindNoPrevious))
			return nil
		}
		newest := make([]rewindTarget, len(targets))
		for i, t := range targets {
			newest[len(targets)-1-i] = t
		}
		behind := a.typedBehindList()
		list := cards.NewList(a.cardEnv(), cards.ListSpec{
			Title: tr(txt.HubsTitleRewind), Glyph: a.toolkit.Glyphs.Sessions, Key: "tui:rewind", Context: rewindContext, Primary: "tui.rewind.edit",
			Columns: []render.Column{{Title: "#", Right: true}, {Title: tr(txt.HubsColPrompt), Flex: 1}},
			Rows: func() [][]string {
				out := make([][]string, len(newest))
				for i, t := range newest {
					typed, _ := splitImageNotes(t.text)
					out[i] = []string{strconv.Itoa(t.turn), strings.Join(strings.Fields(typed), " ")}
				}
				return out
			},
			Detail: func(i int) []string {
				typed, _ := splitImageNotes(newest[i].text)
				return strings.Split(typed, "\n")
			},
			Empty: func() string { return tr(txt.SlashCmdCoreRewindNoPrevious) },
			Typed: func(l *cards.List, text string, pasted bool) tea.Cmd {
				return tea.Batch(wm.RequestDismissOf(l), behind(text, pasted))
			},
			Actions: map[string]func(l *cards.List, i int) tea.Cmd{
				"tui.rewind.edit": func(l *cards.List, i int) tea.Cmd {
					if a.store.ui.Sid != sid {
						return wm.RequestDismissOf(l)
					}
					return tea.Batch(wm.RequestDismissOf(l), a.armRewind(sid, newest[i]))
				},
				"tui.rewind.fork": func(l *cards.List, i int) tea.Cmd {
					if a.store.ui.Sid != sid {
						return wm.RequestDismissOf(l)
					}
					return tea.Batch(wm.RequestDismissOf(l), a.forkFromPrompt(sid, newest[i]))
				},
			},
		})
		return a.wm.ShowOverlay(list)
	})
}

// Sends a line typed while an edit is armed through the rewind, false when the line is handled the ordinary way
func (a *app) sendPendingRewind(full string) (tea.Cmd, bool) {
	pending := a.rewindPending
	if pending == nil {
		return nil, false
	}
	a.rewindPending = nil
	typed := strings.TrimLeftFunc(full, isJSSpace)
	if pending.sid != a.store.ui.Sid || trimJS(full) == "" || strings.HasPrefix(typed, "/") || strings.HasPrefix(typed, "!") {
		return nil, false
	}
	tokens := a.tokens
	text, attachments := prepareSubmission(full, tokens)
	a.store.stagePastes(full, text, tokens)
	a.clearComposerTokens()
	return a.rewindAndSend(pending, text, full, attachments, tokens), true
}

// Cuts the session back to just before the prompt, repaints the transcript from the owner's rows and sends the edit as the next prompt
func (a *app) rewindAndSend(pending *pendingRewind, text, display string, attachments []imageAttachment, tokens []composerToken) tea.Cmd {
	sid, target := pending.sid, pending.target
	stale := func() bool { return a.store.ui.Sid != sid }
	a.rewindFlight = pending
	destination, derr := a.store.captureDestination()
	if derr != nil {
		a.uncaught(derr)
		return nil
	}
	keepEdit := func(rearm, unknown bool) tea.Cmd {
		if stale() {
			key := txt.SlashCmdCoreRewindEditNotSent
			if unknown {
				key = txt.SlashCmdCoreRewindEditNotSentUnknown
			}
			a.sys(tr(key))
			return nil
		}
		if rearm && a.rewindPending == nil && a.rewindFlight == pending {
			a.rewindPending = pending
		}
		if a.rewindFlight == pending {
			a.rewindFlight = nil
		}
		a.tokens = trimTokens(append(a.tokens, tokens...))
		value := display
		if draft := a.card.composer.Value(); trimJS(draft) != "" {
			value = strings.TrimRightFunc(display, isJSSpace) + " " + draft
		}
		return a.card.composer.SetValue(value)
	}
	payload := map[string]any{"target_message_id": target.rowID}
	return a.store.mutateCanonical(a.gw, sid, "rewind", payload, stale, func(m *mutationOutcome, err error) tea.Cmd {
		if err != nil {
			var rerr *rpcError
			restored := keepEdit(true, !errors.As(err, &rerr))
			if !stale() {
				line := rewindBusyText(err, "rewind")
				if line == "" {
					line = errorLine(rpcErrorMessage(err))
				}
				a.sys(line)
			}
			return restored
		}
		if m == nil {
			return keepEdit(false, false)
		}
		removed := val(m.Result.RewoundCount)
		if removed <= 0 {
			restored := keepEdit(false, false)
			if !stale() {
				a.sys(tr(txt.SlashCmdCoreRewindNothing))
			}
			return restored
		}
		if a.rewindFlight == pending {
			a.rewindFlight = nil
		}
		send := func() tea.Cmd {
			opts := submitOpts{Attachments: attachments, Destination: &destination}
			return a.sendExpanded(text, true, display, opts)
		}
		if stale() {
			a.enqueue(text, display, destination)
			return nil
		}
		baseline := a.store.ui.Info
		return call(a.gw, tuigateway.CanonicalSessionResume, tuigateway.CanonicalResumeParams{SessionID: sid, DisplayProjection: true}, func(snapshot *tuigateway.CanonicalSnapshot, err error) tea.Cmd {
			if stale() {
				a.enqueue(text, display, destination)
				return nil
			}
			if err == nil {
				attached := snapshotAttachment(snapshot)
				info := patchedInfo(baseline, *attached.Info)
				a.setHistoryItems(func([]msg) []msg {
					return append([]msg{introMsg(info)}, toTranscriptMessages(attached.Messages, a.store.ui)...)
				})
				a.store.ui.Info = info
			}
			a.sys(tr(txt.SlashCmdCoreRewindDone, strconv.Itoa(target.turn), strconv.FormatInt(removed, 10)))
			return send()
		})
	})
}

// Opens a new session holding everything before the prompt, the prompt ready in its composer and not sent
func (a *app) forkFromPrompt(sid string, target rewindTarget) tea.Cmd {
	prefill := func(session string) tea.Cmd {
		if session == "" || a.store.ui.Sid != session {
			return nil
		}
		filled := a.prefillPrompt(target.text)
		a.sys(tr(txt.SlashCmdCoreRewindForked, strconv.Itoa(target.turn)))
		return filled
	}
	if target.through == 0 {
		return a.life.newSession("", "", func(created string) tea.Cmd { return prefill(created) })
	}
	stale := func() bool { return a.store.ui.Sid != sid }
	payload := map[string]any{"through_message_id": target.through}
	return a.store.mutateCanonical(a.gw, sid, "branch", payload, stale, func(m *mutationOutcome, err error) tea.Cmd {
		if err != nil {
			if !stale() {
				line := rewindBusyText(err, "rewind")
				if line == "" {
					line = errorLine(rpcErrorMessage(err))
				}
				a.sys(line)
			}
			return nil
		}
		if m == nil || stale() {
			return nil
		}
		child := val(m.Result.BranchedSessionID)
		if child == "" {
			a.sys(errorLine(misfitText(tr(txt.SessionCommonInvalidResponse, "branch"))))
			return nil
		}
		return a.resumeByID(child, func(err error) tea.Cmd {
			if err != nil {
				a.uncaught(err)
				return nil
			}
			return prefill(child)
		})
	})
}

// Taker of the text typed or pasted while the earlier prompts are listed: it lands at the end of the draft in arrival order, a key typed behind a paste
// that still resolves waiting for it, and nothing landing once the draft it was meant for is gone
func (a *app) typedBehindList() func(text string, pasted bool) tea.Cmd {
	var waiting []func(next func() tea.Cmd) tea.Cmd
	landing := false
	var pump func() tea.Cmd
	pump = func() tea.Cmd {
		if landing || len(waiting) == 0 {
			return nil
		}
		step := waiting[0]
		waiting = waiting[1:]
		landing = true
		return step(func() tea.Cmd {
			landing = false
			return pump()
		})
	}
	return func(text string, pasted bool) tea.Cmd {
		destination, err := a.store.captureDestination()
		revision := a.composerRevision()
		live := func() bool {
			return err == nil && a.store.isCurrentDestination(destination) && revision == a.composerRevision()
		}
		step := func(next func() tea.Cmd) tea.Cmd {
			if !live() {
				return next()
			}
			return tea.Batch(a.card.composer.Insert(text), next())
		}
		if pasted {
			step = func(next func() tea.Cmd) tea.Cmd { return a.pasteBehindList(text, live, next) }
		}
		waiting = append(waiting, step)
		return pump()
	}
}

// Takes a paste made while the earlier prompts were listed the way the composer takes one, a dropped path that resolves after its draft is gone
// dropped with a notice, then runs next
func (a *app) pasteBehindList(content string, live func() bool, next func() tea.Cmd) tea.Cmd {
	gone := func() tea.Cmd {
		if trimJS(content) != "" {
			a.sys(tr(txt.CanonicalQueuePasteNotApplied))
		}
		return next()
	}
	if !live() {
		return gone()
	}
	cleaned := stripTrailingPasteNewlines(content)
	destination, err := a.store.captureDestination()
	if strings.Trim(cleaned, "\n") == "" || err != nil || destination.Sid == "" || !looksLikeDroppedPath(cleaned) {
		return tea.Batch(a.pasted(content), next())
	}
	land := func(cmd tea.Cmd) tea.Cmd { return tea.Batch(cmd, next()) }
	staged := func(image *stagedImage, err error) tea.Cmd {
		switch {
		case !live():
			return gone()
		case err == nil && image != nil:
			return land(a.attachImageToken(image))
		}
		return call(a.gw, tuigateway.InputDetectDrop, tuigateway.InputDetectDropParams{Text: cleaned, Cwd: a.store.typedPathBase()}, func(r *tuigateway.InputDetectDropResult, err error) tea.Cmd {
			switch {
			case !live():
				return gone()
			case err == nil && r.Matched && r.Text != "":
				return land(a.card.composer.Insert(r.Text))
			}
			return land(a.insertPaste(cleaned))
		})
	}
	return a.store.stageImagePath(cleaned, a.gw, destination, staged)
}
