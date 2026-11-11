package gateway

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"

	"github.com/google/uuid"
	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/provider"
	"github.com/nickheyer/icebath/pkg/state"
)

// Principal and level of a reasoning command that arrived while a turn ran, a nil effort resetting the level
type pendingReasoning struct {
	principal string
	effort    any
}

// Hex of a fresh uuid4
func uuidHex() string { return strings.ReplaceAll(uuid.NewString(), "-", "") }

// Owner's fenced reasoning mutation of a reasoning command on the row's current fences
func reasoningMutation(ref SessionRef, row *pyjson.Object, effort any) (mutationParams, error) {
	return fencedMutation(row, ref.SessionID, "reasoning-command:"+uuidHex(), "reasoning", pyjson.ObjectOf("effort", effort))
}

// Commits a reasoning command that arrived while a turn ran, kept for the next boundary while a worker runs
func applyPendingReasoning(ctx context.Context, a *SessionAuthority, ref SessionRef, live *LiveSession) {
	a.mu.Lock()
	pending := live.PendingReasoning
	live.PendingReasoning = nil
	a.mu.Unlock()
	if pending == nil {
		return
	}
	err := func() error {
		row, err := a.DB.GetSession(ctx, ref.SessionID)
		if err != nil {
			return err
		}
		params, err := reasoningMutation(ref, row, pending.effort)
		if err != nil {
			return err
		}
		_, err = writeMutation(ctx, a, ref, params, nil, pending.principal, nil, nil)
		return err
	}()
	if err == nil {
		return
	}
	var refusal *state.RuntimeStoreError
	guard := state.IsMutationGuardRefusal(err)
	if !guard && !errors.As(err, &refusal) {
		slog.Error(fmt.Sprintf("Reasoning command on %s failed to commit: %s", ref.SessionID, pyExceptionRepr(err)))
		return
	}
	reason := "session_busy"
	if refusal != nil && !guard {
		reason = refusal.Reason
	}
	if reason == "session_busy" || reason == "unknown_execution" {
		a.mu.Lock()
		if live.PendingReasoning == nil {
			live.PendingReasoning = pending
		}
		a.mu.Unlock()
		return
	}
	slog.Warn(fmt.Sprintf("Reasoning command on %s not committed: %s", ref.SessionID, reason))
}

// Authority and live session owning the local session a source belongs to
func localAuthorityLive(runner any, source *SessionSource) (*SessionAuthority, *LiveSession) {
	if source == nil || source.Platform != PlatformLocal {
		return nil, nil
	}
	for _, a := range allAuthorities(runner) {
		live, ok := a.live(source.ChatID)
		if ok && live.Source != nil && live.Source.Platform == PlatformLocal {
			return a, live
		}
	}
	return nil, nil
}

// Live session of the local session a source belongs to, nil for a messaging chat
func localLive(runner any, source *SessionSource) *LiveSession {
	_, live := localAuthorityLive(runner, source)
	return live
}

// Frozen policy of a local session, nil for a messaging chat
func localPolicy(ctx context.Context, runner AuthorityRunner, source *SessionSource) (*LocalSessionPolicy, error) {
	if localLive(runner, source) == nil {
		return nil, nil
	}
	return policyForSource(ctx, runner, source)
}

// Local live session whose route is the session key, nil when none is
func localLiveForRoute(runner any, sessionKey string) *LiveSession {
	if sessionKey == "" {
		return nil
	}
	for _, a := range allAuthorities(runner) {
		a.mu.Lock()
		lives := make([]*LiveSession, 0, len(a.Sessions))
		for _, live := range a.Sessions {
			lives = append(lives, live)
		}
		a.mu.Unlock()
		for _, live := range lives {
			if live.Route == sessionKey && live.Source != nil && live.Source.Platform == PlatformLocal {
				return live
			}
		}
	}
	return nil
}

// Defers a typed model command's resolved target as the session's canonical model mutation for the next boundary
func deferLocalModelPick(ctx context.Context, a *SessionAuthority, live *LiveSession, result provider.ModelSwitchResult, effort string) (*pyjson.Object, error) {
	sid := live.Source.ChatID
	receipt, err := a.DB.LocalReceipt(ctx, sid)
	if err != nil {
		return nil, err
	}
	principal, err := pyItem(receipt, "principal_id")
	if err != nil {
		return nil, err
	}
	model := result.ResolvedViaAlias
	if model == "" {
		model = result.NewModel
	}
	payload := pyjson.ObjectOf("model", model)
	if result.TargetProvider != "" {
		payload.Set("provider", result.TargetProvider)
	}
	if effort != "" {
		payload.Set("reasoning", effort)
	}
	principalID, _ := principal.(string)
	return deferPick(ctx, a, principalID, SessionRef{ProfileID: a.ProfileID, SessionID: sid},
		deferredPickRequest{RequestID: "typed-model:" + uuidHex(), Payload: payload})
}

// Commits a reasoning command on a local session as the owner's fenced mutation, held for the turn boundary while busy
func commitReasoningCommand(ctx context.Context, a *SessionAuthority, actor Principal, ref SessionRef, value string, persistGlobal bool) error {
	var effort any
	switch {
	case value == "reset" && !persistGlobal:
		effort = nil
	case value != "reset" && config.ParseReasoningEffort(value) != nil:
		effort = value
	default:
		return nil
	}
	live, ok := a.live(ref.SessionID)
	if !ok {
		return &pyKeyError{ref.SessionID}
	}
	if live.Source.Platform != PlatformLocal {
		return nil
	}
	for attempt := 0; attempt < fenceAttempts; attempt++ {
		row, err := a.DB.GetSession(ctx, ref.SessionID)
		if err != nil {
			return err
		}
		if row == nil {
			return runtimeRefusal("not_found")
		}
		params, err := reasoningMutation(ref, row, effort)
		if err != nil {
			return err
		}
		request := pyjson.ObjectOf("session_id", params.SessionID, "request_id", params.RequestID, "expected_revision", params.ExpectedRevision,
			"expected_generation", rowValue(row, "runtime_generation"), "operation", params.Operation, "payload", params.Payload)
		_, err = mutateSession(ctx, a, actor, ref, request)
		if err == nil {
			return nil
		}
		reason, refused := mutationRefusalReason(err)
		if !refused {
			return err
		}
		if reason == "revision_conflict" {
			continue
		}
		if reason != "session_busy" && reason != "unknown_execution" {
			return err
		}
		break
	}
	a.mu.Lock()
	live.PendingReasoning = &pendingReasoning{principal: actor.Subject, effort: effort}
	a.mu.Unlock()
	return nil
}

// Fence reads a reasoning command retries before it waits for the turn boundary
const fenceAttempts = 3
