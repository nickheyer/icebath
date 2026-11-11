package gatewayrun

import (
	"context"
	"errors"
	"fmt"
	"math"

	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/i18n"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/observability"
	"github.com/nickheyer/icebath/pkg/state"
)

// Database holding both the session transcript and its authority's admission ledger, nil without an authority
func (r *GatewayRunner) retryOwnedStore(ctx context.Context, sessionID string) (*state.SessionDB, error) {
	authority, err := gateway.ActiveAuthority(ctx, r)
	if err != nil || authority == nil {
		return nil, err
	}
	db := r.sessionStore.DBForSessionID(ctx, sessionID)
	if db == nil || authority.DB == nil {
		return nil, nil
	}
	if owned, ok := db.(*state.SessionDB); ok && owned == authority.DB {
		return owned, nil
	}
	return nil, nil
}

// Refusal the retry re-send would meet in message handling, answered before the transcript rewrite
func (r *GatewayRunner) retryReadmissionReply(ctx context.Context, source *gateway.SessionSource, resendText string) (string, bool, error) {
	resend := gateway.NewMessageEvent(resendText)
	resend.Source = source
	if notice, blocked := r.hmEstopGate(ctx, resend, source, false); blocked {
		return notice, true, nil
	}
	if r.externalDrainActive {
		return i18n.T(ctx, "gateway.busy.draining_maintenance", "", nil), true, nil
	}
	if reply, refused, err := r.retryAuthorityRefusal(ctx, source); err != nil || refused {
		return reply, refused, err
	}
	sessionKey, err := r.sessionKeyForSource(ctx, source)
	if err != nil {
		return "", false, err
	}
	reply, limited := r.activeSessionLimitMessage(sessionKey)
	return reply, limited, nil
}

// Routed authority's own admission refusal for the re-send, a draining runtime or a full queue
func (r *GatewayRunner) retryAuthorityRefusal(ctx context.Context, source *gateway.SessionSource) (string, bool, error) {
	authority, err := gateway.ActiveAuthority(ctx, r)
	if err != nil || authority == nil {
		return "", false, err
	}
	if err := authority.RequireAdmissionOpen(); err != nil {
		var refused *state.RuntimeStoreError
		if !errors.As(err, &refused) {
			return "", false, err
		}
		return i18n.T(ctx, "gateway.busy.drain_rejected", "", map[string]any{"action": r.statusActionGerund(ctx)}), true, nil
	}
	sessionKey, err := r.sessionKeyForSource(ctx, source)
	if err != nil {
		return "", false, err
	}
	entry := r.sessionStore.LookupBySessionKey(ctx, sessionKey)
	if entry == nil {
		return "", false, nil
	}
	owner, err := authority.LogicalOwner(ctx, entry.SessionID)
	if err != nil {
		return "", false, err
	}
	rows, err := state.ListSessionAdmissions(ctx, authority.DB, owner, false)
	if err != nil {
		return "", false, err
	}
	queued := 0
	for _, row := range rows {
		if status, _ := row.Get("status"); status == "queued" {
			queued++
		}
	}
	if queued >= state.AdmissionQueueMax {
		return gateway.QueueFullReply, true, nil
	}
	return "", false, nil
}

// Model friction signal under the profile home owning the source when the gateway multiplexes profiles
func (r *GatewayRunner) recordModelFriction(ctx context.Context, signal string, source *gateway.SessionSource, sessionID string, turns int) {
	profileHome := ""
	if r.multiplexProfiles() {
		func() {
			defer func() { _ = recover() }()
			if h, err := r.ResolveProfileHomeForSource(ctx, source); err == nil {
				profileHome = h
			}
		}()
	}
	observability.RecordModelFriction(ctx, signal, observability.FrictionOptions{SessionID: sessionID, HomeDir: profileHome, Turns: turns})
}

// Backs up N user turns, soft deleting the truncated rows and echoing the backed up text
func (r *GatewayRunner) handleUndoCommand(ctx context.Context, event *gateway.MessageEvent) (string, error) {
	source := event.Source
	n := 1
	rawArgs := pystr.Strip(event.GetCommandArgs())
	if rawArgs != "" {
		first := pystr.SplitWhitespace(rawArgs, -1)[0]
		parsed, ok := pystr.IntText(first)
		if !ok {
			return i18n.T(ctx, "gateway.undo.invalid_count", "", map[string]any{"arg": first}), nil
		}
		switch {
		case parsed.Sign() <= 0:
			n = 1
		case parsed.IsInt64() && parsed.Int64() <= math.MaxInt:
			n = max(1, int(parsed.Int64()))
		default:
			n = math.MaxInt
		}
	}
	store := r.slashStore()
	entry, err := store.GetOrCreateSession(ctx, source, false, true)
	if err != nil {
		return "", err
	}
	result, err := store.RewindSession(ctx, entry.SessionID, n, false)
	if err != nil {
		return "", err
	}
	if result == nil {
		return i18n.T(ctx, "gateway.undo.nothing", "", nil), nil
	}
	entry.LastPromptTokens = 0
	turns := 1
	if undone, _ := result["turns_undone"].(int); undone != 0 {
		turns = undone
	}
	r.recordModelFriction(ctx, "undo", source, entry.SessionID, turns)
	func() {
		defer func() {
			if rec := recover(); rec != nil {
				runLog().Debug(fmt.Sprintf("undo: cached-agent eviction skipped: %s", pyrepr.Str(rec)))
			}
		}()
		sessionKey, err := r.sessionKeyForSource(ctx, source)
		if err != nil {
			runLog().Debug(fmt.Sprintf("undo: cached-agent eviction skipped: %s", pyrepr.ExceptionMessage(err)))
			return
		}
		r.evictCachedAgent(ctx, sessionKey, false, false)
	}()
	targetText, _ := result["target_text"].(string)
	preview := targetText
	if pystr.Len(targetText) > 200 {
		preview = pystr.Slice(targetText, 0, 200) + "..."
	}
	return i18n.T(ctx, "gateway.undo.removed", "", map[string]any{"turns": result["turns_undone"],
		"count": result["rewound_count"], "preview": preview}), nil
}
