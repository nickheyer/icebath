package tui

import (
	"errors"
	"reflect"
	"regexp"
	"slices"
	"strings"

	tea "charm.land/bubbletea/v2"
)

var (
	sessionBusyRe        = regexp.MustCompile(`(?i)session busy|waiting for model response`)
	submissionIDRe       = regexp.MustCompile(`submission_id`)
	imagePathLeadRe      = regexp.MustCompile(`^(?:["']?(?:[/.~]|[A-Za-z]:[/\\])|file://)`)
	imagePathExtensionRe = regexp.MustCompile(`(?i)\.(?:png|jpe?g|gif|webp)(?:["']?)(?:[` + jsWS + `]|$)`)
	undeliveredReasons   = map[string]bool{"execution_not_ready": true, "stale_generation": true, "unsupported_control": true}
)

// Whether a submit failed because the session is still busy with a turn
func isSessionBusyError(err error) bool { return err != nil && sessionBusyRe.MatchString(err.Error()) }

// Whether the owner refused a correction it never delivered, so it can go again as a prompt
func isUndeliveredCorrectionRefusal(err error) bool {
	var rerr *rpcError
	if !errors.As(err, &rerr) {
		return false
	}
	reason, isStr := rerr.Data["reason"].(string)
	return isStr && undeliveredReasons[reason]
}

// Code of a gateway error, false when it carries none
func errorCode(err error) (int64, bool) {
	var rerr *rpcError
	if errors.As(err, &rerr) && rerr.HasCode {
		return rerr.Code, true
	}
	return 0, false
}

// Error JavaScript raises reading a property of a null answer
func nullPropertyError(property string) error {
	return errors.New("Cannot read properties of null (reading '" + property + "')")
}

// Whether two maps are the same object
func sameMap(a, b map[string]any) bool {
	return reflect.ValueOf(a).UnsafePointer() == reflect.ValueOf(b).UnsafePointer()
}

// Gateway surface of a prompt submission, the gateway client in production
type submitGateway interface {
	turnGateway
	clarifyBatch() bool
	publishLocalEvent(ev gatewayEventMsg)
}

var _ submitGateway = (*gatewayClient)(nil)

// Transcript and queue surface a prompt submission writes to, the main model in production
type submitDeps interface {
	appendMessage(m msg)
	enqueue(text, display string, destination submissionDestination)
	expand(text string) string
	setLastUserMsg(text string)
	sys(text string)
	uncaught(err error)
}

// Options of one submission, its attachments, destination, queue entry and whether it waits behind a turn
type submitOpts struct {
	Attachments    []imageAttachment
	SkipDetectDrop bool
	Destination    *submissionDestination
	QueueItem      *queueItem
	BehindTurn     bool
}

// Prompt admission over the gateway, steering, redirects, legacy delivery and the durable queue journal
type promptSubmitter struct {
	store *tuiStore
	turn  *turnController
	gw    submitGateway
	deps  submitDeps
}

// Marks the session busy at the choke point every submit funnels through
func (s *tuiStore) markSubmitting() { s.ui.Busy, s.ui.Status = true, "running…" }

// Starts the turn this client owns, its bubble and busy state
func (p *promptSubmitter) beginOwnedTurn(text, displayOverride, displayText string, show bool) {
	p.turn.clearStatusTimer()
	p.deps.setLastUserMsg(text)
	if show {
		bubble := displayOverride
		if bubble == "" {
			bubble = displayText
		}
		p.deps.appendMessage(msg{Role: "user", Text: bubble})
	}
	p.store.ui.Busy, p.store.ui.Status = true, "running…"
	p.turn.bufRef = ""
	p.turn.interrupted = false
}

