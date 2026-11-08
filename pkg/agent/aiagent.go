package agent

import (
	"context"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"log/slog"
	"reflect"
	"strings"
	"sync"
	"time"
	"weak"

	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pythread"
	"github.com/nickheyer/icebath/pkg/internal/pyurl"
	"github.com/nickheyer/icebath/pkg/mcp"
	"github.com/nickheyer/icebath/pkg/memory"
	"github.com/nickheyer/icebath/pkg/provider"
	"github.com/nickheyer/icebath/pkg/skills"
	"github.com/nickheyer/icebath/pkg/state"
	"github.com/nickheyer/icebath/pkg/tools"
)

// toolCallArgumentsCorruptionMarker replaces tool call arguments dropped as corrupted
const toolCallArgumentsCorruptionMarker = "[icebath-agent: tool call arguments were corrupted in this session and " +
	"have been dropped to keep the conversation alive. See issue #15236.]"

// AIAgent is the tool calling agent runner, hermes attribute names kept as unexported fields
type AIAgent struct {
	provider                         string
	requestedProvider                string
	model                            string
	baseURL                          string
	baseURLLower                     string
	baseURLHostname                  string
	apiMode                          string
	apiKey                           string
	acpCommand                       string
	acpArgs                          []string
	platform                         string
	env                              *tools.Env
	tools                            []any
	validToolNames                   map[string]bool
	contextEngineToolNames           map[string]bool
	forceASCIIPayload                bool
	bedrockConverseFallbackAttempted bool
	nousModelSwitch                  *[2]string
	sessionID                        string
	parentSessionID                  string
	sessionDB                        *state.DB
	sessionDBCreated                 bool
	ownsSessionDB                    bool
	persistDisabled                  bool
	endSessionOnClose                bool
	memoryProviderShutdown           bool
	cachedConversationRoot           string
	dbFlushScanPrefix                any
	streamedAssistantTextParts       []string
	relayPendingTurnID               string
	cacheScope                       *provider.CacheScopeAgent
	inheritedCacheScope              string
	promptCacheForkTag               string
	requestClientCache               map[string]any
	sessionInitModelConfig           map[string]any
	sessionCwd                       string
	sessionStart                     time.Time
	client                           any
	clientKwargs                     map[string]any
	anthropicClient                  any
	anthropicAPIKey                  string
	anthropicBaseURL                 string
	isAnthropicOAuth                 bool
	credentialPool                   *provider.Pool
	customProviders                  []any
	apiMaxRetries                    int
	contextCompressor                ContextEngine
	streamDeliveryState
	imageReplayState
	lastTurnUsage        map[string]any
	lastPromptSizeTokens int
	logPrefix            string
	logPrefixChars       int
	logsDir              string
	turnRouteTask        string
	verboseLogging       bool
	quietMode            bool
	suppressStatusOutput bool
	maxIterations        int
	iterationBudget      *IterationBudget
	budgetWarningRatio   float64
	runBudgetSeconds     float64
	checkpointMgr        *tools.CheckpointManager
	visionPrepState
	sanitizationState
	turnExplainersState
	voiceTurnRouteFields
	systemPromptState
	turnContextState
	toolExecutorState
	chatCompletionAgentState
	runtimeHookState
	turnLeaseState
	sessionPersistenceState
	imageEvictionState
	compressionAttemptState
	compressionTimeoutState
	conversationCompressionState
	statusOutputState
	agentInitState
	credentialRefreshState
	apiKwargsState
	fallbackWalkState
	primaryRestoreState
	skillsIndexDeltaState
	backgroundReviewState
	passSessionID                      bool
	skipContextFiles                   bool
	loadSoulIdentity                   bool
	skipBackgroundReview               bool
	sideAgent                          bool
	saveTrajectories                   bool
	toolProgressMode                   string
	ephemeralSystemPrompt              string
	providersAllowed                   any
	providersIgnored                   any
	providersOrder                     any
	providerSort                       any
	providerRequireParameters          bool
	providerDataCollection             any
	openrouterMinCodingScore           any
	enabledToolsets                    []string
	disabledToolsets                   []string
	configuredDisabledToolsets         []string
	maxTokens                          int
	reasoningConfig                    map[string]any
	serviceTier                        string
	requestOverrides                   map[string]any
	prefillMessages                    []any
	capabilities                       map[string]bool
	runtimeCapabilities                map[string]bool
	textVerbosity                      string
	showCommentary                     bool
	memoryStore                        *memory.Store
	memoryManager                      *memory.MemoryManager
	memoryEnabled                      bool
	userProfileEnabled                 bool
	memoryNotifications                any
	memoryNudgeInterval                int
	skillNudgeInterval                 int
	turnsSinceMemory                   int
	itersSinceSkill                    int
	usePromptCaching                   bool
	useNativeCacheLayout               bool
	cacheTTL                           string
	cacheDisabled                      bool
	sessionActivityLastPersistMono     float64
	oneshotSession                     tools.OneshotSession
	compressionEnabled                 bool
	compressionInPlace                 bool
	compressionCheckpointRequired      bool
	compressionIdleCompactAfterSeconds float64
	compressionThresholdAutoraised     map[string]any
	maxCompressionAttempts             int
	auxCompressionContextLengthConfig  int
	configContextLength                int
	codexResponsesNativeCompaction     bool
	codexResponsesCompactThreshold     float64
	codexAppServerAutoCompaction       string
	lmstudioLoadMode                   string
	fastAutoSeconds                    float64
	reasoningEchoFlag                  any
	autoRecoveryCycles                 int
	intentAckContinuation              any
	subdirectoryHints                  any
	stream5xxProbeTS                   float64
	todoStore                          *tools.TodoStore
	delegationBackend                  string
	delegationBackendSet               bool
	delegationInline                   bool
	delegationInlineSet                bool
	toolSnapshotGeneration             *int64
	toolSnapshotSelection              *string
	stallGuards                        bool
	printFn                            func(string)
	backgroundReviewCallback           any
	userID, userIDAlt, userName        string
	chatID, chatName, chatType         string
	threadID, gatewaySessionKey        string
	profileName                        string
	toolProgressCallback               any
	toolStartCallback                  any
	toolCompleteCallback               any
	toolResultMetadataCallback         any
	thinkingCallback                   any
	reasoningCallback                  any
	clarifyCallback                    any
	readTerminalCallback               any
	readPreviewCallback                any
	drivePreviewCallback               any
	readWindowBelowCallback            any
	connectionCallback                 any
	tourCallback                       any
	themeCallback                      any
	setupBeatCallback                  any
	stepCallback                       any
	streamDeltaCallback                any
	interimAssistantCallback           any
	statusCallback                     any
	noticeCallback                     any
	noticeClearCallback                any
	eventCallback                      func(string, map[string]any)
	reactionCallback                   func(string)
	toolGenCallback                    any
	executingTools                     bool
	trimAfterToolBatch                 bool
	toolGuardrails                     *tools.ToolCallGuardrailController
	toolGuardrailHaltDecision          *tools.ToolGuardrailDecision
	interruptRequested                 bool
	interruptMessage                   string
	hardInterruptRequested             *pythread.Event
	executionThreadID                  int64
	interruptThreadSignalPending       bool
	clientLock                         *pythread.RLock
	modelRequestActive                 *pythread.Event
	supportsActiveTurnRedirect         bool
	pendingSteer                       string
	pendingSteerParts                  []steerPart
	steerTicketFates                   map[any]bool
	steerLastDrain                     *steerDrain
	pendingSteerLock                   *sync.Mutex
	pendingRedirect                    string
	toolInterruptReason                string
	activeCompressionCommitFence       any
	compressionCommitFenceLock         sync.Mutex
	compressionCommitFenceStack        []*commitFenceRegistration
	rejectedThinking                   *provider.RejectedThinkingTracker
	pendingRedirectLock                *sync.Mutex
	toolWorkerThreads                  map[int64]bool
	toolWorkerThreadsLock              *sync.Mutex
	delegateDepth                      int
	delegateParentRef                  weak.Pointer[AIAgent]
	delegateProfileHome                string
	delegateSavedToolNames             []string
	handedOffProcesses                 []map[string]any
	subagentID                         string
	parentSubagentID                   string
	delegateRole                       string
	skipMemory                         bool
	delegateOutputSchema               any
	subagentFinalizationMu             sync.Mutex
	delegateImages                     []string
	liveTranscriptPath                 string
	delegateRoute                      *pyjson.Object
	delegationID                       string
	progressIdentityRef                map[string]any
	oneshotChildrenSpawned             int64
	activeChildren                     []*AIAgent
	activeChildrenLock                 *sync.Mutex
	backgroundReviewAgent              *AIAgent
	backgroundReviewRun                any
	backgroundReviewLock               *sync.Mutex
	iterationBudgetWarningInjected     bool
	budgetExhaustedInjected            bool
	budgetGraceCall                    bool
	runBudgetStartedAt                 float64
	runBudgetWrapupInjected            bool
	lastActivityTS                     float64
	lastActivityDesc                   string
	lastActivityProvenance             state.ActivityProvenance
	turnLivenessActivityLock           sync.Mutex
	turnLivenessActivityLocker         sync.Locker
	turnLivenessActivityGeneration     int64
	turnLivenessAbortClaim             any
	currentTool                        string
	apiCallCount                       int
	skipMCPRefresh                     bool
	credits                            *provider.CreditsCapture
	creditsMu                          sync.Mutex
	sessionMessages                    []*state.Message
	codexReasoningReplayEnabled        bool
	codexReasoningReplayRejected       bool
	memoryWriteOrigin                  string
	memoryWriteContext                 string
	sessionPersistLock                 *pythread.RLock
	pendingCLIUserMessage              any
	lastFlushedDBIdx                   int
	userTurnCount                      int
	isUserInitiatedTurn                bool
	usageAnchor                        *provider.UsageAnchor
	turnBaseUsageAnchor                *provider.UsageAnchor
	requestPressureAnchored            bool
	sessionPromptTokens                int
	sessionCompletionTokens            int
	sessionTotalTokens                 int
	sessionAPICalls                    int
	sessionInputTokens                 int
	sessionOutputTokens                int
	sessionCacheReadTokens             int
	sessionCacheWriteTokens            int
	sessionReasoningTokens             int
	sessionEstimatedCostUSD            float64
	sessionCostStatus                  string
	sessionCostSource                  string
	apiLatencyHistory                  []float64
	apiOutputHistory                   []int
	streamCallback                     func(string)
	streamNeedsBreak                   bool
	streamContextScrubber              *memory.StreamingContextScrubber
	streamThinkScrubber                *provider.StreamingThinkScrubber
	deliveredInterimTexts              map[string]bool
	streamWriterLock                   *sync.Mutex
	streamWriterToken                  int
	streamWriterDropped                int
	persistUserMessageIdx              *int
	persistUserMessageOverride         any
	persistUserMessageTimestamp        any
	inflightTurnSessionID              string
	surfaceSwitchNote                  string
	isSubagent                         bool
	fallbackActivated                  bool
	fallbackIndex                      int
	fallbackChain                      []map[string]any
	holdStream                         bool
	currentTurnID                      string
	ephemeralReasoningOff              bool
	ephemeralMaxOutputTokens           int
	compactionPromptDriftLogged        bool
	turnAuthor                         any
	providerHookFailures               map[string]bool
	codexSession                       *provider.CodexAppServerSession
	codexSessionPrompt                 *string
	codexSessionModelProvider          *string
	lastCompactionInPlace              bool
	compressionBlockedTransient        any
	compressionSkippedDueToLock        any
	ollamaNumCtx                       int
	reviewInputTokenBudget             int
	lastCompressionAttemptRecorded     bool
	lastCompressionAttemptInPlace      *bool
	heldTransforms                     func(ctx context.Context, kwargs map[string]any) (string, bool)
	incrementalPersistenceFailed       bool
	lastPersistenceErrorCause          any
	compressionAdoptionFailed          bool
	authPoolRefreshCounts              map[string]*provider.AuthEpisode
	credentialPoolRevertID             string
	entitlementRejectedModels          map[[2]string]bool
	currentAPIRequestID                string
	turnUserIntervened                 bool
	sanitizeArgsCursor                 map[string]any
	nousWirePending                    any
	emptyContentRetries                int
	lastAPIFirstChunkAt                *float64
	promptRefreshEpoch                 *int
	codexIncompleteRetries             int
	codexReasoningOnlyStreak           int
	metricsScratch                     sync.Map
}

