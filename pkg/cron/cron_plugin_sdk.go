package cron

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/plugins"
)

// Answers the cron plugin SDK calls of a hosted scheduler, handled false for every other op
func ServePluginSDK(ctx context.Context, home, op string, args []any, kwargs map[string]any) (any, bool, error) {
	run, ok := pluginSDKOps[op]
	if !ok {
		return nil, false, nil
	}
	s := openCronStore(home, nil)
	out, err := run(withCronStore(ctx, s), s.home, args, kwargs)
	return out, true, err
}

var pluginSDKOps = map[string]func(ctx context.Context, home string, args []any, kwargs map[string]any) (any, error){
	"cron.recover_interrupted": sdkRecoverInterrupted,
	"cron.claim_fire":          sdkClaimFire,
	"cron.fire_claimed":        sdkFireClaimed,
}

// Binds one required positional and the allowed keywords the way a Python signature would
func bindSDKCall(fn string, positional string, args []any, kwargs map[string]any, keywords ...string) (any, map[string]any, error) {
	allowed := map[string]bool{}
	for _, k := range keywords {
		allowed[k] = true
	}
	for k := range kwargs {
		if !allowed[k] && k != positional {
			return nil, nil, &plugins.HostBuiltinError{Type: "TypeError", Message: fmt.Sprintf("%s() got an unexpected keyword argument %s", fn, pyrepr.StrRepr(k))}
		}
	}
	want := 0
	if positional != "" {
		want = 1
	}
	if len(args) > want {
		return nil, nil, &plugins.HostBuiltinError{Type: "TypeError", Message: fmt.Sprintf("%s() takes %d positional arguments but %d were given", fn, want, len(args))}
	}
	if positional == "" {
		return nil, kwargs, nil
	}
	if len(args) == 1 {
		if _, dup := kwargs[positional]; dup {
			return nil, nil, &plugins.HostBuiltinError{Type: "TypeError", Message: fmt.Sprintf("%s() got multiple values for argument %s", fn, pyrepr.StrRepr(positional))}
		}
		return args[0], kwargs, nil
	}
	if v, ok := kwargs[positional]; ok {
		return v, kwargs, nil
	}
	return nil, nil, &plugins.HostBuiltinError{Type: "TypeError", Message: fmt.Sprintf("%s() missing 1 required positional argument: %s", fn, pyrepr.StrRepr(positional))}
}

// Profile local attempt recovery of the home, the count of attempts it closed
func sdkRecoverInterrupted(ctx context.Context, _ string, args []any, kwargs map[string]any) (any, error) {
	if _, _, err := bindSDKCall("recover_interrupted", "", args, kwargs); err != nil {
		return nil, err
	}
	return openExecutionsLedger(cronStoreFrom(ctx)).RecoverInterrupted(ctx)
}

// Claims one fire under the provider name as the ledger source, the claimed snapshot or None
func sdkClaimFire(ctx context.Context, _ string, args []any, kwargs map[string]any) (any, error) {
	raw, kw, err := bindSDKCall("claim_fire", "job_id", args, kwargs, "source", "force", "manual")
	if err != nil {
		return nil, err
	}
	source, ok := kw["source"]
	if !ok {
		return nil, &plugins.HostBuiltinError{Type: "TypeError", Message: "claim_fire() missing 1 required keyword-only argument: 'source'"}
	}
	claimed, err := claimFireForSource(ctx, pyrepr.Str(source), pyrepr.Str(raw), pyrepr.Truthy(kw["force"]), pyrepr.Truthy(kw["manual"]))
	if err != nil || claimed == nil {
		return nil, err
	}
	return claimedJobToWire(claimed)
}

// Runs an exact claim snapshot through the shared run path, None once the run settles
func sdkFireClaimed(ctx context.Context, home string, args []any, kwargs map[string]any) (any, error) {
	raw, kw, err := bindSDKCall("fire_claimed", "claimed_job", args, kwargs, "adapters", "loop", "cancel_event")
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
			return nil, &plugins.HostBuiltinError{Type: "AttributeError", Message: fmt.Sprintf("%s object has no attribute 'is_set'", pyrepr.StrRepr(pyrepr.TypeName(ev)))}
		}
		watch, stopWatch := context.WithCancel(ctx)
		defer stopWatch()
		cancel = hostEventChannel(watch, hostEvent)
	}
	adapters, loop := hostedFireTransport(home, kw["adapters"], kw["loop"])
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
	out, _ := pyjson.Plain(o).(map[string]any)
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
		return nil, &plugins.HostBuiltinError{Type: "TypeError", Message: fmt.Sprintf("claimed_job must be a dict, not %s", pyrepr.TypeName(raw))}
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

// Adapters and loop each home's hosted scheduler was started with, what their opaque echoes stand for
var hostedStarts sync.Map

// Records the transport a hosted scheduler of the home was started with
func rememberHostedStart(home string, o StartOptions) {
	hostedStarts.Store(home, [2]any{o.Adapters, o.Loop})
}

// Transport for a hosted fire, an opaque echo of the start transport mapped back to the live value
func hostedFireTransport(home string, adapters, loop any) (any, any) {
	started, _ := hostedStarts.Load(home)
	pair, _ := started.([2]any)
	if _, echo := adapters.(*plugins.Opaque); echo {
		adapters = pair[0]
	}
	if _, echo := loop.(*plugins.Opaque); echo {
		loop = pair[1]
	}
	return adapters, loop
}
