package app

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/nickheyer/icebath/pkg/goals"

	"github.com/nickheyer/icebath/pkg/agent"
	"github.com/nickheyer/icebath/pkg/cliui"
	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/pet"
	"github.com/nickheyer/icebath/pkg/plugins"
	"github.com/nickheyer/icebath/pkg/provider"
	"github.com/nickheyer/icebath/pkg/skills"
	"github.com/nickheyer/icebath/pkg/state"
	"github.com/nickheyer/icebath/pkg/tools"
)

var cliLogger = slog.Default().With("logger", "cli")

// inputQueue is the unbounded FIFO the composer, interrupt and pending input paths share
type inputQueue struct {
	mu     sync.Mutex
	items  []any
	notify chan struct{}
}

// Appends one item at the tail and wakes a blocked getter
func (q *inputQueue) Put(item any) {
	q.mu.Lock()
	q.items = append(q.items, item)
	notify := q.waker()
	q.mu.Unlock()
	select {
	case notify <- struct{}{}:
	default:
	}
}

// Wake channel of the queue, created on first use with the lock held
func (q *inputQueue) waker() chan struct{} {
	if q.notify == nil {
		q.notify = make(chan struct{}, 1)
	}
	return q.notify
}

// Removes and returns the head item, waiting up to the timeout for one to arrive
func (q *inputQueue) Get(timeout time.Duration) (any, bool) {
	deadline := time.Now().Add(timeout)
	for {
		q.mu.Lock()
		if len(q.items) > 0 {
			item := q.items[0]
			q.items = q.items[1:]
			notify := q.waker()
			more := len(q.items) > 0
			q.mu.Unlock()
			if more {
				select {
				case notify <- struct{}{}:
				default:
				}
			}
			return item, true
		}
		notify := q.waker()
		q.mu.Unlock()
		remaining := time.Until(deadline)
		if remaining <= 0 {
			return nil, false
		}
		timer := time.NewTimer(remaining)
		select {
		case <-notify:
			timer.Stop()
		case <-timer.C:
		}
	}
}

// Removes and returns the head item, false when the queue is empty
func (q *inputQueue) GetNoWait() (any, bool) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if len(q.items) == 0 {
		return nil, false
	}
	item := q.items[0]
	q.items = q.items[1:]
	return item, true
}

// Reports whether nothing is queued
func (q *inputQueue) Empty() bool {
	q.mu.Lock()
	defer q.mu.Unlock()
	return len(q.items) == 0
}

// Returns the number of queued items
func (q *inputQueue) Len() int {
	q.mu.Lock()
	defer q.mu.Unlock()
	return len(q.items)
}

// syncEvent is a settable flag goroutines can wait on
type syncEvent struct {
	mu  sync.Mutex
	set bool
	ch  chan struct{}
}

func newSyncEvent() *syncEvent { return &syncEvent{ch: make(chan struct{})} }

// Marks the event and releases every waiter
func (e *syncEvent) Set() {
	e.mu.Lock()
	defer e.mu.Unlock()
	if !e.set {
		e.set = true
		close(e.ch)
	}
}

// Clears the event so later waits block again
func (e *syncEvent) Clear() {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.set {
		e.set = false
		e.ch = make(chan struct{})
	}
}

// Reports whether the event is set
func (e *syncEvent) IsSet() bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.set
}

// Blocks until the event is set or the timeout passes, returning the set state
func (e *syncEvent) Wait(timeout time.Duration) bool {
	e.mu.Lock()
	ch := e.ch
	e.mu.Unlock()
	if timeout < 0 {
		<-ch
		return true
	}
	select {
	case <-ch:
		return true
	case <-time.After(timeout):
		return e.IsSet()
	}
}

// keyboardInterrupt is the panic that unwinds a turn when a shutdown signal cancelled the turn context
type keyboardInterrupt struct{}

// voiceInputMessage marks transcribed input so the concise voice prefix never applies to typed text
type voiceInputMessage struct{ Text string }

func (m *voiceInputMessage) String() string { return m.Text }

// seededQueryMessage marks a -q prompt seeded into an interactive session, taken literally
type seededQueryMessage struct {
	Text        string
	Images      []string
	AdmissionID string
}

