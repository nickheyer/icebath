//go:build !icebath_core && !icebath_noextras

package gatewayrun

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/cron"
	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/i18n"
	"github.com/nickheyer/icebath/pkg/internal/pydict"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/internal/pythread"
)

var _ cron.GatewayRunner = (*GatewayRunner)(nil)

// Binds the cron scheduler, deliveries, notices, reports and slash commands of the runner
func init() {
	startCronTicker, cronDeliveryDrain, cronQueuePath = startGatewayCronTicker, drainRestartSafeCronDeliveries, cron.QueuePath
	cronStoreNotices, cronBusyProbe, cronRunningJobIDs = installCronStoreNotices, cronJobsBusy, cron.GetRunningJobIDs
	cronRestartWaitCounts = func(ctx context.Context) cronRestartCounts {
		counts := cron.GetRestartWaitCronCounts(ctx)
		return cronRestartCounts{Awaitable: counts.Awaitable, Wedged: counts.Wedged, RestartSafe: counts.RestartSafe}
	}
	cronMarkInterrupted, cronInterruptNotices = cron.MarkRunningJobsInterrupted, (*GatewayRunner).noticeInterruptedCronJobs
	cronJobStatusUnits = describeCronJobUnits
	cronSuggestionsCommand, cronBlueprintCommand = (*GatewayRunner).runSuggestionsCommand, (*GatewayRunner).runBlueprintCommand
}

// Cron scheduler a gateway started with the ticker goroutine that runs it
type gatewayCronTicker struct {
	provider cron.CronScheduler
	thread   *cron.SupervisedTickerThread
}

func (t *gatewayCronTicker) stopProvider() { stopCronProvider(t.provider) }

func (t *gatewayCronTicker) alive() bool { return t.thread.IsAlive() }

// Misfire catch-up sweep every five ticks and the ticker supervisor every tick
func (t *gatewayCronTicker) chores(ctx context.Context, adapters *pydict.Dict[gateway.Platform, gateway.PlatformAdapter], loop gateway.EventLoop) []housekeepingTask {
	var adaptersArg, loopArg any
	if adapters != nil {
		adaptersArg = adapters
	}
	if loop != nil {
		loopArg = loop
	}
	return []housekeepingTask{
		{5, "Misfire catch-up sweep", func() error { return housekeepingMisfireCatchUp(ctx, t.provider, adaptersArg, loopArg) }},
		{1, "Cron ticker supervisor", func() error { t.thread.RestartIfDead(); return nil }},
	}
}

