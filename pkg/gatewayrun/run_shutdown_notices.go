package gatewayrun

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/cron"
	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/i18n"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
)

// False when the platform config turns gateway restart notifications off
func (r *GatewayRunner) restartNotificationAllowed(platform gateway.Platform) bool {
	cfg := r.config.Platforms[platform]
	return cfg == nil || cfg.GatewayRestartNotification
}

// Whether a shutdown notice may go to the platform, logging the suppression otherwise
func (r *GatewayRunner) noticeAllowed(platform gateway.Platform, what string, platformCfg *gateway.PlatformConfig) bool {
	allowed := false
	if platformCfg == nil {
		allowed = r.restartNotificationAllowed(platform)
	} else {
		allowed = platformCfg.GatewayRestartNotification
	}
	if allowed {
		return true
	}
	runLog().Info(fmt.Sprintf("Shutdown notification suppressed for %s: %s has gateway_restart_notification=false", what, platform))
	return false
}

// Localized restarting or shutting down for a shutdown notice
func (r *GatewayRunner) shutdownActionText(ctx context.Context) string {
	if r.restartRequested {
		return i18n.T(ctx, "gateway.shutdown.action_restarting", "", nil)
	}
	return i18n.T(ctx, "gateway.shutdown.action_shutting_down", "", nil)
}

// Dedupe key of one cron job notice target
type cronNoticeKey struct {
	jobID string
	key   noticeTargetKey
}

// Tells the owner of each just interrupted cron job that its run died, returning the notices sent
func (r *GatewayRunner) notifyInterruptedCronJobs(ctx context.Context, jobIDs []string) int {
	if len(jobIDs) == 0 {
		return 0
	}
	action := r.shutdownActionText(ctx)
	notified := map[cronNoticeKey]bool{}
	for _, jobID := range jobIDs {
		job, err := cron.GetJob(ctx, home.Dir(ctx), jobID)
		if err != nil {
			runLog().Debug(fmt.Sprintf("Cron interrupt targets unresolved for %s: %s", jobID, pyrepr.ExceptionMessage(err)))
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
						runLog().Debug(fmt.Sprintf("Cron interrupt notice to %s:%s raised: %v", platform, chatID, rec))
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
					runLog().Debug(fmt.Sprintf("Cron interrupt notice to %s:%s raised: %s", platform, chatID, pyrepr.ExceptionMessage(err)))
				}
			}()
		}
	}
	if len(notified) > 0 {
		runLog().Info(fmt.Sprintf("Shutdown: delivered %d interrupted-cron-job notice(s)", len(notified)))
	}
	return len(notified)
}

// Where a shutdown notice for a session goes, the persisted origin over the cached source over the parsed key
type shutdownTarget struct {
	source      *gateway.SessionSource
	platformStr string
	chatID      string
	threadID    *string
	profile     *string
}

// Notification target of a session key, nil when nothing names its chat
func (r *GatewayRunner) shutdownNotificationTarget(ctx context.Context, sessionKey string) *shutdownTarget {
	var source *gateway.SessionSource
	if r.sessionStore != nil {
		func() {
			defer func() {
				if rec := recover(); rec != nil {
					runLog().Debug(fmt.Sprintf("Failed to load session origin for shutdown notification %s: %v", sessionKey, rec))
				}
			}()
			if entry := r.sessionStore.LoadedEntry(ctx, sessionKey); entry != nil {
				source = entry.Origin
			}
		}()
	}
	if source == nil {
		source = r.getCachedSessionSource(sessionKey)
	}
	if source != nil {
		return &shutdownTarget{source: source, platformStr: string(source.Platform), chatID: source.ChatID,
			threadID: source.ThreadID, profile: source.Profile}
	}
	parsed := gateway.ParseSessionKey(sessionKey)
	if len(parsed) == 0 {
		return nil
	}
	target := &shutdownTarget{platformStr: pyrepr.Str(parsed["platform"]), chatID: pyrepr.Str(parsed["chat_id"])}
	if thread, ok := parsed["thread_id"].(string); ok {
		target.threadID = &thread
	}
	if profile, ok := parsed["profile"].(string); ok {
		target.profile = &profile
	}
	return target
}

