package pluginsdk

import (
	"context"
	"fmt"
)

// Name the cron operations of the profile answer a loaded scheduler plugin under
const cronFacade = "cron"

// Trigger a plugin offers for scheduled jobs, deciding when a due job fires while icebath runs and delivers it
type CronScheduler struct {
	// Value of cron.provider that selects the scheduler
	Name string
	// Whether the scheduler can run here, true when unset, a cheap check that makes no network call
	IsAvailable func(ctx context.Context) bool
	// Begins firing due jobs and returns once stop is set or at once when its work goes on elsewhere, interval the seconds between ticks icebath asks for
	Start func(ctx context.Context, stop *Event, cron Cron, interval float64) error
	// Eager teardown, stop being the primary signal
	Stop func(ctx context.Context) error
	// Runs after the job store changed, for a scheduler that mirrors it elsewhere
	OnJobsChanged func(ctx context.Context) error
	// Registers the trigger of a newly saved job before icebath reports it as scheduled
	RegisterJob func(ctx context.Context, job map[string]any) error
}

// Cron operations of the profile a scheduler fires its jobs through, each fire claimed once across every machine
type Cron struct {
	host   *host
	source string
}

// Claims one fire of a job and returns its snapshot, nil when another claim holds it or the job is gone, manual marking a run now that is not the scheduled occurrence
func (c Cron) ClaimFire(ctx context.Context, jobID string, force, manual bool) (map[string]any, error) {
	value, err := c.host.Facade(ctx, cronFacade, "claim_fire", []any{jobID}, map[string]any{"source": c.source, "force": force, "manual": manual})
	if err != nil || value == nil {
		return nil, err
	}
	return asMap(value), nil
}

// Runs a claimed snapshot through the run and delivery path of icebath, a set cancel event stopping the run
func (c Cron) FireClaimed(ctx context.Context, claimed map[string]any, cancel *Event) error {
	kwargs := map[string]any{}
	if cancel != nil {
		kwargs["cancel_event"] = cancel
	}
	_, err := c.host.Facade(ctx, cronFacade, "fire_claimed", []any{claimed}, kwargs)
	return err
}

// Claims and runs one job now, false when the claim was lost or the job is gone
func (c Cron) FireDue(ctx context.Context, jobID string, force, manual bool) (bool, error) {
	claimed, err := c.ClaimFire(ctx, jobID, force, manual)
	if err != nil || claimed == nil {
		return false, err
	}
	return true, c.FireClaimed(ctx, claimed, nil)
}

// Closes the attempts a stopped process left running and returns how many it closed
func (c Cron) RecoverInterrupted(ctx context.Context) (int, error) {
	value, err := c.host.Facade(ctx, cronFacade, "recover_interrupted", nil, nil)
	n, _ := integer(value)
	return int(n), err
}

// Provider object of a scheduler, one method per hook it sets
func (h *host) cronSchedulerObject(s CronScheduler) *Object {
	methods := map[string]Func{
		"start": func(ctx context.Context, args []any, kwargs map[string]any) (any, error) {
			stop, _ := argAt(args, 0).(*Event)
			if stop == nil {
				return nil, &Error{Type: "TypeError", Message: "start received no stop event"}
			}
			interval := 60.0
			if seconds, ok := floatNumber(kwargs["interval"]); ok && seconds > 0 {
				interval = seconds
			}
			return nil, s.Start(ctx, stop, Cron{host: h, source: s.Name}, interval)
		},
	}
	if s.IsAvailable != nil {
		methods["is_available"] = func(ctx context.Context, _ []any, _ map[string]any) (any, error) { return s.IsAvailable(ctx), nil }
	}
	if s.Stop != nil {
		methods["stop"] = func(ctx context.Context, _ []any, _ map[string]any) (any, error) { return nil, s.Stop(ctx) }
	}
	if s.OnJobsChanged != nil {
		methods["on_jobs_changed"] = func(ctx context.Context, _ []any, _ map[string]any) (any, error) { return nil, s.OnJobsChanged(ctx) }
	}
	if s.RegisterJob != nil {
		methods["register_job"] = func(ctx context.Context, args []any, _ map[string]any) (any, error) {
			return nil, s.RegisterJob(ctx, argMap(args, 0))
		}
	}
	return &Object{Type: "CronScheduler", Attrs: map[string]any{"name": s.Name}, Methods: methods}
}

// Registers a cron scheduler, loaded when cron.provider of a profile names the plugin directory
func (h *host) RegisterCronScheduler(scheduler CronScheduler) (*Handle, error) {
	if scheduler.Name == "" || scheduler.Start == nil {
		return nil, fmt.Errorf("a cron scheduler needs a name and a start function")
	}
	return h.RegisterProvider("cron_scheduler", h.cronSchedulerObject(scheduler))
}
