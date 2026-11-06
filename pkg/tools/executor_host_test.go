package tools

import (
	"context"
	"sync"
	"sync/atomic"

	"github.com/nickheyer/icebath/pkg/state"
)

// Records activity and exposes an interrupt flag the way the Python fake agent does
type fakeExecutorHost struct {
	env         *Env
	interrupted atomic.Bool
	reason      string
	mu          sync.Mutex
	activity    []string
	enabled     []string
	disabled    []string
	ctxLength   int
	quiet       *bool
	printed     []string
	startCb     func(string, string, map[string]any)
	completeCb  func(string, string, map[string]any, string)
	progressCb  func(string, string, string, map[string]any, map[string]any)
	flush       func([]*state.Message) (bool, error)
	ckptMgr     *CheckpointManager
	memory      MemoryToolManager
	gsk         string
	delegateSp  []Spinner
	lspForwards atomic.Int32
	dispatched  []string
	delegate    func(context.Context, map[string]any) (string, error)
	persistFail atomic.Bool
	currentTool []string
	sessionID   string
	progress    string
	guardOnce   sync.Once
	guard       *ToolCallGuardrailController
	guardHook   func()
	beforeFlush func(*[]*state.Message)
	causes      []string
	interrupts  []string
	steers      []int
	steerHook   func(*[]*state.Message, int)
}

