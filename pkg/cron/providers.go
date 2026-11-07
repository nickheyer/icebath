package cron

import (
	"context"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"unicode/utf8"

	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/plugins"
)

const (
	cronUserNamespace    = "_icebath_user_cron"
	cronSchedulerBaseRef = "cron.scheduler_provider:CronScheduler"
	cronSchedulerCapture = "register_cron_scheduler"
	chronosDescription   = "Chronos \u2014 NAS-mediated managed cron provider for scale-to-zero hosted agents. Delegates the \"wake me at time T\" trigger to Nous infrastructure so an idle gateway can scale to zero and still fire cron jobs. The agent computes each job's next-fire time and asks NAS to arm a one-shot; NAS calls the agent back at fire time over an authenticated webhook. Inert unless cron.provider=chronos."
)

// Bundled cron provider shipped inside the binary, the in process load its register(ctx) performs
type bundledCronProvider struct {
	name        string
	description string
	load        func(s *Store) CronScheduler
}

// Bundled providers in directory order, chronos the only one upstream ships
var bundledCronProviders = []bundledCronProvider{
	{name: "chronos", description: chronosDescription, load: func(s *Store) CronScheduler { return newChronosProvider(s) }},
}

// One discovered provider, a bundled entry or a user plugin directory
type cronProviderDir struct {
	name    string
	dir     string
	bundled *bundledCronProvider
}

// Name, plugin.yaml description and availability probe of one discovered provider
type CronProviderInfo struct {
	Name        string
	Description string
	Available   bool
}

// Whether the first 8192 characters of __init__.py mention the cron scheduler contract
func isCronProviderDir(path string) bool {
	raw, err := os.ReadFile(filepath.Join(path, "__init__.py"))
	if err != nil {
		return false
	}
	source := strings.ToValidUTF8(strings.TrimPrefix(string(raw), "\ufeff"), "\ufffd")
	if utf8.RuneCountInString(source) > 8192 {
		source = string([]rune(source)[:8192])
	}
	return strings.Contains(source, cronSchedulerCapture) || strings.Contains(source, "CronScheduler")
}

// User installed plugin dirs of the profile that look like cron providers
func userCronProviderDirs(ctx context.Context) ([]string, error) {
	userDir, ok := plugins.UserPluginsDir(ctx)
	if !ok {
		return nil, nil
	}
	children, err := plugins.IterPluginDirs(userDir)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, child := range children {
		if isCronProviderDir(child) {
			out = append(out, child)
		}
	}
	return out, nil
}

// Bundled then user providers by name, a bundled name winning a collision
func iterCronProviderDirs(ctx context.Context) ([]cronProviderDir, error) {
	var dirs []cronProviderDir
	seen := map[string]bool{}
	for i := range bundledCronProviders {
		b := &bundledCronProviders[i]
		dirs = append(dirs, cronProviderDir{name: b.name, bundled: b})
		seen[b.name] = true
	}
	user, err := userCronProviderDirs(ctx)
	if err != nil {
		return nil, err
	}
	for _, child := range user {
		if name := filepath.Base(child); !seen[name] {
			dirs = append(dirs, cronProviderDir{name: name, dir: child})
		}
	}
	return dirs, nil
}

// Provider of a name, bundled first then a user plugin dir that holds a cron provider
func findCronProviderDir(ctx context.Context, name string) (cronProviderDir, bool) {
	for i := range bundledCronProviders {
		if bundledCronProviders[i].name == name {
			return cronProviderDir{name: name, bundled: &bundledCronProviders[i]}, true
		}
	}
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

// Every discovered provider with its description and whether it loads and reports available
func DiscoverCronSchedulers(s *Store) ([]CronProviderInfo, error) {
	ctx := providerContext(s)
	dirs, err := iterCronProviderDirs(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]CronProviderInfo, 0, len(dirs))
	for _, d := range dirs {
		description := ""
		if d.bundled != nil {
			description = d.bundled.description
		} else {
			description = plugins.ReadPluginDescription(d.dir)
		}
		out = append(out, CronProviderInfo{Name: d.name, Description: description,
			Available: plugins.ProbeAvailability(func() (any, error) { return loadCronProviderFromDir(ctx, s, d) })})
	}
	return out, nil
}