// Journals a queue entry with the text and attachments it is sent with
func (p *promptSubmitter) prepareQueueItem(item *queueItem, submitText string, opts *submitOpts, ownsTurn func() bool, show bool) error {
	if item.PreparedText == nil {
		item.PreparedText = &submitText
	}
	if item.ControlMethod != "" {
		attempted := true
		item.LegacyAttempted = &attempted
	}
	if item.Attachments == nil && opts.Attachments != nil {
		item.Attachments = slices.Clone(opts.Attachments)
	}
	if opts.BehindTurn {
		queued := true
		item.Queued = &queued
	}
	if ownsTurn() && show {
		p.store.markBubbleShown(item.SubmissionID)
	}
	return pendingJournal.save(*item)
}

// Params of a prompt.submit or a correction
func submitParams(sid string, item *queueItem, submitText string, attachments []imageAttachment, voice, clarifyBatch bool) map[string]any {
	params := map[string]any{"session_id": sid, "text": submitText}
	if item != nil && item.PreparedText != nil {
		params["text"] = *item.PreparedText
	}
	sent := attachments
	if item != nil && item.Attachments != nil {
		sent = item.Attachments
	}
	if len(sent) > 0 {
		list := make([]map[string]any, len(sent))
		for i, a := range sent {
			list[i] = map[string]any{"path": a.Path, "mime": a.Mime}
		}
		params["attachments"] = list
	}
	control := item != nil && item.ControlMethod != ""
	if control {
		var generation any
		if item.ExecutionGeneration != nil {
			generation = *item.ExecutionGeneration
		}
		params["execution_generation"] = generation
	}
	if item != nil && !control {
		params["submission_id"] = item.SubmissionID
		params["queued"] = item.Queued == nil || *item.Queued
	}
	if !control && voice {
		params["voice_turn"] = true
	}
	if !control && clarifyBatch {
		params["clarify_batch"] = true
	}
	return params
}

// Turns a refused correction back into a queued prompt
func readmitAsPrompt(item *queueItem) error {
	item.ControlMethod, item.ExecutionGeneration = "", nil
	attempted, queued := false, true
	item.LegacyAttempted, item.Queued = &attempted, &queued
	return pendingJournal.save(*item)
}

// Settles a correction answer, false when the owner refused it undelivered so it goes again as a prompt
func (p *promptSubmitter) settleControlReply(item *queueItem, r map[string]any, focused func() bool, sid string) (bool, error) {
	if r["voice_stopped"] == true {
		item.settle(true)
		if p.gw.canonical() {
			p.gw.publishLocalEvent(gatewayEventMsg{Type: "voice.transcript", SessionID: sid, Payload: map[string]any{"stop_phrase": true, "typed": true}})
		}
		return true, nil
	}
	generation := any(nil)
	if item.ExecutionGeneration != nil {
		generation = *item.ExecutionGeneration
	}
	if r["status"] == "rejected" && r["delivered"] == false && jsStrictEqual(r["execution_generation"], generation) {
		return false, nil
	}
	if r == nil {
		return false, nullPropertyError("execution_generation")
	}
	status := paramStr(r["status"])
	accepted := jsStrictEqual(r["execution_generation"], generation) && (status == "queued" || status == "redirected")
	item.settle(accepted)
	if focused() {
		if accepted {
			p.deps.sys(tr("canonical.submit.correction", r["status"]))
		} else {
			p.deps.sys(tr("canonical.submit.correctionRejected"))
		}
	}
	return true, nil
}

// Settles an admission answer against the destination it was sent for
func (p *promptSubmitter) settleAdmissionReply(item *queueItem, r map[string]any, destination submissionDestination, focused func() bool, owner submissionDestination, sid string) tea.Cmd {
	id := r["input_id"]
	if id == nil {
		id = r["admission_id"]
	}
	ownerHome := destination.OwnerHome
	if ownerHome == "" {
		ownerHome = destination.ProfileHome
	}
	status := paramStr(r["status"])
	accepted := jsStrictEqual(id, item.SubmissionID) && destination.StoredSid != "" && r["target_session_id"] == destination.StoredSid && r["target_profile_home"] == ownerHome && slices.Contains([]string{"queued", "started", "terminal", "unknown"}, status)
	item.settle(accepted)
	var cmd tea.Cmd
	if accepted && focused() && (status == "terminal" || status == "unknown") {
		cmd = p.refreshExecutionStatus(focused, owner, sid)
	}
	if !accepted && focused() {
		p.deps.sys(tr("canonical.submit.admissionNotConfirmed"))
		p.store.ui.Status = tr("canonical.submit.statusAdmissionUnconfirmed")
	}
	return cmd
}

