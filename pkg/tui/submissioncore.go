package tui

import (
	"errors"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"github.com/nickheyer/icebath/pkg/tuigateway"
	"github.com/nickheyer/icebath/pkg/ui/txt"

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
	// Expansion of the chips the draft holds right now, kept for a submit that settles later
	expander() func(string) string
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
	// Turns the text shown into the text sent, the chips of the draft at submit time when unset
	Expand func(string) string
}

// Prompt admission over the gateway, steering, redirects, legacy delivery and the durable queue journal
type promptSubmitter struct {
	store *tuiStore
	turn  *turnController
	gw    submitGateway
	deps  submitDeps
	// Count of sent turns that carried images, the revision of the pictures of each
	imageTurns int
}

// Marks the session busy at the choke point every submit funnels through
func (s *tuiStore) markSubmitting() {
	s.setBusy(true)
	s.ui.Status = "running…"
}

// Ends the busy state of a submission that began a turn of its own and was not taken, a turn that runs after all marks the session busy again through its events
func (s *tuiStore) releaseUntaken(ownsTurn func() bool) {
	if ownsTurn() {
		s.setBusy(false)
	}
}

// Starts the turn this client owns, its bubble with the images sent along and busy state, each turn its own image revision so a file attached again is drawn again
func (p *promptSubmitter) beginOwnedTurn(text, displayOverride, displayText string, show bool, images []string) {
	p.turn.clearStatusTimer()
	p.deps.setLastUserMsg(text)
	if show {
		bubble := displayOverride
		if bubble == "" {
			bubble = displayText
		}
		row := msg{Role: "user", Text: bubble}
		if staged, found := p.store.takePastes(text); found && staged.display == bubble {
			row.Pastes = staged.pastes
		}
		if len(images) > 0 {
			p.imageTurns++
			row.Images, row.ImageRevision = images, "turn-"+strconv.Itoa(p.imageTurns)
		}
		p.deps.appendMessage(row)
	}
	p.store.setBusy(true)
	p.store.ui.Status = "running…"
	p.turn.bufRef = ""
	p.turn.interrupted = false
}

// Journals a queue entry with the text and attachments it is sent with
func (p *promptSubmitter) prepareQueueItem(item *queueItem, submitText string, opts *submitOpts, ownsTurn func() bool, show bool) error {
	if item.PreparedText == nil {
		item.PreparedText = &submitText
	}
	// A correction carries no input id the owner could tell a second delivery by, so it is sent once and never again
	if item.ControlMethod != "" {
		attempted := true
		item.SentOnce = &attempted
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
	item.SentOnce, item.Queued = &attempted, &queued
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
			p.deps.sys(tr(txt.CanonicalSubmitCorrection, string(status)))
		} else {
			p.deps.sys(tr(txt.CanonicalSubmitCorrectionRejected))
		}
	}
	return true
}

// Ends the voice chat a typed stop phrase closed, the transcript event of the background service raised here
func (p *promptSubmitter) voiceStopped(sid string) {
	p.gw.publishLocalEvent(gatewayEventMsg{Type: "voice.transcript", SessionID: sid, Payload: map[string]any{"stop_phrase": true, "typed": true}})
}

// Settles an admission receipt against the destination it was sent for
func (p *promptSubmitter) settleAdmissionReply(item *queueItem, receipt *tuigateway.AdmissionReceipt, destination submissionDestination, focused, ownsTurn func() bool, owner submissionDestination, sid string) tea.Cmd {
	accepted := receipt != nil && destination.StoredSid != "" && receipt.Ref.SessionID == destination.StoredSid && receipt.Ref.ProfileID == firstText(destination.OwnerHome, destination.ProfileHome)
	item.settle(accepted)
	var cmd tea.Cmd
	if accepted && focused() && (receipt.Status == "terminal" || receipt.Status == "unknown") {
		cmd = p.refreshExecutionStatus(focused, sid)
	}
	if !accepted && focused() {
		p.deps.sys(tr(txt.CanonicalSubmitAdmissionNotConfirmed))
		p.store.ui.Status = tr(txt.CanonicalSubmitStatusAdmissionUnconfirmed)
	}
	if !accepted {
		p.store.releaseUntaken(ownsTurn)
	}
	return cmd
}