func newSeededQueryMessage(text string, images []string, admissionID string) *seededQueryMessage {
	return &seededQueryMessage{Text: text, Images: append([]string{}, images...), AdmissionID: admissionID}
}

func (m *seededQueryMessage) String() string { return m.Text }

// chatTurn holds the per turn state the chat phases and the agent worker share
type chatTurn struct {
	mu                    sync.Mutex
	workerID              atomic.Uint64
	result                map[string]any
	muteNotificationReply bool
	useStreamingTTS       bool
	boxOpened             bool
	thinkingStarted       bool
	textQueue             *inputQueue
	ttsDone               chan struct{}
	streamCallback        func(string)
	stopEvent             *syncEvent
	ttsNormalExit         bool
	voicePrefix           string
}

// cliRuntime snapshots the resolved provider routing an agent is built from
type cliRuntime struct {
	APIKey            string
	BaseURL           string
	Provider          string
	RequestedProvider string
	APIMode           string
	Command           string
	Args              []string
	CredentialPool    *provider.Pool
	Resolved          *provider.Runtime
}

// CLI is the classic terminal chat, hermes HermesCLI with every mixin bound onto one struct
type CLI struct {
	ctx     context.Context
	env     *tools.Env
	console *cliui.Console
	config  map[string]any

	compact                      bool
	toolProgressMode             string
	focusViewEnabled             bool
	focusSavedToolProgress       string
	focusLastCountedTool         string
	focusHiddenLines             int
	resumeDisplay                string
	bellOnComplete               bool
	bellOnPrompt                 bool
	showReasoning                bool
	reasoningFull                bool
	modernLayout                 bool
	busyInputMode                string
	verbose                      bool
	streamingEnabled             bool
	showTimestamps               bool
	timestampFormat              string
	finalResponseMarkdown        string
	inlineDiffsEnabled           bool
	turnSummaryEnabled           bool
	spinnerTokenFlowEnabled      bool
	turnSummaryCollector         *agent.TurnSummaryCollector
	turnSummaryStart             float64
	turnTokenBaseline            int
	interactiveTurn              bool
	userMessagePreviewFirstLines int
	userMessagePreviewLastLines  int
	streamBuf                    string
	reasoningPreviewBuf          string
	streamStarted                bool
	streamBoxOpened              bool
	streamBoxLive                bool
	streamedTextThisTurn         string
	heldStatusLines              []string
	streamTableBuf               []string
	inStreamTable                bool
	streamMu                     sync.Mutex
	streamPrefilt                string
	streamMidLine                bool
	streamTextANSI               string
	inReasoningBlock             bool
	reasoningBoxOpened           bool
	reasoningBuf                 string
	reasoningStartedAt           float64
	reasoningShownThisTurn       bool
	pendingCreditNotices         [][2]string
	toolGenAnnounced             map[string]bool
	runningToolLabels            map[string][]string
	longToolHintFired            bool
	lastTurnFailed               bool
	pendingModelSwitchNote       string
	pendingSkillsReloadNote      string
	deferToolWarnings            bool
	reloadMCPHook                func()
	pendingMoaConfig             map[string]any
	pendingMoaDisableAfterTurn   bool
	pendingMoaRestoreModel       map[string]string
	emitStreamTextHook           func(string)
	streamReasoningDeltaHook     func(string)
	pendingEditSnapshots         map[string]*tools.LocalEditSnapshot
	lastInputModeRecovery        *float64
	lastTermiosDriftCheck        *float64
	inputModeRecoveryNoticeShown bool
	termiosDriftNoticeShown      bool

	explicitModelOverride    bool
	model                    string
	modelIsDefault           bool
	explicitAPIKey           string
	typedAPIKey              string
	explicitBaseURL          string
	requestedProvider        string
	explicitProvider         string
	providerSource           string
	provider                 string
	apiMode                  string
	acpCommand               string
	acpArgs                  []string
	baseURL                  string
	apiKey                   string
	credentialPool           *provider.Pool
	resolvedRuntime          *provider.Runtime
	credentialsRateLimited   bool
	credentialsTerminal      bool
	maxTurns                 int
	runBudgetSeconds         *float64
	explicitRunBudget        *float64
	explicitMaxTurns         *int
	enabledToolsets          []string
	explicitToolsets         bool
	typedToolsets            []string
	disabledToolsets         []string
	checkpointsEnabled       bool
	explicitCheckpoints      bool
	checkpointMaxSnapshots   int
	checkpointMaxTotalSizeMB int
	checkpointMaxFileSizeMB  int
	passSessionID            bool
	ignoreRules              bool
	explicitIgnoreRules      bool
	systemPrompt             string
	personalities            map[string]any
	prefillMessages          []any
	reasoningConfig          map[string]any
	explicitReasoningConfig  map[string]any
	explicitReasoning        string
	reasoningUserSet         bool
	serviceTier              string
	providerSort             any
	providersOnly            any
	providersIgnore          any
	providersOrder           any
	providerRequireParams    bool
	providerDataCollection   any
	openRouterMinCodingScore *float64
	fallbackModel            []map[string]any

	activeAgentRouteSignature  string
	agent                      *agent.AIAgent
	toolCallbacksInstalled     bool
	app                        tuiApplication
	conversationHistory        []map[string]any
	sessionStart               time.Time
	promptStartTime            *float64
	promptDuration             float64
	lastTurnFinishedAt         *float64
	sessionDB                  *state.SessionDB
	sessionDBUnavailable       bool
	pendingTitle               string
	resumed                    bool
	sessionID                  string
	historyFile                string
	lastInvalidate             *float64
	resumeReadID               string
	resumeHistoryError         string
	resumeDisplayHistory       []map[string]any
	pendingOneTurnModelRestore map[string]any
	singleQueryMode            bool
	singleQueryTurnAuthor      map[string]any
	lastTurnResult             map[string]any
	quietNotifyLingerDone      bool
	ownsProcessNotification    func(map[string]any) bool

	pendingInput                    *inputQueue
	interruptQueue                  *inputQueue
	agentRunning                    bool
	shouldExit                      bool
	detachOnExit                    bool
	lastTurnInterrupted             bool
	terminalIOBroken                bool
	deleteSessionOnExit             bool
	pendingRelaunch                 []string
	lastCtrlCTime                   float64
	clarifyState                    *clarifyModalState
	clarifyMultiBase                *[]string
	clarifyFreetext                 bool
	clarifyPrefill                  string
	sudoState                       *sudoModalState
	modalInputSnapshot              *modalInputSnapshot
	approvalState                   *approvalModalState
	slashConfirmState               *slashConfirmModalState
	modelPickerState                any
	clarifyDeadline                 *float64
	sudoDeadline                    float64
	approvalDeadline                float64
	slashConfirmDeadline            float64
	approvalLock                    sync.Mutex
	composerPlaceholder             string
	commandPaletteState             *commandPaletteModalState
	secretState                     *secretModalState
	pendingResumeSessions           []map[string]any
	pendingAgentSeed                any
	secretDeadline                  float64
	toolStartTime                   float64
	pendingToolInfo                 map[string][]map[string]any
	spinnerText                     string
	commandStatus                   string
	lastScrollbackTool              string
	commandRunning                  bool
	commandBlocksInput              bool
	petRenderer                     *pet.Renderer
	petAnimRunning                  bool
	petAnimStop                     chan struct{}
	petSlug                         string
	petKittyPending                 string
	petEnabled                      bool
	petCols                         int
	petScale                        float64
	petFramesCache                  map[string][][][]pet.Cell
	petKittyCache                   map[string]*petKittyEntry
	petAnimDone                     chan struct{}
	statusBarTitleSessionID         string
	statusBarTitleCache             string
	statusBarTitleCheckedAt         float64
	statusBarFieldSet               map[string]bool
	statusBarFieldSetLoaded         bool
	voiceRecordKeyDisplayCache      string
	goalManager                     *goals.GoalManager
	automationStale                 bool
	petKittyImageID                 int
	petFrameIdx                     int
	petLock                         sync.Mutex
	petCfgChecked                   float64
	petEventUntil                   float64
	petEvent                        string
	petReasoning                    bool
	petTurnError                    bool
	attachedImages                  []string
	imageCounter                    int
	promptStash                     *config.PromptStash
	preloadedSkills                 []string
	startupSkillsLineShown          bool
	autoLoadSkillsResult            *skillsPromptResult
	preloadSkillsDone               chan struct{}
	preloadSkillsResult             *skillsPromptResult
	preloadSkillsError              error
	preloadSkillsRequested          []string
	preloadSkillsFinalized          bool
	activeSessionLease              ActiveSessionLease
	voiceLock                       sync.Mutex
	voiceMode                       bool
	voiceTTS                        bool
	voiceRecording                  bool
	voiceProcessing                 bool
	voiceContinuous                 bool
	voiceRecorder                   any
	voiceTTSStop                    *syncEvent
	voiceLiveText                   string
	voiceTTSDone                    *syncEvent
	voiceBargeCapture               *syncEvent
	voiceLastTTSText                string
	voiceBargePhase                 string
	statusBarVisible                bool
	batteryVisible                  bool
	vimMode                         bool
	statusBarSuppressedAfterResize  bool
	resizeRecoveryPending           bool
	resizeRecoveryLock              sync.Mutex
	resizeRecoveryTimer             *time.Timer
	statusBarUnsuppressTimer        *time.Timer
	lastResizeWidth                 *int
	backgroundTasks                 map[string]chan struct{}
	backgroundTaskCounter           int
	cacheHitBaselinePrompt          int
	cacheHitBaselineRead            int
	cacheHitBaselineCompressions    int
	cacheHitBaselineModel           string
	seededFirstMessage              *seededQueryMessage
	gatewaySessionOpen              bool
	backgroundReviewNoticeDelivered bool
	turnCtx                         context.Context
	turnCancel                      context.CancelFunc
	signalStop                      func()
	uiMu                            sync.Mutex
	connectionState                 any
	tui                             tuiRunState
	subagentMonitor                 *subagentMonitor
}

