package gatewayrun

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pyint"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/kanban"
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

// Optional positive kanban cap, nil when unset, unparsable or below one
func positiveIntSetting(kanbanCfg map[string]any, key string) *int {
	raw, ok := kanbanCfg[key]
	if !ok || raw == nil {
		return nil
	}
	value, err := pyint.Int64(raw)
	if err != nil {
		runLog().Warn(fmt.Sprintf("kanban dispatcher: invalid kanban.%s=%s; ignoring", key, pyrepr.Repr(raw)))
		return nil
	}
	if value < 1 {
		runLog().Warn(fmt.Sprintf("kanban dispatcher: kanban.%s=%s is below 1; ignoring", key, pyrepr.Repr(raw)))
		return nil
	}
	runLog().Info(fmt.Sprintf("kanban dispatcher: %s=%d", key, value))
	v := int(value)
	return &v
}

// Live auto decompose toggle and per tick cap, disabled with cap three when config cannot load
func resolveAutoDecomposeSettings(loadConfig func() (config.Tree, error)) (bool, int, error) {
	cfg, err := loadConfig()
	if err != nil {
		return false, 3, nil
	}
	kcfg, err := kanbanSection(cfg)
	if err != nil {
		return false, 0, err
	}
	perTick := int64(3)
	raw, ok := kcfg["auto_decompose_per_tick"]
	if !ok || !pyrepr.Truthy(raw) {
		raw = int64(3)
	}
	if v, err := pyint.Int64(raw); err == nil {
		perTick = v
	}
	enabled, ok := kcfg["auto_decompose"]
	if !ok {
		enabled = true
	}
	return pyrepr.Truthy(enabled), int(max(perTick, 1)), nil
}

// Kanban mapping of a loaded config, empty when absent and an AttributeError when not a mapping
func kanbanSection(cfg config.Tree) (map[string]any, error) {
	raw, ok := cfg["kanban"]
	if !ok {
		return map[string]any{}, nil
	}
	switch m := raw.(type) {
	case map[string]any:
		return m, nil
	case config.Tree:
		return m, nil
	}
	return nil, fmt.Errorf("'%s' object has no attribute 'get'", config.PyTypeName(raw))
}

// Retention days for done subscriptions re read on every sweep, thirty when config cannot load
func gcRetentionDays(ctx context.Context) int64 {
	cfg, err := config.LoadContext(ctx, home.Dir(ctx), nil)
	if err != nil {
		return 30
	}
	kcfg := map[string]any{}
	if cfg != nil {
		if m, ok := cfg["kanban"].(map[string]any); ok && pyrepr.Truthy(m) {
			kcfg = m
		}
	}
	raw, ok := kcfg["done_sub_retention_days"]
	if !ok {
		raw = int64(30)
	}
	v, err := pyint.Int64(raw)
	if err != nil {
		return 30
	}
	return v
}

// False while the global emergency stop holds kanban dispatch
func kanbanDispatchAllowed(ctx context.Context) bool {
	return !tools.CheckPaused(ctx, "kanban", runLog())
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
