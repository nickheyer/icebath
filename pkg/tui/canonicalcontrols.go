package tui

import (
	"context"
	"encoding/json"
	"errors"
	"regexp"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/google/uuid"

	"github.com/nickheyer/icebath/pkg/tuigateway"
	"github.com/nickheyer/icebath/pkg/ui/txt"
)

// Continuation of an asynchronous step, run on the update loop once its work finished
type asyncDoneMsg struct{ run func() tea.Cmd }

// Runs the continuation, the app's Update returns its commands
func (m asyncDoneMsg) resume() tea.Cmd {
	if m.run == nil {
		return nil
	}
	return m.run()
}

// Gateway request whose answer continues on the update loop
func gatewayCall(gw turnGateway, method string, params map[string]any, then func(any, error) tea.Cmd) tea.Cmd {
	return gatewayCallWithin(gw, method, params, 0, then)
}

// Gateway request given its own wait, the client's default for zero, whose answer continues on the update loop
func gatewayCallWithin(gw turnGateway, method string, params map[string]any, timeout time.Duration, then func(any, error) tea.Cmd) tea.Cmd {
	return func() tea.Msg {
		result, err := gw.request(context.Background(), method, params, timeout)
		return asyncDoneMsg{run: func() tea.Cmd { return then(result, err) }}
	}
}

const tuiSessionModelFlag = "--tui-session"

var (
	canonicalReasoningEfforts = map[string]bool{"none": true, "false": true, "disabled": true, "minimal": true, "low": true, "medium": true, "high": true, "xhigh": true, "max": true, "ultra": true}
	jsWhitespaceSplit         = regexp.MustCompile(`[` + jsWS + `]+`)
)

// Words of a trimmed argument split on whitespace runs
func splitArgs(arg string) []string {
	trimmed := strings.TrimFunc(arg, isJSSpace)
	return jsWhitespaceSplit.Split(trimmed, -1)
}

// Model mutation payload of a model argument with its provider, reasoning and scope flags, the login to pin with it under "login"
func modelPayload(arg string) (map[string]any, error) {
	parts := splitArgs(arg)
	model := parts[0]
	parts = parts[1:]
	payload := map[string]any{"model": model}
	if strings.HasPrefix(model, "--") {
		return nil, errors.New(tr(txt.CanonicalControlsModelUsage))
	}
	for len(parts) > 0 {
		flag := parts[0]
		parts = parts[1:]
		switch {
		case flag == "--session" || flag == tuiSessionModelFlag:
			continue
		case flag == "--global":
			payload["persist"] = true
			continue
		case flag == "--provider" && len(parts) > 0 && parts[0] != "" && !strings.HasPrefix(parts[0], "--"):
			payload["provider"] = parts[0]
			parts = parts[1:]
			continue
		case flag == "--reasoning" && len(parts) > 0 && canonicalReasoningEfforts[strings.ToLower(parts[0])]:
			payload["reasoning"] = strings.ToLower(parts[0])
			parts = parts[1:]
			continue
		case flag == "--login" && len(parts) > 0 && parts[0] != "" && !strings.HasPrefix(parts[0], "--"):
			payload["login"] = parts[0]
			parts = parts[1:]
			continue
		}
		return nil, errors.New(tr(txt.CanonicalControlsUnsupportedModelOption, flag))
	}
	return payload, nil
}

// Mutation request in flight, kept for a retry under the same request id
type pendingMutation struct {
	sid    string
	params tuigateway.SessionMutateParams
}

// Answer of a session.mutate and the generation it was checked against
type mutationOutcome struct {
	Result             *tuigateway.SessionMutateResult
	ExpectedGeneration int64
}

// Forgets the session's pending mutations except the one keyed keep
func (s *tuiStore) retireCanonicalControls(sid, keep string) {
	for key, p := range s.pendingMutations {
		if key != keep && p.sid == sid {
			delete(s.pendingMutations, key)
		}
	}
}

// Whether an error carries the owner's refusal reason
func hasRefusalReason(err error) bool {
	var rerr *rpcError
	if !errors.As(err, &rerr) {
		return false
	}
	reason, _ := rerr.Data["reason"].(string)
	return reason != ""
}

// Compare and swap mutation of a session, reading its revision first unless a pending request is retried
func (s *tuiStore) mutateCanonical(gw turnGateway, sid, operation string, payload map[string]any, stale func() bool, then func(*mutationOutcome, error) tea.Cmd) tea.Cmd {
	if s.pendingMutations == nil {
		s.pendingMutations = map[string]pendingMutation{}
	}
	rawKey, _ := json.Marshal([]any{sid, operation, payload})
	key := string(rawKey)
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
			expected := params.ExpectedGeneration
			if expected == nil {
				expected = result.ExecutionGeneration
			}
			return then(&mutationOutcome{Result: result, ExpectedGeneration: val(expected)}, nil)
		})
	}
	if pending, has := s.pendingMutations[key]; has {
		return send(pending.params)
	}
	// The revision is read without attaching: a resume would subscribe again and refresh the session's policy
	return call(gw, tuigateway.CanonicalSessionRevision, tuigateway.SessionRevisionParams{SessionID: sid}, func(fence *tuigateway.SessionRevisionResult, err error) tea.Cmd {
		if err != nil {
			return then(nil, err)
		}
		if stale() {
			return then(nil, nil)
		}
		params := tuigateway.SessionMutateParams{SessionID: sid, RequestID: uuid.NewString(), ExpectedRevision: fence.Revision, Operation: operation, Payload: payload}
		if operation != "rename" {
			params.ExpectedGeneration = ptr(fence.ExecutionGeneration)
		}
		s.pendingMutations[key] = pendingMutation{sid: sid, params: params}
		return send(params)
	})
}