// AIAgentOptions are the AIAgent constructor keywords, nil and zero values standing for None
type AIAgentOptions struct {
	BaseURL, APIKey, Provider, APIMode string
	ACPCommand                         string
	ACPArgs                            []string
	Command                            string
	Args                               []string
	Runtime                            *provider.Runtime
	Model                              string
	MaxIterations                      int
	EnabledToolsets, DisabledToolsets  []string
	SaveTrajectories, VerboseLogging   bool
	QuietMode                          bool
	ToolProgressMode                   string
	EphemeralSystemPrompt              string
	LogPrefixChars                     int
	LogPrefix                          string
	ProvidersAllowed, ProvidersIgnored any
	ProvidersOrder, ProviderSort       any
	ProviderRequireParameters          bool
	ProviderDataCollection             any
	OpenRouterMinCodingScore           any
	SessionID                          string
	ToolProgressCallback               any
	ToolStartCallback                  any
	ToolCompleteCallback               any
	ThinkingCallback                   any
	ReasoningCallback                  any
	ClarifyCallback                    any
	ReadTerminalCallback               any
	ReadPreviewCallback                any
	DrivePreviewCallback               any
	ReadWindowBelowCallback            any
	ConnectionCallback                 any
	TourCallback                       any
	ThemeCallback                      any
	SetupBeatCallback                  any
	StepCallback                       any
	StreamDeltaCallback                any
	InterimAssistantCallback           any
	ToolGenCallback                    any
	StatusCallback                     any
	NoticeCallback                     any
	NoticeClearCallback                any
	EventCallback                      func(string, map[string]any)
	ReactionCallback                   func(string)
	MaxTokens                          int
	ReasoningConfig                    map[string]any
	ServiceTier                        string
	RequestOverrides                   map[string]any
	PrefillMessages                    []any
	Platform                           string
	UserID, UserIDAlt, UserName        string
	ChatID, ChatName, ChatType         string
	ThreadID, GatewaySessionKey        string
	SkipContextFiles, LoadSoulIdentity bool
	SkipMemory, SkipBackgroundReview   bool
	SessionDB                          *state.DB
	ParentSessionID                    string
	IterationBudget                    *IterationBudget
	RunBudgetSeconds                   *float64
	FallbackModel                      []map[string]any
	CredentialPool                     *provider.Pool
	CheckpointsEnabled                 bool
	CheckpointMaxSnapshots             int
	CheckpointMaxTotalSizeMB           int
	CheckpointMaxFileSizeMB            int
	PassSessionID                      bool
	RequestedProvider                  string
	Capabilities                       map[string]bool
	Cwd                                string
	SideAgent                          bool
	MemoryManager                      *memory.MemoryManager
	ToolResultMetadataCallback         any
	Env                                *tools.Env
	// Deprecated keyword kept for compatibility, warned about and ignored
	ToolDelay *float64
}

