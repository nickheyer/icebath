package acp

import (
	"context"
	"errors"
	"os"
	"sync"
	"testing"

	"github.com/nickheyer/icebath/pkg/agent"
	"github.com/nickheyer/icebath/pkg/internal/pyacp"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/memory"
	"github.com/nickheyer/icebath/pkg/state"
	"github.com/nickheyer/icebath/pkg/tools"
)

func swap[T any](t *testing.T, target *T, value T) {
	t.Helper()
	previous := *target
	*target = value
	t.Cleanup(func() { *target = previous })
}

type nsAgent struct {
	model, provider, baseURL, apiMode string
	printFn                           func(string)
}

func (a *nsAgent) Model() string              { return a.model }
func (a *nsAgent) Provider() string           { return a.provider }
func (a *nsAgent) BaseURL() string            { return a.baseURL }
func (a *nsAgent) APIMode() string            { return a.apiMode }
func (a *nsAgent) SetPrintFn(fn func(string)) { a.printFn = fn }

type mockAgent struct {
	mu                   sync.Mutex
	model, provider      string
	baseURL, apiMode     string
	apiKey, sessionID    string
	hasSessionID         bool
	enabledToolsets      []string
	disabledToolsets     []string
	cachedSystemPrompt   string
	toolDefs             []map[string]any
	validToolNames       map[string]bool
	contextLength        int
	thresholdTokens      int
	lastPromptTokens     int
	compressionDisabled  bool
	runResult            map[string]any
	runErr               error
	run                  func(ctx context.Context, userMessage any, o agent.RunConversationOptions) (map[string]any, error)
	runCalls             []mockRun
	steerResult          bool
	steerCalls           []string
	redirectResult       bool
	redirectCalls        []string
	supportsRedirect     bool
	interrupts           int
	resetCalls           int
	resetPanic           any
	toolProgressCallback any
	thinkingCallback     any
	reasoningCallback    any
	stepCallback         any
	streamDeltaCallback  any
	onSessionTitle       func(title, source string)
	db                   *state.DB
	dbCreated            bool
	invalidations        int
	userTurnCount        int
	apiCallCount         int
	env                  *tools.Env
	memoryManager        *memory.MemoryManager
	storeDetached        bool
	delegationBackend    string
	interrupt            func()
}

type mockRun struct {
	userMessage any
	options     agent.RunConversationOptions
}

func (a *mockAgent) Model() string              { return a.model }
func (a *mockAgent) Provider() string           { return a.provider }
func (a *mockAgent) BaseURL() string            { return a.baseURL }
func (a *mockAgent) APIMode() string            { return a.apiMode }
func (a *mockAgent) APIKey() string             { return a.apiKey }
func (a *mockAgent) EnabledToolsets() []string  { return a.enabledToolsets }
func (a *mockAgent) DisabledToolsets() []string { return a.disabledToolsets }
func (a *mockAgent) CachedSystemPrompt() string { return a.cachedSystemPrompt }
func (a *mockAgent) ToolDefs() []map[string]any { return a.toolDefs }
func (a *mockAgent) SessionDB() *state.DB       { return a.db }
func (a *mockAgent) SessionDBCreated() bool     { return a.dbCreated }
func (a *mockAgent) CompressionEnabled() bool   { return !a.compressionDisabled }
func (a *mockAgent) UserTurnCount() int         { return a.userTurnCount }
func (a *mockAgent) APICallCount() int          { return a.apiCallCount }
func (a *mockAgent) SessionID() string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.sessionID
}
func (a *mockAgent) CompressorFigures() (int, int, int) {
	return a.contextLength, a.thresholdTokens, a.lastPromptTokens
}
func (a *mockAgent) SetToolProgressCallback(cb any) { a.toolProgressCallback = cb }
func (a *mockAgent) SetThinkingCallback(cb any)     { a.thinkingCallback = cb }
func (a *mockAgent) SetReasoningCallback(cb any)    { a.reasoningCallback = cb }
func (a *mockAgent) SetStepCallback(cb any)         { a.stepCallback = cb }
func (a *mockAgent) SetStreamDeltaCallback(cb any)  { a.streamDeltaCallback = cb }
func (a *mockAgent) SetOnSessionTitle(cb func(title, source string)) {
	a.onSessionTitle = cb
}
func (a *mockAgent) SetToolsets(enabled, disabled []string) {
	a.enabledToolsets, a.disabledToolsets = enabled, disabled
}
func (a *mockAgent) SetToolSnapshot(defs []map[string]any, names map[string]bool) {
	a.toolDefs, a.validToolNames = defs, names
}
func (a *mockAgent) InvalidateSystemPrompt(ctx context.Context) error {
	a.invalidations++
	return nil
}
func (a *mockAgent) RunConversationContent(ctx context.Context, userMessage any, o agent.RunConversationOptions) (map[string]any, error) {
	a.mu.Lock()
	a.runCalls = append(a.runCalls, mockRun{userMessage, o})
	run := a.run
	a.mu.Unlock()
	if run != nil {
		return run(ctx, userMessage, o)
	}
	return a.runResult, a.runErr
}
func (a *mockAgent) Steer(text string) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.steerCalls = append(a.steerCalls, text)
	return a.steerResult
}
func (a *mockAgent) Redirect(text string) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.redirectCalls = append(a.redirectCalls, text)
	return a.redirectResult
}
func (a *mockAgent) SupportsActiveTurnRedirect() bool { return a.supportsRedirect }
func (a *mockAgent) Interrupt(message string) {
	a.mu.Lock()
	a.interrupts++
	hook := a.interrupt
	a.mu.Unlock()
	if hook != nil {
		hook()
	}
}
func (a *mockAgent) Env() *tools.Env { return a.env }
func (a *mockAgent) MemoryManager() tools.MemoryToolManager {
	if a.memoryManager == nil {
		return nil
	}
	return a.memoryManager
}
func (a *mockAgent) SetDelegationBackend(backend string) { a.delegationBackend = backend }
func (a *mockAgent) WithSessionStoreDetached(fn func()) {
	a.storeDetached = true
	defer func() { a.storeDetached = false }()
	fn()
}
func (a *mockAgent) ResetSessionState() {
	a.resetCalls++
	if a.resetPanic != nil {
		panic(a.resetPanic)
	}
}

