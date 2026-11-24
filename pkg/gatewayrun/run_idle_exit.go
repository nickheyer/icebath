package gatewayrun

import (
	"context"
	"fmt"
	"math"
	"sort"

	"github.com/nickheyer/icebath/pkg/errchain"
	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/settings"
	anyutil "github.com/nickheyer/icebath/pkg/utils/any"
	errorutil "github.com/nickheyer/icebath/pkg/utils/error"
	timeutil "github.com/nickheyer/icebath/pkg/utils/time"
)

const (
	// Idle window of a service started by a `go run` build, whose binary may be gone soon after
	ScratchIdleExitSeconds = 5.0
	IdleExitReason         = "idle_exit"
	IdleExitStopPrefix     = "auto-started gateway idle"
)

// Idle window from gateway.unmanaged_idle_exit_seconds of the launch home, zero disabling it, a few seconds under `go run`
func idleExitSeconds(ctx context.Context) float64 {
	if gateway.GoRunExecutable() != "" {
		return ScratchIdleExitSeconds
	}
	return math.Max(0, settings.GatewayUnmanagedIdleExitSeconds.Get(ctx).Seconds())
}

// Stamps client activity on the idle clock
func noteClientActivity(r *GatewayRunner) {
	r.idleExitLastActivity.Store(math.Float64bits(timeutil.Monotonic()))
}

// Counts one live client connection for the idle check until the returned release runs, stamping activity on both ends
func attachedClient(r *GatewayRunner) (release func()) {
	noteClientActivity(r)
	r.idleExitClients.Add(1)
	return func() {
		r.idleExitClients.Add(-1)
		noteClientActivity(r)
	}
}

// Messaging platforms configured, connected or reconnecting in any profile, LOCAL never holding the gateway
func messagingBusy(r *GatewayRunner) string {
	live := map[string]bool{}
	add := func(p gateway.Platform) {
		if p != gateway.PlatformLocal {
			live[string(p)] = true
		}
	}
	if r.adapters != nil {
		for _, p := range r.adapters.Keys() {
			add(p)
		}
	}
	if r.profileAdapters != nil {
		for _, adapters := range r.profileAdapters.Values() {
			if adapters != nil {
				for _, p := range adapters.Keys() {
					add(p)
				}
			}
		}
	}
	for _, pending := range r.profileFailedPlatforms {
		for p := range pending {
			add(p)
		}
	}
	if r.failedPlatforms != nil {
		for _, p := range r.failedPlatforms.Keys() {
			add(p)
		}
	}
	if r.config != nil {
		for _, p := range r.config.PlatformKeys() {
			if cfg := r.config.Platforms[p]; cfg != nil && cfg.Enabled {
				add(p)
			}
		}
	}
	if len(live) == 0 {
		return ""
	}
	names := make([]string, 0, len(live))
	for n := range live {
		names = append(names, n)
	}
	sort.Strings(names)
	return "messaging " + errchain.Show(names)
}

// Homes of every authority and every cron ticked profile, sorted
func servedHomes(ctx context.Context, r *GatewayRunner) ([]string, error) {
	set := map[string]bool{}
	for _, a := range gateway.AllAuthorities(r) {
		set[a.ProfileID] = true
	}
	ticked, err := cronTickProfileHomes(ctx, r.config)
	if err != nil {
		return nil, err
	}
	for _, sp := range ticked {
		set[sp.Home] = true
	}
	homes := make([]string, 0, len(set))
	for h := range set {
		homes = append(homes, h)
	}
	sort.Strings(homes)
	return homes, nil
}

// Hosted room service whose status reports its current tasks and leased rooms
type hostedStatusReader interface {
	Status(roomID *string) (map[string]any, error)
}

// Pending admissions, live managed workers or hosted room work in any authority
func ledgerBusy(ctx context.Context, r *GatewayRunner) (string, error) {
	for _, a := range gateway.AllAuthorities(r) {
		rows, err := a.DB.ReadAll(ctx, "SELECT 1 FROM session_admissions WHERE status IN ('queued','started') LIMIT 1")
		if err != nil {
			return "", err
		}
		if len(rows) > 0 {
			return "admissions pending in " + a.ProfileID, nil
		}
		if rows, err = a.DB.ReadAll(ctx, "SELECT 1 FROM worker_executions WHERE status != 'terminal' LIMIT 1"); err != nil {
			return "", err
		}
		if len(rows) > 0 {
			return "managed worker live in " + a.ProfileID, nil
		}
		if service, ok := a.HostedRoomService.(hostedStatusReader); ok && service != nil {
			status, err := service.Status(nil)
			if err != nil {
				return "", err
			}
			if anyutil.IsTrue(status["current_tasks"]) || anyutil.IsTrue(status["leased_rooms"]) {
				return "hosted-room work in " + a.ProfileID, nil
			}
		}
	}
	return "", nil
}

// A profile hot serve still building, whose queued admissions recover once it starts
func profileStartBusy(r *GatewayRunner) string {
	if r.sessionAuthorities == nil {
		return ""
	}
	if starting := r.sessionAuthorities.StartingHomes(); len(starting) > 0 {
		return "profile starting in " + starting[0]
	}
	return ""
}

// An active heartbeat, loop or pending handoff in any served home
func storeGatesBusy(ctx context.Context, homes []string) string {
	for _, h := range homes {
		for _, gate := range []struct {
			label string
			fn    func(context.Context, string) bool
		}{{"heartbeat", profileHasActiveHeartbeat}, {"loop", profileHasActiveLoop}, {"handoff", profileHasPendingHandoff}} {
			if gate.fn(ctx, h) {
				return fmt.Sprintf("active %s in %s", gate.label, h)
			}
		}
	}
	return ""
}

