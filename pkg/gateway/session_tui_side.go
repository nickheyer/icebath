package gateway

import (
	"context"
	"strings"
	"sync"

	"github.com/google/uuid"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/state"
	"github.com/nickheyer/icebath/pkg/tools"
)

// Runner surface that builds and runs the side agents of a local session on its frozen route
type tuiSideRunner interface {
	SideBackground(ctx context.Context, env *tools.Env, a *SessionAuthority, policy *LocalSessionPolicy, live *LiveSession, taskID, text string, hold func(agent any)) (string, error)
	SideQuestion(ctx context.Context, a *SessionAuthority, ref SessionRef, policy *LocalSessionPolicy, text string, parent any, history []map[string]any, onFork func(fork any), shouldStop func() bool) (string, error)
}

// Background and side question verbs of a local session
func tuiSideHandlers(c *AuthorityConnection) map[string]controlHandler {
	return map[string]controlHandler{
		"prompt.background": func(ctx context.Context, ref SessionRef, params *pyjson.Object) (any, error) {
			return sideBackground(ctx, c, ref, params)
		},
		"prompt.btw": func(ctx context.Context, ref SessionRef, params *pyjson.Object) (any, error) {
			return sideBtw(ctx, c, ref, params)
		},
	}
}

// Live session, frozen policy, request text and fresh task id of an admitted side request
func admitSide(ctx context.Context, c *AuthorityConnection, ref SessionRef, params *pyjson.Object, prefix, required string, optional ...string) (*LiveSession, *LocalSessionPolicy, string, string, error) {
	text, isStr := params.GetOr(required, nil).(string)
	allowed := append([]string{"session_id", "profile", required}, optional...)
	invalid := paramsOutside(params, allowed...) || ref.SessionID == "" || !isStr || strings.TrimSpace(text) == ""
	for _, key := range optional {
		if value := params.GetOr(key, nil); value != nil {
			if _, ok := value.(string); !ok {
				invalid = true
			}
		}
	}
	if invalid {
		return nil, nil, "", "", runtimeRefusal("invalid_params")
	}
	a := c.authority
	if _, ok := a.live(ref.SessionID); !ok {
		return nil, nil, "", "", runtimeRefusal("not_found")
	}
	if err := authorizeBusy(ctx, a, c.actor, ref, params, "session:submit"); err != nil {
		return nil, nil, "", "", err
	}
	c.mu.Lock()
	_, subscribed := c.subscriptions[ref.SessionID]
	c.mu.Unlock()
	if !subscribed {
		return nil, nil, "", "", runtimeRefusal("permission_denied")
	}
	if err := a.requireAdmissionOpen(false); err != nil {
		return nil, nil, "", "", err
	}
	live, err := liveSession(a, ref.SessionID)
	if err != nil {
		return nil, nil, "", "", err
	}
	policy, err := policyForSource(ctx, a.Runner, live.Source)
	if err != nil {
		return nil, nil, "", "", err
	}
	if policy == nil {
		return nil, nil, "", "", runtimeRefusal("permission_denied")
	}
	return live, policy, text, prefix + "_" + strings.ReplaceAll(uuid.NewString(), "-", "")[:6], nil
}

// Holder slot of a side agent, abandoned once the agent it holds reports so
type sideAgentHold struct {
	mu    sync.Mutex
	agent any
}

// Stores the side agent once built
func (h *sideAgentHold) set(agent any) {
	h.mu.Lock()
	h.agent = agent
	h.mu.Unlock()
}

// Agent the stop paths interrupt, nil until built
func (h *sideAgentHold) HolderAgent() any {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.agent
}

// True when the held agent reports itself abandoned
func (h *sideAgentHold) Abandoned() bool {
	abandoner, ok := h.HolderAgent().(interface{ Abandoned() bool })
	return ok && abandoner.Abandoned()
}

