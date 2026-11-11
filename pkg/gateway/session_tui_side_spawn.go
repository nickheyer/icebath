package gateway

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"

	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/state"
	"github.com/nickheyer/icebath/pkg/tools"
)

// Holder the side agent ledger counts, abandoned once its work no longer holds the owner
type SideAgentHolder = sideAgentHolder

// Ledger key of one side agent goroutine, the thread identity of the ledger
type sideAgentThread struct{ name string }

func sideLog() *slog.Logger { return slog.Default().With("logger", "gateway.session_tui_side") }

// Runs body in the owning profile scope and frozen policy with the task's own session identity and workspace bound
func SideScope(ctx context.Context, a *SessionAuthority, policy *LocalSessionPolicy, taskID string, body func(ctx context.Context, env *tools.Env) error) error {
	profile := ""
	if tools.IsRoutedHome(a.ProfileID) {
		profile = servedProfileName(a.ProfileID)
	}
	return policyScope(ownerScope(ctx, a, true), policy, a, func(scoped context.Context) error {
		env := &tools.Env{Home: home.Dir(scoped), Values: &sync.Map{}}
		tools.SetSessionVars(env, tools.SessionVars{SessionKey: taskID, SessionID: taskID, Cwd: policy.Cwd, Profile: profile, NoAsyncDelivery: true})
		defer tools.ClearSessionVars(env)
		defer tools.UnbindTurnWorkspace(env, taskID)
		if err := BindTurnWorkspace(env, policy, taskID); err != nil {
			return err
		}
		return body(tools.WithToolEnv(scoped, env), env)
	})
}

// True when the holder's work was stopped and no longer counts as owner work
func sideAgentAbandoned(holder SideAgentHolder) bool {
	return holder != nil && holder.Abandoned()
}

// Error text a failed side agent publishes, a refusal naming the check that fired
func sideAgentErrorText(err error) string {
	reason := pyrepr.ExceptionMessage(err)
	named := ""
	var refusal *state.RuntimeStoreError
	if errors.As(err, &refusal) {
		if refusal.Reason != "" {
			reason = refusal.Reason
		}
		if refusal.Field != nil && *refusal.Field != "" && refusal.Detail != nil && *refusal.Detail != "" {
			named = fmt.Sprintf(" (%s: %s)", *refusal.Field, *refusal.Detail)
		}
	}
	return "error: " + reason + named
}

// Runs the side agent body, a panic surfacing as its error
func runSideBody(ctx context.Context, body func(ctx context.Context) (string, error)) (text string, err error) {
	defer func() {
		if recovered := recover(); recovered != nil {
			if failure, ok := recovered.(error); ok {
				err = failure
			} else {
				err = fmt.Errorf("%v", recovered)
			}
		}
	}()
	return body(ctx)
}

// Runs body on its own goroutine counted as owner work, publishing its text or error on the session
func SpawnSideAgent(ctx context.Context, a *SessionAuthority, ref SessionRef, taskID, eventType string,
	body func(ctx context.Context) (string, error), extra *pyjson.Object, holder SideAgentHolder) *pyjson.Object {
	key := &sideAgentThread{name: "side-agent-" + taskID}
	a.mu.Lock()
	if a.sideAgents == nil {
		a.sideAgents = map[any]sideAgentHolder{}
	}
	a.sideAgents[key] = holder
	a.mu.Unlock()
	threadCtx := context.WithoutCancel(ctx)
	go func() {
		defer func() {
			a.mu.Lock()
			delete(a.sideAgents, key)
			a.mu.Unlock()
			persistWorkCount(a)
		}()
		text, err := runSideBody(threadCtx, body)
		if err != nil {
			sideLog().Warn(fmt.Sprintf("Side agent %s failed", taskID), "error", err)
			text = sideAgentErrorText(err)
		}
		live, ok := a.Live(ref.SessionID)
		if ok && live != nil && !sideAgentAbandoned(holder) {
			payload := pyjson.ObjectOf("task_id", taskID)
			if extra != nil {
				payload.Update(extra)
			}
			payload.Set("text", text)
			live.EventStream.Publish(ref.SessionID, payload, eventType, nil)
		}
	}()
	persistWorkCount(a)
	return pyjson.ObjectOf("task_id", taskID)
}
