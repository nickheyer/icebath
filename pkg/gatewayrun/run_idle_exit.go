package gatewayrun

import (
	"context"
	"database/sql"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/nickheyer/icebath/pkg/cron"
	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/internal/pyint"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pythread"
	"github.com/nickheyer/icebath/pkg/kanban"
	"github.com/nickheyer/icebath/pkg/pm"
)

const (
	DefaultIdleExitSeconds = 600.0
	IdleExitReason         = "idle_exit"
	IdleExitStopPrefix     = "auto-started gateway idle"
)

// Kanban statuses the embedded dispatcher acts on without a human
var kanbanDueStatuses = []string{"scheduled", "ready", "running", "review"}

// Idle window from gateway.unmanaged_idle_exit_seconds of the launch home, zero disabling and junk keeping the default
func idleExitSeconds(ctx context.Context) float64 {
	section, _ := gateway.LoadGatewayConfigMap(ctx, "")["gateway"].(map[string]any)
	value, present := section["unmanaged_idle_exit_seconds"]
	if section == nil || !present || value == nil {
		return DefaultIdleExitSeconds
	}
	seconds, ok := pyFloat(value)
	if !ok {
		runLog().Warn(fmt.Sprintf("gateway.unmanaged_idle_exit_seconds=%s is not a number; using %.0f", pyrepr.Repr(value), DefaultIdleExitSeconds))
		return DefaultIdleExitSeconds
	}
	return math.Max(0, seconds)
}

// Python float of a config value, ok false where float raises
func pyFloat(v any) (float64, bool) {
	switch x := v.(type) {
	case bool:
		if x {
			return 1, true
		}
		return 0, true
	case int:
		return float64(x), true
	case int64:
		return float64(x), true
	case float64:
		return x, true
	case string:
		f, err := pyint.ParseFloat(x)
		return f, err == nil
	}
	return 0, false
}

