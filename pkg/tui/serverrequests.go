package tui

import (
	"strings"

	"github.com/nickheyer/icebath/pkg/tuigateway"
)

// Key of a server request id, a text id as it is and a numeric id in its shortest decimal form
func requestKey(id any) string { return argText(id) }

// Keeps a live server request so its card's answer resolves it
func (s *tuiStore) rememberServerRequest(r *serverRequest) {
	if s.serverRequests == nil {
		s.serverRequests = map[string]*serverRequest{}
	}
	s.serverRequests[requestKey(r.ID)] = r
}

// Drops a server request the gateway cancelled or answered elsewhere
func (s *tuiStore) forgetServerRequest(id string) { delete(s.serverRequests, id) }

// Answers and forgets an open request with a typed answer, false when nothing is open under that id
func (s *tuiStore) respondToServerRequest(id string, answer any) bool {
	r, open := s.serverRequests[id]
	if !open {
		return false
	}
	delete(s.serverRequests, id)
	r.respond(wireParams(answer))
	return true
}

// Status line and bell of a prompt that just opened
type promptSink interface {
	setStatus(status string)
	ringPromptBell()
	misfit(name string, err error)
}

// Clarify questions with a question text, trimmed
func clarifyQuestionsOf(questions []tuigateway.ClarifyQuestion) []clarifyQuestion {
	out := []clarifyQuestion{}
	for _, q := range questions {
		if question := strings.TrimFunc(q.Question, isJSSpace); q.QID != "" && question != "" {
			out = append(out, clarifyQuestion{Choices: q.Choices, MultiSelect: q.MultiSelect, QID: q.QID, Question: question})
		}
	}
	return out
}

// Locked answers keyed by question id, a skipped one empty
func clarifyAnswersOf(answers map[string]*string) map[string]string {
	out := make(map[string]string, len(answers))
	for qid, answer := range answers {
		out[qid] = val(answer)
	}
	return out
}

// Params of a server request decoded into its type, a misfit printed and declined
func requestParams[P, R any](r *serverRequest, sink promptSink, req tuigateway.Request[P, R]) (P, bool) {
	params, err := req.Params(r.Params)
	if err != nil {
		sink.misfit(req.Name(), err)
		r.fail(rpcInvalidParams, err.Error())
		return params, false
	}
	return params, true
}

// Opens the card of a server request, false for a method the terminal cannot answer
func (s *tuiStore) handleServerRequest(r *serverRequest, sink promptSink) bool {
	open := func(status string) {
		s.rememberServerRequest(r)
		sink.setStatus(status)
		if !r.Replayed {
			sink.ringPromptBell()
		}
	}
	id := requestKey(r.ID)
	switch r.Method {
	case tuigateway.ClarifyRequest.Name():
		p, ok := requestParams(r, sink, tuigateway.ClarifyRequest)
		if !ok {
			return true
		}
		questions := clarifyQuestionsOf(p.Questions)
		if len(questions) == 0 {
			r.respond(wireParams(tuigateway.ClarifyAnswer{}))
			return true
		}
		s.overlay.Clarify = &clarifyReq{Answers: clarifyAnswersOf(p.Answers), Questions: questions, RequestID: id}
		open(tr("session.status.waitingForInput"))
	case tuigateway.ApprovalRequest.Name():
		p, ok := requestParams(r, sink, tuigateway.ApprovalRequest)
		if !ok {
			return true
		}
		description := p.Description
		if description == "" {
			description = tr("session.request.dangerousCommand")
		}
		choices := make([]string, len(p.Choices))
		for i, c := range p.Choices {
			choices[i] = string(c)
		}
		s.overlay.Approval = &approvalReq{AllowPermanent: p.AllowPermanent == nil || *p.AllowPermanent, Choices: choices, Command: p.Command, Description: description, OpenURL: val(p.OpenURL), RequestID: id, SmartDenied: val(p.SmartDenied)}
		open(tr("session.status.approvalNeeded"))
	case tuigateway.SudoRequest.Name():
		p, ok := requestParams(r, sink, tuigateway.SudoRequest)
		if !ok {
			return true
		}
		s.overlay.Sudo = &sudoReq{Command: p.Command, RequestID: id}
		open(tr("session.status.sudoPasswordNeeded"))
	case tuigateway.DisplayInstallSudoRequest.Name():
		p, ok := requestParams(r, sink, tuigateway.DisplayInstallSudoRequest)
		if !ok {
			return true
		}
		s.overlay.Sudo = &sudoReq{ProfileKey: p.ProfileKey, RequestID: id}
		open(tr("session.status.sudoPasswordNeeded"))
	case tuigateway.SecretRequest.Name():
		p, ok := requestParams(r, sink, tuigateway.SecretRequest)
		if !ok {
			return true
		}
		s.overlay.Secret = &secretReq{EnvVar: p.EnvVar, Prompt: p.Prompt, RequestID: id}
		open(tr("session.status.secretInputNeeded"))
	case tuigateway.VaultUnlockRequest.Name():
		p, ok := requestParams(r, sink, tuigateway.VaultUnlockRequest)
		if !ok {
			return true
		}
		s.overlay.VaultUnlock = &vaultUnlockReq{Backend: p.Backend, DisplayName: p.DisplayName, RequestID: id}
		open(tr("session.status.unlockVault", p.DisplayName))
	case tuigateway.VaultSaveLoginRequest.Name():
		p, ok := requestParams(r, sink, tuigateway.VaultSaveLoginRequest)
		if !ok {
			return true
		}
		site := firstText(p.Site, p.Origin)
		s.overlay.VaultSaveLogin = &vaultSaveLoginReq{Origin: p.Origin, RequestID: id, Site: site}
		open(tr("session.status.saveLogin", site))
	case tuigateway.VaultCodeRequest.Name():
		p, ok := requestParams(r, sink, tuigateway.VaultCodeRequest)
		if !ok {
			return true
		}
		site := val(p.Site)
		s.overlay.VaultCode = &vaultCodeReq{Hint: val(p.Hint), RequestID: id, Site: site}
		open(tr("session.status.verificationCode", site))
	default:
		return false
	}
	return true
}