// petKittyEntry is one cached kitty payload of a pet state with the image id it was encoded for
type petKittyEntry struct {
	payload *pet.KittyPayload
	imageID int
}

// skillsPromptResult is the (prompt, loaded names, missing names) triple a skills preload renders
type skillsPromptResult struct {
	Prompt  string
	Loaded  []string
	Missing []string
}

// ActiveSessionLease is the global active session slot a CLI process holds until release
type ActiveSessionLease interface{ Release() error }

var (
	activeWorktree                           map[string]string
	activeWorktreeMu                         sync.Mutex
	cleanupDone                              bool
	cleanupInProgress                        bool
	cleanupMu                                sync.Mutex
	tuiInputModesActive                      bool
	tuiInputModesMu                          sync.Mutex
	signalWatchdogArmed                      bool
	signalWatchdogMu                         sync.Mutex
	activeAgentRef                           *agent.AIAgent
	activeAgentRefMu                         sync.Mutex
	sessionEndMessages                       []string
	sessionEndMessagesMu                     sync.Mutex
	cliWakeOwner                             any
	handedOffSessionIDs                      = map[string]bool{}
	singleQueryFinalizeAttemptedSessionIDs   = map[string]bool{}
	sessionIDSetsMu                          sync.Mutex
	skillCommandsCache                       skills.SkillCommands
	skillBundlesCache                        skills.SkillCommands
	skillCacheMu                             sync.Mutex
	reverseAliasCache                        map[string]string
	reverseAliasMu                           sync.Mutex
	outputHistoryMu                          sync.Mutex
	outputHistoryEnabled                     = true
	outputHistoryReplaying                   bool
	outputHistorySuppressed                  bool
	outputHistoryMaxLines                    = 200
	outputHistory                            []any
	activeTUIApp                             tuiApplication
	activeTUIAppMu                           sync.Mutex
	osExit                                   = os.Exit
	StopWakeWordListening                    func(owner any)
	EmergencyCleanupAllBrowserSessions       func()
	TryAcquireActiveSession                  func(ctx context.Context, sessionID, surface string, cfg map[string]any, metadata map[string]any) (ActiveSessionLease, string, error)
	FormatRefusalStderr                      func(message string) string
	SecurityAdvisoriesStartupBanner          func(ctx context.Context) string
	oneshotPendingCompletionsPollIntervalSec = 1.0
)