// RunConversationOptions are the run_conversation keywords the callers above pkg/agent pass
type RunConversationOptions struct {
	SystemMessage              *string
	ConversationHistory        []map[string]any
	TaskID                     string
	StreamCallback             func(string)
	PersistUserMessage         any
	PersistUserTimestamp       *float64
	PersistUserDisplayKind     string
	PersistUserDisplayMetadata map[string]any
	PersistUserPlatformID      string
	MoaConfig                  map[string]any
	TurnAuthor                 map[string]any
	RelayMetadata              map[string]any
	TitleUserMessage           *string
	Prelude                    Prelude
}

// Builds an agent through the init_agent forwarder, runtime fields filling the route keywords like cron does
// Python DeprecationWarning channel, recorded on the warnings logger below the default level so it stays silent
var warnDeprecation = func(message string) {
	slog.Default().With("logger", "py.warnings").Debug("DeprecationWarning: " + message)
}

func NewAIAgent(ctx context.Context, o AIAgentOptions) (*AIAgent, error) {
	if o.ToolDelay != nil {
		warnDeprecation("tool_delay is deprecated and ignored; sequential tool calls no longer sleep between executions.")
	}
	if rt := o.Runtime; rt != nil {
		o.APIKey, o.BaseURL, o.Provider = rt.APIKey, rt.BaseURL, rt.Provider
		o.RequestedProvider, o.APIMode = rt.RequestedProvider, string(rt.APIMode)
		o.RequestOverrides, o.ACPCommand, o.ACPArgs = rt.RequestOverrides, rt.Command, rt.Args
	}
	if o.MaxIterations == 0 {
		o.MaxIterations = int(^uint(0) >> 1)
	}
	if o.ToolProgressMode == "" {
		o.ToolProgressMode = "all"
	}
	if o.LogPrefixChars == 0 {
		o.LogPrefixChars = 100
	}
	if o.CheckpointMaxSnapshots == 0 {
		o.CheckpointMaxSnapshots = 20
	}
	if o.CheckpointMaxTotalSizeMB == 0 {
		o.CheckpointMaxTotalSizeMB = 500
	}
	if o.CheckpointMaxFileSizeMB == 0 {
		o.CheckpointMaxFileSizeMB = 10
	}
	a := &AIAgent{}
	if err := initAgent(ctx, a, o); err != nil {
		return nil, err
	}
	return a, nil
}