// Whether a client holds an unredeemed bootstrap ticket
func idleAttaching(r *GatewayRunner) bool {
	return r.sessionTicketStore != nil && r.sessionTicketStore.Outstanding()
}

// Attached clients, a client mid attach, or client activity inside the idle window
func clientBusy(r *GatewayRunner, window float64) string {
	if clients := r.idleExitClients.Load(); clients != 0 {
		return fmt.Sprintf("%d client(s) attached", clients)
	}
	if idleAttaching(r) {
		return "client attaching"
	}
	quiet := timeutil.Monotonic() - math.Float64frombits(r.idleExitLastActivity.Load())
	if quiet < window {
		return fmt.Sprintf("client activity %.0fs ago", quiet)
	}
	return ""
}

// Why the gateway must stay up, empty when it may exit, failing closed on any probe error
func busyReason(ctx context.Context, r *GatewayRunner, window float64) (reason string) {
	defer func() {
		if rec := recover(); rec != nil {
			pkgLog().Debug("idle-exit probe failed; staying up", "exc_info", rec)
			reason = fmt.Sprintf("idle probe failed (%s)", errorutil.Kind(fmt.Errorf("%v", rec)))
		}
	}()
	failed := func(err error) string {
		pkgLog().Debug("idle-exit probe failed; staying up", "exc_info", err)
		return fmt.Sprintf("idle probe failed (%s)", errorutil.Kind(err))
	}
	if reason := clientBusy(r, window); reason != "" {
		return reason
	}
	if r.draining.Load() || !r.running.Load() {
		return "already stopping"
	}
	if r.activeWorkCount() != 0 {
		return "agent work in flight"
	}
	if r.scaleToZeroHasLiveBackgroundWork() {
		return "background work in flight"
	}
	homes, err := servedHomes(ctx, r)
	if err != nil {
		return failed(err)
	}
	if reason := messagingBusy(r); reason != "" {
		return reason
	}
	if reason, err := ledgerBusy(ctx, r); err != nil || reason != "" {
		if err != nil {
			return failed(err)
		}
		return reason
	}
	if reason := profileStartBusy(r); reason != "" {
		return reason
	}
	if reason, err := cronBusy(ctx, homes); err != nil || reason != "" {
		if err != nil {
			return failed(err)
		}
		return reason
	}
	if reason, err := kanbanBusy(ctx); err != nil || reason != "" {
		if err != nil {
			return failed(err)
		}
		return reason
	}
	return storeGatesBusy(ctx, homes)
}

// Runs a disk touching probe on its own goroutine, returning its reason or the context error
func probeOffLoop(ctx context.Context, fn func() string) (string, error) {
	done := make(chan string, 1)
	go func() { done <- fn() }()
	select {
	case reason := <-done:
		return reason, nil
	case <-ctx.Done():
		return "", ctx.Err()
	}
}

// Withdraws admission at once after the last idle check, then stops the gateway
func beginIdleExit(r *GatewayRunner, window float64) {
	r.draining.Store(true)
	r.sessionRuntimeDescriptor.update(func(descriptor map[string]any) {
		descriptor["state"] = "draining"
		descriptor["drain_reason"] = IdleExitReason
	})
	reason := fmt.Sprintf("%s for %.0fs (gateway.unmanaged_idle_exit_seconds)", IdleExitStopPrefix, window)
	r.exitReason = &reason
	pkgLog().Info(fmt.Sprintf("Exiting: %s; the next client starts a fresh gateway", reason))
	r.idleExitStopTask = spawnGatewayTask(context.Background(), "idle_exit_stop", func(ctx context.Context) (any, error) {
		return nil, r.Stop(ctx, false, false, false)
	})
}

// Ends an auto started gateway after two consecutive idle readings across the configured window
func unmanagedIdleExitWatcher(ctx context.Context, r *GatewayRunner) error {
	window := idleExitSeconds(ctx)
	if window <= 0 {
		pkgLog().Info("Auto-started gateway idle exit disabled (gateway.unmanaged_idle_exit_seconds=0)")
		return nil
	}
	pkgLog().Info(fmt.Sprintf("Auto-started gateway: exits after %.0fs with no clients, adapters, cron/kanban work or admissions (gateway.unmanaged_idle_exit_seconds)", window))
	interval := math.Min(30.0, math.Max(0.5, window/4))
	idleSince := 0.0
	for r.running.Load() && !r.draining.Load() {
		if err := sleepCtx(ctx, interval); err != nil {
			return err
		}
		if window = idleExitSeconds(ctx); window <= 0 {
			idleSince = 0
			continue
		}
		reason, err := probeOffLoop(ctx, func() string { return busyReason(ctx, r, window) })
		if err != nil {
			return err
		}
		if reason != "" {
			idleSince = 0
			continue
		}
		if idleSince == 0 {
			idleSince = timeutil.Monotonic()
		}
		if timeutil.Monotonic()-idleSince < interval && window > interval {
			continue
		}
		if busyReason(ctx, r, window) == "" {
			beginIdleExit(r, window)
			return nil
		}
	}
	return nil
}

// Starts the idle watcher for an idle exit launch no supervisor owns, reporting whether it armed
func armUnmanagedIdleExit(r *GatewayRunner) bool {
	if supervisor := gateway.DetectSupervisor(); supervisor != "manual" {
		pkgLog().Info("Auto-started gateway idle exit not armed: supervised by " + supervisor)
		return false
	}
	noteClientActivity(r)
	r.spawnSupervised(func(ctx context.Context) error { return unmanagedIdleExitWatcher(ctx, r) }, "unmanaged_idle_exit_watcher", false, 0, supervisedHooks{})
	return true
}
