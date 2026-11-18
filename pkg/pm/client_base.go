package pm

import (
	"context"
	"fmt"
	"strings"
	"sync"

	"github.com/nickheyer/icebath/pkg/errchain"
)

// Missing packages of a closure, refusing a static registry gap or a lazy install the policy disallows
func missingOrRefuse(ctx context.Context, name string) ([]string, error) {
	chain, err := Walk([]string{name})
	if err != nil {
		return nil, err
	}
	var missing []string
	for _, p := range chain {
		installed, err := clientIsInstalled(ctx, p.Base().Name)
		if err != nil {
			return nil, err
		}
		if !installed {
			missing = append(missing, p.Base().Name)
		}
	}
	if len(missing) == 0 {
		return nil, nil
	}
	target, err := clientCurrentTarget()
	if err != nil {
		return nil, err
	}
	for _, gap := range missing {
		p, err := GetPackage(gap)
		if err != nil {
			return nil, err
		}
		if reason := p.MissingReason(target); reason != "" {
			return nil, NewInstallError(gap, fmt.Sprintf("unavailable on %s: %s", target, reason), "none")
		}
	}
	if !LazyInstallsAllowedFor(ctx) {
		return nil, RefuseLazyFor(ctx, name, strings.Join(missing, ", "))
	}
	return missing, nil
}

// Receives a worker's completed receipt, replaced by tests that publish a competing latest receipt
var acceptWorkerReceipt = AcceptWorkerReceipt

// Install module names the client reads at call time, replaced by tests the way Python patches pm.install
var (
	clientIsInstalled   = IsInstalled
	clientCurrentTarget = CurrentTarget
)

// Worker exchanges the facades start, replaced by tests the way Python patches the client _request
var (
	clientWorkerCall func(ctx context.Context, operation string, args policyArgs, run func(ctx context.Context) error) error
)

// Binds the worker exchange seam at init because the operations it runs reach the facades again
func init() { clientWorkerCall = workerCall }

// Engine readiness a lazy state sync checks, set by the package manager and nil in a build without it
var coldEngine func(ctx context.Context) (*InstallError, error)

// Records a lazy sync refused because PM's engine is not ready and returns the error to raise
func refuseColdRuntime(ctx context.Context, cold *InstallError, extras []string) error {
	refusal := cold
	if len(extras) > 0 {
		hints := make([]string, len(extras))
		for i, extra := range extras {
			hints[i] = InstallHint(extra)
		}
		refusal = NewInstallError(cold.Package, fmt.Sprintf("%s while enabling %s", cold.Cause, errchain.Show(extras)),
			"run `"+strings.Join(hints, "`, `")+"`")
	}
	token := BeginReceipt(ctx, "sync")
	RecordRefusal(ctx, "lazy-install", refusal.Error())
	RecordStep(ctx, "dependency-sync", false, refusal.Error())
	FinalizeReceipt(ctx, "failed", 1, token)
	return refusal
}

// Request fields the prerequisite policy reads
type policyArgs struct {
	Name     string
	Explicit bool
	Repair   bool
	Extras   []string
}

// Decides whether an operation runs with lazy installs off, refusing a lazy state sync PM cannot run cold
func workerPolicy(ctx context.Context, spec *workerOperation, args policyArgs) (bool, error) {
	stateSync := spec.bootstrap == bootstrapPolicy
	if spec.bootstrap == bootstrapState {
		p, err := GetPackage(args.Name)
		if err != nil {
			return false, err
		}
		_, stateSync = p.(StatePackage)
	}
	if !stateSync || args.Explicit || args.Repair || LazyInstallsAllowedFor(ctx) {
		return false, nil
	}
	if coldEngine == nil {
		return false, managerAbsent()
	}
	cold, err := coldEngine(ctx)
	if err != nil {
		return false, err
	}
	if cold != nil {
		return false, refuseColdRuntime(ctx, cold, args.Extras)
	}
	return true, nil
}

