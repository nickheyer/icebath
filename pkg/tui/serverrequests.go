package tui

import (
	"strconv"
	"strings"
)

// Key of a server request id as the channel stringifies it
func requestKey(id any) string { return jsString(id) }

// Keeps a live server request so its card's answer resolves it
func (s *tuiStore) rememberServerRequest(r *serverRequest) {
	if s.serverRequests == nil {
		s.serverRequests = map[string]*serverRequest{}
	}
	s.serverRequests[requestKey(r.ID)] = r
}

// Drops a server request the gateway cancelled or answered elsewhere
func (s *tuiStore) forgetServerRequest(id string) { delete(s.serverRequests, id) }

// Answers and forgets an open request, false when nothing is open under that id
func (s *tuiStore) respondToServerRequest(id string, result map[string]any) bool {
	r, open := s.serverRequests[id]
	if !open {
		return false
	}
	delete(s.serverRequests, id)
	r.respond(result)
	return true
}

// Whether a request is still open under that id
func (s *tuiStore) hasOpenServerRequest(id string) bool {
	_, open := s.serverRequests[id]
	return open
}

// Status line and bell of a prompt that just opened
type promptSink interface {
	setStatus(status string)
	ringPromptBell()
}

// Text value of a param, empty for anything else
func paramStr(v any) string {
	s, _ := v.(string)
	return s
}

// Strings of a non empty list, nil for anything else
func paramStrList(v any) []string {
	list, isList := v.([]any)
	if !isList || len(list) == 0 {
		return nil
	}
	out := []string{}
	for _, item := range list {
		if s, isStr := item.(string); isStr {
			out = append(out, s)
		}
	}
	return out
}

// Question objects of a clarify list in order, an entry that is no object counting as empty
func clarifyQuestionsOf(v any) []clarifyQuestion {
	list, _ := v.([]any)
	out := []clarifyQuestion{}
	for _, raw := range list {
		q, _ := raw.(map[string]any)
		qid, question := paramStr(q["qid"]), strings.TrimFunc(paramStr(q["question"]), isJSSpace)
		if qid == "" || question == "" {
			continue
		}
		out = append(out, clarifyQuestion{Choices: paramStrList(q["choices"]), MultiSelect: q["multi_select"] == true, QID: qid, Question: question})
	}
	return out
}

// Locked answers keyed by question id, null read as empty and other non strings dropped
func clarifyAnswersOf(v any) map[string]string {
	out := map[string]string{}
	put := func(qid string, answer any) {
		if answer == nil {
			answer = ""
		}
		if s, isStr := answer.(string); isStr {
			out[qid] = s
		}
	}
	switch t := v.(type) {
	case map[string]any:
		for k, answer := range t {
			put(k, answer)
		}
	case []any:
		for i, answer := range t {
			put(strconv.Itoa(i), answer)
		}
	}
	return out
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
	p, id := r.Params, requestKey(r.ID)
	switch r.Method {
	case "clarify":
		questions := clarifyQuestionsOf(p["questions"])
		if len(questions) == 0 {
			r.respond(map[string]any{})
			return true
		}
		s.overlay.Clarify = &clarifyReq{Answers: clarifyAnswersOf(p["answers"]), Questions: questions, RequestID: id}
		open(tr("session.status.waitingForInput"))
	case "approval":
		description := paramStr(p["description"])
		if description == "" {
			description = tr("session.request.dangerousCommand")
		}
		s.overlay.Approval = &approvalReq{AllowPermanent: p["allow_permanent"] != false, Choices: paramStrList(p["choices"]), Command: paramStr(p["command"]), Description: description, RequestID: id, SmartDenied: p["smart_denied"] == true}
		open(tr("session.status.approvalNeeded"))
	case "sudo":
		s.overlay.Sudo = &sudoReq{RequestID: id}
		open(tr("session.status.sudoPasswordNeeded"))
	case "secret":
		s.overlay.Secret = &secretReq{EnvVar: paramStr(p["env_var"]), Prompt: paramStr(p["prompt"]), RequestID: id}
		open(tr("session.status.secretInputNeeded"))
	case "vault.unlock_prompt":
		s.overlay.VaultUnlock = &vaultUnlockReq{Backend: paramStr(p["backend"]), DisplayName: paramStr(p["display_name"]), RequestID: id}
		open(tr("session.status.unlockVault", paramStr(p["display_name"])))
	case "vault.save_login":
		site := paramStr(p["site"])
		if site == "" {
			site = paramStr(p["origin"])
		}
		s.overlay.VaultSaveLogin = &vaultSaveLoginReq{Origin: paramStr(p["origin"]), RequestID: id, Site: site}
		open(tr("session.status.saveLogin", site))
	case "vault.code":
		site := paramStr(p["site"])
		s.overlay.VaultCode = &vaultCodeReq{Hint: paramStr(p["hint"]), RequestID: id, Site: site}
		open(tr("session.status.verificationCode", site))
	default:
		return false
	}
	return true
}
