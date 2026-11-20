package cron

import (
	"context"
	"fmt"
	"math"
	"path/filepath"
	"sync"

	"github.com/nickheyer/icebath/pkg/errchain"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/plugins"
)

const (
	cronSchedulerBaseRef = "cron.scheduler_provider:CronScheduler"
	cronSchedulerCapture = "register_cron_scheduler"
	// Name a loaded scheduler plugin reaches the cron operations of its profile under
	cronFacadeName = "cron"
)

// One discovered provider, a user plugin directory
type cronProviderDir struct {
	name string
	dir  string
}

// Whether a user plugin directory holds a wasm or program plugin that offers a cron scheduler
func isCronProviderDir(path string) bool {
	return plugins.GuestProvides(path, plugins.CategoryCronScheduler)
}

// Provider of a name, a user plugin dir that holds a cron provider
func findCronProviderDir(ctx context.Context, name string) (cronProviderDir, bool) {
	if userDir, ok := plugins.UserPluginsDir(ctx); ok {
		if user := filepath.Join(userDir, name); isDir(user) && isCronProviderDir(user) {
			return cronProviderDir{name: name, dir: user}, true
		}
	}
	return cronProviderDir{}, false
}

// Context the provider loader and a hosted provider run under, scoped to the store home
func providerContext(s *Store) context.Context {
	return withCronStore(home.WithOverride(context.Background(), s.home), s)
}

// Provider instance by name, nil when it is not found or fails to load, a seam the tests record through
var loadCronScheduler = func(s *Store, name string) (CronScheduler, error) {
	ctx := providerContext(s)
	d, ok := findCronProviderDir(ctx, name)
	if !ok {
		s.log.Debug(fmt.Sprintf("Cron provider '%s' not found in user plugins", name))
		return nil, nil
	}
	instance, _ := plugins.LoadNamed(name, d.dir, func(string) (any, error) {
		p, err := loadCronProviderFromDir(ctx, s, d)
		if p == nil {
			return nil, err
		}
		return p, err
	}, "Cron provider", "provider").(CronScheduler)
	return instance, nil
}

// A user provider directory loads in the guest of its plugin behind the hosted wrapper, the cron operations of the store handed to it
func loadCronProviderFromDir(ctx context.Context, s *Store, d cronProviderDir) (CronScheduler, error) {
	obj, err := plugins.LoadCategoryInstance(ctx, d.dir, cronSchedulerBaseRef, cronSchedulerCapture,
		map[string]plugins.HostFacade{cronFacadeName: &schedulerFacade{s: s}})
	if err != nil || obj == nil {
		return nil, err
	}
	return &hostedCronScheduler{obj: obj, s: s, ctx: ctx}, nil
}

// CronScheduler whose hooks run on the provider object in the guest of its plugin, a hook the plugin leaves out keeping the base behaviour
type hostedCronScheduler struct {
	obj  *plugins.HostedObject
	s    *Store
	ctx  context.Context
	mu   sync.Mutex
	name string
	// Stop event of the last start, held so the plugin can still read it after it was set
	stop *plugins.HostEvent
}

// Hooks resolve_cron_scheduler reads with their raised exceptions, a hosted provider being the one that raises
type checkedScheduler interface {
	checkAvailable() (bool, error)
	checkName() (string, error)
}

// Name property read from the child, remembered once read
func (h *hostedCronScheduler) checkName() (string, error) {
	v, err := h.obj.Get(h.ctx, "name")
	if err != nil {
		return "", err
	}
	name := pyrepr.Str(v)
	h.mu.Lock()
	h.name = name
	h.mu.Unlock()
	return name, nil
}

// Result of is_available in the plugin, true for a plugin that declares none
func (h *hostedCronScheduler) checkAvailable() (bool, error) {
	if !h.obj.HasMethod("is_available") {
		return true, nil
	}
	res, err := h.obj.Call(h.ctx, "is_available", nil, nil)
	if err != nil {
		return false, err
	}
	return pyrepr.Truthy(res), nil
}

// Remembered name, read from the child on first use, its raised exception escaping as a panic
func (h *hostedCronScheduler) Name() string {
	h.mu.Lock()
	name := h.name
	h.mu.Unlock()
	if name != "" {
		return name
	}
	name, err := h.checkName()
	if err != nil {
		panic(err)
	}
	return name
}

