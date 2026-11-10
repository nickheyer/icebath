package memory

import (
	"context"
	"fmt"
	"regexp"
	"strings"
	"sync"
	"time"
)

// Checkpoint contract version a provider advertises when every successful pre compress hook is durable
const PreCompressCheckpointAPIVersion = 2

// Checkpoint version of providers on the best effort contract
const legacyPreCompressAPIVersion = 1

// Default mark for recall indicators, providers may use their own brand mark
const IndicatorGlyph = "\U0001F9E0"

// Values of memory.provider that select the built in store rather than an external plugin
var coreMemoryProviderSentinels = map[string]bool{"": true, "default": true, "builtin": true, "built-in": true, "none": true}

// Reports whether memory.provider selects the built in store rather than an external plugin
func IsCoreMemoryProvider(name string) bool {
	return coreMemoryProviderSentinels[pyLower(pyStrip(name))]
}

// What the last prefetch injected, a zero count meaning content without a discrete count
type RecallStatus struct {
	ProviderLabel string
	Count         int
	Glyph         string
}

// Recall status carrying the default indicator glyph
func NewRecallStatus(providerLabel string, count int) *RecallStatus {
	return &RecallStatus{ProviderLabel: providerLabel, Count: count, Glyph: IndicatorGlyph}
}

// Prompts with no semantic signal, anchored and followed only by whitespace or punctuation
var trivialPromptRe = regexp.MustCompile(`(?i)^(yes|no|ok|okay|sure|thanks|thank you|y|n|yep|nope|yeah|nah|` +
	`hi|hey|hello|yo|sup|` +
	`continue|go ahead|do it|proceed|got it|cool|nice|great|done|next|lgtm|k)` +
	`[` + pySpaceClass + `!?.:;,"'~\x{2018}\x{2019}\x{201c}\x{201d}\x{2014}\x{2013}\x{2026}()\[\]{}<>*&^%$#@!+=` + "`" + `\x{a0}]*$`)

// True for empty input, slash commands and bare greetings or acknowledgements
func IsTrivialPrompt(text string) bool {
	stripped := pyStrip(text)
	if stripped == "" || strings.HasPrefix(stripped, "/") {
		return true
	}
	return trivialPromptRe.MatchString(stripped)
}

// Scoping values a provider binds to once at agent startup
type InitOptions struct {
	Home               string
	Platform           string
	AgentContext       string
	AgentIdentity      string
	AgentWorkspace     string
	AgentID            string
	ParentSessionID    string
	SessionTitle       string
	SessionTitleSource string
	Cwd                string
	UserID             string
	UserIDAlt          string
	UserName           string
	ChatID             string
	ChatName           string
	ChatType           string
	ThreadID           string
	GatewaySessionKey  string
	Warn               func(string)
	Status             func(string)
}

// Optional values of one completed turn
type SyncOptions struct {
	SessionID  string
	Messages   []map[string]any
	TurnAuthor map[string]any
}

// Profile a provider tool call runs for
type ToolCallOptions struct {
	Home          string
	AgentIdentity string
	CallerGSK     string
}

// Who wrote this turn, empty id and name when nobody did
type TurnOptions struct {
	AuthorID    string
	AuthorName  string
	AuthorIsBot bool
}

// Session id rotation without teardown, Reset only for a genuinely new conversation, Rewound for a truncated transcript
type SessionSwitchOptions struct {
	ParentSessionID string
	Reset           bool
	Rewound         bool
	Reason          string
}

// Child session of a completed delegation
type DelegationOptions struct {
	ChildSessionID string
}

// One pluggable memory backend driven by the memory manager
type Provider interface {
	// Short identifier such as builtin, byterover or retaindb
	Name() string
	// Configured, credentialed and ready, config and dependencies only with no network
	Available(ctx context.Context) bool
	// User facing hint for the provider unavailable warning
	UnavailableReason(ctx context.Context) string
	// Binds connections, stores and threads to the profile once at agent startup
	Initialize(ctx context.Context, sessionID string, opts InitOptions) error
	// Static system prompt text, empty to skip
	SystemPromptBlock(ctx context.Context) (string, error)
	// Formatted recall context for the upcoming turn, fast and cached
	Prefetch(ctx context.Context, query, sessionID string) (string, error)
	// Background recall after a turn that the next prefetch consumes
	QueuePrefetch(ctx context.Context, query, sessionID string) error
	// What the most recent prefetch injected, nil for no indicator
	RecallStatus(ctx context.Context) (*RecallStatus, error)
	// Persists a completed turn without blocking
	SyncTurn(ctx context.Context, userContent, assistantContent string, opts SyncOptions) error
	// Function calling schemas with name, description and parameters
	ToolSchemas(ctx context.Context) ([]map[string]any, error)
	// Handles one of the provider tools, returning a JSON string
	HandleToolCall(ctx context.Context, toolName string, args map[string]any, opts ToolCallOptions) (string, error)
	// Flushes queues and closes connections
	Shutdown(ctx context.Context) error
	// Per turn tick naming the turn author
	OnTurnStart(ctx context.Context, turnNumber int, message string, opts TurnOptions) error
	// Identity mapping values that bust a cached gateway agent when they change
	IdentitySignature(ctx context.Context) map[string]any
	// End of session extraction at real session boundaries
	OnSessionEnd(ctx context.Context, messages []map[string]any) error
	// Rebinds per session state after a session id rotation
	OnSessionSwitch(ctx context.Context, newSessionID string, opts SessionSwitchOptions) error
	// Insights from messages about to be compressed, fed into the summary prompt
	OnPreCompress(ctx context.Context, messages []map[string]any, requireCheckpoint bool) (string, error)
	// Advertised pre compress checkpoint contract version
	CheckpointAPIVersion() int
	// Parent side observation of a completed delegation
	OnDelegation(ctx context.Context, task, result string, opts DelegationOptions) error
	// Setup fields for icebath memory setup
	ConfigSchema() []map[string]any
	// Writes non secret setup values to the provider native config
	SaveConfig(values map[string]any, icebathHome string) error
	// Mirrors a built in memory tool write, previous_content in metadata for replace and remove
	OnMemoryWrite(ctx context.Context, action, target, content string, metadata map[string]any) error
	// Absolute paths of provider state outside the home for backup and import, the error a failing provider raised
	BackupPaths() ([]string, error)
}