const terminalInputModeResetSeq = "\x1b[?1006l\x1b[?1003l\x1b[?1002l\x1b[?1000l" +
	"\x1b[?1004l" + "\x1b[?2004l" + "\x1b[?1049l" + "\x1b[<u" + "\x1b[>4m" + "\x1b[0m\x1b[?25h"

// Records that the TUI app started so runCleanup resets input modes
func markTUIInputModesActive() {
	tuiInputModesMu.Lock()
	tuiInputModesActive = true
	tuiInputModesMu.Unlock()
}

// Arms the exit backstop at twice the cleanup leash the moment a termination signal arrives
func armExitWatchdogOnShutdownSignal(ctx context.Context) {
	signalWatchdogMu.Lock()
	armed := signalWatchdogArmed
	signalWatchdogArmed = true
	signalWatchdogMu.Unlock()
	if armed {
		return
	}
	base := exitWatchdogTimeout()
	if base <= 0 {
		return
	}
	armExitWatchdog(ctx, base*2, true)
}

// Runs resource cleanup exactly once, resetting terminal input modes before the slow teardown steps
func runCleanup(ctx context.Context, env *tools.Env, notifySessionFinalize bool) {
	cleanupMu.Lock()
	if cleanupDone {
		cleanupMu.Unlock()
		return
	}
	cleanupDone = true
	cleanupInProgress = true
	cleanupMu.Unlock()
	defer func() {
		cleanupMu.Lock()
		cleanupInProgress = false
		cleanupMu.Unlock()
	}()
	armExitWatchdog(ctx, exitWatchdogTimeout(), false)
	resetTerminalInputModesOnExit()
	for _, step := range cleanupSteps {
		runCleanupStep(step, ctx, env)
	}
	if notifySessionFinalize {
		cleanupSessionID := ""
		if a := currentActiveAgent(); a != nil {
			cleanupSessionID = a.SessionID()
		}
		if shouldEmitCleanupSessionFinalize(cleanupSessionID) {
			appendSessionEndMessages(notifySessionFinalizeMessages(ctx, cleanupSessionID, "cli", "shutdown"))
		}
	}
	defer func() {
		if r := recover(); r != nil {
			cliLogger.Warn(fmt.Sprintf("CLI cleanup memory shutdown failed: %v", r))
		}
	}()
	shutdownAgentMemoryProvider(ctx, currentActiveAgent())
}

