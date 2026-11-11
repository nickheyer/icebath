package cron

import (
	"context"

	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
)

// Repeat budget from an integer or forever and once words, nil meaning forever
func NormalizeRepeatValue(repeat any) (*int, error) { return normalizeRepeatValue(repeat) }

// Whether fire_due can safely receive the force keyword
func ProviderSupportsForceFire(p CronScheduler) bool { return providerSupportsForceFire(p) }

// Whether the provider fire_due takes the named keyword
func ProviderFireDueAccepts(p CronScheduler, name string) bool {
	return providerFireDueAccepts(p, name)
}

// Runs the provider jobs changed hook, nothing for a provider without one
func SchedulerOnJobsChanged(p CronScheduler) error { return schedulerOnJobsChanged(p) }

// Seconds since the ticker of a home last iterated, false when never or unknown
func TickerHeartbeatAge(h string) (float64, bool) { return openCronStore(h, nil).TickerHeartbeatAge() }

// Terminal attempt states of the executions ledger
var TerminalExecutionStates = map[string]bool{"completed": true, "failed": true, "unknown": true, "cancelled": true, "missed": true}

// Newest attempts of a job in the executions ledger of the given home, at most limit rows
func ListExecutionsIn(ctx context.Context, h, jobID string, limit int) ([]map[string]any, error) {
	return openExecutionsLedger(openCronStore(h, nil)).List(ctx, &jobID, limit, nil)
}

// Stops the live run of a job of the given home, leaving its schedule untouched
func CancelJobRun(h, jobID, reason string) (*pyjson.Object, error) {
	return openCronStore(h, nil).cancelJobRun(jobID, reason)
}

// Records on a job of the given home that a scheduled fire could not be handed to the runner
func NoteFireForwardFailure(ctx context.Context, h, jobID, detail string) (bool, error) {
	return openCronStore(h, nil).noteFireForwardFailure(ctx, jobID, detail)
}

// Delivery targets of the configured gateway platforms for the home in ctx
func CronDeliveryTargets(ctx context.Context) []*pyjson.Object {
	return openCronStore(home.Dir(ctx), nil).cronDeliveryTargets(ctx)
}

// Built-ins then the active profile plugin blueprints
func ListBlueprints(ctx context.Context) []AutomationBlueprint { return listBlueprints(ctx) }

// Built-in or namespaced plugin blueprint of the active profile, nil when unknown
func GetBlueprint(ctx context.Context, key string) *AutomationBlueprint {
	return getBlueprint(ctx, key)
}

// Form schema entry of one blueprint
func BlueprintCatalogEntry(b *AutomationBlueprint) *pyjson.Object { return blueprintCatalogEntry(b) }

// Validated slot values as create_job keyword arguments
func FillBlueprint(b *AutomationBlueprint, values map[string]any, origin any) (*pyjson.Object, error) {
	return fillBlueprint(b, values, origin)
}
