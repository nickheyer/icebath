package gateway

import (
	"context"
	"testing"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/state"
)

// Runner double of the authority tests, never draining and resolving no adapter unless one is set
type authorityRunnerDouble struct {
	draining bool
	store    *SessionStore
	adapter  PlatformAdapter
}

func (r *authorityRunnerDouble) SessionRuntimeDescriptor() map[string]any { return map[string]any{} }
func (r *authorityRunnerDouble) Draining() bool                           { return r.draining }
func (r *authorityRunnerDouble) Running() bool                            { return true }
func (r *authorityRunnerDouble) SessionStore() *SessionStore              { return r.store }
func (r *authorityRunnerDouble) MultiplexProfiles() bool                  { return false }
func (r *authorityRunnerDouble) AdapterForSource(context.Context, *SessionSource) PlatformAdapter {
	return r.adapter
}

var (
	testActor = Principal{Subject: "human", ProfileID: "owned", Capabilities: map[string]bool{"session:submit": true, "session:control": true}, TransportID: "cli"}
	testRef   = SessionRef{ProfileID: "owned", SessionID: "s"}
)

// Authority over a fresh store with one live session s on a route, owned by the human principal
func newTestAuthority(t *testing.T, platform Platform) (context.Context, *state.SessionDB, *SessionAuthority) {
	t.Helper()
	ctx, db := newTestStateDB(t)
	if err := db.CreateSession(state.NewSession{ID: "s", Source: "test"}); err != nil {
		t.Fatal(err)
	}
	epoch, err := db.BeginRuntimeEpoch(ctx, "current")
	if err != nil {
		t.Fatal(err)
	}
	a := NewSessionAuthority(&authorityRunnerDouble{}, "owned", "current", db, epoch)
	owner := "human"
	a.Sessions["s"] = NewLiveSession(&SessionSource{Platform: platform, UserID: &owner}, "route")
	return ctx, db, a
}

// Replaces the drain scheduler so admitted input stays queued and no turn runs
func holdDrains(t *testing.T) {
	t.Helper()
	saved := scheduleSession
	scheduleSession = func(context.Context, *SessionAuthority, SessionRef) error { return nil }
	t.Cleanup(func() { scheduleSession = saved })
}

// Submits a text input for the test session under the request id
func submitText(ctx context.Context, a *SessionAuthority, requestID string) (AdmissionReceipt, error) {
	return a.Submit(ctx, testActor, Submission{RequestID: requestID, Ref: testRef, Payload: map[string]any{"text": requestID}, Intent: "queue"}, SubmitOptions{})
}

// Submits a text input carrying staged attachments
func submitAttachments(ctx context.Context, a *SessionAuthority, requestID string, attachments []any) (AdmissionReceipt, error) {
	return a.Submit(ctx, testActor, Submission{RequestID: requestID, Ref: testRef,
		Payload: map[string]any{"text": "hi", "attachments": attachments}, Intent: "queue"}, SubmitOptions{})
}

// Wire attachment of a staged image
func stagedAttachment(t *testing.T, ctx context.Context, name string, size int) map[string]any {
	t.Helper()
	return map[string]any{"path": stagedImage(t, ctx, name, size), "mime": "image/png"}
}

// First committed attachment reference of an admission
func firstAttachmentRef(t *testing.T, ctx context.Context, db *state.SessionDB, admissionID string) *pyjson.Object {
	t.Helper()
	row, err := db.GetSessionAdmission(ctx, admissionID)
	if err != nil || row == nil {
		t.Fatalf("admission %s: %v %v", admissionID, row, err)
	}
	return nativeAttachmentMedia(t, row)[0].(*pyjson.Object)
}
