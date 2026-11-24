package gatewayrun

import (
	"context"

	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/linktier"
	orderedutil "github.com/nickheyer/icebath/pkg/utils/ordered"
	syncutil "github.com/nickheyer/icebath/pkg/utils/sync"
)

// Running cron scheduler of a gateway with the ticker goroutine that drives it
type cronTicker interface {
	// Stops the provider without letting it choose the gateway exit code
	stopProvider()
	// Whether the ticker goroutine still runs
	alive() bool
	// Housekeeping chores of the scheduler, the misfire sweep and the ticker supervisor
	chores(ctx context.Context, adapters *orderedutil.Map[gateway.Platform, gateway.PlatformAdapter], loop gateway.EventLoop) []housekeepingTask
}

// In flight cron runs split into awaitable, wedged and restart safe for the restart wait
type cronRestartCounts struct {
	Awaitable   int
	Wedged      int
	RestartSafe int
}

// Reply of /blueprint, its text and the seed a matched blueprint runs as the next turn
type blueprintReply struct {
	Text      string
	AgentSeed *string
}

// Cron parts of the gateway runner, bound by the extras tier that carries cron and nil in a build without it
var (
	startCronTicker        func(ctx context.Context, r *GatewayRunner, stop *syncutil.Event) cronTicker
	cronDeliveryDrain      func(ctx context.Context, adapters *orderedutil.Map[gateway.Platform, gateway.PlatformAdapter], loop gateway.EventLoop, r *GatewayRunner) error
	cronQueuePath          func(home string) string
	cronStoreNotices       func(ctx context.Context, r *GatewayRunner, loop gateway.EventLoop)
	cronBusyProbe          func(ctx context.Context, homes []string) (string, error)
	cronRunningJobIDs      func() map[string]bool
	cronRestartWaitCounts  func(ctx context.Context) cronRestartCounts
	cronMarkInterrupted    func(ctx context.Context, reason string) []string
	cronInterruptNotices   func(r *GatewayRunner, ctx context.Context, jobIDs []string) int
	cronJobStatusUnits     func(ctx context.Context, pid int) []map[string]any
	cronSuggestionsCommand func(r *GatewayRunner, ctx context.Context, event *gateway.MessageEvent) (any, error)
	cronBlueprintCommand   func(r *GatewayRunner, ctx context.Context, event *gateway.MessageEvent) blueprintReply
)

// A running or scheduled cron job keeps the gateway up, never in a build without cron
func cronBusy(ctx context.Context, homes []string) (string, error) {
	if cronBusyProbe == nil {
		return "", nil
	}
	return cronBusyProbe(ctx, homes)
}

// Tells the owner of each interrupted cron job that its run died, none sent without cron
func (r *GatewayRunner) notifyInterruptedCronJobs(ctx context.Context, jobIDs []string) int {
	if cronInterruptNotices == nil {
		return 0
	}
	return cronInterruptNotices(r, ctx, jobIDs)
}

// Status units of the running cron jobs, none without cron
func cronJobUnits(ctx context.Context, pid int) []map[string]any {
	if cronJobStatusUnits == nil {
		return nil
	}
	return cronJobStatusUnits(ctx, pid)
}

// Suggestions reply, the line naming the build with cron when this one has none
func (r *GatewayRunner) handleSuggestionsCommand(ctx context.Context, event *gateway.MessageEvent) (any, error) {
	if cronSuggestionsCommand == nil {
		return linktier.Missing("/suggestions", linktier.Extras), nil
	}
	return cronSuggestionsCommand(r, ctx, event)
}

// Blueprint reply, the line naming the build with cron when this one has none
func (r *GatewayRunner) handleBlueprintCommand(ctx context.Context, event *gateway.MessageEvent) blueprintReply {
	if cronBlueprintCommand == nil {
		return blueprintReply{Text: linktier.Missing("/blueprint", linktier.Extras)}
	}
	return cronBlueprintCommand(r, ctx, event)
}
