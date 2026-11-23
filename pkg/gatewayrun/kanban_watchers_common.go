//go:build !icebath_core && !icebath_noextras

package gatewayrun

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/kanban"
	"github.com/nickheyer/icebath/pkg/settings"
	"github.com/nickheyer/icebath/pkg/tools"
)

// Result of a blocking call run on a worker goroutine
type threadResult[T any] struct {
	value T
	err   error
}

// Runs fn on a worker goroutine, returning its result or the context error when ctx ends first
func awaitThread[T any](ctx context.Context, fn func() (T, error)) (T, error) {
	done := make(chan threadResult[T], 1)
	go func() {
		v, err := fn()
		done <- threadResult[T]{v, err}
	}()
	select {
	case res := <-done:
		return res.value, res.err
	case <-ctx.Done():
		var zero T
		return zero, ctx.Err()
	}
}

// Runs fn under an empty context so request scoped values such as a delegated child marker stay behind
func runInFreshContext[T any](fn func(fresh context.Context) (T, error)) (T, error) {
	return fn(context.Background())
}

// Offloads process service work to a goroutine under an empty context so request scoped values stay behind
func toThreadProcessService[T any](ctx context.Context, fn func(fresh context.Context) (T, error)) (T, error) {
	return awaitThread(ctx, func() (T, error) { return runInFreshContext(fn) })
}

// Board listing and connection opener of the kanban package
var (
	kanbanListAllBoards = kanban.ListBoards
	kanbanConnect       = kanban.Connect
)

// Live boards, the default board metadata alone when listing fails
func kanbanListBoards() ([]*pyjson.Object, error) {
	boards, err := kanbanListAllBoards(false)
	if err == nil {
		return boards, nil
	}
	slug := kanban.DefaultBoard
	meta, err := kanban.ReadBoardMetadata(&slug)
	if err != nil {
		return nil, err
	}
	return []*pyjson.Object{meta}, nil
}

// Slug of every live board, the default slug for a board without one
func kanbanBoardSlugs() ([]string, error) {
	boards, err := kanbanListBoards()
	if err != nil {
		return nil, err
	}
	out := make([]string, 0, len(boards))
	for _, b := range boards {
		out = append(out, boardMetaSlug(b))
	}
	return out, nil
}

// Truthy slug of a board metadata object, else the default board
func boardMetaSlug(b *pyjson.Object) string {
	if v := objValue(b, "slug"); pyrepr.Truthy(v) {
		return pyrepr.Str(v)
	}
	return kanban.DefaultBoard
}

// Value of key in a JSON object, nil when the object or key is absent
func objValue(o *pyjson.Object, key string) any {
	if o == nil {
		return nil
	}
	v, _ := o.Get(key)
	return v
}

// Optional positive kanban cap of the profile in ctx, nil when unset or below one
func positiveSetting(ctx context.Context, e *settings.Int) *int {
	v, set := e.Lookup(ctx)
	if !set {
		return nil
	}
	if v < 1 {
		pkgLog().Warn(fmt.Sprintf("kanban dispatcher: %s=%d is below 1; ignoring", e.Key, v))
		return nil
	}
	pkgLog().Info(fmt.Sprintf("kanban dispatcher: %s=%d", e.Key, v))
	return &v
}

// Live auto decompose toggle and per tick cap of the profile in ctx
func resolveAutoDecomposeSettings(ctx context.Context) (bool, int) {
	return settings.KanbanAutoDecompose.Get(ctx), max(settings.KanbanAutoDecomposePerTick.Get(ctx), 1)
}

// Retention days for done subscriptions re read on every sweep
func gcRetentionDays(ctx context.Context) int64 {
	return int64(settings.KanbanDoneSubRetentionDays.Get(ctx).Hours() / 24)
}

// False while the global emergency stop holds kanban dispatch
func kanbanDispatchAllowed(ctx context.Context) bool {
	return !tools.CheckPaused(ctx, "kanban", pkgLog())
}

// Lock state names of the singleton dispatcher lock
const (
	singletonLockHeld        = "held"
	singletonLockContended   = "contended"
	singletonLockUnavailable = "unavailable"
)

// Exclusive non blocking advisory lock for the sole dispatcher, the handle only when held
func acquireSingletonLock(lockPath string) (*os.File, string) {
	if err := os.MkdirAll(filepath.Dir(lockPath), 0o777); err != nil {
		return nil, singletonLockUnavailable
	}
	handle, err := os.OpenFile(lockPath, os.O_RDWR|os.O_CREATE|os.O_APPEND, 0o666)
	if err != nil {
		return nil, singletonLockUnavailable
	}
	if !gateway.TryAcquireFileLock(handle) {
		handle.Close()
		return nil, singletonLockContended
	}
	return handle, singletonLockHeld
}

// Releases and closes a lock taken by acquireSingletonLock
func releaseSingletonLock(handle *os.File) {
	if handle == nil {
		return
	}
	func() {
		defer func() { _ = recover() }()
		gateway.ReleaseFileLock(handle)
	}()
	_ = handle.Close()
}