// Reads the owner's running state after an admission that settled at once, applied while nothing newer arrived
func (p *promptSubmitter) refreshExecutionStatus(focused func() bool, owner submissionDestination, sid string) tea.Cmd {
	observed := p.store.ui.Info
	return gatewayCall(p.gw, "session.activate", map[string]any{"session_id": sid, "omit_messages": true}, func(v any, err error) tea.Cmd {
		snapshot := asRPCResult(v)
		if err == nil && snapshot == nil {
			err = nullPropertyError("info")
		}
		if err != nil {
			if focused() {
				p.deps.sys(tr("canonical.submit.executionUnconfirmed", err.Error()))
			}
			return nil
		}
		current := p.store.ui.Info
		info, _ := snapshot["info"].(map[string]any)
		stored := snapshot["session_key"]
		if !truthyJS(stored) {
			stored = info["stored_session_id"]
		}
		running, isBool := snapshot["running"].(bool)
		ownerStored := any(nil)
		if owner.StoredSid != "" {
			ownerStored = owner.StoredSid
		}
		stale := !focused() || !sameMap(current, observed) || snapshot["session_id"] != sid || !jsStrictEqual(stored, ownerStored) || !isBool
		if _, stamped := field(current, "execution_generation"); !stale && stamped {
			generation := -1.0
			if g, isNum := info["execution_generation"].(float64); isNum {
				generation = g
			}
			stale = !sameField(info, current, "execution_epoch") || !isSafeInteger(info["execution_generation"]) || generation < jsNumberOf(current["execution_generation"])
		}
		if stale {
			return nil
		}
		p.store.ui.Busy, p.store.ui.Status = running, "ready"
		if running {
			p.store.ui.Status = "running…"
		}
		if current != nil {
			next := make(map[string]any, len(current)+3)
			for k, v := range current {
				next[k] = v
			}
			if v, has := info["execution_epoch"]; has && v != nil {
				next["execution_epoch"] = v
			}
			if v, has := info["execution_generation"]; has && v != nil {
				next["execution_generation"] = v
			}
			next["running"] = running
			p.store.ui.Info = next
		}
		return nil
	})
}

// Delivers a queue entry on the legacy wire after the owner refused it before admission
func (p *promptSubmitter) submitLegacy(item *queueItem, sid, submitText string, focused func() bool) tea.Cmd {
	if focused() {
		p.deps.sys(tr("canonical.submit.legacyDelivery"))
	}
	unconfirmed := func(err error) tea.Cmd {
		item.settle(false)
		if focused() {
			p.deps.sys(tr("canonical.submit.legacyUnconfirmedWith", err.Error()))
			p.store.ui.Status = tr("canonical.submit.statusDeliveryUnconfirmed")
		}
		return nil
	}
	attempted := true
	item.LegacyAttempted = &attempted
	if err := pendingJournal.save(*item); err != nil {
		return resolved(func() tea.Cmd { return unconfirmed(err) })
	}
	text := submitText
	if item.PreparedText != nil {
		text = *item.PreparedText
	}
	return gatewayCall(p.gw, "prompt.submit", map[string]any{"session_id": sid, "text": text, "queued": item.Queued == nil || *item.Queued}, func(v any, err error) tea.Cmd {
		if err != nil {
			return unconfirmed(err)
		}
		r := asRPCResult(v)
		status := paramStr(r["status"])
		accepted := truthyJS(r["voice_stopped"]) || slices.Contains([]string{"streaming", "queued", "steered", "redirected"}, status)
		item.settle(accepted)
		if focused() {
			if truthyJS(r["voice_stopped"]) {
				p.store.ui.Busy, p.store.ui.Status = false, "ready"
			} else if !accepted {
				p.deps.sys(tr("canonical.submit.legacyUnconfirmed"))
				p.store.ui.Status = tr("canonical.submit.statusDeliveryUnconfirmed")
			}
		}
		return nil
	})
}