// Sends one shutdown notice, true when delivered and never raising
func sendShutdownNotice(ctx context.Context, adapter gateway.PlatformAdapter, chatID, msg, kind, platformStr string, metadata map[string]any) bool {
	where := ""
	if kind == "home channel" {
		where = "home channel "
	}
	failFmt := "Failed to send shutdown notification to " + where + "%s:%s: %s"
	if !sendNoticeLogged(ctx, adapter, chatID, msg, platformStr, failFmt, "", metadata) {
		return false
	}
	runLog().Info(fmt.Sprintf("Sent shutdown notification to %s %s:%s", kind, platformStr, chatID))
	return true
}

// Adapter send of an interim marked notice whose failure is debug logged, true only when delivered
func sendNoticeLogged(ctx context.Context, adapter gateway.PlatformAdapter, chatID, msg, platformStr, failFmt, raiseFmt string, metadata map[string]any) (delivered bool) {
	if raiseFmt == "" {
		raiseFmt = failFmt
	}
	merged := map[string]any{}
	for k, v := range metadata {
		merged[k] = v
	}
	merged["_interim_send"] = true
	defer func() {
		if rec := recover(); rec != nil {
			runLog().Debug(fmt.Sprintf(raiseFmt, platformStr, chatID, pyrepr.Str(rec)))
			delivered = false
		}
	}()
	result, err := adapter.Send(ctx, chatID, msg, "", merged)
	if err != nil {
		runLog().Debug(fmt.Sprintf(raiseFmt, platformStr, chatID, pyrepr.ExceptionMessage(err)))
		return false
	}
	if sendFailed(result) {
		runLog().Debug(fmt.Sprintf(failFmt, platformStr, chatID, sendError(result)))
		return false
	}
	return true
}

// Sends shutdown or restart notices to every chat with an active agent and every served home channel
func (r *GatewayRunner) notifyActiveSessionsOfShutdown(ctx context.Context) {
	var restartSource *gateway.SessionSource
	if r.restartRequested {
		restartSource = r.restartCommandSource
	}
	msgKey := "gateway.shutdown.notice_shutdown"
	if r.restartRequested {
		msgKey = "gateway.shutdown.notice_restart"
	}
	msg := i18n.T(ctx, msgKey, "", nil)
	var restartKey *noticeTargetKey
	if restartSource != nil {
		key := noticeTargetKeyOf(string(restartSource.Platform), restartSource.ChatID, optAny(restartSource.ThreadID))
		restartKey = &key
	}
	notified := map[noticeTargetKey]bool{}
	for _, sessionKey := range sortedAgentKeys(r.snapshotRunningAgents()) {
		target := r.shutdownNotificationTarget(ctx, sessionKey)
		if target == nil {
			continue
		}
		dedupKey := deliveryTargetKey(target.platformStr, target.chatID, optAny(target.threadID), target.profile)
		if notified[dedupKey] {
			continue
		}
		platform, adapter, metadata, ok := r.activeShutdownTarget(ctx, target, restartSource, restartKey, dedupKey)
		if !ok {
			continue
		}
		diagnostic := restartKey == nil || *restartKey != dedupKey
		scoped := ctx
		if target.source != nil {
			profileHome, err := r.resolveProfileHomeForSource(ctx, target.source)
			if err == nil {
				scoped = gateway.ProfileRuntimeScope(ctx, profileHome, true)
			}
		}
		presented, err := config.PresentNotification(scoped, func(ctx context.Context) error {
			if sendShutdownNotice(ctx, adapter, target.chatID, msg, "active chat", target.platformStr, metadata) {
				notified[dedupKey] = true
			}
			return nil
		}, string(platform), diagnostic, nil)
		if err == nil && !presented {
			notified[dedupKey] = true
		}
	}
	if r.restartRequested && restartSource != nil {
		runLog().Debug("Skipping home-channel shutdown notifications for in-chat restart")
		return
	}
	if r.drainNotificationSuppressed(ctx) {
		runLog().Info("Home-channel shutdown broadcast suppressed by drain marker (suppress_notification=true)")
		return
	}
	for _, served := range r.servedHomeChannelConfigs() {
		r.sendHomeShutdownNotice(ctx, served, msg, notified)
	}
}

