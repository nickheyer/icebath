package gatewayrun

import (
	"context"

	"github.com/nickheyer/icebath/pkg/agent"
	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
)

// Keeps normal reset and topic resolution, then admits only the heartbeat's original lineage
func resolveHeartbeatOwner(ctx context.Context, r *GatewayRunner, event *gateway.MessageEvent, entry *gateway.SessionEntry) (bool, error) {
	expected := event.HeartbeatSessionID()
	if expected == nil || *expected == "" {
		return true, nil
	}
	resolved := entry.SessionID
	if resolved != *expected {
		tip, err := r.runInExecutorWithContext(ctx, func(execCtx context.Context) (any, error) {
			return r.sessionStore.DBForKey(execCtx, entry.SessionKey).GetCompressionTip(execCtx, *expected)
		})
		if err != nil {
			return false, err
		}
		if tip != resolved {
			return false, nil
		}
	}
	event.SetHeartbeatResolvedSessionID(&resolved)
	return heartbeatOwnerIsCurrent(ctx, r, event, entry.SessionKey), nil
}

// Whether the routing entry still names the session the heartbeat resolved to
func heartbeatOwnerIsCurrent(ctx context.Context, r *GatewayRunner, event *gateway.MessageEvent, sessionKey string) bool {
	expected := event.HeartbeatResolvedSessionID()
	if expected == nil || *expected == "" {
		return true
	}
	current := r.sessionStore.LookupBySessionKey(ctx, sessionKey)
	return current != nil && !current.Suspended && current.SessionID == *expected
}

// Refunds a heartbeat fire whose attempt never entered the agent runner
func settleHeartbeatAttempt(event *gateway.MessageEvent, manager *agent.HeartbeatManager) {
	if event.HeartbeatExecutionStarted() {
		return
	}
	defer func() {
		if rec := recover(); rec != nil {
			runLog().Warn("Failed to refund unexecuted heartbeat", "exc_info", rec)
		}
	}()
	manager.AbandonFire()
}

// Whether an admission row carries a heartbeat automation, native or local
func admissionIsHeartbeat(row *pyjson.Object) bool {
	payload, _ := row.Get("payload")
	object, _ := payload.(*pyjson.Object)
	if object == nil {
		return false
	}
	for _, path := range [][2]string{{"native_text_v1", "automation"}, {"local_automation_v1", ""}} {
		outer, _ := object.Get(path[0])
		holder, _ := outer.(*pyjson.Object)
		if holder == nil {
			continue
		}
		if path[1] != "" {
			inner, _ := holder.Get(path[1])
			holder, _ = inner.(*pyjson.Object)
			if holder == nil {
				continue
			}
		}
		if flag, ok := holder.Get("heartbeat"); ok && pyTruthy(flag) {
			return true
		}
	}
	return false
}

// Commits a due heartbeat behind the canonical FIFO of its route, coalescing overdue ticks
func admitHeartbeat(ctx context.Context, r *GatewayRunner, adapter gateway.PlatformAdapter, source *gateway.SessionSource, sessionID, route string) error {
	authority, err := gateway.ActiveAuthority(ctx, r)
	if err != nil {
		return err
	}
	if authority == nil {
		return nil
	}
	owner, err := authority.LogicalOwner(ctx, sessionID)
	if err != nil {
		return err
	}
	pending, err := authority.DB.ListSessionAdmissions(ctx, owner, true)
	if err != nil {
		return err
	}
	for _, row := range pending {
		if admissionIsHeartbeat(row) {
			return nil
		}
	}
	manager := agent.NewHeartbeatManager(ctx, sessionID)
	prompt, due := manager.DuePrompt(nil)
	if !due {
		return nil
	}
	state := manager.State()
	identity, err := pyjson.Dumps([]any{"heartbeat", sessionID, state.CreatedAt, state.FireCount}, pyjson.Separators(",", ":"))
	if err != nil {
		return err
	}
	event := gateway.NewMessageEvent(prompt)
	event.Source = source
	event.Internal = true
	event.MessageID = &identity
	event.Metadata = map[string]any{"gateway_session_key": route, "gateway_session_id": sessionID}
	event.SetHeartbeatSessionID(&sessionID)
	_, err = func() (receipt gateway.AdmissionReceipt, err error) {
		defer func() {
			if rec := recover(); rec != nil {
				if !event.GatewayAccepted() {
					manager.AbandonFire()
				}
				panic(rec)
			}
		}()
		return authority.AdmitAutomation(ctx, adapter, event, identity, nil, nil)
	}()
	if err != nil && !event.GatewayAccepted() {
		manager.AbandonFire()
	}
	return err
}