// Runs one cleanup step, swallowing its panic the way hermes suppresses the step's exception
func runCleanupStep(step func(context.Context, *tools.Env), ctx context.Context, env *tools.Env) {
	defer func() { _ = recover() }()
	step(ctx, env)
}

// Reports whether cleanup is running on some goroutine right now
func cleanupRunning() bool {
	cleanupMu.Lock()
	defer cleanupMu.Unlock()
	return cleanupInProgress
}

// Writes the focus and mouse tracking reset once when the TUI ran, to stdout when a tty else /dev/tty
func resetTerminalInputModesOnExit() {
	tuiInputModesMu.Lock()
	active := tuiInputModesActive
	tuiInputModesActive = false
	tuiInputModesMu.Unlock()
	if !active {
		return
	}
	if c := console(); c.StdoutIsTTY() {
		if _, err := fmt.Fprint(c.Out, terminalInputModeResetSeq); err == nil {
			return
		}
	}
	tty, err := os.OpenFile("/dev/tty", os.O_WRONLY, 0)
	if err != nil {
		return
	}
	defer tty.Close()
	_, _ = tty.WriteString(terminalInputModeResetSeq)
}

func currentActiveAgent() *agent.AIAgent {
	activeAgentRefMu.Lock()
	defer activeAgentRefMu.Unlock()
	return activeAgentRef
}

func setActiveAgent(a *agent.AIAgent) {
	activeAgentRefMu.Lock()
	activeAgentRef = a
	activeAgentRefMu.Unlock()
}

func appendSessionEndMessages(lines []string) {
	sessionEndMessagesMu.Lock()
	sessionEndMessages = append(sessionEndMessages, lines...)
	sessionEndMessagesMu.Unlock()
}

// Returns and clears the session end messages collected for the exit summary
func takeSessionEndMessages() []string {
	sessionEndMessagesMu.Lock()
	defer sessionEndMessagesMu.Unlock()
	lines := sessionEndMessages
	sessionEndMessages = nil
	return lines
}