// Background prompt run to completion by a fresh agent on the session's route
func sideBackground(ctx context.Context, c *AuthorityConnection, ref SessionRef, params *pyjson.Object) (any, error) {
	live, policy, text, taskID, err := admitSide(ctx, c, ref, params, "bg", "text")
	if err != nil {
		return nil, err
	}
	a := c.authority
	runner, ok := a.Runner.(tuiSideRunner)
	if !ok {
		return nil, runnerLacks("resolve the agent runtime of a policy")
	}
	holder := &sideAgentHold{}
	body := func(ctx context.Context) (string, error) {
		var out string
		err := SideScope(ctx, a, policy, taskID, func(scoped context.Context, env *tools.Env) error {
			answer, err := runner.SideBackground(scoped, env, a, policy, live, taskID, text, holder.set)
			out = answer
			return err
		})
		return out, err
	}
	return SpawnSideAgent(ctx, a, ref, taskID, "background.complete", body, nil, holder), nil
}

// Stop handle of a side question, interrupting its cache parity fork and abandoned once stopped with no fork
type sideQuestionStop struct {
	mu      sync.Mutex
	stopped bool
	fork    any
}

// Records the running fork, interrupting it at once when already stopped
func (s *sideQuestionStop) setFork(fork any) {
	s.mu.Lock()
	s.fork = fork
	stopped := s.stopped
	s.mu.Unlock()
	if fork != nil && stopped {
		s.HardInterrupt("Profile runtime is stopping")
	}
}

// Stops the question and interrupts its fork when one runs
func (s *sideQuestionStop) HardInterrupt(message string) {
	s.mu.Lock()
	s.stopped = true
	fork := s.fork
	s.mu.Unlock()
	if fork != nil {
		requestHardInterruptWith(fork, message, "")
	}
}

// Whether a stop was requested
func (s *sideQuestionStop) isStopped() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.stopped
}

// Stopped with no fork running, what is left publishes and writes nothing
func (s *sideQuestionStop) Abandoned() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.stopped && s.fork == nil
}

// The handle itself is what the stop paths interrupt
func (s *sideQuestionStop) HolderAgent() any { return s }

// Side question over a snapshot of the conversation, the live history and FIFO untouched
func sideBtw(ctx context.Context, c *AuthorityConnection, ref SessionRef, params *pyjson.Object) (any, error) {
	_, policy, text, taskID, err := admitSide(ctx, c, ref, params, "btw", "text")
	if err != nil {
		return nil, err
	}
	a := c.authority
	runner, ok := a.Runner.(tuiSideRunner)
	if !ok {
		return nil, runnerLacks("resolve the agent runtime of a policy")
	}
	parent, err := a.agent(ref)
	if err != nil {
		return nil, err
	}
	if nilAgent(parent) {
		parent = nil
	}
	var snapshot []map[string]any
	if held, ok := parent.(interface{ RetiringSessionMessages() []map[string]any }); ok {
		snapshot = append(snapshot, held.RetiringSessionMessages()...)
	}
	target := ""
	if len(snapshot) == 0 {
		if target, err = transcriptTarget(ctx, a, ref); err != nil {
			return nil, err
		}
	}
	handle := &sideQuestionStop{}
	body := func(ctx context.Context) (string, error) {
		history := snapshot
		if len(history) == 0 {
			rows, err := a.DB.GetMessagesAsConversation(ctx, target, state.ConversationOptions{})
			if err != nil {
				return "", err
			}
			history = rows
		}
		var out string
		err := SideScope(ctx, a, policy, taskID, func(scoped context.Context, env *tools.Env) error {
			answer, err := runner.SideQuestion(scoped, a, ref, policy, text, parent, history, handle.setFork, handle.isStopped)
			out = answer
			return err
		})
		return out, err
	}
	return SpawnSideAgent(ctx, a, ref, taskID, "btw.complete", body, pyjson.ObjectOf("question", text), handle), nil
}
