package cron

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"

	"github.com/nickheyer/icebath/pkg/errchain"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/plugins"
	anyutil "github.com/nickheyer/icebath/pkg/utils/any"
	jsonutil "github.com/nickheyer/icebath/pkg/utils/json"
)

// Cron operations of one store a loaded scheduler plugin runs, the claim, the fire and the attempt recovery every provider shares
type schedulerFacade struct{ s *Store }

var _ plugins.HostFacade = (*schedulerFacade)(nil)

var schedulerFacadeOps = map[string]func(ctx context.Context, home string, args []any, kwargs map[string]any) (any, error){
	"recover_interrupted": facadeRecoverInterrupted,
	"claim_fire":          facadeClaimFire,
	"fire_claimed":        facadeFireClaimed,
}

// Every name of the facade is one of its operations
func (f *schedulerFacade) HostAttr(_ context.Context, name string) (any, bool, bool, error) {
	if _, ok := schedulerFacadeOps[name]; !ok {
		return nil, false, false, &plugins.HostBuiltinError{Type: "AttributeError", Message: fmt.Sprintf("the cron operations have no %s", name)}
	}
	return nil, true, false, nil
}

// Runs one operation on the store the scheduler was loaded for
func (f *schedulerFacade) HostCall(ctx context.Context, method string, args []any, kwargs map[string]any) (any, error) {
	run, ok := schedulerFacadeOps[method]
	if !ok {
		return nil, &plugins.HostBuiltinError{Type: "AttributeError", Message: fmt.Sprintf("the cron operations have no %s", method)}
	}
	if kwargs == nil {
		kwargs = map[string]any{}
	}
	return run(withCronStore(home.WithOverride(ctx, f.s.home), f.s), f.s.home, args, kwargs)
}

// Binds one required positional and the allowed keywords the way a Python signature would
func bindFacadeCall(fn string, positional string, args []any, kwargs map[string]any, keywords ...string) (any, map[string]any, error) {
	allowed := map[string]bool{}
	for _, k := range keywords {
		allowed[k] = true
	}
	for k := range kwargs {
		if !allowed[k] && k != positional {
			return nil, nil, &plugins.HostBuiltinError{Type: "TypeError", Message: fmt.Sprintf("%s takes no argument named %s", fn, k)}
		}
	}
	want := 0
	if positional != "" {
		want = 1
	}
	if len(args) > want {
		return nil, nil, &plugins.HostBuiltinError{Type: "TypeError", Message: fmt.Sprintf("%s takes at most %d unnamed arguments, got %d", fn, want, len(args))}
	}
	if positional == "" {
		return nil, kwargs, nil
	}
	if len(args) == 1 {
		if _, dup := kwargs[positional]; dup {
			return nil, nil, &plugins.HostBuiltinError{Type: "TypeError", Message: fmt.Sprintf("%s got the %s argument both by position and by name", fn, positional)}
		}
		return args[0], kwargs, nil
	}
	if v, ok := kwargs[positional]; ok {
		return v, kwargs, nil
	}
	return nil, nil, &plugins.HostBuiltinError{Type: "TypeError", Message: fmt.Sprintf("%s needs the %s argument", fn, positional)}
}

// Profile local attempt recovery of the home, the count of attempts it closed
func facadeRecoverInterrupted(ctx context.Context, _ string, args []any, kwargs map[string]any) (any, error) {
	if _, _, err := bindFacadeCall("recover_interrupted", "", args, kwargs); err != nil {
		return nil, err
	}
	return openExecutionsLedger(cronStoreFrom(ctx)).RecoverInterrupted(ctx)
}

// Claims one fire under the provider name as the ledger source, the claimed snapshot or None
func facadeClaimFire(ctx context.Context, _ string, args []any, kwargs map[string]any) (any, error) {
	raw, kw, err := bindFacadeCall("claim_fire", "job_id", args, kwargs, "source", "force", "manual")
	if err != nil {
		return nil, err
	}
	source, ok := kw["source"]
	if !ok {
		return nil, &plugins.HostBuiltinError{Type: "TypeError", Message: "claim_fire() missing 1 required keyword-only argument: 'source'"}
	}
	claimed, err := claimFireForSource(ctx, anyutil.String(source), anyutil.String(raw), anyutil.IsTrue(kw["force"]), anyutil.IsTrue(kw["manual"]))
	if err != nil || claimed == nil {
		return nil, err
	}
	return claimedJobToWire(claimed)
}