// Default tool call refusal of a provider that exposes no such tool
type NotHandledError struct {
	Provider string
	Tool     string
}

func (e *NotHandledError) Error() string {
	return fmt.Sprintf("Provider %s does not handle tool %s", e.Provider, e.Tool)
}

// Optional hook defaults so a provider overrides only what it opts into
type BaseProvider struct{}

func (BaseProvider) UnavailableReason(ctx context.Context) string { return "" }

func (BaseProvider) SystemPromptBlock(ctx context.Context) (string, error) { return "", nil }

func (BaseProvider) Prefetch(ctx context.Context, query, sessionID string) (string, error) {
	return "", nil
}

func (BaseProvider) QueuePrefetch(ctx context.Context, query, sessionID string) error { return nil }

func (BaseProvider) RecallStatus(ctx context.Context) (*RecallStatus, error) { return nil, nil }

func (BaseProvider) SyncTurn(ctx context.Context, userContent, assistantContent string, opts SyncOptions) error {
	return nil
}

// Refuses every tool, the manager fills in the provider name
func (BaseProvider) HandleToolCall(ctx context.Context, toolName string, args map[string]any, opts ToolCallOptions) (string, error) {
	return "", &NotHandledError{Tool: toolName}
}

func (BaseProvider) Shutdown(ctx context.Context) error { return nil }

func (BaseProvider) OnTurnStart(ctx context.Context, turnNumber int, message string, opts TurnOptions) error {
	return nil
}

func (BaseProvider) IdentitySignature(ctx context.Context) map[string]any { return map[string]any{} }

func (BaseProvider) OnSessionEnd(ctx context.Context, messages []map[string]any) error { return nil }

func (BaseProvider) OnSessionSwitch(ctx context.Context, newSessionID string, opts SessionSwitchOptions) error {
	return nil
}

func (BaseProvider) OnPreCompress(ctx context.Context, messages []map[string]any, requireCheckpoint bool) (string, error) {
	return "", nil
}

func (BaseProvider) CheckpointAPIVersion() int { return legacyPreCompressAPIVersion }

func (BaseProvider) OnDelegation(ctx context.Context, task, result string, opts DelegationOptions) error {
	return nil
}

func (BaseProvider) ConfigSchema() []map[string]any { return []map[string]any{} }

func (BaseProvider) SaveConfig(values map[string]any, icebathHome string) error { return nil }

func (BaseProvider) OnMemoryWrite(ctx context.Context, action, target, content string, metadata map[string]any) error {
	return nil
}

func (BaseProvider) BackupPaths() ([]string, error) { return []string{}, nil }

// Spawner context values without its cancellation, so a background job keeps the profile scope
func CtxBound(ctx context.Context) context.Context {
	if ctx == nil {
		return context.Background()
	}
	return context.WithoutCancel(ctx)
}

// Background job that runs under its spawner context values
type ContextThread struct {
	Name    string
	ctx     context.Context
	fn      func(ctx context.Context)
	once    sync.Once
	started chan struct{}
	done    chan struct{}
}

// Unstarted job running fn under the spawner profile scope, every provider background job goes through it
func SpawnContextThread(ctx context.Context, name string, fn func(ctx context.Context)) *ContextThread {
	return &ContextThread{Name: name, ctx: CtxBound(ctx), fn: fn, started: make(chan struct{}), done: make(chan struct{})}
}

// Starts the job once
func (t *ContextThread) Start() {
	t.once.Do(func() {
		close(t.started)
		go func() {
			defer close(t.done)
			t.fn(t.ctx)
		}()
	})
}

// True between start and completion
func (t *ContextThread) Alive() bool {
	select {
	case <-t.started:
	default:
		return false
	}
	select {
	case <-t.done:
		return false
	default:
		return true
	}
}

// Waits for completion up to timeout, forever when timeout is negative, immediately for an unstarted job
func (t *ContextThread) Join(timeout time.Duration) {
	select {
	case <-t.started:
	default:
		return
	}
	if timeout < 0 {
		<-t.done
		return
	}
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case <-t.done:
	case <-timer.C:
	}
}
