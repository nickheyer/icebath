package gateway

import (
	"context"

	"github.com/nickheyer/icebath/pkg/internal/pydict"
)

// Registers the authority as this process's cron owner of its store home while loop lives
func BindCronOwner(loop context.Context, a *SessionAuthority) { bindCronOwner(loop, a) }

// Drops the authority's cron owner binding of its store home when it still holds it
func UnbindCronOwner(a *SessionAuthority) { unbindCronOwner(a) }

// Requests cancellation of every Bot receipt watcher the authority started
func CancelBotReceiptTasks(a *SessionAuthority) {
	a.mu.Lock()
	tasks := make([]*adapterTask, 0, len(a.botReceiptTasks))
	for task := range a.botReceiptTasks {
		tasks = append(tasks, task)
	}
	a.mu.Unlock()
	for _, task := range tasks {
		task.cancel()
	}
}

// Releases every observer still waiting on an admission of a retired authority as runtime_draining
func ReleaseRetiredWaiters(a *SessionAuthority) {
	a.mu.Lock()
	clear(a.nativeWaiters)
	waiters := make([]*admissionWaiter, 0, len(a.waiters))
	for _, waiter := range a.waiters {
		waiters = append(waiters, waiter)
	}
	clear(a.waiters)
	a.mu.Unlock()
	for _, waiter := range waiters {
		if !waiter.isDone() {
			waiter.setException(runtimeRefusal("runtime_draining"))
		}
	}
}

// Queued API turns of the authority's store, recovered on the published API adapter
func RecoverAuthorityAPITurns(ctx context.Context, a *SessionAuthority) error {
	return recoverAuthorityAPITurns(ctx, a)
}

// Adapters by platform the authority's local sessions restore through
func LocalAdapterMap(ctx context.Context, a *SessionAuthority) (*pydict.Dict[Platform, PlatformAdapter], error) {
	return localAdapterMap(ctx, a)
}

// Prepares the hosted room transport of one served authority
func EnsureAuthorityHostedService(ctx context.Context, runner any, a *SessionAuthority) error {
	return ensureHostedService(ctx, runner, a)
}