// Stamps client activity on the idle clock
func noteClientActivity(r *GatewayRunner) {
	r.idleExitLastActivity.Store(math.Float64bits(pythread.Monotonic()))
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
		for p, cfg := range r.config.Platforms {
			if cfg != nil && cfg.Enabled {
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
	return "messaging " + pyrepr.Repr(names)
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

// A running cron job or a runnable non terminal job stored in any served home
func cronBusy(ctx context.Context, homes []string) (string, error) {
	if len(cron.GetRunningJobIDs()) > 0 {
		return "cron job running", nil
	}
	for _, h := range homes {
		if _, err := os.Stat(filepath.Join(h, "cron", "jobs.json")); err != nil {
			continue
		}
		jobs, err := cron.LoadJobsForHome(ctx, h)
		if err != nil {
			return "", err
		}
		for _, job := range jobs {
			if cron.IsJobRunnable(job) && !cron.IsTerminalJob(job) {
				return "cron jobs scheduled in " + h, nil
			}
		}
	}
	return "", nil
}

// A kanban task on any unarchived board in a status the dispatcher acts on
func kanbanBusy(ctx context.Context) (string, error) {
	boards, err := kanban.ListBoards(false)
	if err != nil {
		return "", err
	}
	marks := strings.TrimSuffix(strings.Repeat("?,", len(kanbanDueStatuses)), ",")
	args := make([]any, len(kanbanDueStatuses))
	for i, s := range kanbanDueStatuses {
		args[i] = s
	}
	for _, meta := range boards {
		slugValue, _ := meta.Get("slug")
		var slug *string
		if s, ok := slugValue.(string); ok {
			slug = &s
		}
		path, err := kanban.KanbanDBPath(ctx, slug)
		if err != nil {
			return "", err
		}
		if _, err := os.Stat(path); err != nil {
			continue
		}
		found, err := func() (bool, error) {
			conn, err := sql.Open("sqlite", "file:"+filepath.ToSlash(path)+"?mode=ro&_pragma=busy_timeout(2000)")
			if err != nil {
				return false, err
			}
			defer conn.Close()
			rows, err := conn.QueryContext(ctx, "SELECT 1 FROM tasks WHERE status IN ("+marks+") LIMIT 1", args...)
			if err != nil {
				return false, err
			}
			defer rows.Close()
			return rows.Next(), rows.Err()
		}()
		if err != nil {
			return "", err
		}
		if found {
			return "kanban work on board " + pyrepr.Str(slugValue), nil
		}
	}
	return "", nil
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
			if pyrepr.Truthy(status["current_tasks"]) || pyrepr.Truthy(status["leased_rooms"]) {
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
	quiet := pythread.Monotonic() - math.Float64frombits(r.idleExitLastActivity.Load())
	if quiet < window {
		return fmt.Sprintf("client activity %.0fs ago", quiet)
	}
	return ""
}

// Why the gateway must stay up, empty when it may exit, failing closed on any probe error
func busyReason(ctx context.Context, r *GatewayRunner, window float64, countClients bool) (reason string) {
	defer func() {
		if rec := recover(); rec != nil {
			runLog().Debug("idle-exit probe failed; staying up", "exc_info", rec)
			reason = fmt.Sprintf("idle probe failed (%s)", pyrepr.ExceptionName(fmt.Errorf("%v", rec)))
		}
	}()
	failed := func(err error) string {
		runLog().Debug("idle-exit probe failed; staying up", "exc_info", err)
		return fmt.Sprintf("idle probe failed (%s)", pyrepr.ExceptionName(err))
	}
	if countClients {
		if reason := clientBusy(r, window); reason != "" {
			return reason
		}
	}
	if !countClients && idleAttaching(r) {
		return "client attaching"
	}
	if r.draining || !r.running {
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
func beginIdleExit(r *GatewayRunner, window float64, restart string) {
	r.draining = true
	if r.sessionRuntimeDescriptor == nil {
		r.sessionRuntimeDescriptor = map[string]any{}
	}
	r.sessionRuntimeDescriptor["state"] = "draining"
	r.sessionRuntimeDescriptor["capabilities"] = []any{}
	r.sessionRuntimeDescriptor["drain_reason"] = IdleExitReason
	reason := fmt.Sprintf("%s for %.0fs (gateway.unmanaged_idle_exit_seconds)", IdleExitStopPrefix, window)
	if restart != "" {
		reason = fmt.Sprintf("%s; restarting to load %s", IdleExitStopPrefix, restart)
	}
	r.exitReason = &reason
	runLog().Info(fmt.Sprintf("Exiting: %s; the next client starts a fresh gateway", reason))
	r.idleExitStopTask = spawnGatewayTask(context.Background(), "idle_exit_stop", func(ctx context.Context) (any, error) {
		return nil, r.Stop(ctx, false, false, false)
	})
}

// Sleeps the interval in one second slices, waking early once a restart is pending
func sleepUnlessRestartPending(ctx context.Context, interval float64) error {
	deadline := pythread.Monotonic() + interval
	for {
		slice := math.Min(1.0, math.Max(0, deadline-pythread.Monotonic()))
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(time.Duration(slice * float64(time.Second))):
		}
		if pm.RestartPending() != "" || pythread.Monotonic() >= deadline {
			return nil
		}
	}
}

// Retires the gateway for a pending dependency restart once nothing but clients holds it
func retiredForRestart(ctx context.Context, r *GatewayRunner, window float64) (bool, error) {
	restart := pm.RestartPending()
	if restart == "" {
		return false, nil
	}
	reason, err := probeOffLoop(ctx, func() string { return busyReason(ctx, r, window, false) })
	if err != nil {
		return false, err
	}
	if reason != "" || busyReason(ctx, r, window, false) != "" {
		return false, nil
	}
	beginIdleExit(r, window, restart)
	return true, nil
}

// Ends an auto started gateway after two consecutive idle readings across the configured window
func unmanagedIdleExitWatcher(ctx context.Context, r *GatewayRunner) error {
	window := idleExitSeconds(ctx)
	if window <= 0 {
		runLog().Info("Auto-started gateway idle exit disabled (gateway.unmanaged_idle_exit_seconds=0)")
		return nil
	}
	runLog().Info(fmt.Sprintf("Auto-started gateway: exits after %.0fs with no clients, adapters, cron/kanban work or admissions (gateway.unmanaged_idle_exit_seconds)", window))
	interval := math.Min(30.0, math.Max(0.5, window/4))
	idleSince := 0.0
	for r.running && !r.draining {
		if err := sleepUnlessRestartPending(ctx, interval); err != nil {
			return err
		}
		if window = idleExitSeconds(ctx); window <= 0 {
			idleSince = 0
			continue
		}
		retired, err := retiredForRestart(ctx, r, window)
		if err != nil || retired {
			return err
		}
		reason, err := probeOffLoop(ctx, func() string { return busyReason(ctx, r, window, true) })
		if err != nil {
			return err
		}
		if reason != "" {
			idleSince = 0
			continue
		}
		if idleSince == 0 {
			idleSince = pythread.Monotonic()
		}
		if pythread.Monotonic()-idleSince < interval && window > interval {
			continue
		}
		if busyReason(ctx, r, window, true) == "" {
			beginIdleExit(r, window, "")
			return nil
		}
	}
	return nil
}

// Starts the idle watcher for an idle exit launch no supervisor owns, reporting whether it armed
func armUnmanagedIdleExit(r *GatewayRunner) bool {
	if supervisor := gateway.DetectSupervisor(); supervisor != "manual" {
		runLog().Info("Auto-started gateway idle exit not armed: supervised by " + supervisor)
		return false
	}
	noteClientActivity(r)
	r.spawnSupervised(func(ctx context.Context) error { return unmanagedIdleExitWatcher(ctx, r) }, "unmanaged_idle_exit_watcher", false, 0, supervisedHooks{})
	return true
}