// Provider instance by name, nil when it is not found or fails to load, a seam the tests record through
var loadCronScheduler = func(s *Store, name string) (CronScheduler, error) {
	ctx := providerContext(s)
	d, ok := findCronProviderDir(ctx, name)
	if !ok {
		s.log.Debug(fmt.Sprintf("Cron provider '%s' not found in bundled or user plugins", name))
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

// Bundled providers load in process, a user provider directory in the plugin host behind the hosted wrapper
func loadCronProviderFromDir(ctx context.Context, s *Store, d cronProviderDir) (CronScheduler, error) {
	if d.bundled != nil {
		return d.bundled.load(s), nil
	}
	host, err := plugins.GetPluginManager(ctx).PluginHost()
	if err != nil {
		return nil, err
	}
	obj, err := host.LoadInstance(ctx, d.dir, cronUserNamespace+"."+d.name, cronSchedulerBaseRef, cronSchedulerCapture, nil, nil)
	if err != nil || obj == nil {
		return nil, err
	}
	return &hostedCronScheduler{obj: obj, s: s, ctx: ctx}, nil
}

// CronScheduler whose every hook runs on the provider object in the plugin host child
type hostedCronScheduler struct {
	obj  *plugins.HostedObject
	s    *Store
	ctx  context.Context
	mu   sync.Mutex
	name string
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

// Result of is_available in the child
func (h *hostedCronScheduler) checkAvailable() (bool, error) {
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

// Eager teardown in the child, a raised exception escaping as a panic
func (h *hostedCronScheduler) Stop() {
	if _, err := h.obj.Call(h.ctx, "stop", nil, nil); err != nil {
		panic(err)
	}
}

// Store mutation notice forwarded to the child
func (h *hostedCronScheduler) OnJobsChanged() error {
	_, err := h.obj.Call(h.ctx, "on_jobs_changed", nil, nil)
	return err
}

// Persisted job dict handed to the child register_job
func (h *hostedCronScheduler) RegisterJob(job *Job) error {
	o, err := jobToObject(job)
	if err != nil {
		return err
	}
	_, err = h.obj.Call(h.ctx, "register_job", []any{pyjson.Plain(o)}, nil)
	return err
}

// Recovered attempt count the child recover_interrupted returns
func (h *hostedCronScheduler) RecoverInterrupted(ctx context.Context) (int, error) {
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
	return 0, fmt.Errorf("recover_interrupted returned %s, not an int", pyrepr.TypeName(res))
}

// Registry convergence forwarded to the child
func (h *hostedCronScheduler) Reconcile() error {
	_, err := h.obj.Call(h.ctx, "reconcile", nil, nil)
	return err
}

// Keyword parameters of the child fire_due signature
func (h *hostedCronScheduler) FireDueKeywords() ([]string, bool) {
	return h.obj.MethodParams("fire_due")
}

// Child fire_due with the transport, force and manual passed only when set as callers gate them
func (h *hostedCronScheduler) FireDue(ctx context.Context, jobID string, o FireOptions) (bool, error) {
	kwargs := map[string]any{"adapters": o.Adapters, "loop": o.Loop}
	if o.Force {
		kwargs["force"] = true
	}
	if o.Manual {
		kwargs["manual"] = true
	}
	res, err := h.obj.Call(h.ctx, "fire_due", []any{jobID}, kwargs)
	if err != nil {
		return false, err
	}
	return pyrepr.Truthy(res), nil
}

// Child claim_fire snapshot, nil when the claim was not acquired
func (h *hostedCronScheduler) ClaimFire(ctx context.Context, jobID string, force, manual bool) (*Job, error) {
	res, err := h.obj.Call(h.ctx, "claim_fire", []any{jobID}, map[string]any{"force": force, "manual": manual})
	if err != nil || res == nil {
		return nil, err
	}
	return claimedJobFromWire(res)
}

// Child fire_claimed of the exact snapshot, a cancel channel crossing as an event set when it closes
func (h *hostedCronScheduler) FireClaimed(ctx context.Context, job *Job, o FireOptions) (bool, error) {
	wire, err := claimedJobToWire(job)
	if err != nil {
		return false, err
	}
	kwargs := map[string]any{"adapters": o.Adapters, "loop": o.Loop, "cancel_event": nil}
	if o.Cancel != nil {
		ev := plugins.NewHostEvent()
		done := make(chan struct{})
		defer close(done)
		go func() {
			select {
			case <-o.Cancel:
				ev.Set()
			case <-done:
			}
		}()
		kwargs["cancel_event"] = ev
	}
	res, err := h.obj.Call(h.ctx, "fire_claimed", []any{wire}, kwargs)
	if err != nil {
		return false, err
	}
	return pyrepr.Truthy(res), nil
}

// Two phase fire contract of the child class, an override of claim_fire or fire_claimed or no fire_due override
func (h *hostedCronScheduler) SupportsSplitFire() bool {
	overrides := func(name string) bool {
		ok, err := h.obj.OverridesBase(h.ctx, name)
		if err != nil {
			panic(err)
		}
		return ok
	}
	if overrides("claim_fire") || overrides("fire_claimed") {
		return true
	}
	return !overrides("fire_due")
}
