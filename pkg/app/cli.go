package app

import (
	"context"
	"fmt"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/nickheyer/icebath/pkg/cliui"
	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/gatewaycli"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/provider"
	"github.com/nickheyer/icebath/pkg/settings"
	"github.com/nickheyer/icebath/pkg/skills"
	"github.com/nickheyer/icebath/pkg/tools"
)

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

// keyboardInterrupt is the panic that unwinds a turn when a shutdown signal cancelled the turn context
type keyboardInterrupt struct{}

// chatTurn holds the per turn state the chat phases and the agent worker share
type chatTurn struct {
	mu                    sync.Mutex
	workerID              atomic.Uint64
	result                map[string]any
	muteNotificationReply bool
}

// cliRuntime snapshots the resolved provider routing an agent is built from
type cliRuntime struct {
	BaseURL           string
	Provider          string
	RequestedProvider string
	APIMode           string
	Command           string
	Args              []string
	CredentialPool    *provider.Pool
	Resolved          *provider.Runtime
}

// The one shot chat client, its turn, stream and session state on one struct
type CLI struct {
	ctx     context.Context
	env     *tools.Env
	console *cliui.Console
	config  map[string]any

	toolProgressMode       string
	bellOnComplete         bool
	showReasoning          bool
	reasoningFull          bool
	modernLayout           bool
	verbose                bool
	streamingEnabled       bool
	showTimestamps         bool
	timestampFormat        string
	finalResponseMarkdown  string
	inlineDiffsEnabled     bool
	streamBuf              string
	reasoningPreviewBuf    string
	streamStarted          bool
	streamBoxOpened        bool
	streamBoxLive          bool
	streamedTextThisTurn   string
	heldStatusLines        []string
	streamTableBuf         []string
	inStreamTable          bool
	streamMu               sync.Mutex
	streamPrefilt          string
	streamMidLine          bool
	streamTextANSI         string
	inReasoningBlock       bool
	reasoningBoxOpened     bool
	reasoningBuf           string
	reasoningStartedAt     float64
	reasoningShownThisTurn bool
	pendingCreditNotices   [][2]string
	toolGenAnnounced       map[string]bool
	runningToolLabels      map[string][]string
	longToolHintFired      bool
	pendingEditSnapshots   map[string]*tools.LocalEditSnapshot

	explicitModelOverride   bool
	model                   string
	modelIsDefault          bool
	explicitAPIKey          string
	typedAPIKey             string
	explicitBaseURL         string
	requestedProvider       string
	explicitProvider        string
	provider                string
	apiMode                 string
	acpCommand              string
	acpArgs                 []string
	baseURL                 string
	apiKey                  string
	credentialPool          *provider.Pool
	resolvedRuntime         *provider.Runtime
	credentialsRateLimited  bool
	credentialsTerminal     bool
	maxTurns                int
	runBudgetSeconds        *float64
	explicitRunBudget       *float64
	explicitMaxTurns        *int
	enabledToolsets         []string
	explicitToolsets        bool
	typedToolsets           []string
	explicitCheckpoints     bool
	passSessionID           bool
	ignoreRules             bool
	explicitIgnoreRules     bool
	systemPrompt            string
	prefillMessages         []any
	reasoningConfig         map[string]any
	explicitReasoningConfig map[string]any
	explicitReasoning       string
	serviceTier             string
	fallbackModel           []map[string]any

	activeAgentRouteSignature string
	agent                     atomic.Pointer[cliAgentSlot]
	conversationHistory       []map[string]any
	sessionStart              time.Time
	sessionDB                 *gatewaycli.GatewaySessionStore
	resumed                   bool
	sessionID                 string
	resumeReadID              string
	resumeHistoryError        string
	singleQueryTurnAuthor     map[string]any
	lastTurnResult            map[string]any
	quietNotifyLingerDone     bool
	ownsProcessNotification   func(map[string]any) bool

	pendingInput                   *inputQueue
	interruptQueue                 *inputQueue
	lastTurnInterrupted            bool
	pendingToolInfo                map[string][]map[string]any
	spinnerText                    string
	lastScrollbackTool             string
	preloadedSkills                []string
	autoLoadSkillsResult           *skillsPromptResult
	preloadSkillsDone              chan struct{}
	preloadSkillsResult            *skillsPromptResult
	preloadSkillsError             error
	preloadSkillsRequested         []string
	preloadSkillsFinalized         bool
	activeSessionLease             ActiveSessionLease
	statusBarSuppressedAfterResize atomic.Bool
	gatewaySessionOpen             bool
	gatewayPluginToolsetCache      *gatewayToolsetCatalog
	turnCtx                        context.Context
	turnCancel                     context.CancelFunc
	signalStop                     func()
	uiMu                           sync.Mutex
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
	activeAgentRef                           *GatewayAgent
	activeAgentRefMu                         sync.Mutex
	sessionEndMessages                       []string
	sessionEndMessagesMu                     sync.Mutex
	singleQueryFinalizeAttemptedSessionIDs   = map[string]bool{}
	sessionIDSetsMu                          sync.Mutex
	osExit                                   = os.Exit
	TryAcquireActiveSession                  = AcquireActiveSessionLease
	oneshotPendingCompletionsPollIntervalSec = 1.0
)

const terminalInputModeResetSeq = "\x1b[?1006l\x1b[?1003l\x1b[?1002l\x1b[?1000l" +
	"\x1b[?1004l" + "\x1b[?2004l" + "\x1b[?1049l" + "\x1b[<u" + "\x1b[>4m" + "\x1b[0m\x1b[?25h"

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
			pkgLog().Warn(fmt.Sprintf("CLI cleanup memory shutdown failed: %v", r))
		}
	}()
	shutdownAgentMemoryProvider(currentActiveAgent())
}

// Runs one cleanup step, swallowing its panic
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

func currentActiveAgent() *GatewayAgent {
	activeAgentRefMu.Lock()
	defer activeAgentRefMu.Unlock()
	return activeAgentRef
}

func setActiveAgent(a *GatewayAgent) {
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

// Builds the skills scope over the active home and the loaded CLI config
func skillsScope(ctx context.Context) *skills.Scope {
	return skills.NewScope(home.Dir(ctx), config.Tree(CLIConfig(ctx)))
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

// SaveConfigValue saves a setting of the home in ctx through the settings writer, true on success
func SaveConfigValue(ctx context.Context, keyPath string, value any) bool {
	if _, err := settings.Set(ctx, keyPath, value, settings.Global); err != nil {
		pkgLog().Error(fmt.Sprintf("Failed to save %s: %v", keyPath, err))
		return false
	}
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

// Returns a map value, the default when the key is absent
func mapValueOr(m map[string]any, key string, def any) any {
	if v, ok := m[key]; ok {
		return v
	}
	return def
}