// A running cron job or a runnable non terminal job stored in any served home
func cronJobsBusy(ctx context.Context, homes []string) (string, error) {
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

// Runs suggestions through the shared handler with the event source as job origin
func (r *GatewayRunner) runSuggestionsCommand(ctx context.Context, event *gateway.MessageEvent) (any, error) {
	reply, err := cron.HandleSuggestionsCommand(ctx, pystr.Strip(event.GetCommandArgs()), commandOriginForSource(event.Source), "gateway")
	if err != nil {
		pkgLog().Debug(fmt.Sprintf("suggestions command failed: %s", err))
		return i18n.T(ctx, "gateway.suggestions.failed", "", map[string]any{"error": err.Error()}), nil
	}
	return reply, nil
}

// Runs blueprint through the shared handler with the event source as job origin
func (r *GatewayRunner) runBlueprintCommand(ctx context.Context, event *gateway.MessageEvent) blueprintReply {
	result, err := cron.HandleBlueprintCommand(ctx, home.Dir(ctx), pystr.Strip(event.GetCommandArgs()), commandOriginForSource(event.Source), "gateway")
	if err != nil {
		pkgLog().Debug(fmt.Sprintf("blueprint command failed: %s", err))
		return blueprintReply{Text: i18n.T(ctx, "gateway.blueprint.failed", "", map[string]any{"error": err.Error()})}
	}
	return blueprintReply{Text: result.Text, AgentSeed: result.AgentSeed}
}

// Tells the owner of each just interrupted cron job that its run died, returning the notices sent
func (r *GatewayRunner) noticeInterruptedCronJobs(ctx context.Context, jobIDs []string) int {
	if len(jobIDs) == 0 {
		return 0
	}
	action := r.shutdownActionText(ctx)
	notified := map[cronNoticeKey]bool{}
	for _, jobID := range jobIDs {
		job, err := cron.GetJob(ctx, home.Dir(ctx), jobID)
		if err != nil {
			pkgLog().Debug(fmt.Sprintf("Cron interrupt targets unresolved for %s: %s", jobID, pyrepr.ExceptionMessage(err)))
			continue
		}
		if job == nil {
			continue
		}
		targets := cron.ResolveDeliveryTargets(ctx, job, true)
		jobName := job.Name
		if jobName == "" {
			jobName = jobID
		}
		msg := i18n.T(ctx, "gateway.shutdown.cron_interrupted", "", map[string]any{"job": jobName, "action": action})
		for _, target := range targets {
			platform, err := gateway.ParsePlatform(ctx, pystr.Lower(target.Platform))
			if err != nil {
				continue
			}
			adapter, ok := r.adapters.Get(platform)
			if !ok || adapter == nil || !r.restartNotificationAllowed(platform) {
				continue
			}
			chatID := target.ChatID
			var threadID *string
			if target.ThreadID != "" {
				thread := target.ThreadID
				threadID = &thread
			}
			dedupKey := cronNoticeKey{jobID, noticeTargetKeyOf(string(platform), chatID, optAny(threadID))}
			if notified[dedupKey] {
				continue
			}
			func() {
				defer func() {
					if rec := recover(); rec != nil {
						pkgLog().Debug(fmt.Sprintf("Cron interrupt notice to %s:%s raised: %v", platform, chatID, rec))
					}
				}()
				metadata := r.threadMetadataForTarget(platform, chatID, threadID, "", nil, adapter)
				_, err := config.PresentNotification(ctx, func(ctx context.Context) error {
					if sendNoticeLogged(ctx, adapter, chatID, msg, string(platform), "Cron interrupt notice to %s:%s failed: %s",
						"Cron interrupt notice to %s:%s raised: %s", metadata) {
						notified[dedupKey] = true
					}
					return nil
				}, string(platform), false, nil)
				if err != nil {
					pkgLog().Debug(fmt.Sprintf("Cron interrupt notice to %s:%s raised: %s", platform, chatID, pyrepr.ExceptionMessage(err)))
				}
			}()
		}
	}
	if len(notified) > 0 {
		pkgLog().Info(fmt.Sprintf("Shutdown: delivered %d interrupted-cron-job notice(s)", len(notified)))
	}
	return len(notified)
}

// Dedupe key of one cron job notice target
type cronNoticeKey struct {
	jobID string
	key   noticeTargetKey
}

// Stops a cron provider without letting it choose the gateway exit code
func stopCronProvider(p cron.CronScheduler) {
	defer func() {
		if rec := recover(); rec != nil {
			var exit *gateway.SystemExitError
			if err, ok := rec.(error); ok && errors.As(err, &exit) {
				pkgLog().Warn(fmt.Sprintf("Cron provider stop() attempted to exit the gateway with code %d; ignoring", exit.Code))
				return
			}
			pkgLog().Debug(fmt.Sprintf("Cron provider stop() error: %v", rec))
		}
	}()
	cron.SchedulerStop(p)
}

// Fires jobs an external cron provider never fired once their misfire grace passed
func housekeepingMisfireCatchUp(ctx context.Context, p cron.CronScheduler, adapters, loop any) error {
	caughtUp, err := cron.FireOverdueJobs(ctx, p, adapters, loop)
	if err != nil {
		return err
	}
	if caughtUp > 0 {
		pkgLog().Info(fmt.Sprintf("Misfire catch-up: fired %d overdue job(s)", caughtUp))
	}
	return nil
}

// Drains each profile's worker queue through its live adapters, a satellite through the primary's routed ones
func drainRestartSafeCronDeliveries(ctx context.Context, adapters *pydict.Dict[gateway.Platform, gateway.PlatformAdapter],
	loop gateway.EventLoop, r *GatewayRunner) error {
	var loopArg any
	if loop != nil {
		loopArg = loop
	}
	if r == nil {
		if adapters != nil {
			_, err := cron.DrainDeliveryQueue(ctx, adapters, loopArg)
			return err
		}
		return nil
	}
	for _, scope := range handoffWatchScopes(ctx, r) {
		var profileAdapters any
		if scope.name == nil {
			if adapters != nil {
				profileAdapters = adapters
			}
		} else if r.profileAdapters != nil {
			if m, ok := r.profileAdapters.Get(*scope.name); ok && m != nil {
				profileAdapters = m
			}
		}
		if profileAdapters == nil {
			continue
		}
		h := scope.home
		if h == "" {
			h = home.Dir(ctx)
		}
		scoped := gateway.ProfileRuntimeScope(ctx, h, true)
		var owned any
		if scope.name != nil {
			if platforms, ok := r.profileBotPlatforms[*scope.name]; ok {
				owned = platforms
			}
		}
		if scope.name != nil && adapters != nil && adapters.Len() > 0 && !cron.HasCredentialPlatform(profileAdapters, owned) {
			if routes := cron.PrimaryProfileRoutesForCurrentHome(scoped); len(routes) > 0 {
				profileAdapters = cron.NewSharedRouteAdapters(adapters, routes)
			}
		}
		if _, err := cron.DrainDeliveryQueue(scoped, profileAdapters, loopArg); err != nil {
			return err
		}
	}
	return nil
}

// Status units of the running cron jobs, the worker pid of a job running outside the gateway
func describeCronJobUnits(ctx context.Context, pid int) (units []map[string]any) {
	defer func() { _ = recover() }()
	wedged := cron.GetWedgedJobIDs(ctx)
	for _, job := range cron.GetRunningJobDetails() {
		jobPID := pid
		if job.WorkerPID != nil && *job.WorkerPID != 0 {
			jobPID = *job.WorkerPID
		}
		var elapsed any
		if job.ElapsedS != nil {
			elapsed = *job.ElapsedS
		}
		units = append(units, map[string]any{"kind": "cron", "job_id": job.JobID, "elapsed_s": elapsed,
			"pid": jobPID, "external": job.WorkerPID != nil && *job.WorkerPID != 0,
			"wedged": wedged[job.JobID], "restart_safe": job.RestartSafe})
	}
	return units
}

// Starts the cron scheduler of the gateway on its supervised ticker goroutine, stopped by stop
func startGatewayCronTicker(ctx context.Context, r *GatewayRunner, stop *pythread.Event) cronTicker {
	homes, err := cronTickProfileHomes(ctx, r.config)
	if err != nil {
		pkgLog().Warn(fmt.Sprintf("Could not resolve profile homes for cron: %v", err))
		homes = nil
	}
	h := home.Dir(ctx)
	p := cron.SchedulerForProfileMode(h, cron.ResolveCronScheduler(h), len(homes) > 1)
	opts := cron.StartOptions{Adapters: r.adapters, Loop: r.gatewayLoop}
	_, inProcess := p.(*cron.InProcessCronScheduler)
	if inProcess && len(homes) > 0 {
		opts.ProfileHomesFn = func() ([]cron.ProfileHome, error) {
			served, err := cronTickProfileHomes(ctx, r.config)
			if err != nil {
				return nil, err
			}
			out := make([]cron.ProfileHome, 0, len(served))
			for _, s := range served {
				name := s.Name
				out = append(out, cron.ProfileHome{Name: &name, Home: s.Home})
			}
			return out, nil
		}
		opts.ProfileGate = func(name *string, profileHome string) bool {
			n := ""
			if name != nil {
				n = *name
			}
			return cronProfileGate(n, profileHome)
		}
		opts.ProfileAdapters = map[string]any{}
		for name, m := range r.profileAdapters.All() {
			opts.ProfileAdapters[name] = m
		}
		opts.ProfileBotPlatforms = map[string]any{}
		for name, m := range r.profileBotPlatforms {
			opts.ProfileBotPlatforms[name] = m
		}
		opts.DefaultProfile = &r.primaryProfileName
		names := make([]string, 0, len(homes))
		for _, s := range homes {
			names = append(names, s.Name)
		}
		pkgLog().Info(fmt.Sprintf("Cron scheduler will tick %d profile(s): %s", len(homes), strings.Join(names, ", ")))
	}
	if inProcess {
		opts.CanDispatch = func() bool { return !r.draining.Load() }
	}
	stopCh := make(chan struct{})
	go func() {
		stop.Wait(-1)
		close(stopCh)
	}()
	cronThread := cron.NewSupervisedTickerThread(func() { p.Start(context.WithoutCancel(ctx), stopCh, opts) }, stopCh, "cron-ticker")
	gateway.ProcessOwnership.StartWriterThread(cronThread.IsAlive, func() { cronThread.Start() })
	if !inProcess {
		if _, hasAPIServer := r.adapters.Get(gateway.PlatformAPIServer); !hasAPIServer {
			pkgLog().Warn(fmt.Sprintf("Cron provider '%s' is active but the api_server adapter is "+
				"NOT running in this gateway — scheduled fires arrive over "+
				"loopback HTTP and will all fail (jobs only run when "+
				"triggered manually). Most common cause: API_SERVER_KEY is "+
				"missing from this gateway process's environment. Restart "+
				"the gateway through its supervisor (`icebath gateway "+
				"restart`) so the profile env loads.", p.Name()))
		}
	}
	return &gatewayCronTicker{provider: p, thread: cronThread}
}
