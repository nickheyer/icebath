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
}

func (h *fakeExecutorHost) Env() *Env                       { return h.env }
func (h *fakeExecutorHost) SessionID() string               { return "" }
func (h *fakeExecutorHost) TurnID() string                  { return "" }
func (h *fakeExecutorHost) APIRequestID() string            { return "" }
func (h *fakeExecutorHost) ValidToolNames() map[string]bool { return nil }
func (h *fakeExecutorHost) EnabledToolsets() []string       { return nil }
func (h *fakeExecutorHost) DisabledToolsets() []string      { return nil }
func (h *fakeExecutorHost) ContextLength() int              { return 0 }
func (h *fakeExecutorHost) QuietMode() bool                 { return true }
func (h *fakeExecutorHost) VerboseLogging() bool            { return false }
func (h *fakeExecutorHost) ToolProgressMode() string        { return "off" }
func (h *fakeExecutorHost) LogPrefix() string               { return "" }
func (h *fakeExecutorHost) LogPrefixChars() int             { return 100 }
func (h *fakeExecutorHost) Print(string)                    {}
func (h *fakeExecutorHost) VPrint(string, bool)             {}
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
func (h *fakeExecutorHost) Interrupt(_, toolReason string) {
	h.mu.Lock()
	h.reason = toolReason
	h.mu.Unlock()
	h.interrupted.Store(true)
}
func (h *fakeExecutorHost) TouchActivity(label string) {
	h.mu.Lock()
	h.activity = append(h.activity, label)
	h.mu.Unlock()
}
func (h *fakeExecutorHost) SetCurrentTool(string)                             {}
func (h *fakeExecutorHost) Guardrails() *ToolCallGuardrailController          { return nil }
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
	return nil
}
func (h *fakeExecutorHost) ToolStartCallback() func(string, string, map[string]any) { return nil }
func (h *fakeExecutorHost) ToolCompleteCallback() func(string, string, map[string]any, string) {
	return nil
}
func (h *fakeExecutorHost) ToolResultMetadataCallback() func(string, string, map[string]any, string) map[string]any {
	return nil
}
func (h *fakeExecutorHost) CheckpointEnabled() bool                                 { return false }
func (h *fakeExecutorHost) EnsureFileCheckpoint(string, map[string]any, string)     {}
func (h *fakeExecutorHost) EnsureTerminalCheckpoint(string, string)                 {}
func (h *fakeExecutorHost) FlushMessagesToSessionDB([]*state.Message) (bool, error) { return true, nil }
func (h *fakeExecutorHost) BeforeToolFlush(*[]*state.Message)                       {}
func (h *fakeExecutorHost) IncrementalPersistenceFailed() bool                      { return false }
func (h *fakeExecutorHost) MarkIncrementalPersistenceFailed(string)                 {}
func (h *fakeExecutorHost) NoteToolDispatched(string)                               {}
func (h *fakeExecutorHost) DispatchDelegateTask(context.Context, map[string]any) (string, error) {
	return "", nil
}
func (h *fakeExecutorHost) ContextEngineTool(string) (func(context.Context, map[string]any, *[]*state.Message) (string, error), bool) {
	return nil, false
}
func (h *fakeExecutorHost) MemoryManagerTool(string) (func(context.Context, map[string]any) (string, error), bool) {
	return nil, false
}
func (h *fakeExecutorHost) ActiveSandbox(string) Environment              { return nil }
func (h *fakeExecutorHost) AttachLiveNotes(*state.Message)                {}
func (h *fakeExecutorHost) KeepLiveNotesAfterTurnBudget([]*state.Message) {}
func (h *fakeExecutorHost) SyncRewrittenToolRows([]*state.Message, []any) {}
func (h *fakeExecutorHost) ApplyPendingSteer(*[]*state.Message, int)      {}
func (h *fakeExecutorHost) MarkTrimAfterToolBatch()                       {}
func (h *fakeExecutorHost) SessionSource() string                         { return "" }
func (h *fakeExecutorHost) RedactToolArgsForDisplay(_ string, args map[string]any) map[string]any {
	return args
}
func (h *fakeExecutorHost) BuildToolPreview(string, map[string]any) string { return "" }
func (h *fakeExecutorHost) ToolRowEmoji(string, map[string]any) string     { return "" }
func (h *fakeExecutorHost) BuildToolLabel(name string, _ map[string]any) string {
	return name
}

var _ ExecutorHost = (*fakeExecutorHost)(nil)