// Submits a prompt or queued correction to the owning session, handling drops, images, refusals and busy sessions
func (p *promptSubmitter) submitPrompt(text string, showUserMessage bool, displayOverride string, opts submitOpts) tea.Cmd {
	voiceTranscript := p.store.takeVoiceTranscript()
	destination := opts.Destination
	if destination == nil {
		captured, err := p.store.captureDestination()
		if err != nil {
			p.deps.uncaught(err)
			return nil
		}
		destination = &captured
	}
	ownerOf := *destination
	if opts.QueueItem != nil && opts.QueueItem.OwnerDestination != nil {
		ownerOf = *opts.QueueItem.OwnerDestination
	}
	owner := pendingJournal.owner(ownerOf)
	sid := owner.Sid
	focused := func() bool { return p.store.isCurrentDestination(owner) }
	ownsTurn := func() bool { return focused() && !opts.BehindTurn }
	if sid == "" {
		p.deps.sys("session not ready yet")
		return nil
	}
	if item := opts.QueueItem; item != nil && item.LegacyAttempted != nil && *item.LegacyAttempted {
		item.settle(false)
		if focused() {
			p.deps.sys(tr("canonical.submit.deliveryNotResent"))
			p.store.ui.Status = tr("canonical.submit.statusDeliveryUnconfirmed")
		}
		return nil
	}
	if ownsTurn() {
		p.store.markSubmitting()
	}
	var startSubmit func(displayText, submitText string, show bool) (tea.Cmd, error)
	readmit := func(item *queueItem) (tea.Cmd, error) {
		if err := readmitAsPrompt(item); err != nil {
			return nil, err
		}
		return p.submitPrompt(text, showUserMessage, displayOverride, submitOpts{BehindTurn: true, Destination: destination, QueueItem: item, SkipDetectDrop: true}), nil
	}
	startSubmit = func(displayText, submitText string, show bool) (tea.Cmd, error) {
		if ownsTurn() {
			p.beginOwnedTurn(text, displayOverride, displayText, show)
		}
		item := opts.QueueItem
		if item != nil {
			if err := p.prepareQueueItem(item, submitText, &opts, ownsTurn, show); err != nil {
				return nil, err
			}
		}
		if item != nil && item.ControlMethod != "" && len(item.Attachments) > 0 {
			item.settle(false)
			if focused() {
				p.deps.sys(tr("canonical.submit.busyTextOnly"))
			}
			return nil, nil
		}
		p.store.retireCanonicalControls(sid, "")
		method := "prompt.submit"
		if item != nil && item.ControlMethod != "" {
			method = item.ControlMethod
		}
		voice := voiceTranscript != nil && *voiceTranscript == strings.TrimFunc(submitText, isJSSpace)
		fail := func(err error) tea.Cmd {
			code, hasCode := errorCode(err)
			preAdmission := hasCode && (code == 4094 || (code == 4000 && submissionIDRe.MatchString(err.Error())))
			switch {
			case item != nil && preAdmission && !p.gw.canonical() && len(item.Attachments) == 0:
				return p.submitLegacy(item, sid, submitText, focused)
			case item != nil && item.ControlMethod != "" && isUndeliveredCorrectionRefusal(err):
				cmd, rerr := readmit(item)
				if rerr != nil {
					p.deps.uncaught(rerr)
				}
				return cmd
			case item != nil:
				item.settle(false)
				if focused() {
					p.deps.sys(tr("canonical.submit.inputRetained", err.Error()))
					p.store.ui.Status = tr("canonical.submit.statusAdmissionUnconfirmed")
				}
				return nil
			case isSessionBusyError(err):
				p.deps.enqueue(submitText, displayOverride, *destination)
				if !focused() {
					return nil
				}
				p.store.ui.Busy, p.store.ui.Status = true, "queued for next turn"
				ellipsis := ""
				if len(units(submitText)) > 50 {
					ellipsis = "…"
				}
				p.deps.sys(`queued: "` + jsSlice(submitText, 0, 50) + ellipsis + `"`)
				return nil
			case !focused():
				return nil
			}
			p.deps.sys("error: " + err.Error())
			p.store.ui.Busy, p.store.ui.Status = false, "ready"
			return nil
		}
		return gatewayCall(p.gw, method, submitParams(sid, item, submitText, opts.Attachments, voice, p.gw.clarifyBatch()), func(v any, err error) tea.Cmd {
			if err != nil {
				return fail(err)
			}
			r := asRPCResult(v)
			if item != nil && item.ControlMethod != "" {
				settled, serr := p.settleControlReply(item, r, focused, sid)
				if serr != nil {
					return fail(serr)
				}
				if !settled {
					cmd, rerr := readmit(item)
					if rerr != nil {
						return fail(rerr)
					}
					return cmd
				}
				return nil
			}
			if r["voice_stopped"] == true {
				if item != nil {
					item.settle(true)
				}
				if p.gw.canonical() {
					p.gw.publishLocalEvent(gatewayEventMsg{Type: "voice.transcript", SessionID: sid, Payload: map[string]any{"stop_phrase": true, "typed": true}})
				}
				if ownsTurn() {
					p.store.ui.Busy, p.store.ui.Status = false, "ready"
				}
				return nil
			}
			if item != nil {
				return p.settleAdmissionReply(item, r, *destination, focused, owner, sid)
			}
			return nil
		}), nil
	}
	direct := func(displayText, submitText string) tea.Cmd {
		cmd, err := startSubmit(displayText, submitText, showUserMessage)
		if err != nil {
			p.deps.uncaught(err)
		}
		return cmd
	}
	if item := opts.QueueItem; item != nil && item.PreparedText != nil {
		return direct(text, *item.PreparedText)
	}
	if opts.SkipDetectDrop {
		return direct(text, p.deps.expand(text))
	}
	if p.gw.canonical() {
		if imagePathLeadRe.MatchString(text) && imagePathExtensionRe.MatchString(text) {
			return p.store.stageImagePath(text, p.gw, *destination, func(image *stagedImage, err error) tea.Cmd {
				var cmd tea.Cmd
				if err == nil {
					opts.Attachments = []imageAttachment{image.imageAttachment}
					question := image.Remainder
					if question == "" {
						question = "What do you see in this image?"
					}
					cmd, err = startSubmit(text, question, showUserMessage)
				}
				if err == nil {
					return cmd
				}
				if opts.QueueItem != nil {
					opts.QueueItem.settle(false)
				}
				if focused() {
					p.deps.sys(tr("canonical.submit.imageNotSubmitted", err.Error()))
					if ownsTurn() {
						p.store.ui.Busy, p.store.ui.Status = false, tr("canonical.submit.statusImageNotSubmitted")
					}
				}
				return nil
			})
		}
		return direct(text, p.deps.expand(text))
	}
	return gatewayCall(p.gw, "input.detect_drop", map[string]any{"session_id": sid, "text": text}, func(v any, err error) tea.Cmd {
		r := asRPCResult(v)
		var cmd tea.Cmd
		if err == nil && truthyJS(r["matched"]) {
			dropped := text
			if truthyJS(r["text"]) {
				dropped = jsValueString(r["text"])
			}
			cmd, err = startSubmit(dropped, p.deps.expand(dropped), showUserMessage)
		} else if err == nil {
			cmd, err = startSubmit(text, p.deps.expand(text), showUserMessage)
		}
		if err == nil {
			return cmd
		}
		return direct(text, p.deps.expand(text))
	})
}
