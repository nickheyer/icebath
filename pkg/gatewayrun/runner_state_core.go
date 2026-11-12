package gatewayrun

import (
	"sync"
	"sync/atomic"

	"github.com/nickheyer/icebath/pkg/agent"
	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/internal/pydict"
	"github.com/nickheyer/icebath/pkg/internal/pythread"
	"github.com/nickheyer/icebath/pkg/state"
)

// Cached agent of a session with the config signature it was built for
type agentCacheEntry struct {
	agent     *agent.AIAgent
	signature string
	msgCount  *int
	sessionID *string
}

// Producer stable identity of one completion delivery
type completionIdentity struct {
	evtType     string
	producerID  string
	incarnation any
}

// One held completion notification waiting for its batch flush
type completionBatchItem struct {
	synthText string
	evt       map[string]any
	future    *gatewayFuture
}

// One recent voice transcript with its monotonic time
type voiceTranscriptSeen struct {
	at   float64
	text string
}

// Pinned session database override, unset while reads resolve the active scope
type sessionDBPin struct {
	set bool
	db  *state.AsyncSessionDB
}

// Attributes the constructor and the runtime init of the runner assign
type runnerCoreState struct {
	config             *gateway.GatewayConfig
	adapters           *pydict.Dict[gateway.Platform, gateway.PlatformAdapter]
	sessionDBInitError *string
	profileAdapters    *pydict.Dict[string, *pydict.Dict[gateway.Platform, gateway.PlatformAdapter]]
	profileConfigs     *pydict.Dict[string, *gateway.GatewayConfig]

	prefillMessages              []any
	reasoningConfig              map[string]any
	serviceTier                  *string
	showReasoning                bool
	busyInputMode                string
	busyTextMode                 string
	busyInputModesByProfile      map[string]string
	busyTextModesByProfile       map[string]string
	busyTextTiming               [2]float64
	busyTextTimingByProfile      map[string][2]float64
	humanDelay                   *[2]int
	humanDelayByProfile          map[string]*[2]int
	restartDrainTimeout          float64
	stopRequestedBySignal        bool
	launchdExitTimeoutS          *float64
	restartAfterTurnTimeout      float64
	cronDrainTimeout             float64
	signalInterruptGraceTimeout  float64
	providerRouting              map[string]any
	fallbackModel                []map[string]any
	fallbackModelByHome          map[string][]map[string]any
	sessionRuntimeDescriptor     runtimeDescriptorState
	sessionStore                 *gateway.SessionStore
	asyncSessionStore            *gateway.AsyncSessionStore
	deliveryRouter               *gateway.DeliveryRouter
	running                      atomic.Bool
	exitCleanly                  bool
	exitWithFailure              bool
	draining                     atomic.Bool
	gatewayLoop                  gateway.EventLoop
	shutdownEvent                *pythread.Event
	exitReason                   *string
	exitCode                     *int
	profileFailedPlatforms       map[string]map[gateway.Platform]*gatewayTask
	profileBotPlatforms          map[string]map[gateway.Platform]struct{}
	systemdWatchdog              *gateway.SystemdWatchdog
	externalDrainActive          bool
	restartRequested             bool
	signalInitiatedShutdown      bool
	restartTaskStarted           bool
	restartDetached              bool
	restartViaService            bool
	detachedRestartHelperStarted bool
	restartCommandSource         *gateway.SessionSource
	startupTime                  float64
	bootedFromRestart            bool
	stopTask                     *gatewayTask
	stopTaskMu                   sync.Mutex
	restartTask                  *gatewayTask
	executorLock                 *sync.Mutex
	executor                     gatewayExecutor
	housekeepingExecutor         gatewayExecutor
	executorClosing              bool
	sessions                     map[string]*SessionState
	turnLeases                   *SessionTurnLeaseRegistry
	sessionStallMu               sync.Mutex
	sessionStallNotified         map[string]bool
	transcriptLagStreaks         map[string]int
	startupRestoreInProgress     bool
	startupRestoreQueue          []*gateway.MessageEvent
	startupRestoreTasks          []*gatewayTask
	startupRestoreTasksMu        sync.Mutex
	platformLockTakeoverOnStart  bool
	sessionSources               *pydict.Dict[string, *gateway.SessionSource]
	sessionSourcesMax            int

	completionDeliveryLock                *sync.Mutex
	completionDeliveriesInflight          map[completionIdentity]struct{}
	completionDeliveriesDelivered         *pydict.Dict[completionIdentity, struct{}]
	completionDeliveryRetention           int
	completionNotificationBatches         map[string][]completionBatchItem
	completionNotificationBatchTasks      map[string]*gatewayTask
	completionNotificationBatchFlushTasks map[*gatewayTask]struct{}
	completionNotificationBatchWindow     float64
	completionNotificationBatchesStopping bool

	agentCache                *pydict.Dict[string, agentCacheEntry]
	agentCacheLock            *sync.Mutex
	kanbanNotifierProfile     string
	primaryProfileName        string
	teamsPipelineRuntime      any
	teamsPipelineRuntimeError *string
	failedPlatforms           *pydict.Dict[gateway.Platform, map[string]any]
	fatalHandlerTasks         map[*gatewayTask]struct{}
	slashConfirmCounter       *atomic.Int64

	sessionDBPinned      sessionDBPin
	sessionDBHandles     map[string]any
	sessionDBHandlesLock *sync.Mutex
	sessionDBHandleCache *gateway.RecoverableHandleCache

	pairingStore                    *gateway.PairingStore
	pairingStores                   map[string]*gateway.PairingStore
	hooks                           *ProfileHookRegistries
	voiceMode                       *pydict.Dict[string, string]
	recentVoiceTranscripts          map[[4]any][]voiceTranscriptSeen
	backgroundTasks                 map[*gatewayTask]struct{}
	gatewayStartedAt                float64
	loopHeartbeatTask               *gatewayTask
	loopFloorTimerHandle            *gateway.LoopFloorTimerHandle
	loopLivenessWatchdog            *gateway.LoopLivenessWatchdogHandle
	lastInboundAt                   float64
	scaleToZeroCooldownUntil        float64
	scaleToZeroNoSuspendLogged      bool
	scaleToZeroDirectPlatformLogged bool
	shutdownWatchdogDone            *pythread.Event
	startupWarmupTask               *gatewayTask
	reconnectWatcherTask            *gatewayTask
}

// Runner holding only the class level defaults, the object.__new__ of hermes
func allocGatewayRunner() *GatewayRunner {
	r := &GatewayRunner{}
	r.busyInputMode = "interrupt"
	r.busyTextMode = "interrupt"
	r.restartDrainTimeout = gateway.DefaultGatewayRestartDrainTimeout
	r.restartAfterTurnTimeout = gateway.DefaultGatewayRestartAfterTurnTimeout
	r.cronDrainTimeout = gateway.DefaultGatewayCronDrainTimeout
	r.signalInterruptGraceTimeout = gateway.DefaultGatewaySignalInterruptGraceTimeout
	r.maxSupervisedRestarts, r.supervisedHealthySecs = defaultMaxSupervisedRestarts, defaultSupervisedHealthySecs
	r.reconnectWatcherSlowRetrySecs, r.maxSlowWatcherRespawns = defaultReconnectWatcherSlowRetrySec, defaultMaxSlowWatcherRespawns
	return r
}