// Stores the base url with its lowercase and hostname forms like the property setter
func (a *AIAgent) setBaseURL(value string) {
	a.baseURL = value
	a.baseURLLower = strings.ToLower(value)
	a.baseURLHostname, _ = pyurl.BaseURLHostname(value)
}

// Context bound to the agent tool env home, the hermes home a method reads
func (a *AIAgent) homeContext() context.Context {
	if a.env != nil && a.env.Home != "" {
		return home.WithOverride(context.Background(), a.env.Home)
	}
	return context.Background()
}

func (a *AIAgent) Provider() string                  { return a.provider }
func (a *AIAgent) Model() string                     { return a.model }
func (a *AIAgent) BaseURL() string                   { return a.baseURL }
func (a *AIAgent) APIMode() string                   { return a.apiMode }
func (a *AIAgent) Platform() string                  { return a.platform }
func (a *AIAgent) SessionID() string                 { return a.sessionID }
func (a *AIAgent) SessionAPICalls() int              { return a.sessionAPICalls }
func (a *AIAgent) SetEndSessionOnClose(v bool)       { a.endSessionOnClose = v }
func (a *AIAgent) DelegateDepth() int                { return a.delegateDepth }
func (a *AIAgent) SubagentID() string                { return a.subagentID }
func (a *AIAgent) ValidToolNames() map[string]bool   { return a.validToolNames }
func (a *AIAgent) ToolDefinitions() []map[string]any { return a.ToolDefs() }
func (a *AIAgent) MemoryWriteOrigin() string         { return a.memoryWriteOrigin }
func (a *AIAgent) PersistDisabled() bool             { return a.persistDisabled }
func (a *AIAgent) MetricsScratch() *sync.Map         { return &a.metricsScratch }
func (a *AIAgent) StreamProvider() string            { return a.provider }
func (a *AIAgent) StreamBaseURL() string             { return a.baseURL }
func (a *AIAgent) ToolEnv() *tools.Env               { return a.env }
func (a *AIAgent) EnabledToolsets() []string         { return a.enabledToolsets }
func (a *AIAgent) DisabledToolsets() []string        { return a.disabledToolsets }
func (a *AIAgent) DelegationBackend() string         { return a.delegationBackend }
func (a *AIAgent) IsOneshot() bool                   { return a.agentIsOneshot() }
func (a *AIAgent) ToolSearchScopedNames() map[string]bool {
	return toolSearchScopedNames(a)
}

