package tui

import (
	"slices"
	"strings"

	"github.com/nickheyer/icebath/pkg/tuigateway"
)

// Field of a payload and whether the payload carries it at all
func field(m map[string]any, k string) (any, bool) {
	v, has := m[k]
	return v, has
}

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
	promptID, named := field(ev.Payload, "prompt_id")
	matches := func(shared *sharedControl) bool {
		if shared == nil {
			return !named
		}
		return named && jsStrictEqual(shared.PromptID, promptID)
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

// Owner stamp of a shared prompt event, nil when the event lacks its prompt id, session or generation
func sharedControlOf(ev gatewayEventMsg) *sharedControl {
	promptID, _ := ev.Payload["prompt_id"].(string)
	generation, isNum := ev.Payload["execution_generation"].(float64)
	if ev.Payload == nil || promptID == "" || ev.SessionID == "" || !isNum {
		return nil
	}
	return &sharedControl{SessionID: ev.SessionID, ExecutionGeneration: int64(generation), PromptID: promptID}
}

// Text of each entry of a non empty list, nil otherwise
func choicesOf(v any) []string {
	list, isList := v.([]any)
	if !isList || len(list) == 0 {
		return nil
	}
	out := make([]string, len(list))
	for i, item := range list {
		out[i] = jsString(item)
	}
	return out
}

// Locked answers of a shared clarify keyed by question id
func sharedAnswersOf(v any) map[string]string {
	raw, _ := v.(map[string]any)
	out := make(map[string]string, len(raw))
	for qid, answer := range raw {
		if answer == nil {
			out[qid] = ""
			continue
		}
		out[qid] = jsString(answer)
	}
	return out
}

// Opens the shared clarify card of the owner's prompt, a batch or its single question
func (s *tuiStore) openCanonicalClarify(ev gatewayEventMsg, sink promptSink) {
	shared := sharedControlOf(ev)
	if shared == nil {
		return
	}
	p := ev.Payload
	questions := []clarifyQuestion{}
	list, _ := p["questions"].([]any)
	for _, raw := range list {
		q, _ := raw.(map[string]any)
		qid, _ := q["qid"].(string)
		question, isStr := q["question"].(string)
		if qid == "" || !isStr || strings.TrimFunc(question, isJSSpace) == "" {
			continue
		}
		questions = append(questions, clarifyQuestion{Choices: choicesOf(q["choices"]), MultiSelect: q["multi_select"] == true, QID: qid, Question: strings.TrimFunc(question, isJSSpace)})
	}
	if len(questions) == 0 {
		single, _ := p["question"].(string)
		if single = strings.TrimFunc(single, isJSSpace); single != "" {
			questions = []clarifyQuestion{{Choices: choicesOf(p["choices"]), MultiSelect: p["multi_select"] == true, QID: shared.PromptID, Question: single}}
		}
	}
	if len(questions) == 0 {
		return
	}
	s.overlay.Clarify = &clarifyReq{Answers: sharedAnswersOf(p["answers"]), Questions: questions, RequestID: shared.PromptID, Shared: shared}
	sink.setStatus("waiting for input…")
	sink.ringPromptBell()
}

// Text of an optional payload value, the fallback when absent or null
func stringOr(v any, fallback string) string {
	if v == nil {
		return fallback
	}
	return jsString(v)
}

// Opens the shared approval card of the owner's prompt
func (s *tuiStore) openCanonicalApproval(ev gatewayEventMsg, sink promptSink) {
	shared := sharedControlOf(ev)
	if shared == nil {
		return
	}
	p := ev.Payload
	s.overlay.Approval = &approvalReq{
		AllowPermanent: p["allow_permanent"] != false,
		Choices:        choicesOf(p["choices"]),
		Command:        stringOr(p["command"], ""),
		Description:    stringOr(p["description"], "dangerous command"),
		RequestID:      shared.PromptID,
		Shared:         shared,
		SmartDenied:    p["smart_denied"] == true,
	}
	sink.setStatus("approval needed")
	sink.ringPromptBell()
}
