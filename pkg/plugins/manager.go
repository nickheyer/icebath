package plugins

import (
	"sync"
	"time"

	"github.com/nickheyer/icebath/pkg/tools"
)

// Runtime state of one loaded plugin
type LoadedPlugin struct {
	Manifest             *Manifest
	Module               string
	ToolsRegistered      []string
	HooksRegistered      []string
	MiddlewareRegistered []string
	CommandsRegistered   []string
	Enabled              bool
	Error                string
	Deferred             bool
}

// Discovers, loads and invokes the plugins of one home
type Manager struct {
	mu                            sync.RWMutex
	discoveryMu                   sync.Mutex
	home                          string
	discovered                    bool
	plugins                       map[string]*LoadedPlugin
	hooks                         map[string][]*hookEntry
	middleware                    map[string][]*middlewareEntry
	pluginToolNames               map[string]bool
	pluginPlatformNames           map[string]bool
	systemPromptSections          map[string]*SystemPromptSection
	memoryHookRegistrations       map[hookSource][]*Registration
	ownershipLedger               map[string][]*Registration
	registrationOrder             []*Registration
	persistentCarryover           []*Registration
	hookTimeoutMu                 sync.Mutex
	hookRunningCallbacks          map[hookGateKey]*struct{}
	hookAbandoned                 map[hookSuppressionKey]map[hookGateKey]bool
	hookTimeoutSuppressedUntil    map[hookSuppressionKey]time.Time
	hookFailuresReported          map[hookFailureKey]bool
	hookTimeoutSuppression        time.Duration
	eventMu                       sync.Mutex
	subscriptions                 map[string][]*eventSubscription
	eventGeneration               int
	eventPending                  map[int]int
	eventQueue                    chan *queuedPluginEvent
	eventWorkerRunning            bool
	eventIdle                     chan struct{}
	eventPendingCap               int
	injectorMu                    sync.Mutex
	gatewayInjector               *ownedInjector
	tuiInjector                   *ownedInjector
	cliRef                        CLIHost
	contextEngine                 any
	cliCommands                   map[string]*CLICommand
	pluginCommands                map[string]*PluginCommand
	pluginSkills                  map[string]*PluginSkill
	automationBlueprints          map[string]*blueprintEntry
	portableMCPServers            map[string]map[string]any
	portableMCPServerPlugins      map[string]string
	auxTasks                      map[string]*auxTask
	approvalTransports            map[string]*tools.RegisteredApprovalTransport
	slackActionHandlers           []*SlackActionHandler
	platformHandlerFactories      map[string][]*PlatformHandlerFactory
	pluginLoadedListeners         []*loadedListener
	predeclaredModules            map[string]string
	predeclaredTools              map[string][]string
	pluginSecretSourcesReconciled bool
	pluginHostInstance            *PluginHost
}

// Manager scoped to one home with empty registries
func NewManager(h string) *Manager {
	return &Manager{
		home:                       h,
		plugins:                    map[string]*LoadedPlugin{},
		hooks:                      map[string][]*hookEntry{},
		middleware:                 map[string][]*middlewareEntry{},
		pluginToolNames:            map[string]bool{},
		pluginPlatformNames:        map[string]bool{},
		systemPromptSections:       map[string]*SystemPromptSection{},
		memoryHookRegistrations:    map[hookSource][]*Registration{},
		ownershipLedger:            map[string][]*Registration{},
		hookRunningCallbacks:       map[hookGateKey]*struct{}{},
		hookAbandoned:              map[hookSuppressionKey]map[hookGateKey]bool{},
		hookTimeoutSuppressedUntil: map[hookSuppressionKey]time.Time{},
		hookFailuresReported:       map[hookFailureKey]bool{},
		hookTimeoutSuppression:     hookTimeoutSuppressionDefault,
		subscriptions:              map[string][]*eventSubscription{},
		eventPending:               map[int]int{0: 0},
		eventQueue:                 make(chan *queuedPluginEvent, eventPendingCapDefault),
		eventIdle:                  make(chan struct{}),
		eventPendingCap:            eventPendingCapDefault,
		cliCommands:                map[string]*CLICommand{},
		pluginCommands:             map[string]*PluginCommand{},
		pluginSkills:               map[string]*PluginSkill{},
		automationBlueprints:       map[string]*blueprintEntry{},
		portableMCPServers:         map[string]map[string]any{},
		portableMCPServerPlugins:   map[string]string{},
		auxTasks:                   map[string]*auxTask{},
		approvalTransports:         map[string]*tools.RegisteredApprovalTransport{},
		platformHandlerFactories:   map[string][]*PlatformHandlerFactory{},
		predeclaredModules:         map[string]string{},
		predeclaredTools:           map[string][]string{},
	}
}
