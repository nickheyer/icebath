package tui

import (
	"slices"
	"strings"

	"github.com/nickheyer/icebath/pkg/tuigateway"
	"github.com/nickheyer/icebath/pkg/ui/txt"
)

// Error event with no execution stamp, a build or request failure rather than a turn settlement
func isGenericErrorEvent(ev gatewayEventMsg) bool {
	if ev.Type != "error" {
		return false
	}
	return ev.Epoch == nil && ev.Generation == nil
}

// Drops lifecycle events of an older owner epoch or generation and advances the stamp of newer ones
func (s *tuiStore) fenceLifecycleEvent(ev gatewayEventMsg, genericError bool) bool {
	current := s.ui.Info
	lifecycle := !genericError && slices.Contains([]string{"session.info", "message.start", "message.complete", "error"}, ev.Type)
	if !lifecycle || current == nil || current.ExecutionGeneration == nil {
		return false
	}
	if !sameStamp(ev.Epoch, current.AuthorityEpoch) || ev.Generation == nil || *ev.Generation < *current.ExecutionGeneration {
		return true
	}
	if ev.Type != "session.info" {
		s.ui.Info = patchedInfo(current, tuigateway.SessionInfo{ExecutionGeneration: ev.Generation})
	}
	return false
}

// Clears the shared approval or clarify card a settled event names, true when the event was one
func (s *tuiStore) settleSharedPrompt(ev gatewayEventMsg) bool {
	if ev.Type != "approval.settled" && ev.Type != "clarify.settled" {
		return false
	}
	settled, _ := tuigateway.ApprovalSettledEvent.Payload(ev.Payload)
	matches := func(shared *sharedControl) bool {
		if shared == nil {
			return settled.PromptID == ""
		}
		return settled.PromptID != "" && shared.PromptID == settled.PromptID
	}
	if ev.Type == "approval.settled" {
		if s.overlay.Approval == nil || matches(s.overlay.Approval.Shared) {
			s.overlay.Approval = nil
		}
	} else if s.overlay.Clarify == nil || matches(s.overlay.Clarify.Shared) {
		s.overlay.Clarify = nil
	}
	return true
}

// Owner stamp of a shared prompt, nil when the event lacks its prompt id or session
func sharedControlOf(ev gatewayEventMsg, prompt tuigateway.CanonicalPendingPrompt) *sharedControl {
	if prompt.PromptID == "" || ev.SessionID == "" {
		return nil
	}
	return &sharedControl{SessionID: ev.SessionID, ExecutionGeneration: prompt.ExecutionGeneration, PromptID: prompt.PromptID}
}

// Opens the shared clarify card of the owner's question
func (s *tuiStore) openCanonicalClarify(ev gatewayEventMsg, sink promptSink) {
	prompt, err := tuigateway.ClarifyRequestEvent.Payload(ev.Payload)
	if err != nil {
		sink.misfit(tuigateway.ClarifyRequestEvent.Name(), err)
		return
	}
	shared := sharedControlOf(ev, prompt)
	question := strings.TrimFunc(val(prompt.Question), isJSSpace)
	if shared == nil || question == "" {
		return
	}
	s.overlay.Clarify = &clarifyReq{Answers: map[string]string{}, Questions: []clarifyQuestion{{Choices: prompt.Choices, MultiSelect: val(prompt.MultiSelect), QID: shared.PromptID, Question: question}}, RequestID: shared.PromptID, Shared: shared}
	sink.setStatus(tr(txt.SessionStatusWaitingForInput))
	sink.ringPromptBell()
}

// Opens the shared approval card of the owner's prompt
func (s *tuiStore) openCanonicalApproval(ev gatewayEventMsg, sink promptSink) {
	prompt, err := tuigateway.ApprovalRequestEvent.Payload(ev.Payload)
	if err != nil {
		sink.misfit(tuigateway.ApprovalRequestEvent.Name(), err)
		return
	}
	shared := sharedControlOf(ev, prompt)
	if shared == nil {
		return
	}
	description := val(prompt.Description)
	if prompt.Description == nil {
		description = tr(txt.SessionRequestDangerousCommand)
	}
	s.overlay.Approval = &approvalReq{AllowPermanent: true, Choices: prompt.Choices, Command: val(prompt.Command), Description: description, OpenURL: val(prompt.OpenURL), RequestID: shared.PromptID, Shared: shared}
	sink.setStatus(tr(txt.SessionStatusApprovalNeeded))
	sink.ringPromptBell()
}
