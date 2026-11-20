package tools

import (
	"context"
	"os"
	"strings"

	"github.com/nickheyer/icebath/pkg/internal/pypath"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
)

type toolEnvKey struct{}

// Binds the session tool environment so context only callers read its session flags
func WithToolEnv(ctx context.Context, env *Env) context.Context {
	return context.WithValue(ctx, toolEnvKey{}, env)
}

// Session tool environment bound to the context, nil when none was bound
func ToolEnvFrom(ctx context.Context) *Env {
	if ctx == nil {
		return nil
	}
	env, _ := ctx.Value(toolEnvKey{}).(*Env)
	return env
}

// Board task this execution owns, ICEBATH_KANBAN_TASK for the dispatcher owned worker and empty otherwise
func OwnedKanbanTask(ctx context.Context) string {
	if IsDelegatedChildContext(ctx) || !isDispatcherOwnedWorkerContext(ToolEnvFrom(ctx)) {
		return ""
	}
	return strings.TrimSpace(os.Getenv("ICEBATH_KANBAN_TASK"))
}

// Whether an explicit kanban board argument must still resolve through the dispatcher env pins
func ExplicitBoardIntentIsPinned(ctx context.Context) bool {
	if IsDelegatedChildContext(ctx) || os.Getenv(delegatedChildEnvMarker) != "" {
		return true
	}
	return pystr.Strip(os.Getenv("ICEBATH_KANBAN_TASK")) != ""
}

// Whether kanban mutations at path are denied, always in a delegate child and under the fence otherwise
func KanbanPathIsFenced(ctx context.Context, path string) bool {
	if IsDelegatedChildContext(ctx) {
		return true
	}
	marker := os.Getenv(delegatedChildEnvMarker)
	if marker == "" {
		return false
	}
	if marker == "1" {
		return true
	}
	target, err := pypath.Resolve(pypath.Expanduser(path))
	if err != nil {
		return true
	}
	if pinned := pystr.Strip(os.Getenv("ICEBATH_KANBAN_DB")); pinned != "" {
		if p, err := pypath.Resolve(pypath.Expanduser(pinned)); err != nil || p == target {
			return true
		}
	}
	root, err := pypath.Resolve(pypath.Expanduser(marker))
	if err != nil {
		return true
	}
	_, under := relativeTo(target, root)
	return under
}
