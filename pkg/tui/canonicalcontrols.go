package tui

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"regexp"
	"strings"

	tea "charm.land/bubbletea/v2"
	"github.com/google/uuid"
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
	return func() tea.Msg {
		result, err := gw.request(context.Background(), method, params, 0)
		return asyncDoneMsg{run: func() tea.Cmd { return then(result, err) }}
	}
}

// Object result of a request, nil for anything else
func asRPCResult(v any) map[string]any {
	m, _ := v.(map[string]any)
	return m
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

// Model mutation payload of a model argument with its provider and reasoning flags
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
	params map[string]any
}

// Answer of a session.mutate and the generation it was checked against
type mutationOutcome struct {
	Result             map[string]any
	ExpectedGeneration float64
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
	return errors.As(err, &rerr) && truthyJS(rerr.Data["reason"])
}

// Compare and swap mutation of a session, reading its revision first unless a pending request is retried
func (s *tuiStore) mutateCanonical(gw turnGateway, sid, operation string, payload map[string]any, stale func() bool, then func(*mutationOutcome, error) tea.Cmd) tea.Cmd {
	if s.pendingMutations == nil {
		s.pendingMutations = map[string]pendingMutation{}
	}
	rawKey, _ := json.Marshal([]any{sid, operation, payload})
	key := string(rawKey)
	s.retireCanonicalControls(sid, key)
	send := func(params map[string]any) tea.Cmd {
		return gatewayCall(gw, "session.mutate", params, func(v any, err error) tea.Cmd {
			result := asRPCResult(v)
			if err == nil && result == nil {
				err = errors.New(tr("session.common.invalidResponse", "session.mutate"))
			} else if err == nil {
				delete(s.pendingMutations, key)
			}
			if err != nil {
				if hasRefusalReason(err) {
					delete(s.pendingMutations, key)
				}
				return then(nil, err)
			}
			expected, hasExpected := params["expected_generation"]
			if !hasExpected || expected == nil {
				expected = result["execution_generation"]
			}
			return then(&mutationOutcome{Result: result, ExpectedGeneration: jsNumberOf(expected)}, nil)
		})
	}
	if pending, has := s.pendingMutations[key]; has {
		return send(pending.params)
	}
	return gatewayCall(gw, "session.resume", map[string]any{"session_id": sid}, func(v any, err error) tea.Cmd {
		if err != nil {
			return then(nil, err)
		}
		snapshot := asRPCResult(v)
		if stale() {
			return then(nil, nil)
		}
		if !isSafeInteger(snapshot["revision"]) || !isSafeInteger(snapshot["execution_generation"]) {
			return then(nil, errors.New(tr("canonical.controls.identityUnavailable")))
		}
		params := map[string]any{"session_id": sid, "request_id": uuid.NewString(), "expected_revision": snapshot["revision"]}
		if operation != "rename" {
			params["expected_generation"] = snapshot["execution_generation"]
		}
		params["operation"], params["payload"] = operation, payload
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
func (s *tuiStore) isSupersededControl(operation string, result map[string]any, expectedGeneration float64, baseline map[string]any) bool {
	if operation == "branch" {
		return false
	}
	current := s.ui.Info
	if !sameField(current, baseline, "execution_epoch") {
		return true
	}
	answered := expectedGeneration
	if g, has := result["execution_generation"]; has && g != nil {
		answered = jsNumberOf(g)
	}
	have := 0.0
	if g, has := current["execution_generation"]; has && g != nil {
		have = jsNumberOf(g)
	}
	return have > answered
}

// Applies a model switch answer to the session info and reports it, a deferred switch only reported
func (s *tuiStore) applyModelResult(result map[string]any, sys func(string)) error {
	if !truthyJS(result["model"]) {
		return errors.New(tr("session.main.invalidModelSwitchResponse"))
	}
	model := jsValueString(result["model"])
	if result["status"] == "deferred" {
		sys(tr("slashCmd.session.model.switchedDeferred", model))
		return nil
	}
	info := make(map[string]any, len(s.ui.Info)+4)
	for k, v := range s.ui.Info {
		info[k] = v
	}
	generation := 0.0
	if g, has := s.ui.Info["execution_generation"]; has && g != nil {
		generation = jsNumberOf(g)
	}
	answered := 0.0
	if g, has := result["execution_generation"]; has && g != nil {
		answered = jsNumberOf(g)
	}
	info["model"], info["execution_generation"] = result["model"], math.Max(generation, answered)
	if info["skills"] == nil {
		info["skills"] = map[string]any{}
	}
	if info["tools"] == nil {
		info["tools"] = map[string]any{}
	}
	s.ui.Info = info
	if note, isStr := result["note"].(string); isStr {
		sys(note)
	} else {
		sys(tr("session.main.modelSwitched", model))
	}
	if effort, isStr := result["effort"].(string); isStr {
		sys(tr("slashCmd.session.reasoning.current", effort))
	}
	return nil
}

// Switches the session's model on the owner, asking first for a guarded target, then runs onApplied
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
		if token, isStr := result["confirm"].(string); result["status"] == "confirmation_required" && isStr {
			if confirm != "" {
				sys(tr("slashCmd.session.model.confirmStale"))
				return then(nil)
			}
			target := stringOr(result["target_model"], "")
			if _, has := result["target_model"]; !has || result["target_model"] == nil {
				target = splitArgs(arg)[0]
			}
			detail, _ := result["confirm_message"].(string)
			s.openModelConfirmation(detail, target, func() tea.Cmd {
				return s.switchCanonicalModel(gw, sid, arg, sys, onApplied, token, func(err error) tea.Cmd {
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
		if result["status"] != "deferred" {
			cmd = onApplied()
		}
		return tea.Batch(cmd, then(nil))
	})
}