// Background review attribution of the agent tool env write origin
func (a *AIAgent) BackgroundReview() bool { return skills.IsBackgroundReview(a.env) }

// Tool definitions the agent carries as dicts, non dict entries skipped
func (a *AIAgent) ToolDefs() []map[string]any {
	if a.tools == nil {
		return nil
	}
	out := make([]map[string]any, 0, len(a.tools))
	for _, t := range a.tools {
		if d, ok := t.(map[string]any); ok {
			out = append(out, d)
		}
	}
	return out
}

// Publishes tools and valid tool names together
func (a *AIAgent) SetToolSnapshot(defs []map[string]any, names map[string]bool) {
	list := make([]any, len(defs))
	for i, d := range defs {
		list[i] = d
	}
	a.tools, a.validToolNames = list, names
}

func (a *AIAgent) SetToolsets(enabled, disabled []string) {
	a.enabledToolsets, a.disabledToolsets = enabled, disabled
}

func (a *AIAgent) SetConfiguredDisabledToolsets(disabled []string) {
	a.configuredDisabledToolsets = disabled
}

func (a *AIAgent) ConfiguredDisabledToolsets() []string { return a.configuredDisabledToolsets }

// Registry generation the tools snapshot was built against, false when unset
func (a *AIAgent) SnapshotGeneration() (int64, bool) {
	if a.toolSnapshotGeneration == nil {
		return 0, false
	}
	return *a.toolSnapshotGeneration, true
}

