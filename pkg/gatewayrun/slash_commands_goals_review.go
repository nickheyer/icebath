package gatewayrun

import (
	"context"
	"errors"
	"sync"

	"github.com/nickheyer/icebath/pkg/agent"
	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/i18n"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/tools"
)

// Session key and idle cached agent for refine and review, or the reply refusing a missing or running agent
func (r *GatewayRunner) idleCachedAgentOrError(ctx context.Context, event *gateway.MessageEvent, verb string) (string, *agent.AIAgent, string, error) {
	quickKey := ""
	if event.Source != nil {
		key, err := r.sessionKeyForSource(ctx, event.Source)
		if err != nil {
			return "", nil, "", err
		}
		quickKey = key
	}
	if quickKey == "" {
		return "", nil, i18n.T(ctx, "gateway."+verb+".unavailable", "", nil), nil
	}
	if r.runningAgents().Contains(quickKey) {
		return quickKey, nil, i18n.T(ctx, "gateway.shared.agent_running_retry_later", "", map[string]any{"command": verb}), nil
	}
	cached := r.cachedAgentFor(quickKey, false)
	if cached == nil {
		return quickKey, nil, i18n.T(ctx, "gateway."+verb+".nothing_yet", "", nil), nil
	}
	return quickKey, cached, "", nil
}

// Conversation snapshot of a cached agent, empty without one
func agentSnapshot(a *agent.AIAgent) []map[string]any {
	if a == nil {
		return nil
	}
	return agent.SessionMessageDicts(a.SessionMessages())
}

// Runs the memory and skill review fork on demand, over the stored transcript when no agent holds it
func (r *GatewayRunner) handleRefineCommand(ctx context.Context, event *gateway.MessageEvent) (string, error) {
	args := pystr.Strip(event.GetCommandArgs())
	quickKey, cached, errorText, err := r.idleCachedAgentOrError(ctx, event, "refine")
	if err != nil {
		return "", err
	}
	snapshot := agentSnapshot(cached)
	sessionID := ""
	if len(snapshot) == 0 && quickKey != "" && !r.runningAgents().Contains(quickKey) {
		local, err := r.refineLocalSession(ctx, event.Source, args)
		if err != nil {
			return "", err
		}
		if local != nil {
			return *local, nil
		}
		sessionID, snapshot, err = r.storedTranscript(ctx, quickKey)
		if err != nil {
			var readErr *gateway.TranscriptReadError
			if errors.As(err, &readErr) {
				return historyUnreadable(ctx), nil
			}
			return "", err
		}
		if len(snapshot) > 0 {
			errorText = ""
		}
	}
	if errorText != "" {
		return errorText, nil
	}
	if len(snapshot) == 0 {
		return i18n.T(ctx, "gateway.refine.empty", "", nil), nil
	}
	var focus *string
	if args != "" {
		focus = &args
	}
	startErr := catchColdReview(func() error {
		if cached == nil {
			return r.startColdRefine(ctx, event.Source, quickKey, sessionID, snapshot, args)
		}
		cached.SpawnBackgroundReview(ctx, snapshot, true, cached.ValidToolNames()["skill_manage"], focus, true)
		return nil
	})
	if startErr != nil {
		return i18n.T(ctx, "gateway.refine.start_failed", "", map[string]any{"error": pyrepr.ExceptionMessage(startErr)}), nil
	}
	tail := ""
	if args != "" {
		tail = i18n.T(ctx, "gateway.refine.focus_suffix", "", map[string]any{"focus": args})
	}
	return i18n.T(ctx, "gateway.refine.started", "", map[string]any{"focus_suffix": tail}), nil
}

// Refusal the review engine raises for a request it cannot dispatch, its text the reply
type reviewRefusal struct{ err error }

func (e *reviewRefusal) Error() string { return e.err.Error() }

// Dispatches the reviewer subagent with the approval session key bound, from a cold parent when no agent is cached
func (r *GatewayRunner) handleReviewCommand(ctx context.Context, event *gateway.MessageEvent) (string, error) {
	args := pystr.Strip(event.GetCommandArgs())
	quickKey, cached, errorText, err := r.idleCachedAgentOrError(ctx, event, "review")
	if err != nil {
		return "", err
	}
	snapshot := agentSnapshot(cached)
	cold := false
	coldSessionID := ""
	if cached == nil && quickKey != "" && !r.runningAgents().Contains(quickKey) {
		coldSessionID, snapshot, err = r.coldReviewHistory(ctx, event.Source)
		if err != nil {
			var readErr *gateway.TranscriptReadError
			if errors.As(err, &readErr) {
				return historyUnreadable(ctx), nil
			}
			return "", err
		}
		cold = true
		errorText = ""
	}
	if errorText != "" {
		return errorText, nil
	}
	startReview := func(reviewCtx context.Context, env *tools.Env, parent *agent.AIAgent) (*pyjson.Object, error) {
		result, err := tools.StartReview(reviewCtx, env, parent, snapshot, args)
		if err != nil {
			return nil, &reviewRefusal{err}
		}
		return result, nil
	}
	dispatch := func(workerCtx context.Context) (any, error) {
		var result *pyjson.Object
		err := catchColdReview(func() error {
			base := tools.ToolEnvFrom(workerCtx)
			if base == nil {
				base = &tools.Env{Home: home.Dir(workerCtx), Values: &sync.Map{}}
			}
			env := tools.CopySessionContext(base)
			tools.SetCurrentSessionKey(env, quickKey)
			defer tools.ResetCurrentSessionKey(env)
			scoped := tools.WithToolEnv(workerCtx, env)
			if !cold {
				var err error
				result, err = startReview(scoped, env, cached)
				return err
			}
			return r.coldReviewParent(scoped, event.Source, quickKey, coldSessionID, func(parentCtx context.Context, parent *agent.AIAgent) error {
				var err error
				result, err = startReview(parentCtx, env, parent)
				return err
			})
		})
		return result, err
	}
	raw, err := r.runInExecutorWithContext(ctx, dispatch)
	if err != nil {
		var refusal *reviewRefusal
		if errors.As(err, &refusal) {
			return refusal.Error(), nil
		}
		return i18n.T(ctx, "gateway.review.start_failed", "", map[string]any{"error": pyrepr.ExceptionMessage(err)}), nil
	}
	result, _ := raw.(*pyjson.Object)
	return tools.FormatDispatchNote(result, args), nil
}
