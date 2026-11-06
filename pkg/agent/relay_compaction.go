package agent

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync/atomic"

	"github.com/nickheyer/icebath/pkg/tools"
)

// Data schema every compaction mark carries
var CompactionDataSchema = map[string]any{"name": CompactionSchemaName, "version": CompactionSchemaVersion}

// Breaker tripped by one stalled publish so a wedged pipeline costs one bounded timeout per process
var relayCompactionStalled atomic.Bool

type relayCompactionTarget struct {
	host    *tools.RelayRuntime
	session *tools.RelaySession
	handle  any
}

// Host, session and parent handle of the session's live turn
func relayCompactionTurnTarget(ctx context.Context, sessionID string) (*relayCompactionTarget, error) {
	turn := tools.ActiveRelayTurn(ctx, &sessionID)
	if turn == nil {
		return nil, nil
	}
	host := turn.Lease.LiveRuntime()
	if host == nil {
		return nil, nil
	}
	session := turn.Lease.Session
	handle := turn.Handle
	if handle == nil {
		if session == nil {
			return nil, errors.New("'NoneType' object has no attribute 'handle'")
		}
		handle = session.Handle
	}
	return &relayCompactionTarget{host: host, session: session, handle: handle}, nil
}

// Host, session and handle of the session's own scope on the active profile's host
func relayCompactionSessionTarget(ctx context.Context, sessionID string) *relayCompactionTarget {
	host, ok := tools.RelayHosts.ForProfile(ctx, "", false).(*tools.RelayRuntime)
	if !ok || host == nil {
		return nil
	}
	session := host.GetSession(sessionID)
	if session == nil || session.Handle == nil {
		return nil
	}
	return &relayCompactionTarget{host: host, session: session, handle: session.Handle}
}

// Target of the session's live turn, then its session scope, then the live turn the agent started in
func relayCompactionResolveTarget(ctx context.Context, sessionID, turnSessionID string) (*relayCompactionTarget, error) {
	if !tools.RelayInstrumentationEnabled(ctx) {
		return nil, nil
	}
	target, err := relayCompactionTurnTarget(ctx, sessionID)
	if err != nil {
		return nil, err
	}
	if target == nil {
		target = relayCompactionSessionTarget(ctx, sessionID)
	}
	if target == nil && turnSessionID != "" {
		return relayCompactionTurnTarget(ctx, turnSessionID)
	}
	return target, nil
}

// Whether a mark for the session has a live Relay scope to receive it
func CompactionTargetAvailable(ctx context.Context, sessionID, turnSessionID string) (bool, error) {
	if sessionID == "" {
		return false, nil
	}
	target, err := relayCompactionResolveTarget(ctx, sessionID, turnSessionID)
	return target != nil, err
}

// Emits one compaction mark under the owning scope, false when no Relay scope owns the session
func EmitCompactionMark(ctx context.Context, sessionID, name string, data map[string]any, turnSessionID string) (bool, error) {
	var target *relayCompactionTarget
	if sessionID != "" {
		var err error
		if target, err = relayCompactionResolveTarget(ctx, sessionID, turnSessionID); err != nil {
			return false, err
		}
	}
	if target == nil {
		shown := sessionID
		if shown == "" {
			shown = "none"
		}
		slog.DebugContext(ctx, fmt.Sprintf("no live Relay session for %s; %s mark not recorded", shown, name))
		return false, nil
	}
	if relayCompactionStalled.Load() {
		return false, nil
	}
	host := target.host
	schema := make(map[string]any, len(CompactionDataSchema))
	for k, v := range CompactionDataSchema {
		schema[k] = v
	}
	timeout := tools.RelayScopeOpTimeout
	_, err := host.RunInSession(ctx, target.session, func(c context.Context) (any, error) {
		if host.Relay.Scope == nil || host.Relay.Scope.Event == nil {
			return nil, tools.RelayAttributeError("NemoRelayScope", "event")
		}
		return host.Relay.Scope.Event(c, name, map[string]any{
			"handle": target.handle, "data": data, "data_schema": schema,
			"metadata": tools.RelayRuntimeMetadata(host.RuntimeID, nil),
		})
	}, false, &timeout)
	var te *tools.RelayTimeoutError
	if errors.As(err, &te) {
		relayCompactionStalled.Store(true)
		return false, err
	}
	if err != nil {
		return false, err
	}
	return true, nil
}