// Platform, adapter and thread metadata of one active chat notice, false when it must be skipped
func (r *GatewayRunner) activeShutdownTarget(ctx context.Context, target *shutdownTarget, restartSource *gateway.SessionSource,
	restartKey *noticeTargetKey, dedupKey noticeTargetKey) (platform gateway.Platform, adapter gateway.PlatformAdapter, metadata map[string]any, ok bool) {
	defer func() {
		if rec := recover(); rec != nil {
			runLog().Debug(fmt.Sprintf("Failed to send shutdown notification to %s:%s: %v", target.platformStr, target.chatID, rec))
			ok = false
		}
	}()
	platform, err := gateway.ParsePlatform(ctx, target.platformStr)
	if err != nil {
		runLog().Debug(fmt.Sprintf("Failed to send shutdown notification to %s:%s: %s", target.platformStr, target.chatID, pyrepr.ExceptionMessage(err)))
		return "", nil, nil, false
	}
	if target.source != nil {
		if adapter, err = r.deliveryAdapterFor(ctx, target.source); err != nil {
			runLog().Debug(fmt.Sprintf("Failed to send shutdown notification to %s:%s: %s", target.platformStr, target.chatID, pyrepr.ExceptionMessage(err)))
			return "", nil, nil, false
		}
	}
	if adapter == nil {
		adapter = r.authorizationAdapter(ctx, platform, target.profile)
	}
	if adapter == nil || !r.noticeAllowed(platform, "active session", nil) {
		return "", nil, nil, false
	}
	var replyTo *string
	chatType := ""
	if target.source != nil {
		replyTo = target.source.MessageID
		chatType = target.source.ChatType
	}
	if replyTo == nil && restartKey != nil && *restartKey == dedupKey {
		replyTo = restartSource.MessageID
	}
	metadata = r.threadMetadataForTarget(platform, target.chatID, target.threadID, chatType, replyTo, adapter)
	return platform, adapter, metadata, true
}

// Whether the current drain marker asks to suppress the home channel broadcast, false when the check fails
func (r *GatewayRunner) drainNotificationSuppressed(ctx context.Context) (suppressed bool) {
	defer func() {
		if rec := recover(); rec != nil {
			runLog().Debug(fmt.Sprintf("drain_notification_suppressed check failed: %v", rec))
			suppressed = false
		}
	}()
	return gateway.DrainNotificationSuppressed(ctx, home.Dir(ctx))
}

// Sends the shutdown notice to one served profile's home channel through that profile's own bot
func (r *GatewayRunner) sendHomeShutdownNotice(ctx context.Context, served servedHomeConfig, msg string, notified map[noticeTargetKey]bool) {
	cfg := served.cfg
	if cfg == nil || cfg.HomeChannel == nil || cfg.HomeChannel.ChatID == "" {
		return
	}
	homeChannel := cfg.HomeChannel
	var adapters = r.adapters
	if served.profile != nil {
		adapters, _ = r.profileAdapters.Get(*served.profile)
	}
	adapter, ok := adapters.Get(served.platform)
	if !ok || adapter == nil {
		return
	}
	if !r.noticeAllowed(served.platform, "home channel", cfg) {
		return
	}
	dedupKey := deliveryTargetKey(string(served.platform), homeChannel.ChatID, optAny(homeChannel.ThreadID), served.profile)
	if notified[dedupKey] {
		return
	}
	metadata, ok := func() (metadata map[string]any, ok bool) {
		defer func() {
			if rec := recover(); rec != nil {
				runLog().Debug(fmt.Sprintf("Failed to send shutdown notification to home channel %s:%s: %v", served.platform, homeChannel.ChatID, rec))
				ok = false
			}
		}()
		return r.threadMetadataForTarget(served.platform, homeChannel.ChatID, homeChannel.ThreadID, "", nil, adapter), true
	}()
	if !ok {
		return
	}
	scoped := ctx
	if served.profile != nil {
		if profileHome, found := r.servedProfileHomes.Get(*served.profile); found && profileHome != "" {
			scoped = gateway.ProfileRuntimeScope(ctx, profileHome, true)
		}
	}
	if _, err := config.PresentNotification(scoped, func(ctx context.Context) error {
		if sendShutdownNotice(ctx, adapter, homeChannel.ChatID, msg, "home channel", string(served.platform), metadata) {
			notified[dedupKey] = true
		}
		return nil
	}, string(served.platform), false, nil); err != nil {
		logSuppressed(slog.LevelDebug, err, "Failed to send shutdown notification to home channel %s:%s: %v", served.platform, homeChannel.ChatID)
	}
}