// Ambient update correlation id of a context, nil when no update is open
func ambientUpdate(ctx context.Context) *string {
	s := scopeOf(ctx)
	s.mu.Lock()
	id := ambientUpdateID(ctx, s)
	s.mu.Unlock()
	if text, ok := id.(string); ok {
		return &text
	}
	return nil
}

// Runs an operation in process behind the worker boundary, its receipt crossing back by correlation
func workerCall(ctx context.Context, operation string, args policyArgs, run func(ctx context.Context) error) error {
	spec, err := lookupOperation(operation)
	if err != nil {
		return err
	}
	lazyDisabled, err := workerPolicy(ctx, spec, args)
	if err != nil {
		return err
	}
	updateID := ambientUpdate(ctx)
	workerCtx := context.WithValue(ctx, receiptScopeKey{}, &receiptScope{})
	if lazyDisabled {
		workerCtx = withLazyInstallsDisabled(workerCtx)
	}
	restore := WorkerContext(workerCtx, updateID)
	failure := run(workerCtx)
	completed := LastCompletedObject(workerCtx)
	restore()
	if err := acceptWorkerReceipt(ctx, completed, updateID); err != nil {
		return err
	}
	return failure
}

// Caller observers whose first failure a facade returns in place of the operation's own error
type observedHooks struct {
	mu  sync.Mutex
	err error
}

// Remembers the first observer failure
func (o *observedHooks) note(err error) error {
	if err != nil {
		o.mu.Lock()
		if o.err == nil {
			o.err = err
		}
		o.mu.Unlock()
	}
	return err
}

// Install progress observer whose failure is remembered
func (o *observedHooks) progress(f InstallProgress) InstallProgress {
	if f == nil {
		return nil
	}
	return func(stage string, done, total int64, label string) error { return o.note(f(stage, done, total, label)) }
}

// Download coverage observer whose failure is remembered
func (o *observedHooks) download(f ProgressFunc) ProgressFunc {
	if f == nil {
		return nil
	}
	return func(done, total int64, ranges []RangeSet) error { return o.note(f(done, total, ranges)) }
}

// The observer's failure when one raised, else the operation's result
func (o *observedHooks) result(err error) error {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.err != nil {
		return o.err
	}
	return err
}

// Ensures a package for an application caller, installed closures answering without crossing the worker boundary
func Ensure(ctx context.Context, name string, opts EnsureOptions) (*Runner, error) {
	p, err := GetPackage(name)
	if err != nil {
		return nil, err
	}
	if _, state := p.(StatePackage); !opts.Explicit && !state {
		missing, err := missingOrRefuse(ctx, name)
		if err != nil {
			return nil, err
		}
		if len(missing) == 0 {
			env, err := envFor(ctx, []string{name}, opts.BaseEnv)
			if err != nil {
				return nil, err
			}
			return &Runner{Name: name, Env: env}, nil
		}
	}
	if opts.PauseEvent != nil && opts.PauseEvent.IsSet() {
		return nil, downloadError(DownloadWasPaused, "install paused")
	}
	observed := &observedHooks{}
	if err := observed.result(clientWorkerCall(ctx, "ensure", policyArgs{Name: name, Explicit: opts.Explicit}, func(workerCtx context.Context) error {
		_, err := ensure(workerCtx, name, EnsureOptions{Explicit: opts.Explicit, SkipVerify: opts.SkipVerify,
			Progress: observed.progress(opts.Progress), PauseEvent: opts.PauseEvent, DownloadProgress: observed.download(opts.DownloadProgress)})
		return err
	})); err != nil {
		return nil, err
	}
	env, err := envFor(ctx, []string{name}, opts.BaseEnv)
	if err != nil {
		return nil, err
	}
	return &Runner{Name: name, Env: env}, nil
}

// VenvIsCurrentOptions name the checkout, the extras to add to the recorded union and the plugin members to probe
type VenvIsCurrentOptions struct {
	ProjectRoot string
	Extras      []string
	Plugins     PluginInput
}