// Whether the provider can run here, a raised exception escaping as a panic
func (h *hostedCronScheduler) IsAvailable() bool {
	ok, err := h.checkAvailable()
	if err != nil {
		panic(err)
	}
	return ok
}

// Records the start transport, then runs the hosted start with an event set once stop closes
func (h *hostedCronScheduler) Start(ctx context.Context, stop <-chan struct{}, o StartOptions) {
	rememberHostedStart(h.s.home, o)
	ev := plugins.NewHostEvent()
	h.mu.Lock()
	h.stop = ev
	h.mu.Unlock()
	go func() {
		<-stop
		ev.Set()
	}()
	kwargs := map[string]any{"adapters": o.Adapters, "loop": o.Loop}
	if o.Interval != nil {
		if *o.Interval == math.Trunc(*o.Interval) {
			kwargs["interval"] = int64(*o.Interval)
		} else {
			kwargs["interval"] = *o.Interval
		}
	}
	if _, err := h.obj.Call(h.ctx, "start", []any{ev}, kwargs); err != nil {
		panic(err)
	}
}

// Eager teardown in the plugin when it declares one, a raised exception escaping as a panic
func (h *hostedCronScheduler) Stop() {
	if !h.obj.HasMethod("stop") {
		return
	}
	if _, err := h.obj.Call(h.ctx, "stop", nil, nil); err != nil {
		panic(err)
	}
}

// Store mutation notice forwarded to a plugin that takes it
func (h *hostedCronScheduler) OnJobsChanged() error {
	if !h.obj.HasMethod("on_jobs_changed") {
		return nil
	}
	_, err := h.obj.Call(h.ctx, "on_jobs_changed", nil, nil)
	return err
}

// Persisted job dict handed to the register_job of a plugin that declares one
func (h *hostedCronScheduler) RegisterJob(job *Job) error {
	if !h.obj.HasMethod("register_job") {
		return nil
	}
	o, err := jobToObject(job)
	if err != nil {
		return err
	}
	_, err = h.obj.Call(h.ctx, "register_job", []any{pyjson.Plain(o)}, nil)
	return err
}

// Recovered attempt count the plugin recover_interrupted returns, the executions ledger recovery for a plugin that declares none
func (h *hostedCronScheduler) RecoverInterrupted(ctx context.Context) (int, error) {
	if !h.obj.HasMethod("recover_interrupted") {
		return recoverInterruptedExecutions(ctx, cronStoreFrom(ctx))
	}
	res, err := h.obj.Call(h.ctx, "recover_interrupted", nil, nil)
	if err != nil {
		return 0, err
	}
	switch v := res.(type) {
	case int64:
		return int(v), nil
	case int:
		return v, nil
	case float64:
		return int(v), nil
	case bool:
		if v {
			return 1, nil
		}
		return 0, nil
	}
	return 0, fmt.Errorf("recover_interrupted returned %s, expected a whole number", errchain.Kind(res))
}

// Plugin claim_fire snapshot, nil when the claim was not acquired, the base claim for a plugin that declares none
func (h *hostedCronScheduler) ClaimFire(ctx context.Context, jobID string, force, manual bool) (*Job, error) {
	if !h.obj.HasMethod("claim_fire") {
		return baseClaimFire(ctx, h, jobID, force, manual)
	}
	res, err := h.obj.Call(h.ctx, "claim_fire", []any{jobID}, map[string]any{"force": force, "manual": manual})
	if err != nil || res == nil {
		return nil, err
	}
	return claimedJobFromWire(res)
}

// Plugin fire_claimed of the exact snapshot, the shared run path for a plugin that declares none
func (h *hostedCronScheduler) FireClaimed(ctx context.Context, job *Job, o FireOptions) (bool, error) {
	if !h.obj.HasMethod("fire_claimed") {
		return baseFireClaimed(ctx, h, job, o)
	}
	wire, err := claimedJobToWire(job)
	if err != nil {
		return false, err
	}
	kwargs := map[string]any{"adapters": o.Adapters, "loop": o.Loop, "cancel_event": nil}
	res, err := h.obj.Call(h.ctx, "fire_claimed", []any{wire}, kwargs)
	if err != nil {
		return false, err
	}
	return pyrepr.Truthy(res), nil
}