// Reports whether a session id was handed off to another owner
func sessionHandedOff(sessionID string) bool {
	sessionIDSetsMu.Lock()
	defer sessionIDSetsMu.Unlock()
	return handedOffSessionIDs[sessionID]
}

// Marks a session id as handed off so shutdown leaves its rows alone
func markSessionHandedOff(sessionID string) {
	sessionIDSetsMu.Lock()
	handedOffSessionIDs[sessionID] = true
	sessionIDSetsMu.Unlock()
}

// Returns the active TUI application when run owns the screen, nil otherwise
func tuiApp() tuiApplication {
	activeTUIAppMu.Lock()
	defer activeTUIAppMu.Unlock()
	return activeTUIApp
}

func setTUIApp(app tuiApplication) {
	activeTUIAppMu.Lock()
	activeTUIApp = app
	activeTUIAppMu.Unlock()
}

// Configures the recent CLI output replayed after terminal redraws
func configureOutputHistory(enabled bool, maxLines any) {
	outputHistoryMu.Lock()
	defer outputHistoryMu.Unlock()
	outputHistoryEnabled = enabled
	outputHistoryMaxLines = coerceOutputHistoryLimit(maxLines)
	outputHistory = nil
}

// Suppresses output history recording until the returned restore function runs
func suspendOutputHistory() func() {
	outputHistoryMu.Lock()
	old := outputHistorySuppressed
	outputHistorySuppressed = true
	outputHistoryMu.Unlock()
	return func() {
		outputHistoryMu.Lock()
		outputHistorySuppressed = old
		outputHistoryMu.Unlock()
	}
}

// Returns the cached interactive skill commands, scanning them on first use
func ensureSkillCommands(ctx context.Context) skills.SkillCommands {
	skillCacheMu.Lock()
	defer skillCacheMu.Unlock()
	if skillCommandsCache != nil {
		return skillCommandsCache
	}
	cmds, err := skillsScope(ctx).GetInteractiveSkillCommands(ctx)
	if err != nil {
		cliLogger.Debug(fmt.Sprintf("interactive skill commands scan failed: %v", err))
	}
	return cmds
}

// GetSkillCommands returns the interactive skill command table the slash dispatcher matches
func GetSkillCommands(ctx context.Context) skills.SkillCommands { return ensureSkillCommands(ctx) }

// Returns the skill commands this CLI's own process scans, the Go CLI owning its own session
func sessionSkillCommands(c *CLI) skills.SkillCommands { return ensureSkillCommands(c.ctx) }

// Returns the cached skill bundle table, loading it on first use
func getSkillBundles(ctx context.Context) skills.SkillCommands {
	skillCacheMu.Lock()
	defer skillCacheMu.Unlock()
	if skillBundlesCache == nil {
		bundles, err := skillsScope(ctx).GetSkillBundles()
		if err != nil {
			cliLogger.Debug(fmt.Sprintf("skill bundles scan failed: %v", err))
		}
		skillBundlesCache = bundles
	}
	return skillBundlesCache
}

// Builds the skills scope over the active home and the loaded CLI config
func skillsScope(ctx context.Context) *skills.Scope {
	return skills.NewScope(home.Dir(ctx), config.Tree(CLIConfig(ctx)))
}

// Returns the plugin command names without their slash prefix for dispatch matching
func getPluginCmdHandlerNames(ctx context.Context) map[string]bool {
	names := map[string]bool{}
	for name := range plugins.GetPluginCommands(ctx) {
		names[name] = true
	}
	return names
}

// Normalizes repeated or comma separated skills flags into a deduplicated identifier list
func parseSkillsArgument(skillsArg []string) []string {
	out := []string{}
	seen := map[string]bool{}
	for _, raw := range skillsArg {
		for _, p := range strings.Split(raw, ",") {
			p = strings.TrimSpace(p)
			if p == "" || seen[p] {
				continue
			}
			seen[p] = true
			out = append(out, p)
		}
	}
	return out
}