// Runs an exact claim snapshot through the shared run path with the transport the scheduler was started with, None once the run settles
func facadeFireClaimed(ctx context.Context, home string, args []any, kwargs map[string]any) (any, error) {
	raw, kw, err := bindFacadeCall("fire_claimed", "claimed_job", args, kwargs, "cancel_event")
	if err != nil {
		return nil, err
	}
	job, err := claimedJobFromWire(raw)
	if err != nil {
		return nil, err
	}
	var cancel <-chan struct{}
	if ev := kw["cancel_event"]; ev != nil {
		hostEvent, ok := ev.(*plugins.HostEvent)
		if !ok {
			return nil, &plugins.HostBuiltinError{Type: "AttributeError", Message: fmt.Sprintf("cancel_event must be an event the host created, got %s", errchain.Kind(ev))}
		}
		watch, stopWatch := context.WithCancel(ctx)
		defer stopWatch()
		cancel = hostEventChannel(watch, hostEvent)
	}
	adapters, loop := hostedStartTransport(home)
	_, err = runOneJobSeam(ctx, cronStoreFrom(ctx), job, runOneJobOptions{Adapters: adapters, Loop: loop, Cancel: cancel})
	return nil, err
}

// Channel closed once the hosted event is set, the watch ending with the context
func hostEventChannel(ctx context.Context, ev *plugins.HostEvent) <-chan struct{} {
	ch := make(chan struct{})
	go func() {
		if ev.Wait(ctx, -1) && ctx.Err() == nil {
			close(ch)
		}
	}()
	return ch
}

// Wire dict of a claimed snapshot with its scheduled instant and execution id, as claim_fire returns it
func claimedJobToWire(job *Job) (map[string]any, error) {
	o, err := jobToObject(job)
	if err != nil {
		return nil, err
	}
	out, _ := jsonutil.Plain(o).(map[string]any)
	if out == nil {
		return nil, fmt.Errorf("claimed job %s did not encode as an object", job.ID)
	}
	out["_scheduled_instant"] = strOrNil(job.ScheduledInstant)
	out["execution_id"] = strOrNil(job.ExecutionID)
	return out, nil
}

// Job of a claimed snapshot dict a hosted scheduler hands back, its instant and execution id restored
func claimedJobFromWire(raw any) (*Job, error) {
	m, ok := raw.(map[string]any)
	if !ok {
		return nil, &plugins.HostBuiltinError{Type: "TypeError", Message: fmt.Sprintf("claimed_job must be a mapping, got %s", errchain.Kind(raw))}
	}
	rest := make(map[string]any, len(m))
	for k, v := range m {
		if k != "_scheduled_instant" && k != "execution_id" {
			rest[k] = v
		}
	}
	b, err := json.Marshal(rest)
	if err != nil {
		return nil, err
	}
	job := &Job{}
	if err := job.UnmarshalJSON(b); err != nil {
		return nil, err
	}
	if v, ok := m["_scheduled_instant"].(string); ok {
		job.ScheduledInstant = &v
	}
	if v, ok := m["execution_id"].(string); ok {
		job.ExecutionID = &v
	}
	return job, nil
}

// Adapters and loop each home's hosted scheduler was started with, what a fire of that scheduler delivers through
var hostedStarts sync.Map

// Records the transport a hosted scheduler of the home was started with
func rememberHostedStart(home string, o StartOptions) {
	hostedStarts.Store(home, [2]any{o.Adapters, o.Loop})
}

// Adapters and loop the hosted scheduler of a home was started with, none before it started
func hostedStartTransport(home string) (any, any) {
	started, _ := hostedStarts.Load(home)
	pair, _ := started.([2]any)
	return pair[0], pair[1]
}