// Toolset selection the tools snapshot was built from, false when unset
func (a *AIAgent) SnapshotSelection() (string, bool) {
	if a.toolSnapshotSelection == nil {
		return "", false
	}
	return *a.toolSnapshotSelection, true
}

func (a *AIAgent) RecordSnapshot(generation int64, selection string) {
	a.toolSnapshotGeneration, a.toolSnapshotSelection = &generation, &selection
}

// Replaces the context engine tool names in place only when the set exists
func (a *AIAgent) PublishContextEngineToolNames(names map[string]bool) {
	if a.contextEngineToolNames == nil {
		return
	}
	clear(a.contextEngineToolNames)
	for n := range names {
		a.contextEngineToolNames[n] = true
	}
}

// Session store the tools pin writes to, nil without a session db
func (a *AIAgent) ToolPinStore() mcp.ToolPinStore {
	if a.sessionDB == nil {
		return nil
	}
	return a.sessionDB
}

// Memory manager tool schema reader, nil without a memory manager
func (a *AIAgent) MemoryToolSchemas() func() ([]any, error) {
	if a.memoryManager == nil {
		return nil
	}
	m, ctx := a.memoryManager, a.homeContext()
	return func() ([]any, error) { return schemaList(m.GetAllToolSchemas(ctx)), nil }
}

// Context engine tool schema reader, nil without a context engine
func (a *AIAgent) ContextEngineToolSchemas() func() ([]any, error) {
	if a.contextCompressor == nil {
		return nil
	}
	engine := a.contextCompressor
	return func() ([]any, error) { return schemaList(engine.GetToolSchemas()), nil }
}

func schemaList(defs []map[string]any) []any {
	out := make([]any, len(defs))
	for i, d := range defs {
		out[i] = d
	}
	return out
}

