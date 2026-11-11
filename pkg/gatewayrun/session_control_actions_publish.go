package gatewayrun

import (
	"context"

	"github.com/nickheyer/icebath/pkg/agent"
	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
)

// Goal, loop and heartbeat snapshot of the compression tip of a conversation
func controlSnapshot(ctx context.Context, a *gateway.SessionAuthority, ref gateway.SessionRef) (*pyjson.Object, error) {
	target, err := gateway.PhysicalTarget(ctx, a, ref)
	if err != nil {
		return nil, err
	}
	tip, err := a.DB.GetCompressionTip(ctx, target)
	if err != nil {
		return nil, err
	}
	if tip != "" {
		target = tip
	}
	return agent.ControlSnapshot(ctx, a.DB.GetMeta, target)
}

// Snapshot of one session, read under the owner profile scope when scoped
func readControlSnapshot(ctx context.Context, a *gateway.SessionAuthority, sessionID string, scoped bool) (*pyjson.Object, error) {
	ref := gateway.SessionRef{ProfileID: a.ProfileID, SessionID: sessionID}
	if scoped {
		ctx = gateway.OwnerScope(ctx, a, false)
	}
	return controlSnapshot(ctx, a, ref)
}

// Takes the next control publish ticket of a live session
func takeControlTicket(live *gateway.LiveSession) int {
	live.ControlMu.Lock()
	defer live.ControlMu.Unlock()
	live.ControlSeq++
	return live.ControlSeq
}

// Pushes one live session control snapshot to every viewer as session.control.update
func controlPublish(ctx context.Context, a *gateway.SessionAuthority, sessionID string, onlyIfPresent, onlyIfChanged bool) {
	if a == nil {
		return
	}
	live, ok := a.Live(sessionID)
	if !ok || live == nil {
		return
	}
	ticket := takeControlTicket(live)
	snapshot, err := readControlSnapshot(ctx, a, sessionID, false)
	if err != nil {
		runLog().Debug("session.control.update skipped for "+sessionID, "error", err)
		return
	}
	publishControlSnapshot(live, sessionID, snapshot, ticket, onlyIfPresent, onlyIfChanged)
}

// Publish with the snapshot read off the caller, dropped when a later ticket already painted or the session was remounted
func controlPublishAsync(ctx context.Context, a *gateway.SessionAuthority, sessionID string, onlyIfPresent, onlyIfChanged bool) {
	if a == nil {
		return
	}
	live, ok := a.Live(sessionID)
	if !ok || live == nil {
		return
	}
	ticket := takeControlTicket(live)
	snapshot, err := readControlSnapshot(ctx, a, sessionID, true)
	if err != nil {
		runLog().Debug("session.control.update skipped for "+sessionID, "error", err)
		return
	}
	if current, ok := a.Live(sessionID); !ok || current != live {
		return
	}
	publishControlSnapshot(live, sessionID, snapshot, ticket, onlyIfPresent, onlyIfChanged)
}

// Paints the snapshot unless a newer ticket published or the revision gate holds it back
func publishControlSnapshot(live *gateway.LiveSession, sessionID string, snapshot *pyjson.Object, ticket int, onlyIfPresent, onlyIfChanged bool) {
	live.ControlMu.Lock()
	if live.ControlPublishedSeq > ticket {
		live.ControlMu.Unlock()
		return
	}
	revision := pyrepr.Str(snapshot.GetOr("revision", ""))
	if (onlyIfPresent && revision == "") || (onlyIfChanged && revision == live.ControlRevision) {
		live.ControlMu.Unlock()
		return
	}
	live.ControlPublishedSeq = ticket
	live.ControlRevision = revision
	live.ControlMu.Unlock()
	live.EventStream.Publish(sessionID, pyjson.ObjectOf("control", snapshot), "session.control.update", nil)
}
