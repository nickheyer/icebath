package gateway

import (
	"context"
	"errors"

	"github.com/nickheyer/icebath/pkg/state"
	anyutil "github.com/nickheyer/icebath/pkg/utils/any"
	jsonutil "github.com/nickheyer/icebath/pkg/utils/json"
)

// Pushes one live session's control snapshot to every viewer as session.control.update
func PublishControl(ctx context.Context, a *SessionAuthority, sessionID string, onlyIfPresent, onlyIfChanged bool) error {
	if a == nil {
		return nil
	}
	live, ok := a.live(sessionID)
	if !ok || live == nil {
		return nil
	}
	ticket := takeControlTicket(live)
	snapshot, err := readControlSnapshot(ctx, a, sessionID, false)
	if err != nil {
		return skippedControlPublish(sessionID, err)
	}
	publishControlSnapshot(live, sessionID, snapshot, ticket, onlyIfPresent, onlyIfChanged)
	return nil
}

// Publish with the snapshot read under the owner scope, dropped when a later ticket painted or the session was remounted
func PublishControlAsync(ctx context.Context, a *SessionAuthority, sessionID string, onlyIfPresent, onlyIfChanged bool) error {
	if a == nil {
		return nil
	}
	live, ok := a.live(sessionID)
	if !ok || live == nil {
		return nil
	}
	ticket := takeControlTicket(live)
	snapshot, err := readControlSnapshot(ctx, a, sessionID, true)
	if err != nil {
		return skippedControlPublish(sessionID, err)
	}
	if current, ok := a.live(sessionID); !ok || current != live {
		return nil
	}
	publishControlSnapshot(live, sessionID, snapshot, ticket, onlyIfPresent, onlyIfChanged)
	return nil
}

// Unreadable row skipped at debug, any other failure raised to the caller
func skippedControlPublish(sessionID string, err error) error {
	var refusal *state.RuntimeStoreError
	if errors.As(err, &refusal) {
		pkgLog().Debug("session.control.update skipped for "+sessionID, "error", err)
		return nil
	}
	return err
}

// Takes the next control publish ticket of a live session
func takeControlTicket(live *LiveSession) int {
	live.ControlMu.Lock()
	defer live.ControlMu.Unlock()
	live.ControlSeq++
	return live.ControlSeq
}

// Control snapshot of one session, read under the owner profile scope when scoped
func readControlSnapshot(ctx context.Context, a *SessionAuthority, sessionID string, scoped bool) (*jsonutil.Object, error) {
	ref := SessionRef{ProfileID: a.ProfileID, SessionID: sessionID}
	if scoped {
		ctx = ownerScope(ctx, a, false)
	}
	return controlSnapshot(ctx, a, ref)
}

// Paints the snapshot unless a newer ticket published or the revision gate holds it back
func publishControlSnapshot(live *LiveSession, sessionID string, snapshot *jsonutil.Object, ticket int, onlyIfPresent, onlyIfChanged bool) {
	live.ControlMu.Lock()
	if live.ControlPublishedSeq > ticket {
		live.ControlMu.Unlock()
		return
	}
	revision := snapshot.ValueOr("revision", nil)
	text := anyutil.String(revision)
	if (onlyIfPresent && !anyutil.IsTrue(revision)) || (onlyIfChanged && text == live.ControlRevision) {
		live.ControlMu.Unlock()
		return
	}
	live.ControlPublishedSeq = ticket
	live.ControlRevision = text
	live.ControlMu.Unlock()
	live.EventStream.Publish(sessionID, jsonutil.ObjectOf("control", snapshot), "session.control.update", nil)
}