// Connector tools a side agent never sees, empty for every other agent
func sideAgentToolDrops(a *AIAgent) map[string]bool {
	if a.sideAgent {
		return tools.SideAgentToolDrops
	}
	return map[string]bool{}
}

// Tool names the agent never carries, the side agent drops and the platform toolset drops
func (a *AIAgent) ToolDrops() map[string]bool {
	out := map[string]bool{}
	for n := range sideAgentToolDrops(a) {
		out[n] = true
	}
	for n := range tools.SessionPlatformToolDrops(a.platform) {
		out[n] = true
	}
	return out
}

// Fresh definitions as the agent carries them, drops removed and the joined delegate variant when oneshot
func (a *AIAgent) ToolSurface(defs []map[string]any) []map[string]any {
	return a.toolSurface(a.homeContext(), defs)
}

// Tool surface under the build context whose gateway scope freezes the inline delegation decision
func (a *AIAgent) toolSurface(ctx context.Context, defs []map[string]any) []map[string]any {
	drops := a.ToolDrops()
	kept := make([]map[string]any, 0, len(defs))
	for _, d := range defs {
		if len(drops) == 0 || !drops[snapshotToolName(d)] {
			kept = append(kept, d)
		}
	}
	if a.agentIsOneshot() || tools.AgentJoinsDelegation(ctx, a) {
		joined, err := OneshotToolSchemas(kept)
		if err != nil {
			panic(err)
		}
		return joined
	}
	return kept
}

func snapshotToolName(def map[string]any) string {
	fn, _ := def["function"].(map[string]any)
	name, _ := fn["name"].(string)
	return name
}

func (a *AIAgent) MemoryProviderToolsEnabled(enabled, disabled []string, memoryToolPresent bool) bool {
	return memory.MemoryProviderToolsEnabled(a.homeContext(), a.env, enabled, disabled, memoryToolPresent)
}

// Delegation backend config.yaml has now, what a tool build reads
func (a *AIAgent) SavedDelegationBackend() string {
	return tools.SavedDelegationBackend(a.homeContext())
}

func delegateDef(list []any) any {
	for _, t := range list {
		if d, ok := t.(map[string]any); ok && snapshotToolName(d) == "delegate_task" {
			return d
		}
	}
	return nil
}

// Records the backend as the session's when the carried delegate_task is the one defs carries
func (a *AIAgent) AdoptBuiltBackend(defs []any, backend any) {
	if reflect.DeepEqual(delegateDef(a.tools), delegateDef(defs)) {
		name, _ := backend.(string)
		a.setDelegationBackend(name)
	}
}

// Refreezes the inline delegation decision from the carried delegate_task after a pin restore
func (a *AIAgent) AdoptSessionDelegationMode() {
	if delegateDef(a.tools) != nil {
		a.SetDelegationInline(sessionJoinsDelegation(a))
	}
}

func (a *AIAgent) MessageAgentAuthorized() bool           { return messageAgentAuthorized(a) }
func (a *AIAgent) MessageAgentToolSchema() map[string]any { return messageAgentToolSchema() }

// Claims the delta sink for a stream attempt, superseding every earlier claim
func (a *AIAgent) ClaimStreamWriter() int {
	a.ensureStreamWriterState()
	a.streamWriterLock.Lock()
	a.streamWriterToken++
	token := a.streamWriterToken
	a.streamWriterLock.Unlock()
	a.streamWriterTLS.Set(token)
	return token
}

// Whether no newer stream attempt claimed the sink since the token was issued
func (a *AIAgent) StreamWriterIsCurrent(token int) bool {
	a.ensureStreamWriterState()
	a.streamWriterLock.Lock()
	defer a.streamWriterLock.Unlock()
	return token == a.streamWriterToken
}

var (
	_ mcp.AgentToolSnapshot      = (*AIAgent)(nil)
	_ provider.StreamWriterFence = (*AIAgent)(nil)
	_ provider.StreamDiagAgent   = (*AIAgent)(nil)
)
