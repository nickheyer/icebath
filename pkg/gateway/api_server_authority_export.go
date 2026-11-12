package gateway

import (
	"context"
	"errors"
	"time"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/state"
)

// Saved terminal result of an admission, nil while it is not terminal or nothing was saved
func AdmissionResult(ctx context.Context, db *state.SessionDB, admissionID string) (any, error) {
	return admissionResult(ctx, db, admissionID)
}

// Handle of one session the authority holds
func (a *SessionAuthority) Handle(ctx context.Context, ref SessionRef) (SessionHandle, error) {
	return a.handle(ctx, ref)
}

// Whether the authority holds a live session under the id
func (a *SessionAuthority) HasLiveSession(sessionID string) bool {
	_, ok := a.live(sessionID)
	return ok
}

// Pending prompts of a live session's generation after stale ones settle, false when the session is not live
func (a *SessionAuthority) PendingPrompts(sessionID string, generation *int64) ([]*pyjson.Object, bool) {
	live, ok := a.live(sessionID)
	if !ok {
		return nil, false
	}
	return live.controls.snapshotObjects(sessionID, generation), true
}

// Drops an admission's waiter, resolving it with no result unless it already resolved
func (a *SessionAuthority) ReleaseAdmissionWaiter(admissionID string) {
	a.mu.Lock()
	waiter := a.waiters[admissionID]
	delete(a.waiters, admissionID)
	a.mu.Unlock()
	if waiter != nil && !waiter.isDone() {
		waiter.setResult(nil)
	}
}

// Channel closed once the admission's waiter resolves, the waiter registered when none is held yet
func (a *SessionAuthority) AdmissionWaiterDone(admissionID string) <-chan struct{} {
	a.mu.Lock()
	defer a.mu.Unlock()
	waiter := a.waiters[admissionID]
	if waiter == nil {
		waiter = newAdmissionWaiter()
		a.waiters[admissionID] = waiter
	}
	return waiter.done
}

// Waits until the owner's reply watcher for a delivery published its outcome, the timeout bounding the wait
func (a *SessionAuthority) AwaitBotReplyPublished(ctx context.Context, deliveryID string, timeout *time.Duration) error {
	a.mu.Lock()
	watch := a.botReplyWatchers[deliveryID]
	a.mu.Unlock()
	if watch == nil || watch.task.finished() {
		return nil
	}
	var expired <-chan time.Time
	if timeout != nil {
		timer := time.NewTimer(*timeout)
		defer timer.Stop()
		expired = timer.C
	}
	select {
	case <-watch.task.done:
	case <-expired:
	case <-ctx.Done():
		return ctx.Err()
	}
	return nil
}

// Mints the canonical Bot Chat for a peer as tracked owner work a cancelled caller cannot abandon midway
func CreatePeerBotChat(ctx context.Context, a *SessionAuthority, actor Principal) (*SessionRef, error) {
	if err := a.requireAdmissionOpen(false); err != nil {
		return nil, err
	}
	task := trackMutation(ctx, a, func(taskCtx context.Context) (any, error) {
		return createBotChat(taskCtx, a, actor, "")
	})
	select {
	case <-task.done:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	if task.err != nil {
		return nil, task.err
	}
	ref, _ := task.result.(*SessionRef)
	return ref, nil
}

// Id holding the canonical title when the Bot Chat is a conversation no delivery can adopt, else empty
func DisplaceableBotChat(ctx context.Context, a *SessionAuthority, row *pyjson.Object) (string, error) {
	return displaceableBotChat(ctx, a, row, nil)
}

// Receipt of a Bot Chat delivery, its status and reply following the one retry once that was admitted
func BotResult(ctx context.Context, a *SessionAuthority, record map[string]any) (map[string]any, error) {
	return botResult(ctx, a, record)
}

// Dataclass dict of an admission receipt
func AdmissionReceiptDict(r AdmissionReceipt) *pyjson.Object { return admissionReceiptDict(r) }

// Whether the error is the KeyError Python raises for a missing mapping key
func IsKeyError(err error) bool {
	var keyErr *pyKeyError
	return errors.As(err, &keyErr)
}

// Agent a steer for the session's running generation reaches, the text latched under the stream lock when none is resident
func (a *SessionAuthority) SteerRunningGeneration(ctx context.Context, sessionID string, generation int64, text string) (any, bool, error) {
	live, ok := a.live(sessionID)
	if !ok {
		return nil, false, &pyKeyError{sessionID}
	}
	live.EventStream.Lock.Lock()
	defer live.EventStream.Lock.Unlock()
	if err := a.checkApprovalGeneration(ctx, sessionID, generation); err != nil {
		return nil, false, err
	}
	return a.SteerAgent(ctx, SessionRef{a.ProfileID, sessionID}, generation, &text)
}

// Binding of an API session as write steps, for a door outside the package
func BindAPISteps(ctx context.Context, a *SessionAuthority, sessionID string, hostedDispatch map[string]any, callerKey, walkedFrom string) WriteSteps {
	return bindAPISteps(ctx, a, sessionID, apiBindOptions{HostedDispatch: hostedDispatch, CallerKey: callerKey, WalkedFrom: walkedFrom})
}

// Drives write steps as one tracked task, each write off the caller in admission order
func RunStepsOffLoop(ctx context.Context, a *SessionAuthority, steps WriteSteps) (any, error) {
	return runStepsOffLoop(ctx, a, steps, nil)
}

// Generation and id of the hidden session a hosted room member dispatch lands on
func HostedMemberSession(ctx context.Context, db *state.SessionDB, identity []string) (int, string, error) {
	return hostedMemberSession(ctx, db, identity)
}

// Restores an API session the authority bound earlier
func RestoreAPISession(ctx context.Context, a *SessionAuthority, sessionID string) error {
	return restoreAPISession(ctx, a, sessionID)
}

// Refuses an admitted API turn its session can no longer run
func CheckAPITurn(ctx context.Context, a *SessionAuthority, ref SessionRef, payload *pyjson.Object) error {
	_, err := checkAPITurn(ctx, a, ref, payload)
	return err
}

// Transient content of an admitted API turn with its committed media references restored
func RehydrateAPIImages(ctx context.Context, content any, media any) (any, error) {
	return rehydrateAPIImages(ctx, content, media)
}

// Execution state of one admitted API turn, its adapter, settings, history and content
func PrepareAPIExecution(ctx context.Context, a *SessionAuthority, ref SessionRef, payload *pyjson.Object) (*pyjson.Object, error) {
	return prepareAPIExecution(ctx, a, ref, payload)
}