func (h *fakeExecutorHost) Env() *Env                       { return h.env }
func (h *fakeExecutorHost) SessionID() string               { return h.sessionID }
func (h *fakeExecutorHost) TurnID() string                  { return "" }
func (h *fakeExecutorHost) APIRequestID() string            { return "" }
func (h *fakeExecutorHost) ValidToolNames() map[string]bool { return nil }
func (h *fakeExecutorHost) EnabledToolsets() []string       { return h.enabled }
func (h *fakeExecutorHost) DisabledToolsets() []string      { return h.disabled }
func (h *fakeExecutorHost) ContextLength() int              { return h.ctxLength }
func (h *fakeExecutorHost) QuietMode() bool                 { return h.quiet == nil || *h.quiet }
func (h *fakeExecutorHost) VerboseLogging() bool            { return false }
func (h *fakeExecutorHost) ToolProgressMode() string {
	if h.progress == "" {
		return "off"
	}
	return h.progress
}
func (h *fakeExecutorHost) LogPrefix() string   { return "" }
func (h *fakeExecutorHost) LogPrefixChars() int { return 100 }
func (h *fakeExecutorHost) Print(text string) {
	h.mu.Lock()
	h.printed = append(h.printed, text)
	h.mu.Unlock()
}
func (h *fakeExecutorHost) VPrint(string, bool) {}
func (h *fakeExecutorHost) WrapVerbose(prefix, text string) string {
	return prefix + text
}
func (h *fakeExecutorHost) ShouldEmitQuietToolMessages() bool { return false }
func (h *fakeExecutorHost) ShouldStartQuietSpinner() bool     { return false }
func (h *fakeExecutorHost) StartSpinner(string) Spinner       { return nil }
func (h *fakeExecutorHost) CuteToolMessage(string, map[string]any, float64, any) string {
	return ""
}
func (h *fakeExecutorHost) Interrupted() bool { return h.interrupted.Load() }
func (h *fakeExecutorHost) ToolInterruptReason() string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.reason
}
func (h *fakeExecutorHost) Interrupt(message, toolReason string) {
	h.mu.Lock()
	h.interrupts = append(h.interrupts, message+"|"+toolReason)
	h.reason = toolReason
	h.mu.Unlock()
	h.interrupted.Store(true)
}
func (h *fakeExecutorHost) TouchActivity(label string) {
	h.mu.Lock()
	h.activity = append(h.activity, label)
	h.mu.Unlock()
}
func (h *fakeExecutorHost) SetCurrentTool(name string) {
	h.mu.Lock()
	h.currentTool = append(h.currentTool, name)
	h.mu.Unlock()
}
func (h *fakeExecutorHost) Guardrails() *ToolCallGuardrailController {
	if h.guardHook != nil {
		h.guardHook()
	}
	h.guardOnce.Do(func() {
		if h.guard == nil {
			h.guard = NewToolCallGuardrailController(nil)
		}
	})
	return h.guard
}
func (h *fakeExecutorHost) GuardrailBlockResult(ToolGuardrailDecision) string { return "" }
func (h *fakeExecutorHost) AppendGuardrailObservation(_ string, _ map[string]any, result string, _ bool, _ string) string {
	return result
}
func (h *fakeExecutorHost) RecordFileMutationResult(string, map[string]any, string, bool, string) {}
func (h *fakeExecutorHost) ObserveOnboardingResult(_ string, _ map[string]any, result string) string {
	return result
}
func (h *fakeExecutorHost) ObserveToolOutcome(string, bool)                 {}
func (h *fakeExecutorHost) NoteToolResult(string, string, string, string)   {}
func (h *fakeExecutorHost) RecordToolBatch([]*state.Message, []any)         {}
func (h *fakeExecutorHost) SubdirectoryHints(string, map[string]any) string { return "" }
func (h *fakeExecutorHost) ToolResultContentForActiveModel(_ string, result string) any {
	return result
}
func (h *fakeExecutorHost) ToolProgressCallback() func(string, string, string, map[string]any, map[string]any) {
	return h.progressCb
}
func (h *fakeExecutorHost) ToolStartCallback() func(string, string, map[string]any) { return h.startCb }
func (h *fakeExecutorHost) ToolCompleteCallback() func(string, string, map[string]any, string) {
	return h.completeCb
}
func (h *fakeExecutorHost) ToolResultMetadataCallback() func(string, string, map[string]any, string) map[string]any {
	return nil
}
func (h *fakeExecutorHost) CheckpointManager() *CheckpointManager { return h.ckptMgr }
func (h *fakeExecutorHost) FlushMessagesToSessionDB(msgs []*state.Message) (bool, error) {
	if h.flush != nil {
		return h.flush(msgs)
	}
	return true, nil
}
func (h *fakeExecutorHost) BeforeToolFlush(messages *[]*state.Message) {
	if h.beforeFlush != nil {
		h.beforeFlush(messages)
	}
}
func (h *fakeExecutorHost) IncrementalPersistenceFailed() bool { return h.persistFail.Load() }
func (h *fakeExecutorHost) MarkIncrementalPersistenceFailed(cause string) {
	h.mu.Lock()
	h.causes = append(h.causes, cause)
	h.mu.Unlock()
	h.persistFail.Store(true)
}
func (h *fakeExecutorHost) NoteToolDispatched(name string) {
	h.mu.Lock()
	h.dispatched = append(h.dispatched, name)
	h.mu.Unlock()
}
func (h *fakeExecutorHost) DispatchDelegateTask(ctx context.Context, args map[string]any) (string, error) {
	if h.delegate != nil {
		return h.delegate(ctx, args)
	}
	return "", nil
}
func (h *fakeExecutorHost) ContextEngineTool(string) (func(context.Context, map[string]any, *[]*state.Message) (string, error), bool) {
	return nil, false
}
func (h *fakeExecutorHost) MemoryManager() MemoryToolManager { return h.memory }
func (h *fakeExecutorHost) GatewaySessionKey() string        { return h.gsk }
func (h *fakeExecutorHost) SetDelegateSpinner(s Spinner) {
	h.mu.Lock()
	h.delegateSp = append(h.delegateSp, s)
	h.mu.Unlock()
}
func (h *fakeExecutorHost) ForwardDeferredLSPNotes()                      { h.lspForwards.Add(1) }
func (h *fakeExecutorHost) ActiveSandbox(string) Environment              { return nil }
func (h *fakeExecutorHost) AttachLiveNotes(*state.Message)                {}
func (h *fakeExecutorHost) KeepLiveNotesAfterTurnBudget([]*state.Message) {}
func (h *fakeExecutorHost) SyncRewrittenToolRows([]*state.Message, []any) {}
func (h *fakeExecutorHost) ApplyPendingSteer(messages *[]*state.Message, numTools int) {
	h.mu.Lock()
	h.steers = append(h.steers, numTools)
	h.mu.Unlock()
	if h.steerHook != nil {
		h.steerHook(messages, numTools)
	}
}
func (h *fakeExecutorHost) MarkTrimAfterToolBatch() {}
func (h *fakeExecutorHost) SessionSource() string   { return "" }
func (h *fakeExecutorHost) RedactToolArgsForDisplay(_ string, args map[string]any) map[string]any {
	return args
}
func (h *fakeExecutorHost) BuildToolPreview(string, map[string]any) string { return "" }
func (h *fakeExecutorHost) ToolRowEmoji(string, map[string]any) string     { return "" }
func (h *fakeExecutorHost) BuildToolLabel(name string, _ map[string]any) string {
	return name
}

var _ ExecutorHost = (*fakeExecutorHost)(nil)
