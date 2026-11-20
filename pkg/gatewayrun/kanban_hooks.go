package gatewayrun

import (
	"context"

	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/linktier"
)

// Supervised watcher of the runner by its task name
type runnerWatcher struct {
	name string
	run  func(context.Context) error
}

// Kanban parts of the runner, bound by the extras tier that carries kanban and nil in a build without it
var (
	kanbanCommand   func(r *GatewayRunner, ctx context.Context, event *gateway.MessageEvent) (string, error)
	kanbanWatchers  func(r *GatewayRunner) []runnerWatcher
	kanbanBusyProbe func(ctx context.Context) (string, error)
)

// Kanban slash reply, the line naming the build with kanban when this one has none
func (r *GatewayRunner) handleKanbanCommand(ctx context.Context, event *gateway.MessageEvent) (string, error) {
	if kanbanCommand == nil {
		return linktier.Missing("/kanban", linktier.Extras), nil
	}
	return kanbanCommand(r, ctx, event)
}

// A kanban task the dispatcher acts on keeps the gateway up, never in a build without kanban
func kanbanBusy(ctx context.Context) (string, error) {
	if kanbanBusyProbe == nil {
		return "", nil
	}
	return kanbanBusyProbe(ctx)
}
