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

// Model mutation payload of a model argument with its provider, reasoning and scope flags
func modelPayload(arg string) (map[string]any, error) {
	parts := splitArgs(arg)
	model := parts[0]
	parts = parts[1:]
	payload := map[string]any{"model": model}
	if strings.HasPrefix(model, "--") {
		return nil, errors.New(tr("canonical.controls.modelUsage"))
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
		}
		return nil, errors.New(tr("canonical.controls.unsupportedModelOption", flag))
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
	return s.mutateCanonicalRead(gw, sid, operation, payload, stale, nil, then)
}

// Compare and swap mutation handing onRead the snapshot of the revision read before it is checked
func (s *tuiStore) mutateCanonicalRead(gw turnGateway, sid, operation string, payload map[string]any, stale func() bool, onRead func(*tuigateway.CanonicalSnapshot), then func(*mutationOutcome, error) tea.Cmd) tea.Cmd {
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
	return call(gw, tuigateway.CanonicalSessionResume, tuigateway.CanonicalResumeParams{SessionID: sid, OmitMessages: true}, func(snapshot *tuigateway.CanonicalSnapshot, err error) tea.Cmd {
		if err != nil {
			return then(nil, err)
		}
		if onRead != nil {
			onRead(snapshot)
		}
		if stale() {
			return then(nil, nil)
		}
		params := tuigateway.SessionMutateParams{SessionID: sid, RequestID: uuid.NewString(), ExpectedRevision: snapshot.Revision, Operation: operation, Payload: payload}
		if operation != "rename" {
			params.ExpectedGeneration = ptr(snapshot.ExecutionGeneration)
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
		detail = tr("slashCmd.session.model.expensiveDetail")
	}
	s.overlay.Confirm = &confirmReq{CancelLabel: tr("slashCmd.session.model.cancel"), ConfirmLabel: tr("slashCmd.session.model.switchAnyway"), Danger: true, Detail: detail, OnConfirm: onConfirm, Title: tr("slashCmd.session.model.confirmTitle", target)}
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
		return errors.New(tr("session.main.invalidModelSwitchResponse"))
	}
	if val(result.Status) == "deferred" {
		sys(tr("slashCmd.session.model.switchedDeferred", model))
		return nil
	}
	generation := max(val(infoOf(s.ui.Info).ExecutionGeneration), val(result.ExecutionGeneration))
	s.ui.Info = patchedInfo(s.ui.Info, tuigateway.SessionInfo{Model: &model, ExecutionGeneration: &generation})
	if result.Note != nil {
		sys(*result.Note)
	} else {
		sys(tr("session.main.modelSwitched", model))
	}
	if result.Effort != nil {
		sys(tr("slashCmd.session.reasoning.current", *result.Effort))
	}
	return nil
}

// Switches the session's model on the owner, asking first for a guarded target, then runs onApplied when one is given
func (s *tuiStore) switchCanonicalModel(gw turnGateway, sid, arg string, sys func(string), onApplied func() tea.Cmd, confirm string, then func(error) tea.Cmd) tea.Cmd {
	baseline := s.ui.Info
	stale := func() bool { return s.ui.Sid != sid }
	return s.mutateCanonicalSession(gw, sid, "model", arg, stale, confirm, func(m *mutationOutcome, err error) tea.Cmd {
		if err != nil {
			return then(err)
		}
		if m == nil || stale() || s.isSupersededControl("model", m.Result, m.ExpectedGeneration, baseline) {
			return then(nil)
		}
		result := m.Result
		if token := result.Confirm; val(result.Status) == "confirmation_required" && token != nil {
			if confirm != "" {
				sys(tr("slashCmd.session.model.confirmStale"))
				return then(nil)
			}
			target := val(result.TargetModel)
			if result.TargetModel == nil {
				target = splitArgs(arg)[0]
			}
			detail := val(result.ConfirmMessage)
			s.openModelConfirmation(detail, target, func() tea.Cmd {
				return s.switchCanonicalModel(gw, sid, arg, sys, onApplied, *token, func(err error) tea.Cmd {
					if err != nil {
						sys("error: " + rpcErrorMessage(err))
					}
					return nil
				})
			})
			return then(nil)
		}
		if err := s.applyModelResult(result, sys); err != nil {
			return then(err)
		}
		var cmd tea.Cmd
		if onApplied != nil && val(result.Status) != "deferred" {
			cmd = onApplied()
		}
		return tea.Batch(cmd, then(nil))
	})
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
				r.fail(errors.New(trimJS(detail + "\n\n" + tr("slashCmd.session.model.confirmStale"))))
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