// SaveConfigValue persists a dot separated key into ICEBATH_HOME config.yaml, true on success
func SaveConfigValue(ctx context.Context, keyPath string, value any) bool {
	configPath := filepath.Join(home.Dir(ctx), "config.yaml")
	if _, err := home.Mkdir(ctx, filepath.Dir(configPath)); err != nil {
		cliLogger.Error(fmt.Sprintf("Failed to save config: %v", err))
		return false
	}
	if err := config.WriteConfigKey(ctx, configPath, keyPath, value); err != nil {
		cliLogger.Error(fmt.Sprintf("Failed to save config: %v", err))
		return false
	}
	_ = os.Chmod(configPath, 0o600)
	return true
}

// Turns a moa prefixed model into the moa provider with the preset after the prefix as model, anything else unchanged
func normalizeMoaModel(model string) (string, string) {
	trimmed := strings.TrimSpace(model)
	if strings.HasPrefix(strings.ToLower(trimmed), "moa:") {
		preset := strings.TrimSpace(trimmed[len("moa:"):])
		if preset != "" {
			return "moa", preset
		}
	}
	return "", model
}

// Reports whether -q seeds an interactive session, only on a real TTY without oneshot or quiet
func shouldSeedInteractive(query, image string, quiet, oneshot bool) bool {
	if (query == "" && image == "") || oneshot || quiet {
		return false
	}
	c := console()
	return c.StdinIsTTY() && c.StdoutIsTTY()
}

// Returns the shortest configured alias for a model name, else the name itself
func reverseAliasForDisplay(ctx context.Context, modelName string) string {
	if modelName == "" {
		return modelName
	}
	reverseAliasMu.Lock()
	defer reverseAliasMu.Unlock()
	if reverseAliasCache == nil {
		rmap := map[string]string{}
		put := func(m, alias string) {
			if m == "" {
				return
			}
			if cur, ok := rmap[m]; !ok || len(alias) < len(cur) {
				rmap[m] = alias
			}
		}
		cfg, err := config.Load(home.Dir(ctx), nil)
		if err != nil {
			cfg = config.Tree{}
		}
		if ma, ok := cfg["model_aliases"].(map[string]any); ok {
			for alias, entry := range ma {
				if e, ok := entry.(map[string]any); ok {
					put(strings.TrimSpace(cliStr(e["model"], "")), alias)
				}
			}
		}
		if mdl, ok := cfg["model"].(map[string]any); ok {
			if simple, ok := mdl["aliases"].(map[string]any); ok {
				for alias, val := range simple {
					v, isStr := val.(string)
					if !isStr || strings.TrimSpace(v) == "" {
						continue
					}
					v = strings.TrimSpace(v)
					if i := strings.Index(v, "/"); i >= 0 {
						v = v[i+1:]
					}
					put(v, alias)
				}
			}
		}
		reverseAliasCache = rmap
	}
	if alias, ok := reverseAliasCache[modelName]; ok {
		return alias
	}
	return modelName
}

// Returns the string form of a config value, the fallback for nil or empty
func cliStr(v any, def string) string {
	switch t := v.(type) {
	case nil:
		return def
	case string:
		if t == "" {
			return def
		}
		return t
	case bool:
		if t {
			return "True"
		}
		return "False"
	default:
		return fmt.Sprint(t)
	}
}

// Returns the Python truthiness of a config value
func cliTruthy(v any) bool {
	switch t := v.(type) {
	case nil:
		return false
	case bool:
		return t
	case string:
		return t != ""
	case int:
		return t != 0
	case int64:
		return t != 0
	case float64:
		return t != 0
	case []any:
		return len(t) > 0
	case []string:
		return len(t) > 0
	case map[string]any:
		return len(t) > 0
	default:
		return true
	}
}

// Returns a nested config map, an empty map when the key is absent or not a map
func cliMap(cfg map[string]any, key string) map[string]any {
	if m, ok := cfg[key].(map[string]any); ok {
		return m
	}
	return map[string]any{}
}

// Returns the string list a config value holds, nil when it is not a list
func cliStrList(v any) []string {
	switch t := v.(type) {
	case []string:
		return append([]string{}, t...)
	case []any:
		out := make([]string, 0, len(t))
		for _, x := range t {
			out = append(out, fmt.Sprint(x))
		}
		return out
	}
	return nil
}

// Returns a map value, the default when the key is absent
func mapValueOr(m map[string]any, key string, def any) any {
	if v, ok := m[key]; ok {
		return v
	}
	return def
}