// Model, branch or compress mutation of a session with its argument and optional confirmation token
func (s *tuiStore) mutateCanonicalSession(gw turnGateway, sid, operation, arg string, stale func() bool, confirm string, then func(*mutationOutcome, error) tea.Cmd) tea.Cmd {
	payload := map[string]any{}
	switch {
	case operation == "model":
		model, err := modelPayload(arg)
		if err != nil {
			return func() tea.Msg { return asyncDoneMsg{run: func() tea.Cmd { return then(nil, err) }} }
		}
		payload = model
		// A login named with the model is pinned for the session once the model is switched
		if login, named := payload["login"].(string); named {
			delete(payload, "login")
			switched := then
			then = func(m *mutationOutcome, err error) tea.Cmd {
				if err != nil || m == nil {
					return switched(m, err)
				}
				provider, _ := payload["provider"].(string)
				pin := tuigateway.SessionSetLoginParams{SessionID: sid, Provider: provider, Login: &login}
				return call(gw, tuigateway.SessionSetLogin, pin, func(_ *tuigateway.SessionSetLoginResult, err error) tea.Cmd {
					if err != nil {
						return switched(nil, err)
					}
					return switched(m, nil)
				})
			}
		}
	case arg != "" && operation == "branch":
		payload["title"] = arg
	case arg != "":
		payload["focus"] = arg
	}
	if confirm != "" {
		payload["confirm"] = confirm
	}
	return s.mutateCanonical(gw, sid, operation, payload, stale, then)
}

// Opens the confirm card of a guarded model target
func (s *tuiStore) openModelConfirmation(detail, target string, onConfirm func() tea.Cmd) {
	if detail == "" {
		detail = tr(txt.SlashCmdSessionModelExpensiveDetail)
	}
	s.overlay.Confirm = &confirmReq{CancelLabel: tr(txt.SlashCmdSessionModelCancel), ConfirmLabel: tr(txt.SlashCmdSessionModelSwitchAnyway), Danger: true, Detail: detail, OnConfirm: onConfirm, Title: tr(txt.SlashCmdSessionModelConfirmTitle, target)}
}

// Whether a newer epoch or generation superseded a control's answer
func (s *tuiStore) isSupersededControl(operation string, result *tuigateway.SessionMutateResult, expectedGeneration int64, baseline *tuigateway.SessionInfo) bool {
	if operation == "branch" {
		return false
	}
	current := infoOf(s.ui.Info)
	if !sameStamp(current.AuthorityEpoch, infoOf(baseline).AuthorityEpoch) {
		return true
	}
	answered := expectedGeneration
	if result.ExecutionGeneration != nil {
		answered = *result.ExecutionGeneration
	}
	return val(current.ExecutionGeneration) > answered
}

// Applies a model switch answer to the session info and reports it, a deferred switch only reported
func (s *tuiStore) applyModelResult(result *tuigateway.SessionMutateResult, sys func(string)) error {
	model := val(result.Model)
	if model == "" {
		return errors.New(tr(txt.SessionMainInvalidModelSwitchResponse))
	}
	if val(result.Status) == "deferred" {
		sys(tr(txt.SlashCmdSessionModelSwitchedDeferred, model))
		return nil
	}
	generation := max(val(infoOf(s.ui.Info).ExecutionGeneration), val(result.ExecutionGeneration))
	s.ui.Info = patchedInfo(s.ui.Info, tuigateway.SessionInfo{Model: &model, ExecutionGeneration: &generation})
	if result.Note != nil {
		sys(*result.Note)
	} else {
		sys(tr(txt.SessionMainModelSwitched, model))
	}
	if result.Effort != nil {
		sys(tr(txt.SlashCmdSessionReasoningCurrent, *result.Effort))
	}
	return nil
}

// Switches the session's model for the slash command, asking once for a guarded target and reporting a confirmed pick the owner refused again
func (r *slashRun) canonicalModel(arg, confirm string) tea.Cmd {
	a := r.a
	baseline := a.store.ui.Info
	return a.store.mutateCanonicalSession(a.gw, r.sid, "model", arg, r.stale, confirm, func(m *mutationOutcome, err error) tea.Cmd {
		if r.stale() {
			return nil
		}
		if err != nil {
			r.fail(err)
			return nil
		}
		if m == nil || a.store.isSupersededControl("model", m.Result, m.ExpectedGeneration, baseline) {
			return nil
		}
		result := m.Result
		if token := result.Confirm; val(result.Status) == "confirmation_required" && token != nil {
			detail := val(result.ConfirmMessage)
			if confirm != "" {
				r.fail(errors.New(trimJS(detail + "\n\n" + tr(txt.SlashCmdSessionModelConfirmStale))))
				return nil
			}
			target := val(result.TargetModel)
			if result.TargetModel == nil {
				target = splitArgs(arg)[0]
			}
			a.store.openModelConfirmation(detail, target, func() tea.Cmd { return r.canonicalModel(arg, *token) })
			return nil
		}
		if err := a.store.applyModelResult(result, r.sys); err != nil {
			r.fail(err)
			return nil
		}
		return a.persistPickedEffort(r.sid, arg)
	})
}