// Reads the owner's running state after an admission that settled at once, applied while nothing newer arrived
func (p *promptSubmitter) refreshExecutionStatus(focused func() bool, sid string) tea.Cmd {
	observed := p.store.ui.Info
	return call(p.gw, tuigateway.CanonicalSessionRevision, tuigateway.SessionRevisionParams{SessionID: sid}, func(fence *tuigateway.SessionRevisionResult, err error) tea.Cmd {
		if err != nil {
			if focused() {
				p.deps.sys(tr(txt.CanonicalSubmitExecutionUnconfirmed, err.Error()))
			}
			return nil
		}
		current := p.store.ui.Info
		stale := !focused() || current != observed || fence.SessionID != sid
		if held := infoOf(current).ExecutionGeneration; !stale && held != nil {
			stale = !sameStamp(&fence.AuthorityEpoch, current.AuthorityEpoch) || fence.ExecutionGeneration < *held
		}
		if stale {
			return nil
		}
		p.store.setBusy(fence.Running)
		p.store.ui.Status = "ready"
		if fence.Running {
			p.store.ui.Status = "running…"
		}
		if current != nil {
			running := fence.Running
			p.store.ui.Info = patchedInfo(current, tuigateway.SessionInfo{AuthorityEpoch: &fence.AuthorityEpoch, ExecutionGeneration: &fence.ExecutionGeneration, Running: &running})
		}
		return nil
	})
}

// Submits a prompt or queued correction to the owning session, handling drops, images, refusals and busy sessions
func (p *promptSubmitter) submitPrompt(text string, showUserMessage bool, displayOverride string, opts submitOpts) tea.Cmd {
	voiceTranscript := p.store.takeVoiceTranscript()
	expand := opts.Expand
	if expand == nil {
		expand = p.deps.expander()
	}
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
		p.deps.sys(tr(txt.SessionNoticesNotReady))
		return nil
	}
	if item := opts.QueueItem; item != nil && item.SentOnce != nil && *item.SentOnce {
		item.settle(false)
		if focused() {
			p.deps.sys(tr(txt.CanonicalSubmitDeliveryNotResent))
			p.store.ui.Status = tr(txt.CanonicalSubmitStatusDeliveryUnconfirmed)
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
			sent := opts.Attachments
			if opts.QueueItem != nil && opts.QueueItem.Attachments != nil {
				sent = opts.QueueItem.Attachments
			}
			images := make([]string, len(sent))
			for i, attachment := range sent {
				images[i] = attachment.Path
			}
			p.beginOwnedTurn(text, displayOverride, displayText, show, images)
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
				p.deps.sys(tr(txt.CanonicalSubmitBusyTextOnly))
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
					p.deps.sys(tr(txt.CanonicalSubmitInputRetained, err.Error()))
					p.store.ui.Status = tr(txt.CanonicalSubmitStatusAdmissionUnconfirmed)
				}
				p.store.releaseUntaken(ownsTurn)
				return nil
			case isSessionBusyError(err):
				p.deps.enqueue(submitText, displayOverride, *destination)
				if !focused() {
					return nil
				}
				p.store.setBusy(true)
				p.store.ui.Status = tr(txt.StatusQueuedForNextTurn)
				ellipsis := ""
				if len(units(submitText)) > 50 {
					ellipsis = "…"
				}
				p.deps.sys(tr(txt.SlashCmdCoreQueueQueued, jsSlice(submitText, 0, 50)+ellipsis))
				return nil
			case !focused():
				return nil
			}
			p.deps.sys(errorLine(err.Error()))
			p.store.setBusy(false)
			p.store.ui.Status = "ready"
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
				p.store.setBusy(false)
				p.store.ui.Status = "ready"
			}
			return nil
		}
		text, attachments := submissionBody(item, submitText, opts.Attachments)
		if item != nil && item.ControlMethod != "" {
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
		}
		params := tuigateway.CanonicalSubmitParams{SessionID: sid, Text: text, Attachments: attachments, VoiceTurn: voice, Interrupted: speech.takeInterrupted(), ClarifyBatch: p.gw.clarifyBatch()}
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
				return p.settleAdmissionReply(item, r.AdmissionReceipt, *destination, focused, ownsTurn, owner, sid)
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
		return direct(text, expand(text))
	}
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
				p.deps.sys(tr(txt.CanonicalSubmitImageNotSubmitted, err.Error()))
				if ownsTurn() {
					p.store.setBusy(false)
					p.store.ui.Status = tr(txt.CanonicalSubmitStatusImageNotSubmitted)
				}
			}
			return nil
		})
	}
	return direct(text, expand(text))
}
