package gatewayrun

import (
	"context"
	"sync"
	"sync/atomic"
	"time"

	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/internal/pydict"
	"github.com/nickheyer/icebath/pkg/internal/pythread"
	"github.com/nickheyer/icebath/pkg/tools"
)

// Seconds since the epoch like time.time
func wallClock() float64 { return float64(time.Now().UnixNano()) / 1e9 }

// Loads the per call config of prefill, reasoning, busy modes, timeouts and provider routing
func (r *GatewayRunner) initRuntimeSettings(ctx context.Context) {
	r.prefillMessages = loadPrefillMessages(ctx)
	r.reasoningConfig = loadReasoningConfig(ctx, "")
	r.serviceTier = loadServiceTier(ctx)
	r.showReasoning = loadShowReasoning(ctx)
	r.busyInputMode = loadBusyInputMode(ctx)
	r.busyTextMode = loadBusyTextMode(ctx)
	r.busyInputModesByProfile = map[string]string{}
	r.busyTextModesByProfile = map[string]string{}
	r.busyTextTiming = busyTextTimingFromConfig(gateway.LoadGatewayConfigMap(ctx, ""))
	r.busyTextTimingByProfile = map[string][2]float64{}
	r.humanDelay = humanDelayFromConfig(gateway.LoadGatewayConfigMap(ctx, ""))
	r.humanDelayByProfile = map[string]*[2]int{}
	r.restartDrainTimeout = loadRestartDrainTimeout(ctx)
	r.stopRequestedBySignal = false
	r.launchdExitTimeoutS = loadLaunchdExitTimeout(r.restartDrainTimeout)
	r.restartAfterTurnTimeout = loadRestartAfterTurnTimeout(ctx)
	r.cronDrainTimeout = loadCronDrainTimeout(ctx)
	r.signalInterruptGraceTimeout = loadSignalInterruptGraceTimeout(ctx)
	r.providerRouting = loadProviderRouting(ctx)
	r.fallbackModel = loadFallbackModel(ctx)
}

// Session store guarded against resets while processes run, its async facade and the delivery router
func (r *GatewayRunner) initSessionStore(ctx context.Context) {
	r.sessionStore = gateway.NewSessionStore(ctx, r.config.SessionsDir, r.config, func(key string) (bool, error) {
		return tools.DefaultProcessRegistry().HasActiveForSession(key, nil), nil
	})
	r.asyncSessionStore = gateway.NewAsyncSessionStore(r.sessionStore)
	r.deliveryRouter = gateway.NewDeliveryRouter(r.config, nil)
}

// Run, exit and restart flags, per session state and the completion delivery bookkeeping
func (r *GatewayRunner) initLifecycleState() {
	r.exitCleanly, r.exitWithFailure = false, false
	r.running.Store(false)
	r.draining.Store(false)
	r.gatewayLoop = nil
	r.shutdownEvent = &pythread.Event{}
	r.exitReason, r.exitCode = nil, nil
	r.profileFailedPlatforms = map[string]map[gateway.Platform]*gatewayTask{}
	r.profileBotPlatforms = map[string]map[gateway.Platform]struct{}{}
	r.systemdWatchdog = nil
	r.externalDrainActive = false
	r.restartRequested, r.signalInitiatedShutdown, r.restartTaskStarted = false, false, false
	r.restartDetached, r.restartViaService, r.detachedRestartHelperStarted = false, false, false
	r.restartCommandSource = nil
	r.startupTime = wallClock()
	r.bootedFromRestart = false
	r.stopTaskMu.Lock()
	r.stopTask, r.restartTask = nil, nil
	r.stopTaskMu.Unlock()
	r.executorLock = &sync.Mutex{}
	r.executor = nil
	r.housekeepingExecutor = nil
	r.executorClosing = false
	r.sessions = map[string]*SessionState{}
	r.turnLeases = NewSessionTurnLeaseRegistry(defaultMaxLeases)
	r.sessionStallNotified = map[string]bool{}
	r.transcriptLagStreaks = map[string]int{}
	r.startupRestoreInProgress = false
	r.startupRestoreQueue = []*gateway.MessageEvent{}
	r.startupRestoreTasks = []*gatewayTask{}
	r.platformLockTakeoverOnStart = false
	r.sessionSources = pydict.New[string, *gateway.SessionSource]()
	r.sessionSourcesMax = 512
	r.completionDeliveryLock = &sync.Mutex{}
	r.completionDeliveriesInflight = map[completionIdentity]struct{}{}
	r.completionDeliveriesDelivered = pydict.New[completionIdentity, struct{}]()
	r.completionDeliveryRetention = 2048
	r.completionNotificationBatches = map[string][]completionBatchItem{}
	r.completionNotificationBatchTasks = map[string]*gatewayTask{}
	r.completionNotificationBatchFlushTasks = map[*gatewayTask]struct{}{}
	r.completionNotificationBatchWindow = 0.1
	r.completionNotificationBatchesStopping = false
}

// Agent cache, launch profile identity, Teams runtime slot, failed platform tracking and the confirm counter
func (r *GatewayRunner) initRuntimeCaches(ctx context.Context) {
	r.agentCache = pydict.New[string, agentCacheEntry]()
	r.agentCacheLock = &sync.Mutex{}
	launch := r.activeProfileName(ctx)
	r.kanbanNotifierProfile = launch
	r.primaryProfileName = launch
	if r.multiplexing() {
		r.primaryProfileName = "default"
	}
	r.teamsPipelineRuntime = nil
	r.teamsPipelineRuntimeError = nil
	r.failedPlatforms = pydict.New[gateway.Platform, map[string]any]()
	r.fatalHandlerTasks = map[*gatewayTask]struct{}{}
	r.slashConfirmCounter = &atomic.Int64{}
}

// Pairing stores, hook registries, voice modes, the background task set and the liveness and idle clocks
func (r *GatewayRunner) initRegistriesAndClocks(ctx context.Context) error {
	store, err := gateway.NewPairingStore(ctx, "")
	if err != nil {
		return err
	}
	r.pairingStore = store
	r.pairingStores = map[string]*gateway.PairingStore{}
	r.hooks = NewProfileHookRegistries()
	modes, err := r.loadVoiceModes()
	if err != nil {
		return err
	}
	r.voiceMode = modes
	r.recentVoiceTranscripts = map[[4]any][]voiceTranscriptSeen{}
	r.backgroundTasks = map[*gatewayTask]struct{}{}
	r.gatewayStartedAt = wallClock()
	r.loopHeartbeatTask = nil
	r.loopFloorTimerHandle, r.loopLivenessWatchdog = nil, nil
	r.lastInboundAt = wallClock()
	r.scaleToZeroCooldownUntil = 0
	r.scaleToZeroNoSuspendLogged = false
	r.scaleToZeroDirectPlatformLogged = false
	return nil
}
