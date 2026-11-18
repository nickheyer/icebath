package tui

import (
	"errors"
	"github.com/nickheyer/icebath/pkg/tuigateway"
	"regexp"
	"slices"
	"strings"

	tea "charm.land/bubbletea/v2"
)

var (
	sessionBusyRe        = regexp.MustCompile(`(?i)session busy|waiting for model response`)
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

// Text and images a submission sends, the journaled entry's own when it has them
func submissionBody(item *queueItem, submitText string, attachments []imageAttachment) (string, []tuigateway.CanonicalAttachment) {
	text, sent := submitText, attachments
	if item != nil && item.PreparedText != nil {
		text = *item.PreparedText
	}
	if item != nil && item.Attachments != nil {
		sent = item.Attachments
	}
	var wire []tuigateway.CanonicalAttachment
	for _, a := range sent {
		wire = append(wire, tuigateway.CanonicalAttachment{Path: a.Path, Mime: a.Mime})
	}
	return text, wire
}

// Turns a refused correction back into a queued prompt
func readmitAsPrompt(item *queueItem) error {
	item.ControlMethod, item.ExecutionGeneration = "", nil
	attempted, queued := false, true
	item.LegacyAttempted, item.Queued = &attempted, &queued
	return pendingJournal.save(*item)
}

// Settles a correction verdict, false when the owner refused it undelivered so it goes again as a prompt
func (p *promptSubmitter) settleCorrection(item *queueItem, status tuigateway.CorrectionStatus, current, undelivered bool, focused func() bool) bool {
	if undelivered {
		return false
	}
	accepted := current && (status == "queued" || status == "redirected")
	item.settle(accepted)
	if focused() {
		if accepted {
			p.deps.sys(tr("canonical.submit.correction", string(status)))
		} else {
			p.deps.sys(tr("canonical.submit.correctionRejected"))
		}
	}
	return true
}

// Ends the voice chat a typed stop phrase closed, the canonical owner's transcript event raised here
func (p *promptSubmitter) voiceStopped(sid string) {
	if p.gw.canonical() {
		p.gw.publishLocalEvent(gatewayEventMsg{Type: "voice.transcript", SessionID: sid, Payload: map[string]any{"stop_phrase": true, "typed": true}})
	}
}

// Settles an admission receipt against the destination it was sent for
func (p *promptSubmitter) settleAdmissionReply(item *queueItem, receipt *tuigateway.AdmissionReceipt, destination submissionDestination, focused func() bool, owner submissionDestination, sid string) tea.Cmd {
	accepted := receipt != nil && destination.StoredSid != "" && receipt.Ref.SessionID == destination.StoredSid && receipt.Ref.ProfileID == firstText(destination.OwnerHome, destination.ProfileHome)
	item.settle(accepted)
	var cmd tea.Cmd
	if accepted && focused() && (receipt.Status == "terminal" || receipt.Status == "unknown") {
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
	settle := func(a *attachment, err error) tea.Cmd {
		if err != nil {
			if focused() {
				p.deps.sys(tr("canonical.submit.executionUnconfirmed", err.Error()))
			}
			return nil
		}
		current := p.store.ui.Info
		stale := !focused() || current != observed || a.SessionID != sid || a.StoredSID != owner.StoredSid
		if held := infoOf(current).ExecutionGeneration; !stale && held != nil {
			answered := a.Info.ExecutionGeneration
			stale = !sameStamp(a.Info.AuthorityEpoch, current.AuthorityEpoch) || answered == nil || *answered < *held
		}
		if stale {
			return nil
		}
		p.store.ui.Busy, p.store.ui.Status = a.Running, "ready"
		if a.Running {
			p.store.ui.Status = "running…"
		}
		if current != nil {
			running := a.Running
			p.store.ui.Info = patchedInfo(current, tuigateway.SessionInfo{AuthorityEpoch: a.Info.AuthorityEpoch, ExecutionGeneration: a.Info.ExecutionGeneration, Running: &running})
		}
		return nil
	}
	if p.gw.canonical() {
		return call(p.gw, tuigateway.CanonicalSessionResume, tuigateway.CanonicalResumeParams{SessionID: sid, OmitMessages: true}, func(r *tuigateway.CanonicalSnapshot, err error) tea.Cmd {
			if err != nil {
				return settle(nil, err)
			}
			a := snapshotAttachment(r)
			return settle(&a, nil)
		})
	}
	return call(p.gw, tuigateway.SessionActivate, tuigateway.SessionActivateParams{SessionID: sid, OmitMessages: true}, func(r *tuigateway.SessionActivateResult, err error) tea.Cmd {
		if err != nil {
			return settle(nil, err)
		}
		a := resumedAttachment(r, firstText(val(r.SessionKey), val(r.Info.StoredSessionID)))
		return settle(&a, nil)
	})
}

// Delivers a queue entry on the method table route, which takes no submission id so the entry is never resent
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
	text, _ := submissionBody(item, submitText, nil)
	queued := item.Queued == nil || *item.Queued
	return call(p.gw, tuigateway.PromptSubmit, tuigateway.PromptSubmitParams{SessionID: sid, Text: text, Queued: &queued}, func(r *tuigateway.PromptSubmitResult, err error) tea.Cmd {
		if err != nil {
			return unconfirmed(err)
		}
		stopped := val(r.VoiceStopped)
		accepted := stopped || r.Status != nil
		item.settle(accepted)
		if focused() {
			if stopped {
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
		voice := voiceTranscript != nil && *voiceTranscript == strings.TrimFunc(submitText, isJSSpace)
		fail := func(err error) tea.Cmd {
			switch {
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
		readmitOr := func(settled bool) tea.Cmd {
			if settled {
				return nil
			}
			cmd, rerr := readmit(item)
			if rerr != nil {
				return fail(rerr)
			}
			return cmd
		}
		stopped := func() tea.Cmd {
			if item != nil {
				item.settle(true)
			}
			p.voiceStopped(sid)
			if ownsTurn() {
				p.store.ui.Busy, p.store.ui.Status = false, "ready"
			}
			return nil
		}
		text, attachments := submissionBody(item, submitText, opts.Attachments)
		canonical := p.gw.canonical()
		switch {
		case item != nil && item.ControlMethod != "" && canonical:
			if item.ExecutionGeneration == nil {
				return resolved(func() tea.Cmd { return fail(tuigateway.MissingParam(item.ControlMethod, "execution_generation")) }), nil
			}
			generation := *item.ExecutionGeneration
			correction := tuigateway.CanonicalSessionSteer
			if item.ControlMethod == tuigateway.CanonicalSessionRedirect.Name() {
				correction = tuigateway.CanonicalSessionRedirect
			}
			return call(p.gw, correction, tuigateway.CanonicalCorrectionParams{SessionID: sid, ExecutionGeneration: generation, Text: text}, func(r *tuigateway.CorrectionAnswer, err error) tea.Cmd {
				if err != nil {
					return fail(err)
				}
				if r.VoiceStopped {
					item.settle(true)
					p.voiceStopped(sid)
					return nil
				}
				ack := r.CorrectionAck
				current := ack.ExecutionGeneration == generation
				return readmitOr(p.settleCorrection(item, ack.Status, current, current && ack.Status == "rejected" && ack.Delivered != nil && !*ack.Delivered, focused))
			}), nil
		case item != nil && item.ControlMethod != "":
			correction := tuigateway.SessionSteer
			if item.ControlMethod == tuigateway.SessionRedirect.Name() {
				correction = tuigateway.SessionRedirect
			}
			return call(p.gw, correction, tuigateway.SessionCorrectionParams{SessionID: sid, Text: text}, func(r *tuigateway.SessionCorrectionResult, err error) tea.Cmd {
				if err != nil {
					return fail(err)
				}
				return readmitOr(p.settleCorrection(item, r.Status, true, false, focused))
			}), nil
		case canonical:
			params := tuigateway.CanonicalSubmitParams{SessionID: sid, Text: text, Attachments: attachments, VoiceTurn: voice, ClarifyBatch: p.gw.clarifyBatch()}
			if item != nil {
				params.InputID, params.Queued = &item.SubmissionID, item.Queued == nil || *item.Queued
			}
			return call(p.gw, tuigateway.CanonicalPromptSubmit, params, func(r *tuigateway.SubmitAnswer, err error) tea.Cmd {
				switch {
				case err != nil:
					return fail(err)
				case r.VoiceStopped:
					return stopped()
				case item != nil:
					return p.settleAdmissionReply(item, r.AdmissionReceipt, *destination, focused, owner, sid)
				}
				return nil
			}), nil
		case len(attachments) > 0:
			return resolved(func() tea.Cmd { return fail(tuigateway.UnknownParam(tuigateway.PromptSubmit.Name(), "attachments")) }), nil
		case item != nil:
			return p.submitLegacy(item, sid, submitText, focused), nil
		}
		params := tuigateway.PromptSubmitParams{SessionID: sid, Text: text}
		if voice {
			params.VoiceTurn = &voice
		}
		return call(p.gw, tuigateway.PromptSubmit, params, func(r *tuigateway.PromptSubmitResult, err error) tea.Cmd {
			switch {
			case err != nil:
				return fail(err)
			case val(r.VoiceStopped):
				return stopped()
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