func mockFactory() any {
	return &mockAgent{env: &tools.Env{Home: os.Getenv("ICEBATH_HOME"), Values: &sync.Map{}}}
}

type connUpdate struct {
	sessionID string
	update    pyacp.SessionUpdate
}

type captureConn struct {
	mu         sync.Mutex
	updates    []connUpdate
	permission func(options []pyacp.PermissionOption, sessionID string, toolCall *pyacp.ToolCallUpdate) (*pyacp.RequestPermissionResponse, error)
}

func (c *captureConn) SessionUpdate(ctx context.Context, sessionID string, update pyacp.SessionUpdate, meta *pyjson.Object) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.updates = append(c.updates, connUpdate{sessionID, update})
	return nil
}

func (c *captureConn) RequestPermission(ctx context.Context, options []pyacp.PermissionOption, sessionID string, toolCall *pyacp.ToolCallUpdate, meta *pyjson.Object) (*pyacp.RequestPermissionResponse, error) {
	if c.permission != nil {
		return c.permission(options, sessionID, toolCall)
	}
	return &pyacp.RequestPermissionResponse{Outcome: &pyacp.AllowedOutcome{OptionID: "allow_once"}}, nil
}

func (c *captureConn) seen() []connUpdate {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]connUpdate(nil), c.updates...)
}

func (c *captureConn) reset() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.updates = nil
}

func textPrompt(sessionID, text string) *pyacp.PromptRequest {
	return &pyacp.PromptRequest{SessionID: sessionID, Prompt: []pyacp.ContentBlock{pyacp.TextBlock(text)}}
}

func newTestAgent(t *testing.T) (*HermesACPAgent, *SessionManager) {
	t.Helper()
	m := newMockManager(t)
	return NewHermesACPAgent(m), m
}

func newMockManager(t *testing.T) *SessionManager {
	t.Helper()
	t.Setenv("ICEBATH_HOME", t.TempDir())
	m := NewSessionManager(mockFactory, nil)
	t.Cleanup(func() {
		m.dbLock.Lock()
		db := m.dbInstance
		m.dbLock.Unlock()
		if db != nil {
			state.Release(db)
		}
	})
	return m
}

func mustCreate(t *testing.T, m *SessionManager, cwd string) *SessionState {
	t.Helper()
	st, err := m.CreateSession(context.Background(), cwd)
	if err != nil {
		t.Fatal(err)
	}
	return st
}

func dropFromMemory(m *SessionManager, sessionID string) {
	m.lock.Lock()
	defer m.lock.Unlock()
	delete(m.sessions, sessionID)
	for i, sid := range m.order {
		if sid == sessionID {
			m.order = append(m.order[:i], m.order[i+1:]...)
			break
		}
	}
}

var errNoStore = errors.New("store unavailable")
